// Derived from github.com/e2b-dev/infra, packages/shared/pkg/storage/storage_fs.go at commit
// cf536c2 (Copyright 2023 FoundryLabs, Inc., Apache-2.0, see ../LICENSE-e2b). Modified for
// loam: the Backend interface, multipart writes through part files, no signed upload URLs.

package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// fsBackend keeps objects as files under a directory: for tests, and a single node that needs
// no bucket. Every write lands through a temporary file and a rename, so a reader never sees a
// partial object.
type fsBackend struct {
	base string
}

// NewFS opens (creating it if need be) a directory as a backend.
func NewFS(base string) (Backend, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	return &fsBackend{base: base}, nil
}

func (b *fsBackend) String() string { return "file://" + b.base }

func (b *fsBackend) path(key string) (string, error) {
	if err := ValidKey(key); err != nil {
		return "", err
	}
	return filepath.Join(b.base, filepath.FromSlash(key)), nil
}

func notExist(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotExist
	}
	return err
}

func (b *fsBackend) Get(_ context.Context, key string) ([]byte, error) {
	p, err := b.path(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	return data, notExist(err)
}

func (b *fsBackend) Put(_ context.Context, key string, data []byte) error {
	p, err := b.path(key)
	if err != nil {
		return err
	}
	return writeAtomic(p, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	})
}

// writeAtomic writes path through a temporary file in its directory and a rename.
func writeAtomic(path string, fill func(*os.File) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	err = fill(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

func (b *fsBackend) OpenRange(_ context.Context, key string, off, n int64) (io.ReadCloser, error) {
	p, err := b.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, notExist(err)
	}
	return struct {
		io.Reader
		io.Closer
	}{io.NewSectionReader(f, off, n), f}, nil
}

func (b *fsBackend) List(_ context.Context, prefix string) ([]string, error) {
	// Walk only the deepest directory the prefix names in full.
	dir := ""
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		dir = prefix[:i]
	}
	root := b.base
	if dir != "" {
		if err := ValidKey(dir); err != nil {
			return nil, err
		}
		root = filepath.Join(b.base, filepath.FromSlash(dir))
	}
	var keys []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".tmp-") || strings.HasPrefix(d.Name(), ".part-") {
			return nil
		}
		rel, err := filepath.Rel(b.base, p)
		if err != nil {
			return err
		}
		if key := filepath.ToSlash(rel); strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
		return nil
	})
	sort.Strings(keys)
	return keys, err
}

func (b *fsBackend) Delete(_ context.Context, key string) error {
	p, err := b.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Drop directories the removal emptied, so a deleted layer or snapshot leaves nothing behind.
	for d := filepath.Dir(p); d != b.base && strings.HasPrefix(d, b.base); d = filepath.Dir(d) {
		if os.Remove(d) != nil {
			break
		}
	}
	return nil
}

func (b *fsBackend) NewMultipart(_ context.Context, key string) (Multipart, error) {
	p, err := b.path(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	var r [8]byte
	rand.Read(r[:])
	return &fsMultipart{path: p, prefix: filepath.Join(filepath.Dir(p), ".part-"+hex.EncodeToString(r[:])+"-"), parts: map[int]string{}}, nil
}

// fsMultipart keeps each part in a file of its own until Complete concatenates them.
type fsMultipart struct {
	path   string
	prefix string
	mu     sync.Mutex
	parts  map[int]string
	done   bool
}

func (m *fsMultipart) UploadPart(_ context.Context, n int, data ...[]byte) error {
	name := fmt.Sprintf("%s%05d", m.prefix, n)
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	for _, d := range data {
		if _, err = f.Write(d); err != nil {
			break
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(name)
		return err
	}
	m.mu.Lock()
	m.parts[n] = name
	m.mu.Unlock()
	return nil
}

func (m *fsMultipart) Complete(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	nums := make([]int, 0, len(m.parts))
	for n := range m.parts {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	err := writeAtomic(m.path, func(f *os.File) error {
		for _, n := range nums {
			src, err := os.Open(m.parts[n])
			if err != nil {
				return err
			}
			_, err = io.Copy(f, src)
			src.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		m.done = true
		m.removeParts()
	}
	return err
}

func (m *fsMultipart) Abort(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.done {
		m.removeParts()
	}
	return nil
}

func (m *fsMultipart) removeParts() {
	for _, name := range m.parts {
		os.Remove(name)
	}
	m.parts = map[int]string{}
}
