package storage

import (
	"io"
	"strings"
	"testing"
)

func TestLocalFS(t *testing.T) {
	fs, err := NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := BlobKey("ab12cd")

	if ok, _ := fs.Has(key); ok {
		t.Fatal("Has on missing object")
	}
	if _, err := fs.Get(key); err != ErrNotFound {
		t.Fatalf("Get missing: want ErrNotFound, got %v", err)
	}

	n, err := fs.Put(key, strings.NewReader("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Put: n=%d err=%v", n, err)
	}
	if ok, _ := fs.Has(key); !ok {
		t.Fatal("Has after Put = false")
	}
	rc, err := fs.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello" {
		t.Fatalf("Get content = %q", b)
	}

	// Overwrite replaces content.
	if _, err := fs.Put(key, strings.NewReader("world!")); err != nil {
		t.Fatal(err)
	}
	rc, _ = fs.Get(key)
	b, _ = io.ReadAll(rc)
	rc.Close()
	if string(b) != "world!" {
		t.Fatalf("after overwrite = %q", b)
	}

	if err := fs.Delete(key); err != nil {
		t.Fatal(err)
	}
	if err := fs.Delete(key); err != nil {
		t.Fatalf("double delete must not error: %v", err)
	}
	if ok, _ := fs.Has(key); ok {
		t.Fatal("Has after Delete = true")
	}
}
