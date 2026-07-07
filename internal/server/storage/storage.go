// Package storage abstracts where backup payload data (blobs and manifests)
// physically lives. v1 ships a local-filesystem backend; Google Drive and
// OneDrive backends can be added later by implementing Backend — nothing
// above this interface knows the medium.
package storage

import (
	"errors"
	"io"
)

var ErrNotFound = errors.New("object not found")

// Backend is a flat key -> object store. Keys use forward slashes
// (e.g. "blobs/ab/abcd....zst", "manifests/<id>.jsonl.zst").
type Backend interface {
	// Put streams an object into storage, replacing any existing object.
	Put(key string, r io.Reader) (written int64, err error)
	// Get opens an object for reading. Returns ErrNotFound if missing.
	Get(key string) (io.ReadCloser, error)
	// Has reports whether the object exists.
	Has(key string) (bool, error)
	// Delete removes an object. Deleting a missing object is not an error.
	Delete(key string) error
}

// BlobKey maps a content hash to its storage key, fanned out over a
// two-character prefix directory to keep directory sizes sane.
func BlobKey(hash string) string {
	return "blobs/" + hash[:2] + "/" + hash + ".zst"
}

// ManifestKey maps a snapshot ID to its manifest's storage key.
func ManifestKey(snapshotID string) string {
	return "manifests/" + snapshotID + ".jsonl.zst"
}
