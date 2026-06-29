package ui

// Issue #560 — client half of the fix. decide() is now transport-only: it blind-
// retries a failed POST (safe because the endpoint is idempotent) and returns the
// daemon's status ("resolved"/"late") + final error. reportDecision() surfaces a
// genuinely-lost decision as a kind-aware [System] notice, and a sticky grant the
// daemon reconciled late ("late") tells the user it will apply going forward. The
// common in-time success is silent. These tests drive the real APIClient+RemoteClient
// against a stub daemon so the wiring (not just the logic) is exercised.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gogent/internal/agent"
	"gogent/internal/gogent"
	"gogent/internal/permission"
)

// withFastRetries swaps the package-level decideRetryBackoffDefault for a near-zero
// schedule and restores it at the end of the test. Each RemoteClient snapshots the
// default at construction (into rc.decideBackoff), so this must be called BEFORE
// NewRemoteClient for the client under test to pick it up. Tests in this package run
// sequentially (none call t.Parallel) and the default is read only on the test
// goroutine at construction, so mutating it here is safe — a background poll/handler
// goroutine that outlives an earlier test reads its own client's snapshot, not this.
func withFastRetries(t *testing.T) {
	t.Helper()
	old := decideRetryBackoffDefault
	decideRetryBackoffDefault = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { decideRetryBackoffDefault = old })
}

// stubDaemon wraps an httptest.Server whose handler consults a per-request hook,
// so each test can script the sequence of status codes/bodies decide() will see.
type stubDaemon struct {
	srv   *httptest.Server
	calls atomic.Int32
	// respond receives the 1-based call number and the request body; it writes the
	// response and returns. nil => default 200 {"status":"resolved"}.
	respond func(call int, body []byte, w http.ResponseWriter)
}

func newStubDaemon(t *testing.T, respond func(call int, body []byte, w http.ResponseWriter)) *stubDaemon {
	t.Helper()
	d := &stubDaemon{respond: respond}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		call := int(d.calls.Add(1))
		if d.respond != nil {
			d.respond(call, body, w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"apr_x","status":"resolved"}`))
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *stubDaemon) client(t *testing.T) *APIClient {
	t.Helper()
	c, err := NewAPIClient(d.srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	return c
}

// capturingSink records every event the RemoteClient emits to its EventSink.
type capturingSink struct {
	mu      sync.Mutex
	events  []agent.SessionEvent
	lastSid string
}

func (c *capturingSink) fn(sessionID string, ev agent.SessionEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
	c.lastSid = sessionID
}

func (c *capturingSink) notices() []agent.SessionEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []agent.SessionEvent
	for _, e := range c.events {
		if e.Type == agent.SessionEventNotice {
			out = append(out, e)
		}
	}
	return out
}

// --- decide() retry behaviour ---------------------------------------------

func TestDecideRetriesTransientThenSucceeds(t *testing.T) {
	withFastRetries(t)
	d := newStubDaemon(t, func(call int, _ []byte, w http.ResponseWriter) {
		if call <= 2 {
			http.Error(w, "transient", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"apr_x","status":"resolved"}`))
	})
	rc := NewRemoteClient(d.client(t), nil, nil)

	status, err := rc.decide("apr_x", "always")
	if err != nil {
		t.Fatalf("decide returned error after retries recovered: %v", err)
	}
	if status != "resolved" {
		t.Errorf("status = %q, want resolved", status)
	}
	if got := d.calls.Load(); got != 3 {
		t.Errorf("daemon calls = %d, want 3 (1 initial + 2 retries)", got)
	}
}

