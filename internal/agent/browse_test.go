package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestListDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "bfile.txt"), []byte("hi"), 0o644)
	os.WriteFile(filepath.Join(dir, "Afile.txt"), []byte("hello"), 0o644)
	os.Mkdir(filepath.Join(dir, "zsub"), 0o755)

	l := listDir(dir)
	if l.Error != "" {
		t.Fatalf("unexpected error: %s", l.Error)
	}
	if l.Path != filepath.ToSlash(dir) {
		t.Errorf("path = %q, want %q", l.Path, filepath.ToSlash(dir))
	}
	if l.Parent != filepath.ToSlash(filepath.Dir(dir)) {
		t.Errorf("parent = %q, want %q", l.Parent, filepath.ToSlash(filepath.Dir(dir)))
	}
	if len(l.Entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(l.Entries))
	}
	// Directories first, then case-insensitive by name.
	if !l.Entries[0].IsDir || l.Entries[0].Name != "zsub" {
		t.Errorf("dir should sort first, got %+v", l.Entries[0])
	}
	if l.Entries[1].Name != "Afile.txt" || l.Entries[2].Name != "bfile.txt" {
		t.Errorf("files not case-insensitively ordered: %q, %q", l.Entries[1].Name, l.Entries[2].Name)
	}
	// The bfile entry carries a full slash path and a size.
	bf := l.Entries[2]
	if bf.Path != filepath.ToSlash(filepath.Join(dir, "bfile.txt")) || bf.Size != 2 {
		t.Errorf("file entry wrong: %+v", bf)
	}
}

func TestListDirErrors(t *testing.T) {
	l := listDir(filepath.Join(t.TempDir(), "does-not-exist"))
	if l.Error == "" {
		t.Error("listing a missing directory should report an error")
	}
}

func TestListDirRoots(t *testing.T) {
	l := listDir("")
	if runtime.GOOS == "windows" {
		// Empty path lists drive letters; each is a root with no parent.
		if len(l.Entries) == 0 {
			t.Fatal("expected at least one drive")
		}
		for _, e := range l.Entries {
			if !e.IsDir {
				t.Errorf("drive %q should be a directory", e.Name)
			}
		}
	} else {
		// On unix the single root "/" is listed directly, with no parent.
		if l.Path != "/" || l.Parent != "" {
			t.Errorf("root listing: path=%q parent=%q", l.Path, l.Parent)
		}
	}
}

func TestParentDir(t *testing.T) {
	if runtime.GOOS != "windows" {
		if got := parentDir("/a/b"); got != "/a" {
			t.Errorf("parentDir(/a/b) = %q", got)
		}
		if got := parentDir("/"); got != "" {
			t.Errorf("parentDir(/) = %q, want empty (root)", got)
		}
	}
}
