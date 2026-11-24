package agent

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"centralbackup/internal/proto"
)

// runPushFile downloads one admin-uploaded file from the server and writes
// it to the client's disk. It runs in the background service, so nothing is
// shown on the client's screen; the outcome is reported back to the server.
func (a *Agent) runPushFile(cmd proto.PushFile) {
	ctx, cancel := context.WithCancel(context.Background())
	a.xferMu.Lock()
	a.xfers[cmd.TransferID] = cancel
	a.xferMu.Unlock()
	defer func() {
		cancel()
		a.xferMu.Lock()
		delete(a.xfers, cmd.TransferID)
		a.xferMu.Unlock()
	}()

	target, err := a.doPushFile(ctx, cmd)
	done := proto.TransferDone{TransferID: cmd.TransferID, Path: target}
	switch {
	case errors.Is(err, context.Canceled):
		done.Status = proto.RunCancelled
		log.Printf("[transfer %s] cancelled", cmd.TransferID)
	case err != nil:
		done.Status = proto.RunError
		done.Error = err.Error()
		log.Printf("[transfer %s] failed: %v", cmd.TransferID, err)
	default:
		done.Status = proto.RunSuccess
		log.Printf("[transfer %s] wrote %s", cmd.TransferID, target)
	}
	if err := a.send(proto.MsgTransferDone, done); err != nil {
		log.Printf("[transfer %s] could not report completion: %v", cmd.TransferID, err)
	}
}

func (a *Agent) doPushFile(ctx context.Context, cmd proto.PushFile) (string, error) {
	target, err := transferTarget(cmd.DestPath, cmd.Filename)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(target); err == nil && !cmd.Overwrite {
		return target, fmt.Errorf("%s already exists on the client (overwrite not enabled)", target)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return target, err
	}

	rc, err := a.client.getTransferContent(ctx, cmd.TransferID)
	if err != nil {
		return target, fmt.Errorf("fetch file: %w", err)
	}
	defer rc.Close()
	dec, err := zstd.NewReader(rc)
	if err != nil {
		return target, err
	}
	defer dec.Close()

	tmp, err := os.CreateTemp(filepath.Dir(target), ".cbpush-*")
	if err != nil {
		return target, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed away

	hasher := sha256.New()
	pr := &pushProgress{a: a, id: cmd.TransferID, total: cmd.Size}
	_, err = io.Copy(tmp, io.TeeReader(dec.IOReadCloser(), io.MultiWriter(hasher, pr)))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return target, err
	}
	if cmd.Hash != "" {
		if got := hex.EncodeToString(hasher.Sum(nil)); got != cmd.Hash {
			return target, fmt.Errorf("downloaded data corrupt (hash mismatch)")
		}
	}
	if runtime.GOOS != "windows" && cmd.Mode != 0 {
		os.Chmod(tmpName, os.FileMode(cmd.Mode))
	}
	if err := os.Rename(tmpName, target); err != nil {
		return target, err
	}
	pr.flush()
	return target, nil
}

// runPullFile reads a file off the client's disk and uploads it to the
// server. Runs in the background service; the operator observes it only from
// the server side.
func (a *Agent) runPullFile(cmd proto.PullFile) {
	ctx, cancel := context.WithCancel(context.Background())
	a.xferMu.Lock()
	a.xfers[cmd.TransferID] = cancel
	a.xferMu.Unlock()
	defer func() {
		cancel()
		a.xferMu.Lock()
		delete(a.xfers, cmd.TransferID)
		a.xferMu.Unlock()
	}()

	err := a.doPullFile(ctx, cmd)
	done := proto.TransferDone{TransferID: cmd.TransferID}
	switch {
	case errors.Is(err, context.Canceled):
		done.Status = proto.RunCancelled
		log.Printf("[transfer %s] pull cancelled", cmd.TransferID)
	case err != nil:
		done.Status = proto.RunError
		done.Error = err.Error()
		log.Printf("[transfer %s] pull failed: %v", cmd.TransferID, err)
	default:
		done.Status = proto.RunSuccess
		log.Printf("[transfer %s] pulled %s", cmd.TransferID, cmd.SourcePath)
	}
	if err := a.send(proto.MsgTransferDone, done); err != nil {
		log.Printf("[transfer %s] could not report completion: %v", cmd.TransferID, err)
	}
}