func TestDecideReturnsLateStatusWithoutRetry(t *testing.T) {
	withFastRetries(t)
	d := newStubDaemon(t, func(call int, _ []byte, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"apr_x","status":"late"}`))
	})
	rc := NewRemoteClient(d.client(t), nil, nil)

	status, err := rc.decide("apr_x", "always")
	if err != nil {
		t.Fatalf("decide late: %v", err)
	}
	if status != "late" {
		t.Errorf("status = %q, want late", status)
	}
	if got := d.calls.Load(); got != 1 {
		t.Errorf("a success must not be retried; daemon calls = %d, want 1", got)
	}
}

func TestDecideSuccessFirstTryNoRetry(t *testing.T) {
	withFastRetries(t)
	d := newStubDaemon(t, nil) // default 200 resolved.
	rc := NewRemoteClient(d.client(t), nil, nil)

	if _, err := rc.decide("apr_x", "allow"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if got := d.calls.Load(); got != 1 {
		t.Errorf("daemon calls = %d, want 1", got)
	}
}

func TestDecideExhaustsRetriesAndReturnsError(t *testing.T) {
	withFastRetries(t)
	d := newStubDaemon(t, func(call int, _ []byte, w http.ResponseWriter) {
		http.Error(w, "down", http.StatusBadGateway)
	})
	rc := NewRemoteClient(d.client(t), nil, nil)

	status, err := rc.decide("apr_x", "always")
	if err == nil {
		t.Fatal("decide returned nil error after all retries failed")
	}
	if status != "" {
		t.Errorf("status = %q, want empty on total failure", status)
	}
	if got := d.calls.Load(); got != 3 {
		t.Errorf("daemon calls = %d, want 3", got)
	}
}

func TestDecideStopsRetryingOnContextCancel(t *testing.T) {
	// A long backoff so the cancel lands inside the retry wait, not after it.
	old := decideRetryBackoffDefault
	decideRetryBackoffDefault = []time.Duration{400 * time.Millisecond}
	t.Cleanup(func() { decideRetryBackoffDefault = old })

	d := newStubDaemon(t, func(call int, _ []byte, w http.ResponseWriter) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	rc := NewRemoteClient(d.client(t), nil, nil)

	go func() {
		time.Sleep(30 * time.Millisecond)
		rc.cancel()
	}()

	start := time.Now()
	status, err := rc.decide("apr_x", "always")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("decide should return the ctx error, not nil")
	}
	if status != "" {
		t.Errorf("status = %q, want empty", status)
	}
	if got := d.calls.Load(); got != 1 {
		t.Errorf("daemon calls = %d, want 1 (cancel must abort before the retry)", got)
	}
	// Must NOT have waited out the full 400ms backoff.
	if elapsed >= 400*time.Millisecond {
		t.Errorf("decide waited %v (≈ the full backoff); cancel should have cut it short", elapsed)
	}
}

// --- DecideApproval status parsing (api_client) ---------------------------

func TestDecideApprovalParsesStatusAndBody(t *testing.T) {
	var seenBody string
	d := newStubDaemon(t, func(_ int, body []byte, w http.ResponseWriter) {
		seenBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"apr_x","status":"late"}`))
	})

	status, err := d.client(t).DecideApproval("apr_x", "always")
	if err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	if status != "late" {
		t.Errorf("status = %q, want late", status)
	}
	if !strings.Contains(seenBody, "always") {
		t.Errorf("request body %q does not carry the decision token", seenBody)
	}
}

func TestDecideApprovalErrorOnUnknownId(t *testing.T) {
	d := newStubDaemon(t, func(_ int, _ []byte, w http.ResponseWriter) {
		http.Error(w, "approval not found", http.StatusNotFound)
	})
	status, err := d.client(t).DecideApproval("apr_bogus", "always")
	if err == nil {
		t.Fatal("DecideApproval should surface a 404 as an error")
	}
	if status != "" {
		t.Errorf("status = %q, want empty on error", status)
	}
}

// --- reportDecision surfacing (kind-aware) --------------------------------

func newReportingRC(t *testing.T) (*RemoteClient, *capturingSink) {
	t.Helper()
	c := &capturingSink{}
	return NewRemoteClient(nil, c.fn, nil), c
}

