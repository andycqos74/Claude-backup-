package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"centralbackup/internal/proto"
)

// The backup engine is file-level and content-addressed: every snapshot's
// manifest lists all files with their SHA-256 hashes, and only blobs the
// server doesn't already have are uploaded. Incremental runs skip
// re-reading files whose size+mtime match the previous manifest; full runs
// re-read and re-hash everything.

const hookTimeout = 30 * time.Minute

func (a *Agent) runBackup(cmd proto.RunBackup) {
	a.runs.Lock()
	defer a.runs.Unlock()

	ctx := a.beginRun(cmd.RunID)
	defer a.endRun(cmd.RunID)

	start := time.Now()
	stats, snapshotID, err := a.doBackup(ctx, cmd)
	stats.DurationMS = time.Since(start).Milliseconds()

	done := proto.RunDone{RunID: cmd.RunID, SnapshotID: snapshotID, Stats: stats}
	switch {
	case errors.Is(err, context.Canceled):
		done.Status = proto.RunCancelled
		a.runLog(cmd.RunID, "info", "backup cancelled; no snapshot was created")
	case err != nil:
		done.Status = proto.RunError
		done.Error = err.Error()
	case stats.FilesSkipped > 0:
		done.Status = proto.RunPartial
	default:
		done.Status = proto.RunSuccess
	}
	a.runDone(done)
}

type scanResult struct {
	entries []proto.ManifestEntry
	stats   proto.RunStats
}

func (a *Agent) doBackup(ctx context.Context, cmd proto.RunBackup) (proto.RunStats, string, error) {
	job := cmd.Job
	var stats proto.RunStats
	a.runLog(cmd.RunID, "info", fmt.Sprintf("starting %s backup of job %q", cmd.Mode, job.Name))

	if job.PreHook != "" {
		if out, err := runHook(job.PreHook); err != nil {
			return stats, "", fmt.Errorf("pre-hook failed: %v: %s", err, out)
		} else if out != "" {
			a.runLog(cmd.RunID, "info", "pre-hook: "+out)
		}
	}
	if job.PostHook != "" {
		defer func() {
			if out, err := runHook(job.PostHook); err != nil {
				a.runLog(cmd.RunID, "warn", fmt.Sprintf("post-hook failed: %v: %s", err, out))
			} else if out != "" {
				a.runLog(cmd.RunID, "info", "post-hook: "+out)
			}
		}()
	}

	// Previous manifest gives the size+mtime shortcut for incrementals.
	var prev map[string]proto.ManifestEntry
	if cmd.Mode == proto.ModeIncremental {
		prev = a.loadPrevManifest(ctx, job.ID, cmd.PrevSnapshotID)
	}

	res, err := a.scan(ctx, cmd, prev)
	if err != nil {
		return stats, "", err
	}
	stats = res.stats
	// scan() stops early on cancellation without returning an error (it
	// just has an incomplete result), so check explicitly here.
	if err := ctx.Err(); err != nil {
		return stats, "", err
	}

	if err := a.uploadMissing(ctx, cmd, res, &stats); err != nil {
		return stats, "", err
	}
	if err := ctx.Err(); err != nil {
		return stats, "", err
	}

	// A cancelled run commits no snapshot: partial data is never presented
	// as a restorable backup.
	snapshotID, err := a.commitManifest(ctx, cmd, res)
	if err != nil {
		return stats, "", err
	}
	a.runLog(cmd.RunID, "info", fmt.Sprintf(
		"snapshot %s committed: %d files (%d changed, %d skipped), %s uploaded",
		snapshotID, stats.FilesTotal, stats.FilesChanged, stats.FilesSkipped, fmtBytes(stats.BytesUploaded)))
	return stats, snapshotID, nil
}

