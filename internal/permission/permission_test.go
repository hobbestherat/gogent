package permission

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
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
}

// TestCheckRequestCarriesContext verifies the session/agent attribution rides
// through to the prompter, which is what lets the UI badge the right session.
func TestCheckRequestCarriesContext(t *testing.T) {
	s := New("")
	p := &stubPrompter{decision: DecisionAllow}
	s.SetPrompter(p)
	_ = s.CheckRequest(Request{Action: ActionShell, Session: "session-2", Agent: "root"})
	if p.last.Session != "session-2" || p.last.Agent != "root" {
		t.Fatalf("context not propagated: session=%q agent=%q", p.last.Session, p.last.Agent)
	}
}

// blockingPrompter holds AskPermission inside the call until released, so a test
// can observe the request while it is pending. It records the pending snapshot
// seen at that moment plus every observer notification count.
type blockingPrompter struct {
	s        *Service
	entered  chan struct{}
	release  chan struct{}
	snapshot []Request
}

func (b *blockingPrompter) AskPermission(_ Request) Decision {
	b.snapshot = b.s.PendingRequests()
	close(b.entered)
	<-b.release
	return DecisionAllow
}

// TestPendingRequestsTracksInFlight checks a request is exposed as pending only
// while the prompt is outstanding (added before AskPermission, dropped after),
// and that the observer fires for both transitions.
func TestPendingRequestsTracksInFlight(t *testing.T) {
	s := New("")
	if got := len(s.PendingRequests()); got != 0 {
		t.Fatalf("expected no pending requests initially, got %d", got)
	}

	var notifications int32
	s.SetPendingObserver(func() { atomic.AddInt32(&notifications, 1) })

	bp := &blockingPrompter{s: s, entered: make(chan struct{}), release: make(chan struct{})}
	s.SetPrompter(bp)

	done := make(chan error, 1)
	go func() {
		done <- s.CheckRequest(Request{Action: ActionShell, Session: "session-7"})
	}()

	select {
	case <-bp.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("prompter was never entered")
	}

	// While the prompt is outstanding the request is visible as pending.
	if len(bp.snapshot) != 1 || bp.snapshot[0].Session != "session-7" {
		t.Fatalf("pending snapshot during prompt = %+v, want one request for session-7", bp.snapshot)
	}

	close(bp.release)
	if err := <-done; err != nil {
		t.Fatalf("CheckRequest returned error: %v", err)
	}

	// Once answered, the pending set is empty again.
	if got := len(s.PendingRequests()); got != 0 {
		t.Fatalf("expected pending cleared after answer, got %d", got)
	}
	// One add + one remove notification.
	if n := atomic.LoadInt32(&notifications); n != 2 {
		t.Fatalf("expected 2 observer notifications, got %d", n)
	}
}

// TestPendingRequestsAllowedSkipsTracking confirms a request resolved by policy
// (no prompt) is never published as pending and fires no observer.
func TestPendingRequestsAllowedSkipsTracking(t *testing.T) {
	s := New("")
	s.AddRule(Rule{Action: "write", Resource: "*", Effect: "allow"})
	var notifications int32
	s.SetPendingObserver(func() { atomic.AddInt32(&notifications, 1) })
	if err := s.Check(ActionWrite, "foo.txt"); err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
	if n := atomic.LoadInt32(&notifications); n != 0 {
		t.Fatalf("policy-resolved check should not touch pending set, got %d notifications", n)
	}
	if got := len(s.PendingRequests()); got != 0 {
		t.Fatalf("expected no pending requests, got %d", got)
	}
}