func TestReportDecisionFailurePermissionEmitsNotice(t *testing.T) {
	rc, c := newReportingRC(t)
	rc.reportDecision("s1", "permission", "example.com", "always", "", errors.New("boom"))

	ns := c.notices()
	if len(ns) != 1 {
		t.Fatalf("notices = %d, want 1", len(ns))
	}
	if !strings.Contains(ns[0].Text, "Always allow'") || !strings.Contains(ns[0].Text, "did not take effect") {
		t.Errorf("permission failure notice text wrong: %q", ns[0].Text)
	}
	if c.lastSid != "s1" {
		t.Errorf("notice routed to session %q, want s1", c.lastSid)
	}
}

func TestReportDecisionFailureEditReviewEmitsKindText(t *testing.T) {
	rc, c := newReportingRC(t)
	rc.reportDecision("s1", "edit_review", "/p/a.go", "approve", "", errors.New("boom"))

	ns := c.notices()
	if len(ns) != 1 {
		t.Fatalf("notices = %d, want 1", len(ns))
	}
	if !strings.Contains(ns[0].Text, "edit was not applied") {
		t.Errorf("edit-review failure notice text wrong: %q", ns[0].Text)
	}
	// Must NOT reuse the permission-specific wording for an edit review.
	if strings.Contains(ns[0].Text, "Always allow") {
		t.Errorf("edit-review notice used permission wording: %q", ns[0].Text)
	}
}

func TestReportDecisionLateAlwaysEmitsFutureRequestNotice(t *testing.T) {
	rc, c := newReportingRC(t)
	rc.reportDecision("s1", "permission", "example.com", "always", "late", nil)

	ns := c.notices()
	if len(ns) != 1 {
		t.Fatalf("notices = %d, want 1", len(ns))
	}
	for _, want := range []string{"example.com", "future requests", "allow"} {
		if !strings.Contains(ns[0].Text, want) {
			t.Errorf("late-always notice missing %q: %q", want, ns[0].Text)
		}
	}
}

func TestReportDecisionLateAlwaysDenyEmitsDenyNotice(t *testing.T) {
	rc, c := newReportingRC(t)
	rc.reportDecision("s1", "permission", "example.com", "always_deny", "late", nil)

	ns := c.notices()
	if len(ns) != 1 {
		t.Fatalf("notices = %d, want 1", len(ns))
	}
	if !strings.Contains(ns[0].Text, "deny") || !strings.Contains(ns[0].Text, "example.com") {
		t.Errorf("late-always_deny notice text wrong: %q", ns[0].Text)
	}
}

// TestReportDecisionLateNoticeDoesNotClaimSafeDefault locks in the fixes-round-1
// wording change: "late" also fires when another attached client answered the
// prompt in-time (not just on a timeout). Claiming "the request used the safe
// default" would be wrong in that case, so the late notice must avoid that phrase
// and only state what is certain — the grant was saved and applies going forward.
func TestReportDecisionLateNoticeDoesNotClaimSafeDefault(t *testing.T) {
	rc, c := newReportingRC(t)
	rc.reportDecision("s1", "permission", "example.com", "always", "late", nil)

	ns := c.notices()
	if len(ns) != 1 {
		t.Fatalf("notices = %d, want 1", len(ns))
	}
	if strings.Contains(strings.ToLower(ns[0].Text), "safe default") {
		t.Errorf("late notice must not claim the request used the safe default (only true for the timeout case): %q", ns[0].Text)
	}
	for _, want := range []string{"example.com", "future requests"} {
		if !strings.Contains(ns[0].Text, want) {
			t.Errorf("late notice missing %q: %q", want, ns[0].Text)
		}
	}
}

func TestReportDecisionLateNonStickyIsSilent(t *testing.T) {
	rc, c := newReportingRC(t)
	// A late one-shot allow carries no future effect → must not surface.
	rc.reportDecision("s1", "permission", "example.com", "allow", "late", nil)
	if got := len(c.notices()); got != 0 {
		t.Errorf("non-sticky late notice should be silent; got %d", got)
	}
}

