package permission

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type stubPrompter struct {
	decision Decision
	calls    int
	last     Request
}

func (p *stubPrompter) AskPermission(r Request) Decision {
	p.calls++
	p.last = r
	return p.decision
}

func TestRuleAllow(t *testing.T) {
	s := New("")
	s.AddRule(Rule{Action: "write", Resource: "*", Effect: "allow"})
	if err := s.Check(ActionWrite, "foo.txt"); err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
}

func TestRuleDeny(t *testing.T) {
	s := New("")
	s.AddRule(Rule{Action: "*", Resource: "secret*", Effect: "deny"})
	if err := s.Check(ActionRead, "secret.txt"); err == nil {
		t.Fatalf("expected deny")
	}
}

func TestAskNoPrompterDenies(t *testing.T) {
	s := New("")
	if err := s.Check(ActionShell, ""); err == nil {
		t.Fatalf("expected deny when no prompter is installed")
	}
}

func TestAskAllowOnce(t *testing.T) {
	s := New("")
	p := &stubPrompter{decision: DecisionAllow}
	s.SetPrompter(p)
	if err := s.Check(ActionShell, ""); err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
	// Allow-once does not persist: a second check asks again.
	if err := s.Check(ActionShell, ""); err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
	if p.calls != 2 {
		t.Fatalf("expected 2 prompts, got %d", p.calls)
	}
}

func TestAskAlwaysPersistsInMemory(t *testing.T) {
	s := New("")
	p := &stubPrompter{decision: DecisionAlways}
	s.SetPrompter(p)
	if err := s.Check(ActionShell, ""); err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
	if err := s.Check(ActionShell, ""); err != nil {
		t.Fatalf("expected cached allow, got %v", err)
	}
	if p.calls != 1 {
		t.Fatalf("expected 1 prompt after 'always', got %d", p.calls)
	}
}

// TestDiagnosticsGrantDoesNotBlessShell confirms a dedicated ActionDiagnostics
// gate is independent of ActionShell: an "always" grant for diagnostics must not
// also bless the shell tool (and vice versa). This is why diagnostics gets its
// own action rather than reusing ActionShell (issue #42).
func TestDiagnosticsGrantDoesNotBlessShell(t *testing.T) {
	s := New("")
	p := &stubPrompter{decision: DecisionAlways}
	s.SetPrompter(p)

	// Approving diagnostics "always" caches the grant…
	if err := s.Check(ActionDiagnostics, ""); err != nil {
		t.Fatalf("expected diagnostics allow, got %v", err)
	}
	// …but the shell tool must still reach the prompter rather than be covered by
	// that grant. (The stub answers Always, so shell is allowed too — what matters
	// is that it prompted at all.)
	before := p.calls
	_ = s.Check(ActionShell, "")
	if p.calls != before+1 {
		t.Errorf("shell should still prompt after a diagnostics grant: prompts before=%d after=%d", before, p.calls)
	}
}

func TestAlwaysExternalRootCoversChildren(t *testing.T) {
	s := New("")
	p := &stubPrompter{decision: DecisionAlways}
	s.SetPrompter(p)
	if err := s.Check(ActionExternal, "/etc"); err != nil {
		t.Fatalf("expected allow for /etc, got %v", err)
	}
	// A child path under the granted root is covered without re-asking.
	if err := s.Check(ActionExternal, "/etc/hosts"); err != nil {
		t.Fatalf("expected child allow, got %v", err)
	}
	if p.calls != 1 {
		t.Fatalf("expected 1 prompt, got %d", p.calls)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	p := &stubPrompter{decision: DecisionAlways}
	s.SetPrompter(p)
	if err := s.Check(ActionExternal, "/var/data"); err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
	if _, err := filepath.Glob(filepath.Join(dir, "permissions.json")); err != nil {
		t.Fatalf("glob failed: %v", err)
	}

	// A fresh Service over the same dir loads the persisted decision.
	s2 := New(dir)
	if err := s2.Check(ActionExternal, "/var/data/file"); err != nil {
		t.Fatalf("expected persisted allow, got %v", err)
	}
}

func TestPromptReceivesDetail(t *testing.T) {
	s := New("")
	p := &stubPrompter{decision: DecisionDeny}
	s.SetPrompter(p)
	_ = s.CheckWithDetail(ActionShell, "", "rm -rf /tmp/x")
	if p.last.Detail != "rm -rf /tmp/x" {
		t.Fatalf("detail not propagated: %q", p.last.Detail)
	}
	// CheckWithDetail leaves the requester context empty.
	if p.last.Context != (RequestContext{}) {
		t.Fatalf("unexpected context on CheckWithDetail: %+v", p.last.Context)
	}
}

// TestPromptReceivesContext verifies CheckWithContext propagates the requesting
// session/agent to the prompter (issue #55), so the UI can badge and route to it.
func TestPromptReceivesContext(t *testing.T) {
	s := New("")
	p := &stubPrompter{decision: DecisionAllow}
	s.SetPrompter(p)
	rc := RequestContext{SessionID: "session-2", Agent: "agent-7"}
	if err := s.CheckWithContext(rc, ActionShell, "", "ls"); err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
	if p.last.Context != rc {
		t.Fatalf("context not propagated: got %+v, want %+v", p.last.Context, rc)
	}
}

// TestContextNotConsultedWhenResolved confirms the requester context is only
// attached when the request reaches the prompter: a rule-resolved decision never
// prompts, so no context is needed and the prompter is not called.
func TestContextNotConsultedWhenResolved(t *testing.T) {
	s := New("")
	s.AddRule(Rule{Action: "*", Resource: "*", Effect: "allow"})
	p := &stubPrompter{decision: DecisionDeny}
	s.SetPrompter(p)
	if err := s.CheckWithContext(RequestContext{SessionID: "s1"}, ActionShell, "", "ls"); err != nil {
		t.Fatalf("expected rule allow, got %v", err)
	}
	if p.calls != 0 {
		t.Fatalf("prompter consulted despite allow rule: %d calls", p.calls)
	}
}

// decisionPrompter returns a fixed Decision without touching shared state, so it
// is safe to share across the goroutines used by the concurrency test.
type decisionPrompter struct {
	decision Decision
}

func (p *decisionPrompter) AskPermission(Request) Decision { return p.decision }

// TestPermissionsOwnerOnly verifies the grant file (0600) and its config
// directory (0700) are created owner-only, so other local users cannot read what
// the agent is allowed to do (issue #16, CWE-732). The previous 0644/0755 modes
// left both world-readable.
func TestPermissionsOwnerOnly(t *testing.T) {
	// A nested, non-existent config dir so MkdirAll actually creates it and the
	// observed mode is what our code applies, not the harness-created temp dir.
	configDir := filepath.Join(t.TempDir(), "gogent")
	s := New(configDir)
	s.SetPrompter(&decisionPrompter{decision: DecisionAlways})
	if err := s.Check(ActionExternal, "/var/data"); err != nil {
		t.Fatalf("expected allow, got %v", err)
	}

	for _, tc := range []struct {
		name string
		path string
	}{
		{"config dir", configDir},
		{"grant file", filepath.Join(configDir, "permissions.json")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, err := os.Stat(tc.path)
			if err != nil {
				t.Fatalf("stat %s: %v", tc.path, err)
			}
			// No access for group or other, regardless of umask.
			if perm := info.Mode().Perm(); perm&0077 != 0 {
				t.Fatalf("%s is accessible by group/other: %#o", tc.path, perm)
			}
		})
	}
}

