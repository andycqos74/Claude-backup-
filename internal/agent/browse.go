package agent

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"centralbackup/internal/proto"
)

// The remote file browser: the server asks the agent to list a directory on
// the client so the GUI can navigate its filesystem and choose files to push
// or pull. Read-only — this only enumerates directories, it never opens file
// contents (that is what a pull is for).

// maxBrowseEntries caps a single listing so an enormous directory can't
// produce a control-socket frame that trips the server's read limit.
const maxBrowseEntries = 5000

func (a *Agent) handleBrowseDir(cmd proto.BrowseDir) {
	l := listDir(cmd.Path)
	l.RequestID = cmd.RequestID
	if err := a.send(proto.MsgDirListing, l); err != nil {
		log.Printf("[browse %s] could not reply: %v", cmd.RequestID, err)
	}
}

func listDir(reqPath string) proto.DirListing {
	// An empty path means "the filesystem roots".
	if reqPath == "" {
		roots := filesystemRoots()
		if len(roots) == 1 {
			return readDirListing(roots[0])
		}
		l := proto.DirListing{Path: "", Parent: ""}
		for _, r := range roots {
			l.Entries = append(l.Entries, proto.DirEntry{Name: r, Path: r, IsDir: true})
		}
		return l
	}
	return readDirListing(filepath.FromSlash(reqPath))
}

func readDirListing(nativePath string) proto.DirListing {
	clean := filepath.Clean(nativePath)
	l := proto.DirListing{Path: filepath.ToSlash(clean), Parent: parentDir(clean)}

	entries, err := os.ReadDir(clean)
	if err != nil {
		l.Error = err.Error()
		return l
	}
	for i, e := range entries {
		if i >= maxBrowseEntries {
			l.Error = fmt.Sprintf("directory has more than %d entries; only the first %d are shown",
				maxBrowseEntries, maxBrowseEntries)
			break
		}
		var size, mtime int64
		if info, ierr := e.Info(); ierr == nil {
			size = info.Size()
			mtime = info.ModTime().Unix()
		}
		l.Entries = append(l.Entries, proto.DirEntry{
			Name:  e.Name(),
			Path:  filepath.ToSlash(filepath.Join(clean, e.Name())),
			IsDir: e.IsDir(),
			Size:  size,
			Mtime: mtime,
		})
	}
	// Directories first, then case-insensitive by name — the familiar order.
	sort.SliceStable(l.Entries, func(i, j int) bool {
		if l.Entries[i].IsDir != l.Entries[j].IsDir {
			return l.Entries[i].IsDir
		}
		return lessFold(l.Entries[i].Name, l.Entries[j].Name)
	})
	return l
}

// parentDir returns the parent of a native path in slash form, or "" when
// the path is already a filesystem root (so the GUI hides the "up" control,
// or steps up to the drive list on Windows).
func parentDir(nativePath string) string {
	parent := filepath.Dir(nativePath)
	if parent == filepath.Clean(nativePath) {
		return ""
	}
	return filepath.ToSlash(parent)
}

// filesystemRoots is where browsing starts: the drive letters on Windows,
// "/" everywhere else.
func filesystemRoots() []string {
	if runtime.GOOS != "windows" {
		return []string{"/"}
	}
	var roots []string
	for c := 'A'; c <= 'Z'; c++ {
		d := string(c) + `:\`
		if _, err := os.Stat(d); err == nil {
			roots = append(roots, filepath.ToSlash(d)) // e.g. "C:/"
		}
	}
	if len(roots) == 0 {
		roots = []string{"C:/"}
	}
	return roots
}

func lessFold(a, b string) bool {
	na, nb := len(a), len(b)
	for i := 0; i < na && i < nb; i++ {
		ca, cb := lower(a[i]), lower(b[i])
		if ca != cb {
			return ca < cb
		}
	}
	return na < nb
}

func lower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}
