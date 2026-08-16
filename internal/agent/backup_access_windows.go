//go:build windows

package agent

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This file lets the Windows agent read files and traverse directories
// whose ACLs deny the service account (typically LocalSystem) — the same
// technique dedicated backup software uses. It enables SeBackupPrivilege
// and opens every handle with FILE_FLAG_BACKUP_SEMANTICS, which instructs
// Windows to grant access based on the backup privilege rather than the
// object's ACL. Directory listing is done through the privileged handle
// (GetFileInformationByHandleEx), because the path-based enumeration APIs
// re-check the ACL and would still be denied.

// enableBackupPrivilege turns on the backup (and restore, for future use)
// privileges in the current process token. Best effort: if the account
// doesn't hold them, access simply falls back to normal ACL checks.
func enableBackupPrivilege() {
	enablePrivilege("SeBackupPrivilege")
	enablePrivilege("SeRestorePrivilege")
}

func enablePrivilege(name string) {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok); err != nil {
		return
	}
	defer tok.Close()
	np, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return
	}
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, np, &luid); err != nil {
		return
	}
	tp := windows.Tokenprivileges{PrivilegeCount: 1}
	tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
	// Ignore the result: ERROR_NOT_ALL_ASSIGNED just means the privilege
	// wasn't held, in which case we operate under normal ACLs.
	windows.AdjustTokenPrivileges(tok, false, &tp, 0, nil, nil)
}

