package diag

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoggerLevels(t *testing.T) {
	var buf bytes.Buffer
	lg := New(&buf)
	lg.Infof("info %d", 1)
	lg.Warnf("warn %d", 2)
	lg.Errorf("err %d", 3)

	out := buf.String()
	for _, want := range []string{"INFO info 1", "WARN warn 2", "ERROR err 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
	// Each record is one line.
	if got := strings.Count(out, "\n"); got != 3 {
		t.Errorf("expected 3 lines, got %d", got)
	}
}

func TestNewNilSinkDiscards(t *testing.T) {
	lg := New(nil) // must not panic, must not write anywhere
	lg.Warnf("discarded %s", "msg")
}

func TestNilLoggerIsSafe(t *testing.T) {
	var lg *Logger // a nil *Logger is a safe no-op
	lg.Infof("noop")
	lg.Warnf("noop")
	lg.Errorf("noop")
}

func TestNewFileCreatesParentAndAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "gogent.log")
	lg, err := NewFile(path)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	lg.Warnf("hello %s", "world")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "hello world") {
		t.Errorf("log file missing message, got: %s", data)
	}

	// Re-opening appends rather than truncating, so prior diagnostics survive.
	lg2, err := NewFile(path)
	if err != nil {
		t.Fatalf("NewFile reopen: %v", err)
	}
	lg2.Errorf("again")
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), "hello world") || !strings.Contains(string(data), "again") {
		t.Errorf("log file should contain both messages, got: %s", data)
	}
}