func TestReportDecisionResolvedIsSilent(t *testing.T) {
	rc, c := newReportingRC(t)
	rc.reportDecision("s1", "permission", "example.com", "always", "resolved", nil)
	if got := len(c.notices()); got != 0 {
		t.Errorf("in-time resolved decision should be silent; got %d notices", got)
	}
}

func TestReportDecisionNilSinkIsNoOp(t *testing.T) {
	rc := NewRemoteClient(nil, nil, nil) // nil sink — narrow test config.
	rc.reportDecision("s1", "permission", "example.com", "always", "", errors.New("boom"))
	// Reaching here without panicking is the assertion.
}

// --- emitNotice -----------------------------------------------------------

func TestEmitNoticeRoutesSessionEventNoticeToSink(t *testing.T) {
	rc, c := newReportingRC(t)
	rc.emitNotice("s2", "hello world")
	ns := c.notices()
	if len(ns) != 1 || ns[0].Type != agent.SessionEventNotice || ns[0].Text != "hello world" {
		t.Errorf("emitNotice did not route a SessionEventNotice: %+v", ns)
	}
	if c.lastSid != "s2" {
		t.Errorf("notice routed to %q, want s2", c.lastSid)
	}
}

func TestEmitNoticeNilSinkIsNoOp(t *testing.T) {
	rc := NewRemoteClient(nil, nil, nil)
	rc.emitNotice("s", "x") // must not panic.
}

// --- handleApproval end-to-end (issue #560 integration) -------------------

type fixedApprover struct {
	perm permission.Decision
	edit gogent.EditReviewDecision
}

func (a *fixedApprover) AskPermission(permission.Request) permission.Decision { return a.perm }
func (a *fixedApprover) ReviewEdit(gogent.EditReviewRequest) gogent.EditReviewDecision {
	return a.edit
}

// TestHandleApprovalLateAlwaysEmitsUserNotice stands up the full client path: the
// user answers "always" through the Approver, the (stub) daemon reconciles it
// late, and the user is told in-band that the grant will apply going forward — the
// exact scenario the issue says currently fails silently.
func TestHandleApprovalLateAlwaysEmitsUserNotice(t *testing.T) {
	withFastRetries(t)
	d := newStubDaemon(t, func(_ int, _ []byte, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"apr_1","status":"late"}`))
	})
	sink := &capturingSink{}
	rc := NewRemoteClient(d.client(t), sink.fn, &fixedApprover{perm: permission.DecisionAlways})

	rc.handleApproval(ApprovalDTO{
		ID:        "apr_1",
		Kind:      "permission",
		SessionID: "sess-1",
		Permission: &PermissionDetail{
			Action:   "network",
			Resource: "example.com",
			Detail:   "https://example.com/",
		},
	})

	ns := sink.notices()
	if len(ns) != 1 {
		t.Fatalf("user notices = %d, want 1 for a late sticky grant (got %d)", len(ns), len(ns))
	}
	if !strings.Contains(ns[0].Text, "example.com") || !strings.Contains(ns[0].Text, "future requests") {
		t.Errorf("late-always notice did not tell the user the grant applies going forward: %q", ns[0].Text)
	}
}

// TestHandleApprovalResolvedStaysSilent confirms the common happy path produces
// no in-band noise.
func TestHandleApprovalResolvedStaysSilent(t *testing.T) {
	withFastRetries(t)
	d := newStubDaemon(t, nil) // 200 resolved.
	sink := &capturingSink{}
	rc := NewRemoteClient(d.client(t), sink.fn, &fixedApprover{perm: permission.DecisionAlways})

	rc.handleApproval(ApprovalDTO{
		ID: "apr_2", Kind: "permission", SessionID: "sess-2",
		Permission: &PermissionDetail{Action: "network", Resource: "example.com"},
	})
	if got := len(sink.notices()); got != 0 {
		t.Errorf("in-time resolved approval should be silent; got %d notices", got)
	}
}

// guard against accidental drift in the ctx import used by the cancel test.
var _ = context.Canceled
