package server

import (
	"log"
	"time"

	"centralbackup/internal/proto"
	"centralbackup/internal/server/storage"
	"centralbackup/internal/server/store"
)

// PruneAndGC applies every job's retention policy, then removes blobs no
// longer referenced by any snapshot manifest.
func (s *Server) PruneAndGC() error {
	if err := s.pruneRetention(); err != nil {
		return err
	}
	return s.gcBlobs()
}

// pruneRetention deletes snapshots that fall outside a job's retention
// policy (keep newest KeepLast + everything younger than KeepDays). The
// newest snapshot of a job is never deleted, and jobs with no policy keep
// everything.
func (s *Server) pruneRetention() error {
	rows, err := s.store.ListJobs("")
	if err != nil {
		return err
	}
	for _, row := range rows {
		job := row.Job
		if job.KeepLast <= 0 && job.KeepDays <= 0 {
			continue
		}
		snaps, err := s.store.ListSnapshots(s.backendKey(), "", job.ID) // newest first, this backend
		if err != nil {
			return err
		}
		cutoff := time.Now().AddDate(0, 0, -job.KeepDays).Unix()
		for i, sn := range snaps {
			if i == 0 {
				continue // always keep the newest snapshot
			}
			if job.KeepLast > 0 && i < job.KeepLast {
				continue
			}
			if job.KeepDays > 0 && sn.CreatedAt >= cutoff {
				continue
			}
			if err := s.deleteSnapshot(sn.ID); err != nil {
				return err
			}
			log.Printf("retention: pruned snapshot %s of job %s (%s)", sn.ID, job.Name, job.ID)
		}
	}
	return nil
}

// deleteSnapshot removes the snapshot row and its manifest object. Blob
// data is reclaimed by the next gcBlobs pass.
func (s *Server) deleteSnapshot(id string) error {
	sn, err := s.store.GetSnapshot(id)
	if err == store.ErrNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.backend().Delete(sn.ManifestKey); err != nil {
		return err
	}
	return s.store.DeleteSnapshot(id)
}

// blobGCGrace protects blobs uploaded by an in-flight backup whose manifest
// has not been committed yet from being swept (variable for tests).
var blobGCGrace = 24 * time.Hour

// gcBlobs mark-and-sweeps the blob store: everything referenced by any
// snapshot manifest is kept; unreferenced blobs older than the grace period
// are deleted. Holding commitMu (write) excludes concurrent manifest
// commits.
func (s *Server) gcBlobs() error {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()

	// Scope to the active backend: reference-count from this backend's
	// snapshots and sweep only this backend's blob index/objects. Snapshots
	// and blobs belonging to other backends are a separate namespace.
	backend := s.backendKey()
	referenced := map[string]bool{}
	snaps, err := s.store.ListSnapshots(backend, "", "")
	if err != nil {
		return err
	}
	for _, sn := range snaps {
		err := s.eachManifestEntry(sn.ManifestKey, func(e proto.ManifestEntry) error {
			if e.Type == "f" && e.Hash != "" {
				referenced[e.Hash] = true
			}
			return nil
		})
		if err != nil {
			// A single unreadable manifest must abort the sweep: we can no
			// longer prove which blobs are unreferenced.
			return err
		}
	}

	all, err := s.store.AllBlobs(backend)
	if err != nil {
		return err
	}
	graceCutoff := time.Now().Add(-blobGCGrace).Unix()
	removed := 0
	for hash, createdAt := range all {
		if referenced[hash] || createdAt > graceCutoff {
			continue
		}
		if err := s.backend().Delete(storage.BlobKey(hash)); err != nil {
			return err
		}
		if err := s.store.DeleteBlob(backend, hash); err != nil {
			return err
		}
		removed++
	}
	if removed > 0 {
		log.Printf("gc: removed %d unreferenced blobs", removed)
	}
	return nil
}
