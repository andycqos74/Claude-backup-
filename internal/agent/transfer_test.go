package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestTransferTarget(t *testing.T) {
	// A trailing separator means "drop into this directory under the file's
	// own name"; a plain path is the full target filename.
	cases := []struct {
		dest, filename, want string
	}{
		{"/etc/app/config.yaml", "whatever.yaml", filepath.FromSlash("/etc/app/config.yaml")},
		{"/etc/app/", "config.yaml", filepath.FromSlash("/etc/app/config.yaml")},
		{"/etc/app/", "sub/evil.yaml", filepath.FromSlash("/etc/app/evil.yaml")}, // base name only
	}
	for _, c := range cases {
		got, err := transferTarget(c.dest, c.filename)
		if err != nil {
			t.Errorf("transferTarget(%q,%q): %v", c.dest, c.filename, err)
			continue
		}
		if got != c.want {
			t.Errorf("transferTarget(%q,%q) = %q, want %q", c.dest, c.filename, got, c.want)
		}
	}

	if _, err := transferTarget("", "f"); err == nil {
		t.Error("empty destination should error")
	}
}

func TestTransferTargetExistingDir(t *testing.T) {
	dir := t.TempDir()
	// An existing directory (no trailing slash) is treated as a directory.
	got, err := transferTarget(dir, "dropped.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "dropped.txt") {
		t.Errorf("got %q, want file inside the dir", got)
	}
}

func TestTransferTargetWindowsSeparator(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("backslash directory suffix is Windows-specific")
	}
	got, err := transferTarget(`C:\ProgramData\app\`, "config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(`C:\ProgramData\app`, "config.yaml") {
		t.Errorf("got %q", got)
	}
	_ = os.PathSeparator
}