// extendedPath rewrites an absolute path into the \\?\ extended-length form
// so CreateFileW accepts long paths and unusual names.
func extendedPath(p string) string {
	if strings.HasPrefix(p, `\\?\`) {
		return p
	}
	if len(p) >= 2 && p[1] == ':' { // drive-letter path, e.g. E:\...
		return `\\?\` + p
	}
	if strings.HasPrefix(p, `\\`) { // UNC path \\server\share
		return `\\?\UNC\` + p[2:]
	}
	return p
}

// createBackupHandle opens path with backup semantics. openReparse controls
// whether a reparse point (symlink/junction) is opened as itself (Lstat
// semantics) rather than followed.
func createBackupHandle(path string, openReparse bool) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(extendedPath(path))
	if err != nil {
		return windows.InvalidHandle, err
	}
	flags := uint32(windows.FILE_FLAG_BACKUP_SEMANTICS)
	if openReparse {
		flags |= windows.FILE_FLAG_OPEN_REPARSE_POINT
	}
	return windows.CreateFile(p,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, flags, 0)
}

// openForBackup opens a regular file for reading with backup semantics.
func openForBackup(path string) (*os.File, error) {
	h, err := createBackupHandle(path, false)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// lstatForBackup stats a path without following reparse points, using a
// privileged handle so ACL-denied items can still be stat'd.
func lstatForBackup(path string) (os.FileInfo, error) {
	h, err := createBackupHandle(path, true)
	if err != nil {
		return nil, &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	var bi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &bi); err != nil {
		return nil, &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	return backupFileInfo{
		name:  filepath.Base(path),
		size:  int64(bi.FileSizeHigh)<<32 | int64(bi.FileSizeLow),
		attrs: bi.FileAttributes,
		mtime: time.Unix(0, bi.LastWriteTime.Nanoseconds()),
	}, nil
}

// backupFileInfo implements os.FileInfo from Windows file attributes,
// avoiding a second (ACL-checked) stat of items enumerated through a
// privileged directory handle.
type backupFileInfo struct {
	name  string
	size  int64
	attrs uint32
	mtime time.Time
}

func (fi backupFileInfo) Name() string       { return fi.name }
func (fi backupFileInfo) Size() int64        { return fi.size }
func (fi backupFileInfo) ModTime() time.Time { return fi.mtime }
func (fi backupFileInfo) Sys() any           { return nil }
func (fi backupFileInfo) IsDir() bool        { return fi.attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0 }

func (fi backupFileInfo) Mode() os.FileMode {
	var m os.FileMode = 0o666
	if fi.attrs&windows.FILE_ATTRIBUTE_READONLY != 0 {
		m = 0o444
	}
	if fi.attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		m |= os.ModeDir | 0o111
	}
	// Reparse points (symlinks, junctions, cloud placeholders) are surfaced
	// as symlinks so the engine handles them via its symlink path (readlink)
	// instead of recursing into or reading them as regular files.
	if fi.attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		m |= os.ModeSymlink
	}
	return m
}

// walkBackup traverses root with backup semantics, calling fn for each
// entry. It honours fs.SkipDir (skip a directory's contents) and fs.SkipAll
// (stop the walk), matching filepath.WalkDir's contract for the cases the
// scanner uses.
func walkBackup(root string, fn walkFunc) error {
	info, err := lstatForBackup(root)
	if err != nil {
		return fn(root, nil, err)
	}
	werr := walkBackupEntry(root, info, fn)
	if werr == fs.SkipAll || werr == fs.SkipDir {
		return nil
	}
	return werr
}

func walkBackupEntry(path string, info os.FileInfo, fn walkFunc) error {
	if err := fn(path, info, nil); err != nil {
		if err == fs.SkipDir && info.IsDir() {
			return nil // skip this directory's contents, continue siblings
		}
		return err // fs.SkipAll or a real error propagates
	}
	// Only descend into true directories (not reparse-point "symlinks").
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil
	}

	entries, derr := readBackupDir(path)
	if derr != nil {
		// Mirror WalkDir: report the read error against the directory, then
		// continue with whatever entries we managed to read.
		if e := fn(path, info, derr); e != nil {
			return e
		}
	}
	for _, e := range entries {
		child := filepath.Join(path, e.Name())
		if err := walkBackupEntry(child, e, fn); err != nil {
			return err
		}
	}
	return nil
}

// fileIdBothDirInfo mirrors the Win32 FILE_ID_BOTH_DIR_INFO structure
// (x/sys/windows does not export it). Field order and natural alignment
// match the C layout on amd64.
type fileIdBothDirInfo struct {
	NextEntryOffset uint32
	FileIndex       uint32
	CreationTime    int64
	LastAccessTime  int64
	LastWriteTime   int64
	ChangeTime      int64
	EndOfFile       int64
	AllocationSize  int64
	FileAttributes  uint32
	FileNameLength  uint32
	EaSize          uint32
	ShortNameLength uint8
	ShortName       [12]uint16
	FileId          int64
	FileName        [1]uint16
}

func filetimeToTime(v int64) time.Time {
	ft := windows.Filetime{LowDateTime: uint32(v), HighDateTime: uint32(v >> 32)}
	return time.Unix(0, ft.Nanoseconds())
}

// readBackupDir lists a directory's children through a privileged handle so
// the listing succeeds even when the directory ACL denies the account.
func readBackupDir(path string) ([]backupFileInfo, error) {
	h, err := createBackupHandle(path, false)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)

	var out []backupFileInfo
	buf := make([]byte, 64*1024)
	class := uint32(windows.FileIdBothDirectoryRestartInfo)
	for {
		err := windows.GetFileInformationByHandleEx(h, class, &buf[0], uint32(len(buf)))
		if err != nil {
			if err == windows.ERROR_NO_MORE_FILES {
				break
			}
			return out, err
		}
		class = uint32(windows.FileIdBothDirectoryInfo) // subsequent pages
		for offset := 0; ; {
			info := (*fileIdBothDirInfo)(unsafe.Pointer(&buf[offset]))
			nameLen := int(info.FileNameLength) / 2
			nameRunes := unsafe.Slice(&info.FileName[0], nameLen)
			name := windows.UTF16ToString(nameRunes)
			if name != "." && name != ".." {
				out = append(out, backupFileInfo{
					name:  name,
					size:  info.EndOfFile,
					attrs: info.FileAttributes,
					mtime: filetimeToTime(info.LastWriteTime),
				})
			}
			if info.NextEntryOffset == 0 {
				break
			}
			offset += int(info.NextEntryOffset)
		}
	}
	return out, nil
}
