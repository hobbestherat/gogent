package permission

import (
	"path/filepath"
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
