package agent

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestZipDirIncludesSubfolders(t *testing.T) {
	root := filepath.Join(t.TempDir(), "photos")
	files := map[string]string{
		"a.txt":            "top",
		"sub/b.txt":        "one level",
		"sub/deep/c.txt":   "two levels",
		"sub/deep/x/d.bin": "three levels",
	}
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Join(root, "empty"), 0o755)

	var buf bytes.Buffer
	if err := zipDir(context.Background(), root, &buf); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") {
			got[f.Name] = ""
			continue
		}
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = string(b)
	}
	for rel, body := range files {
		if got["photos/"+rel] != body {
			t.Errorf("photos/%s: got %q, want %q", rel, got["photos/"+rel], body)
		}
	}
	if _, ok := got["photos/empty/"]; !ok {
		t.Error("empty subfolder missing from archive")
	}
	if _, ok := got["_pull_errors.txt"]; ok {
		t.Error("unexpected error list for a fully readable tree")
	}
}
