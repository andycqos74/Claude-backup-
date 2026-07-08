package agent

import (
	"path/filepath"
	"testing"
)

func TestExcluded(t *testing.T) {
	root := filepath.FromSlash("/data")
	cases := []struct {
		patterns []string
		path     string
		want     bool
	}{
		{[]string{"*.tmp"}, "/data/a/b/file.tmp", true},
		{[]string{"*.tmp"}, "/data/a/b/file.txt", false},
		{[]string{"node_modules"}, "/data/app/node_modules", true},
		{[]string{"node_modules"}, "/data/app/src", false},
		{[]string{"cache/*"}, "/data/cache/x", true},
		{[]string{"cache/*"}, "/data/other/x", false},
		{[]string{}, "/data/anything", false},
		{[]string{"", " "}, "/data/anything", false},
	}
	for _, c := range cases {
		got := excluded(c.patterns, root, filepath.FromSlash(c.path))
		if got != c.want {
			t.Errorf("excluded(%v, %s) = %v, want %v", c.patterns, c.path, got, c.want)
		}
	}
}

func TestRestoreTarget(t *testing.T) {
	// Original location.
	got, err := restoreTarget("/home/x/file.txt", "")
	if err != nil || got != filepath.FromSlash("/home/x/file.txt") {
		t.Errorf("original: got %q err %v", got, err)
	}
	// Re-rooted, unix path.
	got, err = restoreTarget("/home/x/file.txt", filepath.FromSlash("/tmp/r"))
	if err != nil || got != filepath.FromSlash("/tmp/r/home/x/file.txt") {
		t.Errorf("re-rooted: got %q err %v", got, err)
	}
	// Re-rooted, windows path: drive colon stripped.
	got, err = restoreTarget("C:/Users/x/file.txt", filepath.FromSlash("/tmp/r"))
	if err != nil || got != filepath.FromSlash("/tmp/r/C/Users/x/file.txt") {
		t.Errorf("windows: got %q err %v", got, err)
	}
	// Path traversal rejected.
	if _, err := restoreTarget("/../../etc/passwd", filepath.FromSlash("/tmp/r")); err == nil {
		t.Error("expected error for traversal path")
	}
}

func TestRestoreMatch(t *testing.T) {
	if !restoreMatch("/a/b/c.txt", nil) {
		t.Error("empty prefixes should match everything")
	}
	if !restoreMatch("/a/b/c.txt", []string{"a/b"}) {
		t.Error("prefix should match")
	}
	if restoreMatch("/a/bb/c.txt", []string{"a/b"}) {
		t.Error("sibling dir must not match (no partial segment matches)")
	}
	if !restoreMatch("/a/b", []string{"a/b"}) {
		t.Error("exact match should match")
	}
}

func TestYamlJobRoundTrip(t *testing.T) {
	// Omitted enabled/catchup must default to true.
	y := YamlJob{Name: "j", Paths: []string{"/x"}}
	j := y.toProto()
	if !j.Enabled || !j.Catchup {
		t.Errorf("defaults: enabled=%v catchup=%v, want true/true", j.Enabled, j.Catchup)
	}
	j.Enabled = false
	back := yamlFromProto(j)
	if back.Enabled == nil || *back.Enabled {
		t.Error("round-trip lost enabled=false")
	}
	if !jobSpecEqual(back.toProto(), j) {
		t.Error("round-trip changed the job spec")
	}
	// Version/identity fields must not affect spec equality.
	j2 := j
	j2.Version = 99
	j2.Origin = "server"
	j2.AgentID = "a"
	if !jobSpecEqual(j, j2) {
		t.Error("metadata fields must be ignored by jobSpecEqual")
	}
}
