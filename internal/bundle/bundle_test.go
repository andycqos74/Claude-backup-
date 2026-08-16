package bundle

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBundle(t *testing.T, payload []byte, e Enrollment) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Append(&buf, bytes.NewReader(payload), e); err != nil {
		t.Fatalf("Append: %v", err)
	}
	path := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(path, buf.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRoundTrip(t *testing.T) {
	want := Enrollment{
		ServerURL:   "https://backup.example.com:8443",
		Token:       "3f7c2a",
		Fingerprint: "d1e830546c68d9e4",
		Name:        "laptop",
	}
	exe := []byte("\x7fELF not really a binary, but trailing bytes are ignored either way")
	path := writeBundle(t, exe, want)

	got, ok, err := Read(path)
	if err != nil || !ok {
		t.Fatalf("Read: ok=%v err=%v", ok, err)
	}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}

	// The executable image must be byte-identical, or the binary won't run.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, exe) {
		t.Error("the original executable bytes were not preserved verbatim")
	}
}

func TestReadPlainBinaryIsNotAnError(t *testing.T) {
	// A normal build has nothing appended. That is the ordinary case for
	// anyone using command-line flags, and must not look like a failure.
	path := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(path, []byte("just an ordinary binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	e, ok, err := Read(path)
	if err != nil || ok {
		t.Errorf("Read(plain) = %+v ok=%v err=%v, want no enrollment and no error", e, ok, err)
	}

	// Shorter than the footer itself.
	short := filepath.Join(t.TempDir(), "tiny")
	if err := os.WriteFile(short, []byte("hi"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := Read(short); err != nil || ok {
		t.Errorf("Read(tiny) ok=%v err=%v, want no enrollment and no error", ok, err)
	}
}

func TestReadRejectsImplausibleLength(t *testing.T) {
	// A corrupt length must be refused rather than driving a huge
	// allocation or a negative offset.
	path := writeBundle(t, []byte("binary"), Enrollment{ServerURL: "https://x"})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Overwrite the length prefix with something far larger than the file.
	for i := len(data) - footerLen; i < len(data)-16; i++ {
		data[i] = 0xff
	}
	bad := filepath.Join(t.TempDir(), "bad")
	if err := os.WriteFile(bad, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := Read(bad); err == nil || ok {
		t.Errorf("Read(corrupt) ok=%v err=%v, want an error", ok, err)
	}
}

func TestReadRejectsGarbagePayload(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("binary")
	buf.WriteString("not json")
	buf.Write([]byte{0, 0, 0, 0, 0, 0, 0, 8})
	buf.Write(magic[:])
	path := filepath.Join(t.TempDir(), "garbage")
	if err := os.WriteFile(path, buf.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	_, ok, err := Read(path)
	if ok || err == nil || !strings.Contains(err.Error(), "valid JSON") {
		t.Errorf("Read(garbage) ok=%v err=%v, want a JSON error", ok, err)
	}
}
