package nodelet

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// A template's disk is built from its container image on first use and kept
// (docs/NODE.md, "Images"). The image is pulled with go-containerregistry (no
// podman, no daemon), its layers flattened into one tar, and that tar written
// into an ext4 filesystem by mke2fs -d (e2fsprogs >= 1.47.1 reads tar), so the
// ownership recorded in the image comes out right without a user namespace.
//
// The cache is keyed by the digest the reference resolves to. A reference
// pinned by digest never touches the network once cached; one by tag is
// resolved with a HEAD on every create, which is what makes a moved tag show up.
// The flattened tar is kept as <digest>.tar, and one disk per size as
// <digest>-<N>g.ext4: mke2fs writes the inode tables and the journal of the
// whole size once, there, and every VM's disk is a reflink of it that shares
// them. (Growing a small filesystem per VM with resize2fs instead leaves the
// new inode tables for the guest kernel to zero on its first mount, a few
// hundred MiB of writes into every VM's disk.)
//
// Every node builds the same disk from the same image, byte for byte: a
// snapshot's disk is stored as its difference from the template's
// (docs/STORE.md, "Base"), and restored on another node over that node's own
// build of it. mke2fs is made reproducible with a filesystem UUID and
// directory hash seed derived from the digest and size, and a fixed clock.
// (The disks phase 1 built had random ones; the "-r1" in the name keeps them
// apart, and VMs made from them store their disks whole.)
//
// Every use refreshes a file's mtime, and Evict drops what hasn't been used
// for a while. That is safe for the VMs: each VM's disk is a reflink clone,
// a file of its own that keeps the extents it shares with the template.

// buildTimeout bounds one pull-and-build.
const buildTimeout = 30 * time.Minute

// ImageMetaPath is where on the disk the nodelet records the image's config
// for the agent (docs/AGENT.md, "The guest disk"): {"env": [...]}, which the
// agent gives every process, and what the image says to run (its entrypoint,
// command, working directory and user), which loam doesn't run itself but a
// client that deploys the image as a service reads back through the files API.
const ImageMetaPath = "etc/loam/image.json"

// ImageMeta is the content of ImageMetaPath.
type ImageMeta struct {
	Env        []string `json:"env"`
	Entrypoint []string `json:"entrypoint,omitempty"`
	Cmd        []string `json:"cmd,omitempty"`
	Workdir    string   `json:"workdir,omitempty"`
	User       string   `json:"user,omitempty"`
}

// ImageCache builds and keeps template disks.
type ImageCache struct {
	dir     string
	log     *slog.Logger
	options []remote.Option

	mu      sync.Mutex
	flights map[string]*flight
	builds  chan struct{} // one pull-and-build at a time
}

type flight struct {
	done chan struct{}
	path string
	err  error
}

