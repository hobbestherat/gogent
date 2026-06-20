package diag

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditRecordsEvents(t *testing.T) {
	var buf bytes.Buffer
	a := NewAudit(&buf)
	a.Permission("sess-1", "root", "shell", "rm -rf /tmp/x", false)
	a.ToolCall("sess-1", "", "write")

	out := buf.String()
	for _, want := range []string{
		`msg=permission`,
		"session=sess-1",
		"action=shell",
		"allowed=false",
		`msg=tool_call`,
		"tool=write",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("audit output missing %q\ngot:\n%s", want, out)
		}
	}
	if got := strings.Count(out, "\n"); got != 2 {
		t.Errorf("expected 2 audit records, got %d", got)
	}
}

func TestNilAuditIsSafe(t *testing.T) {
	var a *Audit // nil audit is a safe no-op
	a.Permission("s", "a", "read", "x", true)
	a.ToolCall("s", "a", "read")
}

func TestNewAuditNilSinkDiscards(t *testing.T) {
	a := NewAudit(nil) // must not panic
	a.ToolCall("s", "a", "read")
}

func TestNewAuditFileAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit", "audit.log")
	a, err := NewAuditFile(path)
	if err != nil {
		t.Fatalf("NewAuditFile: %v", err)
	}
	a.Permission("s", "", "write", "notes.txt", true)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if !strings.Contains(string(data), "resource=notes.txt") || !strings.Contains(string(data), "allowed=true") {
		t.Errorf("audit file missing event, got: %s", data)
	}
}
