package storage

import (
	"io"
	"os"
	"path/filepath"
	"strings"
)

// LocalFS stores objects as plain files under a root directory. Writes go
// through a temp file + rename so partially-written objects are never
// visible.
type LocalFS struct {
	root string
}

func NewLocalFS(root string) (*LocalFS, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &LocalFS{root: root}, nil
}

func (l *LocalFS) path(key string) string {
	return filepath.Join(l.root, filepath.FromSlash(strings.TrimPrefix(key, "/")))
}

func (l *LocalFS) Put(key string, r io.Reader) (int64, error) {
	dst := l.path(key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, r)
	if err != nil {
		tmp.Close()
		return n, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return n, err
	}
	if err := tmp.Close(); err != nil {
		return n, err
	}
	return n, os.Rename(tmp.Name(), dst)
}

func (l *LocalFS) Get(key string) (io.ReadCloser, error) {
	f, err := os.Open(l.path(key))
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	return f, err
}

func (l *LocalFS) Has(key string) (bool, error) {
	_, err := os.Stat(l.path(key))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func (l *LocalFS) Delete(key string) error {
	err := os.Remove(l.path(key))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
