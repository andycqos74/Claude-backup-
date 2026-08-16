//go:build !windows

package agent

import (
	"io/fs"
	"os"
	"path/filepath"
)

// enableBackupPrivilege is a no-op outside Windows.
func enableBackupPrivilege() {}

func openForBackup(path string) (*os.File, error) { return os.Open(path) }

func lstatForBackup(path string) (os.FileInfo, error) { return os.Lstat(path) }

// walkBackup is a plain filepath.WalkDir on non-Windows platforms, adapted
// to deliver an os.FileInfo to the callback.
func walkBackup(root string, fn walkFunc) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fn(p, nil, err)
		}
		info, ierr := d.Info()
		if ierr != nil {
			return fn(p, nil, ierr)
		}
		return fn(p, info, nil)
	})
}