func (a *Agent) doPullFile(ctx context.Context, cmd proto.PullFile) error {
	src := filepath.FromSlash(cmd.SourcePath)
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}

	// produce writes the raw (uncompressed) payload; it is zstd-compressed
	// on the way to the server. A folder is sent as a zip of its whole tree.
	var produce func(w io.Writer) error
	var total int64
	format := ""
	if fi.IsDir() {
		format = "zip"
		total = dirSize(src)
		produce = func(w io.Writer) error { return zipDir(ctx, src, w) }
	} else {
		f, err := openForBackup(src)
		if err != nil {
			return err
		}
		defer f.Close()
		total = fi.Size()
		produce = func(w io.Writer) error { _, err := io.Copy(w, f); return err }
	}

	pr, pw := io.Pipe()
	prog := &pushProgress{a: a, id: cmd.TransferID, total: total}
	copyDone := make(chan struct{})
	go func() {
		defer close(copyDone)
		enc, cerr := zstd.NewWriter(pw)
		if cerr != nil {
			pw.CloseWithError(cerr)
			return
		}
		cerr = produce(io.MultiWriter(enc, prog))
		if cerr == nil {
			cerr = enc.Close()
		}
		pw.CloseWithError(cerr)
	}()

	// Tie the pipe to ctx so a cancellation unblocks a parked Read/Write
	// immediately (see the matching note in engine.uploadFile).
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			pr.CloseWithError(ctx.Err())
		case <-watchDone:
		}
	}()

	uploadErr := a.client.putTransferContent(ctx, cmd.TransferID, format, pr)
	pr.CloseWithError(fmt.Errorf("upload finished"))
	close(watchDone)
	<-copyDone
	if uploadErr != nil {
		return uploadErr
	}
	prog.flush()
	return nil
}

// dirSize totals the regular files under root, for progress reporting only.
func dirSize(root string) int64 {
	var n int64
	walkBackup(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			n += info.Size()
		}
		return nil
	})
	return n
}

// zipDir writes a zip archive of root — every subfolder and file in it — to
// w. Entries are named relative to root's parent, so the archive unpacks
// into a folder of the same name. Files or folders that cannot be read
// (locked, access denied) are skipped rather than failing the whole pull,
// and listed in a _pull_errors.txt entry inside the archive. Symlinks are
// not followed.
func zipDir(ctx context.Context, root string, w io.Writer) error {
	zw := zip.NewWriter(w)
	base := filepath.Dir(filepath.Clean(root))
	var skipped []string

	err := walkBackup(root, func(p string, info os.FileInfo, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", p, err))
			if info != nil && info.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(base, p)
		if rerr != nil {
			return rerr
		}
		name := filepath.ToSlash(rel)
		switch {
		case info.IsDir():
			if p != root && isPseudoFS(p) {
				return fs.SkipDir
			}
			_, err := zw.CreateHeader(&zip.FileHeader{Name: name + "/", Modified: info.ModTime()})
			return err
		case !info.Mode().IsRegular():
			return nil // symlinks, devices, sockets, pipes
		}

		f, oerr := openForBackup(p)
		if oerr != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", p, oerr))
			return nil
		}
		defer f.Close()
		hdr, herr := zip.FileInfoHeader(info)
		if herr != nil {
			return herr
		}
		hdr.Name = name
		hdr.Method = zip.Deflate
		zf, cerr := zw.CreateHeader(hdr)
		if cerr != nil {
			return cerr
		}
		if _, cerr := io.Copy(zf, f); cerr != nil {
			// The entry is already half-written, so the archive can't
			// recover from a mid-file read error; fail the pull.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read %s: %w", p, cerr)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(skipped) > 0 {
		ew, err := zw.Create("_pull_errors.txt")
		if err != nil {
			return err
		}
		fmt.Fprintf(ew, "%d item(s) could not be read and were left out:\r\n", len(skipped))
		for _, s := range skipped {
			fmt.Fprintf(ew, "%s\r\n", s)
		}
	}
	return zw.Close()
}

// transferTarget resolves where to write on the client. A destination that
// ends in a path separator, or that names an existing directory, is treated
// as a directory the file is dropped into; otherwise it is the full target
// path.
func transferTarget(dest, filename string) (string, error) {
	dest = strings.TrimSpace(dest)
	if dest == "" {
		return "", fmt.Errorf("no destination path given")
	}
	isDir := strings.HasSuffix(dest, "/") || strings.HasSuffix(dest, `\`)
	if !isDir {
		if fi, err := os.Stat(dest); err == nil && fi.IsDir() {
			isDir = true
		}
	}
	if isDir {
		base := filepath.Base(filepath.FromSlash(strings.ReplaceAll(filename, `\`, "/")))
		if base == "." || base == string(os.PathSeparator) || base == "" {
			return "", fmt.Errorf("cannot derive a filename for a directory destination")
		}
		return filepath.Join(filepath.FromSlash(dest), base), nil
	}
	return filepath.FromSlash(dest), nil
}

// pushProgress is an io.Writer that reports bytes written back to the server,
// throttled so a large file does not flood the control socket.
type pushProgress struct {
	a     *Agent
	id    string
	total int64
	done  int64
	last  time.Time
}

func (p *pushProgress) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if time.Since(p.last) >= 700*time.Millisecond {
		p.last = time.Now()
		p.report()
	}
	return len(b), nil
}

func (p *pushProgress) flush() { p.report() }

func (p *pushProgress) report() {
	p.a.send(proto.MsgTransferProgress, proto.TransferProgress{
		TransferID: p.id, BytesDone: p.done, BytesTotal: p.total,
	})
}
