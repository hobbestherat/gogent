package gogent

import (
	"bytes"
	"strings"
	"testing"

	"gogent/internal/agent"
	"gogent/internal/diag"
	"gogent/internal/model"
	"gogent/internal/permission"
)

// TestAuditTrailRecordsPermissionAndToolCalls covers issue #51: once an audit
// sink is installed, resolved permission decisions and tool invocations land on
// the append-only trail.
func TestAuditTrailRecordsPermissionAndToolCalls(t *testing.T) {
	g := NewGogent(t.TempDir())

	var buf bytes.Buffer
	g.SetAudit(diag.NewAudit(&buf))

	// A resolved permission decision is audited through the sink wired in
	// NewGogentWithWorkspace.
	if err := g.GetPermissionService().CheckWithContext(
		permission.RequestContext{SessionID: "sess-1", Agent: "root"},
		permission.ActionWrite, "notes.txt", ""); err != nil {
		t.Fatalf("expected the default write rule to allow, got %v", err)
	}

	// A tool invocation is audited through the session's tool callback.
	sess := model.NewModelSession("main", g.defaultConnection())
	root := agent.NewAgent("root", sess)
	us := g.CreateUserSession("sess-1", root)
	if us.ToolCallback == nil {
		t.Fatal("expected a tool callback to be wired")
	}
	if err := us.ToolCallback("write", map[string]interface{}{"path": "notes.txt"}); err != nil {
		t.Fatalf("tool callback returned error: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		`msg=permission`,
		"action=write",
		"resource=notes.txt",
		"allowed=true",
		`msg=tool_call`,
		"tool=write",
		"session=sess-1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("audit trail missing %q\ngot:\n%s", want, out)
		}
	}
}

// TestAuditDefaultsToDiscard verifies the trail is a safe no-op before SetAudit
// is called (the headless/default path), so audit calls never panic or block.
func TestAuditDefaultsToDiscard(t *testing.T) {
	g := NewGogent(t.TempDir())
	g.auditLog().ToolCall("s", "a", "read") // must not panic
	if err := g.GetPermissionService().CheckWithContext(
		permission.RequestContext{}, permission.ActionRead, "x.txt", ""); err != nil {
		t.Fatalf("default read rule should allow, got %v", err)
	}
}
