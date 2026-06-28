package permission

// Issue #560 — the daemon-side late-decision reconcile persists a sticky grant
// out-of-band via Service.Persist. These tests cover that seam directly:
//   - Persist records always/always_deny and fires the audit sink once (allowed
//     == DecisionAlways), mirroring the in-time CheckWithContext path so a late
//     grant is never off-record.
//   - Non-sticky decisions are ignored (a late one-shot allow never broadens).
//   - The grant survives a reload and stays host-scoped.
//   - Fix 3: a failed write/parse is reported on the diagnostics logger instead
//     of being silently swallowed; a nil logger stays a safe no-op.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gogent/internal/diag"
)

// captureAudit is a counting permission.AuditSink safe for concurrent use.
type captureAudit struct {
	mu      sync.Mutex
	allowed []bool
	calls   int
	last    struct {
		action   Action
		resource string
		session  string
	}
}

func (c *captureAudit) sink(rc RequestContext, action Action, resource string, allowed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.allowed = append(c.allowed, allowed)
	c.last.action = action
	c.last.resource = resource
	c.last.session = rc.SessionID
}

func TestPersistAlwaysRecordsAuditAllow(t *testing.T) {
	s := New("")
	c := &captureAudit{}
	s.SetAuditSink(c.sink)

	s.Persist(RequestContext{SessionID: "s1"}, ActionNetwork, "example.com", DecisionAlways)

	if c.calls != 1 {
		t.Fatalf("audit calls = %d, want 1", c.calls)
	}
	if !c.allowed[0] {
		t.Error("an always-grant should be audited as allowed=true")
	}
	if c.last.action != ActionNetwork || c.last.resource != "example.com" || c.last.session != "s1" {
		t.Errorf("audit captured {%s %s %q}, want {network example.com s1}", c.last.action, c.last.resource, c.last.session)
	}
	// The grant is live in-memory.
	if err := s.CheckWithContext(RequestContext{}, ActionNetwork, "example.com", "https://example.com/"); err != nil {
		t.Fatalf("persisted always-grant not honored: %v", err)
	}
}

func TestPersistAlwaysDenyRecordsAuditDeny(t *testing.T) {
	s := New("")
	c := &captureAudit{}
	s.SetAuditSink(c.sink)

	s.Persist(RequestContext{}, ActionNetwork, "example.com", DecisionAlwaysDeny)

	if c.calls != 1 || c.allowed[0] {
		t.Fatalf("always_deny audit = %d calls allowed=%v, want 1 call allowed=false", c.calls, c.allowed)
	}
	if err := s.CheckWithContext(RequestContext{}, ActionNetwork, "example.com", "https://example.com/"); err == nil {
		t.Error("persisted always_deny should deny the host")
	}
}

func TestPersistIgnoresNonStickyDecisions(t *testing.T) {
	s := New("")
	c := &captureAudit{}
	s.SetAuditSink(c.sink)

	for _, d := range []Decision{DecisionAllow, DecisionDeny} {
		s.Persist(RequestContext{}, ActionNetwork, "example.com", d)
	}
	if c.calls != 0 {
		t.Errorf("non-sticky Persist fired the audit sink %d times, want 0", c.calls)
	}
	if err := s.CheckWithContext(RequestContext{}, ActionNetwork, "example.com", "https://example.com/"); err == nil {
		t.Error("a non-sticky Persist must not broaden into a sticky grant")
	}
}

func TestPersistSurvivesReloadAndIsHostScoped(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	s.Persist(RequestContext{}, ActionNetwork, "example.com", DecisionAlways)

	// Reload from the same dir (simulates a daemon restart).
	reloaded := New(dir)
	if err := reloaded.CheckWithContext(RequestContext{}, ActionNetwork, "example.com", "https://example.com/anywhere"); err != nil {
		t.Fatalf("always-grant did not survive reload: %v", err)
	}
	// Host-scoping: a different host is unaffected.
	if err := reloaded.CheckWithContext(RequestContext{}, ActionNetwork, "other.com", "https://other.com/"); err == nil {
		t.Fatal("granting example.com must not authorize other.com after reload")
	}
	// And the grant actually hit disk.
	data, err := os.ReadFile(filepath.Join(dir, "permissions.json"))
	if err != nil {
		t.Fatalf("read permissions.json: %v", err)
	}
	if !strings.Contains(string(data), "network:example.com") {
		t.Errorf("permissions.json missing network:example.com key: %s", data)
	}
}

// TestPersistAuditFiresEvenWhenWriteFails confirms the audit records the user's
// decision regardless of disk durability: the in-memory grant (and thus the
// session's authorization) is real, so it must be audited even if the file write
// fails. (The write failure is separately diagnosable via the logger — Fix 3.)
func TestPersistAuditFiresEvenWhenWriteFails(t *testing.T) {
	// configDir points under a path whose ancestor is a regular file, so the
	// MkdirAll inside write() fails.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	s := New(filepath.Join(blocker, "nested"))
	c := &captureAudit{}
	s.SetAuditSink(c.sink)

	s.Persist(RequestContext{}, ActionNetwork, "example.com", DecisionAlways)

	if c.calls != 1 || !c.allowed[0] {
		t.Errorf("audit = %d calls allowed=%v, want 1 call allowed=true even when the write fails", c.calls, c.allowed)
	}
}

// --- Fix 3: persistence failures are diagnosable, not silent ---------------

func TestSetLoggerIsNilSafe(t *testing.T) {
	s := New(t.TempDir())
	s.SetLogger(nil) // must not panic.
	s.Persist(RequestContext{}, ActionNetwork, "example.com", DecisionAlways)
	// No prompter → an ask denies; the always-grant should still allow.
	if err := s.CheckWithContext(RequestContext{}, ActionNetwork, "example.com", "https://example.com/"); err != nil {
		t.Fatalf("always-grant not honored with nil logger: %v", err)
	}
}

func TestPersistWriteFailureIsLogged(t *testing.T) {
	var buf threadBuffer
	// configDir nests under a regular file, so write()'s MkdirAll fails with
	// ENOTDIR — robust even under root (you cannot mkdir beneath a file).
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	s := New(filepath.Join(blocker, "nested"))
	s.SetLogger(diag.New(&buf))

	s.Persist(RequestContext{}, ActionNetwork, "example.com", DecisionAlways)

	out := buf.String()
	if !strings.Contains(out, "permission: persist decision to disk") {
		t.Errorf("expected a persist-write failure to be logged, got: %q", out)
	}
}

func TestLoadCorruptStoreIsLogged(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "permissions.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed corrupt store: %v", err)
	}
	var buf threadBuffer
	// SetLogger before New so load() (called inside New) can report. New does not
	// expose a hook, so set the logger on the constructed service and re-trigger
	// load via a fresh ReadFile path: exercise load() directly by constructing then
	// reloading from a second New would lose the logger. Instead, build with the
	// logger via the exported seam and call load() through a re-read by deleting
	// and re-adding — simplest is to assert load() logs when handed a corrupt file:
	s := New(dir)
	s.SetLogger(diag.New(&buf))
	s.load() // re-parse the corrupt store through the logging path.

	out := buf.String()
	if !strings.Contains(out, "permission: parse") {
		t.Errorf("expected a corrupt-store parse to be logged, got: %q", out)
	}
}

// threadBuffer is a concurrency-safe bytes.Buffer for capturing slog output.
type threadBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *threadBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// strings.Builder.Write is documented as never returning an error; consume it
	// so wrapcheck does not flag a forwarded external-package error here.
	n, _ := b.buf.Write(p)
	return n, nil
}
func (b *threadBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