// scan walks all job paths, reusing hashes from prev for files whose
// size+mtime are unchanged and hashing the rest. On cancellation it returns
// early with whatever was collected so far and a nil error; callers must
// check ctx.Err() themselves to detect that case.
func (a *Agent) scan(ctx context.Context, cmd proto.RunBackup, prev map[string]proto.ManifestEntry) (*scanResult, error) {
	res := &scanResult{}
	progress := a.newProgressReporter(cmd.RunID, "scanning")
	var toHash []int // indices into res.entries

	for _, root := range cmd.Job.Paths {
		if ctx.Err() != nil {
			break
		}
		root = filepath.Clean(root)
		// walkBackup enumerates with OS backup semantics where available
		// (on Windows this uses SeBackupPrivilege so files/dirs whose ACLs
		// deny the service account are still readable); elsewhere it is a
		// plain filepath.WalkDir.
		walk := func(path string, info os.FileInfo, walkErr error) error {
			if ctx.Err() != nil {
				return filepath.SkipAll
			}
			if walkErr != nil {
				a.runLog(cmd.RunID, "warn", fmt.Sprintf("%s: %v", path, walkErr))
				res.stats.FilesSkipped++
				if info != nil && info.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if path != root && excluded(cmd.Job.Excludes, root, path) {
				if info.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			// Kernel pseudo-filesystems hold no data worth storing and are
			// largely unreadable; descending into one buries a run pointed
			// at a filesystem root in thousands of permission errors.
			if info.IsDir() && path != root && isPseudoFS(path) {
				a.runLog(cmd.RunID, "info", path+": skipping kernel filesystem")
				return fs.SkipDir
			}
			e := proto.ManifestEntry{
				Path:  filepath.ToSlash(path),
				Mode:  uint32(info.Mode().Perm()),
				Mtime: info.ModTime().UnixNano(),
			}
			switch {
			case info.Mode()&fs.ModeSymlink != 0:
				target, err := os.Readlink(path)
				if err != nil {
					a.runLog(cmd.RunID, "warn", fmt.Sprintf("%s: %v", path, err))
					res.stats.FilesSkipped++
					return nil
				}
				e.Type, e.Target = "l", target
			case info.IsDir():
				e.Type = "d"
			case info.Mode().IsRegular():
				e.Type = "f"
				e.Size = info.Size()
				res.stats.FilesTotal++
				res.stats.BytesTotal += e.Size
				if p, ok := prev[e.Path]; ok && p.Size == e.Size && p.Mtime == e.Mtime && p.Hash != "" {
					e.Hash = p.Hash
				}
			default:
				return nil // sockets, devices, pipes: not backed up
			}
			res.entries = append(res.entries, e)
			if e.Type == "f" && e.Hash == "" {
				toHash = append(toHash, len(res.entries)-1)
			}
			progress.update(res.stats.FilesTotal, 0, res.stats.BytesTotal, 0)
			return nil
		}
		if err := walkBackup(root, walk); err != nil {
			return nil, err
		}
	}

	// Hash pass for new/changed files.
	progress.phase = "hashing"
	hashed := int64(0)
	kept := res.entries[:0]
	drop := map[int]bool{}
	for _, idx := range toHash {
		if ctx.Err() != nil {
			break
		}
		e := &res.entries[idx]
		hash, err := hashFile(fromSlash(e.Path))
		if err != nil {
			a.runLog(cmd.RunID, "warn", fmt.Sprintf("%s: %v", e.Path, err))
			res.stats.FilesSkipped++
			res.stats.FilesTotal--
			res.stats.BytesTotal -= e.Size
			drop[idx] = true
			continue
		}
		e.Hash = hash
		hashed++
		res.stats.FilesChanged++
		progress.update(hashed, int64(len(toHash)), res.stats.BytesTotal, 0)
	}
	if len(drop) > 0 {
		for i, e := range res.entries {
			if !drop[i] {
				kept = append(kept, e)
			}
		}
		res.entries = kept
	}
	return res, nil
}

// uploadMissing asks the server which content hashes it lacks and uploads
// those blobs (zstd-compressed). Files that vanish or change mid-upload are
// re-hashed once; entries that still can't be made consistent are dropped
// so the manifest never references data the server doesn't hold.
func (a *Agent) uploadMissing(ctx context.Context, cmd proto.RunBackup, res *scanResult, stats *proto.RunStats) error {
	byHash := map[string]int{} // hash -> representative entry index
	for i, e := range res.entries {
		if e.Type == "f" && e.Hash != "" {
			if _, ok := byHash[e.Hash]; !ok {
				byHash[e.Hash] = i
			}
		}
	}
	missing, err := a.checkAllBlobs(ctx, keys(byHash))
	if err != nil {
		return err
	}

	var uploadTotal int64
	for _, h := range missing {
		uploadTotal += res.entries[byHash[h]].Size
	}
	progress := a.newProgressReporter(cmd.RunID, "uploading")

	var mu sync.Mutex
	var uploaded, uploadedFiles int64
	var firstErr error
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
uploadLoop:
	for _, h := range missing {
		if ctx.Err() != nil {
			break
		}
		idx := byHash[h]
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break uploadLoop
		}
		wg.Add(1)
		go func(hash string, idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			n, newHash, err := a.uploadFile(ctx, fromSlash(res.entries[idx].Path), hash)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			if newHash != "" && newHash != hash {
				// File changed between hashing and upload.
				res.entries[idx].Hash = newHash
				a.runLog(cmd.RunID, "warn", res.entries[idx].Path+": changed during backup, stored current content")
			}
			uploaded += n
			uploadedFiles++
			progress.update(uploadedFiles, int64(len(missing)), uploaded, uploadTotal)
		}(h, idx)
	}
	wg.Wait()
	if firstErr != nil {
		return fmt.Errorf("blob upload failed: %w", firstErr)
	}
	stats.BytesUploaded = uploaded
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Final consistency check: drop any entry whose blob the server still
	// doesn't have (e.g. same-hash siblings of a file that changed mid-run).
	finalHashes := map[string]bool{}
	for _, e := range res.entries {
		if e.Type == "f" && e.Hash != "" {
			finalHashes[e.Hash] = true
		}
	}
	stillMissing, err := a.checkAllBlobs(ctx, mapKeys(finalHashes))
	if err != nil {
		return err
	}
	if len(stillMissing) > 0 {
		missSet := map[string]bool{}
		for _, h := range stillMissing {
			missSet[h] = true
		}
		kept := res.entries[:0]
		for _, e := range res.entries {
			if e.Type == "f" && missSet[e.Hash] {
				a.runLog(cmd.RunID, "warn", e.Path+": data unavailable, excluded from snapshot")
				stats.FilesSkipped++
				stats.FilesTotal--
				stats.BytesTotal -= e.Size
				continue
			}
			kept = append(kept, e)
		}
		res.entries = kept
	}
	return nil
}

// checkAllBlobs batches the hash existence check.
func (a *Agent) checkAllBlobs(ctx context.Context, hashes []string) ([]string, error) {
	var missing []string
	const batch = 1000
	for i := 0; i < len(hashes); i += batch {
		end := min(i+batch, len(hashes))
		m, err := a.client.checkBlobs(ctx, hashes[i:end])
		if err != nil {
			return nil, err
		}
		missing = append(missing, m...)
	}
	return missing, nil
}

// uploadFile streams one file zstd-compressed to the server, verifying the
// hash on the way. If the content no longer matches expectHash, the file is
// re-uploaded under its current hash, which is returned.
func (a *Agent) uploadFile(ctx context.Context, path, expectHash string) (rawBytes int64, actualHash string, err error) {
	for attempt := 0; attempt < 2; attempt++ {
		if ctx.Err() != nil {
			return 0, "", ctx.Err()
		}
		f, err := openForBackup(path)
		if err != nil {
			return 0, "", fmt.Errorf("%s: %w", path, err)
		}
		hasher := sha256.New()
		var read int64
		pr, pw := io.Pipe()
		copyDone := make(chan struct{})
		go func() {
			defer close(copyDone)
			enc, cerr := zstd.NewWriter(pw)
			if cerr != nil {
				pw.CloseWithError(cerr)
				return
			}
			n, cerr := io.Copy(enc, io.TeeReader(f, hasher))
			read = n
			if cerr == nil {
				cerr = enc.Close()
			}
			pw.CloseWithError(cerr)
		}()

		// Explicitly tie the pipe to ctx rather than relying on net/http to
		// propagate cancellation into a custom io.Reader request body: if
		// the transport's body-forwarding goroutine is parked on a Read
		// from pr waiting for more compressed bytes, closing the
		// connection (what ctx cancellation triggers internally) does not
		// unblock that Read, and both this goroutine and the writer above
		// would hang forever. Closing pr ourselves unblocks any pending
		// Read *and* Write on the pipe immediately.
		watchDone := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				pr.CloseWithError(ctx.Err())
			case <-watchDone:
			}
		}()

		// First attempt sends under the expected hash; the server verifies
		// decompressed content, so a changed file is rejected there.
		uploadErr := a.client.putBlob(ctx, expectHash, pr)
		pr.CloseWithError(fmt.Errorf("upload finished")) // unblock writer if still running
		close(watchDone)
		<-copyDone
		f.Close()
		got := hex.EncodeToString(hasher.Sum(nil))

		if uploadErr == nil {
			return read, got, nil
		}
		if ctx.Err() != nil {
			return 0, "", ctx.Err() // cancelled mid-upload, not a content mismatch
		}
		if got == expectHash {
			return 0, "", uploadErr // genuine upload failure
		}
		// Content changed since hashing: retry under the current hash.
		expectHash = got
	}
	return 0, "", fmt.Errorf("%s: keeps changing during upload", path)
}

