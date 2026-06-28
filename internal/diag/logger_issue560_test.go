package diag

// Issue #560 — the attached TUI redirects BOTH the diag.Logger and the standard
// library's log package off os.Stderr onto one shared append file so neither
// bleeds onto turbotui's alternate screen. OpenLogFile is the seam that hands out
// that shared *os.File. These tests cover it directly plus criterion 6b's
// mechanism: while the redirect is in place, a stdlib log.Printf lands in the
// diagnostics file and NOT on fd 2.

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenLogFileCreatesParentAndIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "gogent.log")
	f, err := OpenLogFile(path)
	if err != nil {
		t.Fatalf("OpenLogFile: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("OpenLogFile did not create the file: %v", statErr)
	}
	info, _ := os.Stat(path)
	// openAppend creates the file 0600 (owner-only) so other local users can't
	// read the diagnostics/audit-style contents (CWE-732).
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("log file mode = %o, want 0600", mode)
	}
}

func TestOpenLogFileAppendsAcrossOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gogent.log")
	f1, err := OpenLogFile(path)
	if err != nil {
		t.Fatalf("OpenLogFile f1: %v", err)
	}
	if _, err := f1.WriteString("first\n"); err != nil {
		t.Fatalf("write f1: %v", err)
	}
	_ = f1.Close()

	f2, err := OpenLogFile(path)
	if err != nil {
		t.Fatalf("OpenLogFile f2: %v", err)
	}
	_ = f2.Close()

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "first") {
		t.Errorf("reopen should append, not truncate; got: %q", data)
	}
}

// TestStdlibLogRedirectedOffStderr is criterion 6b: with the redirect the design
// installs (log.SetOutput on the shared diag file), a stdlib log.Printf must land
// in the diagnostics file and must NOT appear on os.Stderr. This is the exact
// mechanism attach.go uses; proving it here pins the no-flash guarantee without
// needing to run the full attach loop (which requires a live daemon).
func TestStdlibLogRedirectedOffStderr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gogent.log")
	f, err := OpenLogFile(path)
	if err != nil {
		t.Fatalf("OpenLogFile: %v", err)
	}
	t.Cleanup(func() {
		_ = f.Close()
		// Restore the process-wide default so no other test is affected.
		log.SetOutput(os.Stderr)
	})

	// Capture os.Stderr so we can prove nothing was written there.
	rErr, wErr, errPipe := os.Pipe()
	if errPipe != nil {
		t.Fatalf("pipe: %v", errPipe)
	}
	t.Cleanup(func() { _ = rErr.Close() })
	origStderr := os.Stderr
	os.Stderr = wErr
	t.Cleanup(func() { os.Stderr = origStderr; _ = wErr.Close() })

	// The redirect: both sinks share one handle, exactly as runAttached does.
	logger := New(f)
	log.SetOutput(f)

	logger.Warnf("diag-sink-marker")
	log.Printf("stdlib-sink-marker apr_test")

	// Flush + read whatever (if anything) landed on the captured stderr.
	_ = wErr.Close()
	var stderrBuf bytes.Buffer
	_, _ = io.Copy(&stderrBuf, rErr)

	if strings.Contains(stderrBuf.String(), "stdlib-sink-marker") || strings.Contains(stderrBuf.String(), "diag-sink-marker") {
		t.Errorf("a diagnostics line leaked onto os.Stderr (alternate-screen flash): %q", stderrBuf.String())
	}

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "diag-sink-marker") {
		t.Errorf("diag.Logger output missing from the shared file: %q", data)
	}
	// stdlib log default flags prepend a timestamp; match on the message body.
	if !strings.Contains(string(data), "stdlib-sink-marker") {
		t.Errorf("stdlib log.Printf output missing from the shared file: %q", data)
	}
}