// TestPersistConcurrentMutations confirms persist is safe under concurrent use:
// every persisted decision lands in the in-memory state. Issue #16's fix keeps
// the map mutation and its marshalling in a single critical section, so the
// marshalled snapshot always reflects the state at the moment of mutation.
// (File-write ordering is not asserted here: concurrent writes to the same path
// are last-writer-wins by design.)
func TestPersistConcurrentMutations(t *testing.T) {
	s := New(t.TempDir())
	s.SetPrompter(&decisionPrompter{decision: DecisionAlways})

	const n = 64
	resources := make([]string, n)
	for i := range resources {
		resources[i] = fmt.Sprintf("/var/data/%d", i)
	}

	var wg sync.WaitGroup
	for _, r := range resources {
		wg.Add(1)
		go func(r string) {
			defer wg.Done()
			if err := s.Check(ActionExternal, r); err != nil {
				t.Errorf("persist %s: %v", r, err)
			}
		}(r)
	}
	wg.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range resources {
		if got := s.saved[key(ActionExternal, r)]; got != DecisionAlways {
			t.Fatalf("missing in-memory decision for %s: got %q", r, got)
		}
	}
}

// auditRecord captures one resolved decision delivered to the audit sink.
type auditRecord struct {
	rc       RequestContext
	action   Action
	resource string
	allowed  bool
}

func TestAuditSinkRecordsDecisions(t *testing.T) {
	s := New("")
	s.AddRule(Rule{Action: "read", Resource: "*", Effect: "allow"})
	s.AddRule(Rule{Action: "write", Resource: "secret*", Effect: "deny"})

	var got []auditRecord
	s.SetAuditSink(func(rc RequestContext, a Action, resource string, allowed bool) {
		got = append(got, auditRecord{rc, a, resource, allowed})
	})

	// Allowed by rule.
	if err := s.CheckWithContext(RequestContext{SessionID: "s1"}, ActionRead, "notes.txt", ""); err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
	// Denied by rule.
	if err := s.CheckWithContext(RequestContext{SessionID: "s2", Agent: "sub"}, ActionWrite, "secret.txt", ""); err == nil {
		t.Fatalf("expected deny")
	}
	// Ask with no prompter resolves to deny — still audited.
	if err := s.CheckWithContext(RequestContext{SessionID: "s3"}, ActionShell, "ls", ""); err == nil {
		t.Fatalf("expected deny (no prompter)")
	}

	want := []auditRecord{
		{RequestContext{SessionID: "s1"}, ActionRead, "notes.txt", true},
		{RequestContext{SessionID: "s2", Agent: "sub"}, ActionWrite, "secret.txt", false},
		{RequestContext{SessionID: "s3"}, ActionShell, "ls", false},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d audit records, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("record %d = %+v, want %+v", i, got[i], w)
		}
	}
}

func TestAuditSinkRecordsPromptedDecision(t *testing.T) {
	s := New("")
	s.SetPrompter(&stubPrompter{decision: DecisionAllow})

	var allowed *bool
	s.SetAuditSink(func(rc RequestContext, a Action, resource string, ok bool) {
		allowed = &ok
	})
	if err := s.CheckWithContext(RequestContext{}, ActionShell, "echo hi", ""); err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
	if allowed == nil || !*allowed {
		t.Fatalf("expected an audited allow, got %v", allowed)
	}
}