// NewImageCache keeps its files in dir. options are added to every registry call.
func NewImageCache(dir string, log *slog.Logger, options ...remote.Option) (*ImageCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// Left by a build the nodelet died in.
	tmps, _ := filepath.Glob(filepath.Join(dir, ".tmp-*"))
	for _, f := range tmps {
		os.Remove(f)
	}
	return &ImageCache{dir: dir, log: log, flights: map[string]*flight{}, builds: make(chan struct{}, 1),
		options: append([]remote.Option{remote.WithAuthFromKeychain(authn.DefaultKeychain),
			remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"})}, options...)}, nil
}

// Disk returns the cached disk of image at gib GiB, building it first if it is
// not there, and image pinned by its digest. The work is detached from ctx: a
// caller that stops waiting leaves it to finish, and the next create finds it
// ready. The returned file is never removed, so it may be cloned without
// holding anything.
func (c *ImageCache) Disk(ctx context.Context, image string, gib int) (string, string, error) {
	ref, err := name.ParseReference(image, name.StrictValidation)
	if err != nil {
		return "", "", invalid("template image %q: %v", image, err)
	}
	digest, err := c.resolve(ctx, ref)
	if err != nil {
		return "", "", err
	}
	pinned := ref.Context().Digest(digest.String()).String()
	disk := filepath.Join(c.dir, fmt.Sprintf("%s-%dg-r1.ext4", digest.Hex, gib))
	if _, err := os.Stat(disk); err == nil {
		now := time.Now()
		os.Chtimes(disk, now, now) // used: Evict keeps it
		return disk, pinned, nil
	}
	c.mu.Lock()
	f := c.flights[disk]
	if f == nil {
		f = &flight{done: make(chan struct{})}
		c.flights[disk] = f
		go func() {
			bctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
			defer cancel()
			f.path, f.err = c.build(bctx, ref, digest, gib, disk)
			c.mu.Lock()
			delete(c.flights, disk)
			c.mu.Unlock()
			close(f.done)
		}()
	}
	c.mu.Unlock()
	select {
	case <-f.done:
		return f.path, pinned, f.err
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}

// resolve is the digest ref names: its own, or the registry's answer for a tag.
func (c *ImageCache) resolve(ctx context.Context, ref name.Reference) (v1.Hash, error) {
	if d, ok := ref.(name.Digest); ok {
		return v1.NewHash(d.DigestStr())
	}
	desc, err := remote.Head(ref, append(c.options, remote.WithContext(ctx))...)
	if err != nil {
		return v1.Hash{}, fmt.Errorf("resolve %s: %w", ref, err)
	}
	return desc.Digest, nil
}

func (c *ImageCache) build(ctx context.Context, ref name.Reference, digest v1.Hash, gib int, disk string) (string, error) {
	select {
	case c.builds <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-c.builds }()
	start := time.Now()
	tarPath := filepath.Join(c.dir, digest.Hex+".tar")
	if _, err := os.Stat(tarPath); err != nil {
		if err := c.flatten(ctx, ref, digest, tarPath); err != nil {
			return "", err
		}
	}
	tmp := filepath.Join(c.dir, ".tmp-"+filepath.Base(disk))
	defer os.Remove(tmp)
	if err := makeExt4(ctx, tarPath, tmp, int64(gib)<<30, fsUUID(digest, gib)); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, disk); err != nil {
		return "", err
	}
	c.log.Info("template disk built", "image", ref.String(), "digest", digest.String(), "gib", gib, "took", time.Since(start).Round(time.Millisecond))
	return disk, nil
}

// flatten pulls the image and writes its filesystem as one tar at dst.
func (c *ImageCache) flatten(ctx context.Context, ref name.Reference, digest v1.Hash, dst string) error {
	pinned := ref.Context().Digest(digest.String())
	c.log.Info("pulling image", "image", ref.String(), "digest", digest.String())
	img, err := remote.Image(pinned, append(c.options, remote.WithContext(ctx))...)
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return fmt.Errorf("pull %s: config: %w", ref, err)
	}
	if cfg.OS != "" && cfg.OS != "linux" {
		return invalid("%s is a %s image; VMs run linux", ref, cfg.OS)
	}
	env := cfg.Config.Env
	if env == nil {
		env = []string{}
	}
	meta, _ := json.MarshalIndent(ImageMeta{Env: env, Entrypoint: cfg.Config.Entrypoint, Cmd: cfg.Config.Cmd,
		Workdir: cfg.Config.WorkingDir, User: cfg.Config.User}, "", "  ")
	rc := mutate.Extract(img)
	defer rc.Close()
	tmp := dst + ".tmp"
	defer os.Remove(tmp + ".1")
	defer os.Remove(tmp)
	if err := orderTar(rc, tmp, meta); err != nil {
		return fmt.Errorf("flatten %s: %w", ref, err)
	}
	return os.Rename(tmp, dst)
}

// orderTar rewrites a flattened image's tar stream into the order mke2fs -d
// needs, at dst, adding meta at ImageMetaPath. mutate.Extract walks the layers
// top down, so a file from the top layer comes before the directory it is in,
// which a lower layer made, and a hard link can come before its target; mke2fs
// wants a directory before anything in it and a link's target before the link,
// and refuses a directory twice. So: every directory first, parents before
// children (sorted by path), any missing parent made up; then the files,
// symlinks and devices in stream order; then the hard links.
func orderTar(in io.Reader, dst string, meta []byte) error {
	// Pass one: the stream to a scratch file, the directories and hard links
	// (headers only, which are small) aside.
	scratch := dst + ".1"
	sf, err := os.OpenFile(scratch, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer sf.Close()
	dirs := map[string]*tar.Header{}
	var links []*tar.Header
	tr, sw := tar.NewReader(in), tar.NewWriter(sf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		n := cleanName(hdr.Name)
		if n == "" {
			continue // the root directory itself: mkfs makes it
		}
		if n == ImageMetaPath {
			continue // the nodelet's to write, below
		}
		hdr.Name = n
		switch hdr.Typeflag {
		case tar.TypeDir:
			if dirs[n] == nil {
				dirs[n] = hdr
			}
			continue
		case tar.TypeLink:
			hdr.Linkname = cleanName(hdr.Linkname)
			links = append(links, hdr)
			continue
		}
		if err := sw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.Copy(sw, tr); err != nil {
			return err
		}
	}
	if err := sw.Close(); err != nil {
		return err
	}
	now := time.Now()
	if dirs[path.Dir(ImageMetaPath)] == nil {
		dirs[path.Dir(ImageMetaPath)] = &tar.Header{Typeflag: tar.TypeDir, Name: path.Dir(ImageMetaPath), Mode: 0o755, ModTime: now}
	}
	for n := range dirs {
		for p := path.Dir(n); p != "."; p = path.Dir(p) {
			if dirs[p] == nil {
				dirs[p] = &tar.Header{Typeflag: tar.TypeDir, Name: p, Mode: 0o755, ModTime: now}
			}
		}
	}
	// Files' and links' own parents, too, where no layer had the directory.
	for _, h := range links {
		for p := path.Dir(h.Name); p != "."; p = path.Dir(p) {
			if dirs[p] == nil {
				dirs[p] = &tar.Header{Typeflag: tar.TypeDir, Name: p, Mode: 0o755, ModTime: now}
			}
		}
	}
	if _, err := sf.Seek(0, io.SeekStart); err != nil {
		return err
	}
	for r := tar.NewReader(sf); ; {
		hdr, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		for p := path.Dir(hdr.Name); p != "."; p = path.Dir(p) {
			if dirs[p] == nil {
				dirs[p] = &tar.Header{Typeflag: tar.TypeDir, Name: p, Mode: 0o755, ModTime: now}
			}
		}
	}

	// Pass two: the ordered tar.
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	tw := tar.NewWriter(out)
	names := make([]string, 0, len(dirs))
	for n := range dirs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		h := *dirs[n]
		h.Name = n + "/"
		if err := tw.WriteHeader(&h); err != nil {
			return err
		}
	}
	if _, err := sf.Seek(0, io.SeekStart); err != nil {
		return err
	}
	for r := tar.NewReader(sf); ; {
		hdr, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.Copy(tw, r); err != nil {
			return err
		}
	}
	for _, h := range links {
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: ImageMetaPath, Mode: 0o644, Size: int64(len(meta)), ModTime: now}); err != nil {
		return err
	}
	if _, err := tw.Write(meta); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return out.Close()
}

// cleanName makes a tar name relative and slash-free at the ends; "" is the root.
func cleanName(n string) string {
	return strings.TrimPrefix(path.Clean("/"+n), "/")
}

// fsUUID is the filesystem UUID (and directory hash seed) of the template disk
// of digest at gib GiB: the same on every node, and a version 4 UUID in form.
func fsUUID(digest v1.Hash, gib int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("loam template %s %d", digest, gib)))
	h[6] = h[6]&0x0f | 0x40
	h[8] = h[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// templateEpoch is the clock mke2fs sees: every timestamp it makes up itself
// (the superblock's, lost+found's) is this, so two builds are byte for byte
// the same. The files keep the times the image gives them.
const templateEpoch = "1767225600" // 2026-01-01T00:00:00Z

// makeExt4 builds an ext4 filesystem of size bytes at dst from the tar at src,
// reproducibly (uuid; see templateEpoch). Inode tables and the journal are
// written out in full (no lazy init), so a guest never spends its first
// minutes zeroing them into its own disk.
func makeExt4(ctx context.Context, src, dst string, size int64, uuid string) error {
	os.Remove(dst)
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	err = f.Truncate(size)
	f.Close()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "mke2fs", "-q", "-F", "-t", "ext4", "-L", "loam", "-U", uuid, "-d", src,
		"-E", "lazy_itable_init=0,lazy_journal_init=0,root_owner=0:0,hash_seed="+uuid, dst)
	// libarchive refuses a name it can't convert to the locale's charset, and
	// the container's default locale is ASCII; image paths are UTF-8.
	cmd.Env = append(os.Environ(), "LC_ALL=C.UTF-8", "E2FSPROGS_FAKE_TIME="+templateEpoch, "SOURCE_DATE_EPOCH="+templateEpoch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		var ee *exec.Error
		if errors.As(err, &ee) {
			msg = "mke2fs not found (install e2fsprogs)"
		} else if strings.Contains(msg, "libarchive") {
			msg += " (mke2fs reads tar through libarchive, which it loads at run time: install libarchive13)"
		} else if strings.Contains(msg, "tar") || strings.Contains(msg, "Not a directory") {
			msg += " (building a disk from an image needs e2fsprogs 1.47.1 or newer, whose mke2fs reads tar files)"
		}
		return fmt.Errorf("mke2fs: %v: %s", err, msg)
	}
	return nil
}

