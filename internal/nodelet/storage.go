package nodelet

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// Every disk the nodelet makes is a clone: a VM's disk of its template's cached
// image or of a snapshot, a snapshot's disk of the VM's. On a volume with
// reflinks (XFS with reflink=1, btrfs) a clone is instant and shares every
// block until one side writes it; that is what makes a sandbox cost only what
// it changed, and a checkpoint cost milliseconds. --data must be such a volume,
// and the nodelet refuses to start on one that isn't, rather than quietly
// copying 20 GiB per sandbox. --dev-allow-copy lifts that for development on a
// laptop's ext4 or a tmpfs: clones become sparse copies.

// ProbeReflink asks the filesystem directly rather than guessing from its type.
func ProbeReflink(dir string) bool {
	src, err := os.CreateTemp(dir, ".reflink-probe-*")
	if err != nil {
		return false
	}
	defer os.Remove(src.Name())
	defer src.Close()
	dst, err := os.CreateTemp(dir, ".reflink-probe-*")
	if err != nil {
		return false
	}
	defer os.Remove(dst.Name())
	defer dst.Close()
	if _, err := src.Write(make([]byte, 4096)); err != nil {
		return false
	}
	return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())) == nil
}

// cloner makes clones, by reflink or (when allowed) by sparse copy.
type cloner struct {
	allowCopy bool
}

// clone makes dst a copy of src, through a temporary file so dst is only ever
// whole: as a reflink, or as a sparse copy when the volume has no reflinks and
// copies are allowed.
func (c cloner) clone(src, dst string) error {
	tmp := dst + ".tmp"
	err := reflink(src, tmp)
	if err != nil && c.allowCopy && errors.Is(err, errNoReflink) {
		err = sparseCopyFile(src, tmp)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

var errNoReflink = errors.New("the volume does not support reflinks")

func reflink(src, dst string) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	defer s.Close()
	d, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := unix.IoctlFileClone(int(d.Fd()), int(s.Fd())); err != nil {
		if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTTY) {
			return fmt.Errorf("reflink %s: %w (%v)", filepath.Base(src), errNoReflink, err)
		}
		return fmt.Errorf("reflink %s: %w", filepath.Base(src), err)
	}
	return nil
}

// sparseCopyFile copies src's data extents to dst and leaves its holes holes,
// using SEEK_DATA and SEEK_HOLE.
func sparseCopyFile(src, dst string) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	defer s.Close()
	fi, err := s.Stat()
	if err != nil {
		return err
	}
	d, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer d.Close()
	size := fi.Size()
	if err := d.Truncate(size); err != nil {
		return err
	}
	for off := int64(0); off < size; {
		data, err := s.Seek(off, unix.SEEK_DATA)
		if errors.Is(err, syscall.ENXIO) {
			break // nothing but a hole to the end
		}
		if err != nil {
			return err
		}
		hole, err := s.Seek(data, unix.SEEK_HOLE)
		if err != nil {
			return err
		}
		if _, err := s.Seek(data, io.SeekStart); err != nil {
			return err
		}
		if _, err := d.Seek(data, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(d, s, hole-data); err != nil {
			return err
		}
		off = hole
	}
	return d.Sync()
}

// allocated is the disk space path occupies (shared reflinked blocks counted
// in full: phase 1 reports no sharing).
func allocated(path string) int64 {
	var st syscall.Stat_t
	if syscall.Stat(path, &st) != nil {
		return 0
	}
	return st.Blocks * 512
}

// freeBytes is the space left on dir's volume.
func freeBytes(dir string) int64 {
	var st unix.Statfs_t
	if unix.Statfs(dir, &st) != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// writeFileAtomic writes b to path through a temporary file and an fsync, so
// a crash leaves the old content or the new, never half.
func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
