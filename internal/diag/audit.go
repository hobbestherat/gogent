package diag

import (
	"context"
	"io"
	"log/slog"
)

// Audit is a separate, append-only structured log for security-relevant events —
// permission decisions and tool calls — kept apart from the diagnostic stream so
// it survives as a post-incident artifact: the record of what the agent was
// allowed to do, and what it did (issue #51).
//
// A nil *Audit is a safe no-op, so wiring is optional: code paths emit audit
// events unconditionally and the events are simply dropped when no sink is set.
type Audit struct {
	sl *slog.Logger
}

// NewAudit returns an audit log writing text-format records to w. A nil sink is
// discarded via io.Discard.
func NewAudit(w io.Writer) *Audit {
	if w == nil {
		w = io.Discard
	}
	return &Audit{sl: slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))}
}

// NewAuditFile opens (or creates) an append-only audit log at path. The parent
// directory is created if needed.
func NewAuditFile(path string) (*Audit, error) {
	f, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	return NewAudit(f), nil
}

func (a *Audit) log(msg string, args ...any) {
	if a == nil || a.sl == nil {
		return
	}
	a.sl.Log(context.Background(), slog.LevelInfo, msg, args...)
}

// Permission records the outcome of a permission check: which session/agent
// asked, the action and resource, and whether it was allowed.
func (a *Audit) Permission(session, agent, action, resource string, allowed bool) {
	a.log("permission",
		slog.String("session", session),
		slog.String("agent", agent),
		slog.String("action", action),
		slog.String("resource", resource),
		slog.Bool("allowed", allowed),
	)
}

// ToolCall records that a session invoked a tool. Arguments are intentionally
// omitted — they may carry file contents or secrets — so only the tool name is
// kept; the permission events above capture the resource a side-effecting tool
// touched.
func (a *Audit) ToolCall(session, agent, tool string) {
	a.log("tool_call",
		slog.String("session", session),
		slog.String("agent", agent),
		slog.String("tool", tool),
	)
}