// commitManifest streams the manifest (zstd JSONL) to the server and caches
// it locally as the base for the next incremental run.
func (a *Agent) commitManifest(ctx context.Context, cmd proto.RunBackup, res *scanResult) (string, error) {
	tmp, err := os.CreateTemp(a.stateDir, "manifest-*.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	enc, err := zstd.NewWriter(tmp)
	if err != nil {
		tmp.Close()
		return "", err
	}
	bw := bufio.NewWriter(enc)
	je := json.NewEncoder(bw)
	for _, e := range res.entries {
		if err := je.Encode(e); err != nil {
			tmp.Close()
			return "", err
		}
	}
	if err := bw.Flush(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := enc.Close(); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		return "", err
	}

	snapshotID, err := a.client.commitSnapshot(ctx, cmd.Job.ID, cmd.RunID, cmd.Mode,
		res.stats.FilesTotal, res.stats.BytesTotal, tmp)
	tmp.Close()
	if err != nil {
		return "", err
	}
	if err := copyFile(tmp.Name(), a.manifestCachePath(cmd.Job.ID)); err != nil {
		a.runLog(cmd.RunID, "warn", "could not cache manifest locally: "+err.Error())
	}
	return snapshotID, nil
}

func (a *Agent) manifestCachePath(jobID string) string {
	return filepath.Join(a.stateDir, "manifest-"+jobID+".jsonl.zst")
}

// loadPrevManifest returns the previous snapshot's entries by path, from
// the local cache or (failing that) fetched from the server. Returning nil
// degrades gracefully to hashing everything.
func (a *Agent) loadPrevManifest(ctx context.Context, jobID, prevSnapshotID string) map[string]proto.ManifestEntry {
	if m := readManifestFile(a.manifestCachePath(jobID)); m != nil {
		return m
	}
	if prevSnapshotID == "" {
		return nil
	}
	rc, err := a.client.getManifest(ctx, prevSnapshotID)
	if err != nil {
		return nil
	}
	defer rc.Close()
	return readManifestStream(rc)
}

func readManifestFile(path string) map[string]proto.ManifestEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	return readManifestStream(f)
}