// evictFloor is how recently used a file must not have been for Evict to drop
// it, whatever the limits say: Disk hands out a path that its caller clones
// without holding anything, so a file must outlive any clone of it in flight.
const evictFloor = time.Hour

// Evict removes cached files (flattened tars and template disks) unused for
// longer than maxAge, then the least recently used ones until the cache is
// within maxBytes (allocated, not apparent: the disks are sparse). Builds in
// flight and anything used within evictFloor are kept. It returns what it
// removed.
func (c *ImageCache) Evict(maxAge time.Duration, maxBytes int64) ([]string, error) {
	type entry struct {
		path  string
		mtime time.Time
		bytes int64
	}
	c.mu.Lock()
	building := map[string]bool{}
	for disk := range c.flights {
		building[disk] = true
	}
	c.mu.Unlock()
	des, err := os.ReadDir(c.dir)
	if err != nil {
		return nil, err
	}
	var entries []entry
	var total int64
	for _, de := range des {
		name := de.Name()
		if strings.HasPrefix(name, ".tmp-") || !(strings.HasSuffix(name, ".tar") || strings.HasSuffix(name, ".ext4")) {
			continue
		}
		path := filepath.Join(c.dir, name)
		var st syscall.Stat_t
		if err := syscall.Stat(path, &st); err != nil {
			continue
		}
		e := entry{path, time.Unix(st.Mtim.Sec, st.Mtim.Nsec), st.Blocks * 512}
		total += e.bytes
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].mtime.Before(entries[j].mtime) })
	now := time.Now()
	var removed []string
	for _, e := range entries {
		if now.Sub(e.mtime) < evictFloor || building[e.path] {
			continue
		}
		if now.Sub(e.mtime) <= maxAge && total <= maxBytes {
			continue
		}
		if err := os.Remove(e.path); err != nil {
			continue
		}
		total -= e.bytes
		removed = append(removed, filepath.Base(e.path))
	}
	if len(removed) > 0 {
		c.log.Info("image cache evicted", "files", removed, "bytes_left", total)
	}
	return removed, nil
}
