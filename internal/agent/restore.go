package agent

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"centralbackup/internal/proto"
)

func (a *Agent) runRestore(cmd proto.Restore) {
	a.runs.Lock()
	defer a.runs.Unlock()

	start := time.Now()
	stats, err := a.doRestore(cmd)
	stats.DurationMS = time.Since(start).Milliseconds()

	done := proto.RunDone{RunID: cmd.RunID, Stats: stats}
	switch {
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

func (a *Agent) doRestore(cmd proto.Restore) (proto.RunStats, error) {
	var stats proto.RunStats
	where := "original locations"
	if cmd.TargetDir != "" {
		where = cmd.TargetDir
	}
	a.runLog(cmd.RunID, "info", fmt.Sprintf("restoring snapshot %s to %s", cmd.SnapshotID, where))

	rc, err := a.client.getManifest(cmd.SnapshotID)
	if err != nil {
		return stats, fmt.Errorf("fetch manifest: %w", err)
	}
	defer rc.Close()
	dec, err := zstd.NewReader(rc)
	if err != nil {
		return stats, err
	}
	defer dec.Close()

	// First pass: collect matching entries (dirs first is guaranteed by
	// walk order within the stream).
	var entries []proto.ManifestEntry
	sc := bufio.NewScanner(dec)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var e proto.ManifestEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return stats, fmt.Errorf("corrupt manifest: %w", err)
		}
		if !restoreMatch(e.Path, cmd.Paths) {
			continue
		}
		entries = append(entries, e)
		if e.Type == "f" {
			stats.FilesTotal++
			stats.BytesTotal += e.Size
		}
	}
	if err := sc.Err(); err != nil {
		return stats, err
	}
	if len(entries) == 0 {
		return stats, fmt.Errorf("nothing in the snapshot matches the selected paths")
	}

	progress := a.newProgressReporter(cmd.RunID, "restoring")
	var done, doneBytes int64
	for _, e := range entries {
		target, err := restoreTarget(e.Path, cmd.TargetDir)
		if err != nil {
			a.runLog(cmd.RunID, "warn", e.Path+": "+err.Error())
			stats.FilesSkipped++
			continue
		}
		switch e.Type {
		case "d":
			if err := os.MkdirAll(target, os.FileMode(e.Mode)|0o700); err != nil {
				a.runLog(cmd.RunID, "warn", target+": "+err.Error())
				stats.FilesSkipped++
			}
		case "l":
			if _, err := os.Lstat(target); err == nil {
				if !cmd.Overwrite {
					stats.FilesSkipped++
					a.runLog(cmd.RunID, "warn", target+": exists, skipped (overwrite disabled)")
					continue
				}
				os.Remove(target)
			}
			os.MkdirAll(filepath.Dir(target), 0o755)
			if err := os.Symlink(e.Target, target); err != nil {
				a.runLog(cmd.RunID, "warn", target+": symlink: "+err.Error())
				stats.FilesSkipped++
			}
		case "f":
			n, err := a.restoreFile(e, target, cmd.Overwrite)
			if err != nil {
				a.runLog(cmd.RunID, "warn", target+": "+err.Error())
				stats.FilesSkipped++
				continue
			}
			if n < 0 {
				stats.FilesSkipped++
				a.runLog(cmd.RunID, "warn", target+": exists, skipped (overwrite disabled)")
				continue
			}
			done++
			doneBytes += n
			stats.FilesChanged++
			progress.update(done, stats.FilesTotal, doneBytes, stats.BytesTotal)
		}
	}
	a.runLog(cmd.RunID, "info", fmt.Sprintf("restore finished: %d files written (%s), %d skipped",
		stats.FilesChanged, fmtBytes(doneBytes), stats.FilesSkipped))
	return stats, nil
}

// restoreFile downloads a blob, verifies its hash and writes it to target
// atomically. Returns -1 when skipped because the file exists.
func (a *Agent) restoreFile(e proto.ManifestEntry, target string, overwrite bool) (int64, error) {
	if _, err := os.Lstat(target); err == nil && !overwrite {
		return -1, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, err
	}
	rc, err := a.client.getBlob(e.Hash)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	dec, err := zstd.NewReader(rc)
	if err != nil {
		return 0, err
	}
	defer dec.Close()

	tmp, err := os.CreateTemp(filepath.Dir(target), ".restore-*")
	if err != nil {
		return 0, err
	}
	hasher := sha256.New()
	n, err := io.Copy(tmp, io.TeeReader(dec.IOReadCloser(), hasher))
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return 0, err
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != e.Hash {
		os.Remove(tmp.Name())
		return 0, fmt.Errorf("downloaded data corrupt (hash mismatch)")
	}
	if runtime.GOOS != "windows" && e.Mode != 0 {
		os.Chmod(tmp.Name(), os.FileMode(e.Mode))
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		os.Remove(tmp.Name())
		return 0, err
	}
	if e.Mtime != 0 {
		mt := time.Unix(0, e.Mtime)
		os.Chtimes(target, mt, mt)
	}
	return n, nil
}

// restoreMatch mirrors the server's prefix filter (slash paths).
func restoreMatch(path string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	p := strings.TrimLeft(path, "/")
	for _, pre := range prefixes {
		pre = strings.Trim(strings.TrimSpace(pre), "/")
		if pre == "" || p == pre || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	return false
}

// restoreTarget maps a manifest path (slash form, possibly "C:/...") to the
// filesystem location to write. With a target dir, the original path is
// re-rooted beneath it (drive colons stripped).
func restoreTarget(manifestPath, targetDir string) (string, error) {
	if targetDir == "" {
		return filepath.FromSlash(manifestPath), nil
	}
	rel := strings.TrimLeft(manifestPath, "/")
	rel = strings.ReplaceAll(rel, ":", "")
	if rel == "" || strings.Contains(rel, "..") {
		return "", fmt.Errorf("unsafe path in manifest")
	}
	return filepath.Join(targetDir, filepath.FromSlash(rel)), nil
}