func readManifestStream(r io.Reader) map[string]proto.ManifestEntry {
	dec, err := zstd.NewReader(r)
	if err != nil {
		return nil
	}
	defer dec.Close()
	out := map[string]proto.ManifestEntry{}
	sc := bufio.NewScanner(dec)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var e proto.ManifestEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Path != "" {
			out[e.Path] = e
		}
	}
	return out
}

// ---- helpers ----

// excluded matches a path against the job's exclude globs: patterns without
// a separator match the base name; patterns with one match the path
// relative to the job root (slash form).
func excluded(patterns []string, root, path string) bool {
	base := filepath.Base(path)
	rel, err := filepath.Rel(root, path)
	relSlash := ""
	if err == nil {
		relSlash = filepath.ToSlash(rel)
	}
	for _, pat := range patterns {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if !strings.ContainsAny(pat, `/\`) {
			if ok, _ := filepath.Match(pat, base); ok {
				return true
			}
		} else if relSlash != "" {
			if ok, _ := filepath.Match(filepath.ToSlash(pat), relSlash); ok {
				return true
			}
		}
	}
	return false
}

func hashFile(path string) (string, error) {
	f, err := openForBackup(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fromSlash(p string) string { return filepath.FromSlash(p) }

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func runHook(command string) (string, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(hookTimeout):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		<-done
		return strings.TrimSpace(string(out)), fmt.Errorf("hook timed out after %s", hookTimeout)
	}
	return strings.TrimSpace(string(out)), err
}

func fmtBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func keys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// progressReporter throttles run_progress messages.
type progressReporter struct {
	a     *Agent
	runID string
	phase string
	last  time.Time
	mu    sync.Mutex
}

func (a *Agent) newProgressReporter(runID, phase string) *progressReporter {
	return &progressReporter{a: a, runID: runID, phase: phase}
}

func (p *progressReporter) update(filesDone, filesTotal, bytesDone, bytesTotal int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.last) < 700*time.Millisecond {
		return
	}
	p.last = time.Now()
	p.a.send(proto.MsgRunProgress, proto.RunProgress{
		RunID: p.runID, Phase: p.phase,
		FilesDone: filesDone, FilesTotal: filesTotal,
		BytesDone: bytesDone, BytesTotal: bytesTotal,
	})
}
