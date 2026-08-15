// Package bundle embeds enrollment details into an agent binary so a client
// can be installed by downloading one file and running it with no arguments.
//
// The details are appended after the executable image, followed by a fixed
// footer giving their length. Both PE and ELF ignore trailing bytes, so the
// binary still runs normally; an agent with nothing appended simply finds no
// footer and falls back to command-line flags.
//
// This exists because pasting a long install command was the single biggest
// source of failed enrollments: shell quoting, truncated tokens, and
// PowerShell's TLS quirks all disappear when there is nothing to paste.
package bundle

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// magic marks the footer. Fixed 16 bytes, versioned so the format can change
// without an older agent misreading a newer bundle.
var magic = [16]byte{'C', 'B', 'A', 'G', 'E', 'N', 'T', 'C', 'F', 'G', 'v', '1', 0, 0, 0, 0}

// footerLen is the length prefix plus the magic.
const footerLen = 8 + 16

// maxLen bounds what will be read back, so a corrupt or hostile length can't
// make the agent allocate wildly.
const maxLen = 1 << 20

// Enrollment is what the server bakes in: everything `backup-agent install`
// would otherwise need as flags.
type Enrollment struct {
	ServerURL   string `json:"server_url"`
	Token       string `json:"token"`
	Fingerprint string `json:"fingerprint"`
	Name        string `json:"name,omitempty"`
}

// Append writes exe followed by e and the footer. The caller supplies the
// plain agent binary; the result is a self-enrolling copy of it.
func Append(w io.Writer, exe io.Reader, e Enrollment) error {
	if _, err := io.Copy(w, exe); err != nil {
		return fmt.Errorf("copy agent binary: %w", err)
	}
	blob, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(blob) > maxLen {
		return fmt.Errorf("enrollment blob too large (%d bytes)", len(blob))
	}
	if _, err := w.Write(blob); err != nil {
		return err
	}
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(blob)))
	if _, err := w.Write(size[:]); err != nil {
		return err
	}
	_, err = w.Write(magic[:])
	return err
}

// Read returns the enrollment appended to the binary at path. The second
// result is false when there is none, which is the ordinary case for a
// plain build and is not an error.
func Read(path string) (Enrollment, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return Enrollment{}, false, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return Enrollment{}, false, err
	}
	if st.Size() < footerLen {
		return Enrollment{}, false, nil
	}

	footer := make([]byte, footerLen)
	if _, err := f.ReadAt(footer, st.Size()-footerLen); err != nil {
		return Enrollment{}, false, err
	}
	if [16]byte(footer[8:]) != magic {
		return Enrollment{}, false, nil
	}
	n := int64(binary.BigEndian.Uint64(footer[:8]))
	if n <= 0 || n > maxLen || st.Size()-footerLen-n < 0 {
		return Enrollment{}, false, fmt.Errorf("embedded enrollment has an implausible length (%d)", n)
	}

	blob := make([]byte, n)
	if _, err := f.ReadAt(blob, st.Size()-footerLen-n); err != nil {
		return Enrollment{}, false, err
	}
	var e Enrollment
	if err := json.Unmarshal(blob, &e); err != nil {
		return Enrollment{}, false, fmt.Errorf("embedded enrollment is not valid JSON: %w", err)
	}
	return e, true, nil
}

// ReadSelf returns the enrollment appended to the running executable.
func ReadSelf() (Enrollment, bool, error) {
	exe, err := os.Executable()
	if err != nil {
		return Enrollment{}, false, err
	}
	return Read(exe)
}
