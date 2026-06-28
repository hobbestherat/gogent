package server

// Issue #560 — remote-TUI approval decisions fail silently.
//
// These tests cover the daemon half of the fix: the approvalBridge's recall ring
// (a removed approval is remembered so a late decision POST is reconciled instead
// of hard-404ing) and the idempotent Decide endpoint (late "always"/"always_deny"
// permission grants are persisted directly; a raced double-consumer returns 200
// instead of 409; a genuinely unknown/evicted id still 404s). They also assert the
// user-facing guarantees: per-host "always allow" sticks across the session,
// survives a daemon restart, stays host-scoped, and hits the audit trail.

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gogent/internal/gogent"
	"gogent/internal/permission"
)

// newIssue560Server builds a Server (with a real Gogent over a temp home, so
// permission persistence is real) and returns it plus the home dir, so the
// reload-from-disk assertion can construct a fresh permission.Service from the
// same configDir the daemon used.
func newIssue560Server(t *testing.T) (*Server, string) {
	t.Helper()
	home := t.TempDir()
	g := gogent.NewGogent(home)
	srv := NewServer(g, Options{Password: "x"})
	return srv, home
}

// decideOverHTTP POSTs a decision for aid and returns the recorder.
func decideOverHTTP(t *testing.T, srv *Server, aid, decision string) *responseSnapshot {
	t.Helper()
	body := `{"decision":"` + decision + `"}`
	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/approvals/"+aid+"/decision", strings.NewReader(body)))
	return &responseSnapshot{Code: rec.Code, Body: rec.Body.String()}
}

type responseSnapshot struct {
	Code int
	Body string
}

func (r *responseSnapshot) statusField(t *testing.T) string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal([]byte(r.Body), &m); err != nil {
		t.Fatalf("decide response not a string map: %v (body=%q)", err, r.Body)
	}
	return m["status"]
}

// mintPermissionApproval registers a pending network permission for host and
// returns its id. It does NOT start a wait() goroutine, so there is no blocked
// tool goroutine to clean up — the test drives remove()/recall() directly.
func mintPermissionApproval(srv *Server, sessionID, host string) string {
	return srv.approvals.alloc("permission", sessionID, "root", &permissionDetail{
		Action:   string(permission.ActionNetwork),
		Resource: host,
		Detail:   "https://" + host + "/",
	}, nil)
}

// --- recall ring -----------------------------------------------------------

func TestRecallRemembersRemovedApproval(t *testing.T) {
	srv, _ := newIssue560Server(t)
	id := mintPermissionApproval(srv, "s1", "example.com")

	if _, ok := srv.approvals.recall(id); ok {
		t.Fatal("an approval still pending should not be recallable (only removed ones)")
	}
	srv.approvals.remove(id)

	rec, ok := srv.approvals.recall(id)
	if !ok {
		t.Fatal("recall returned false after remove; the late-decision reconcile path would 404")
	}
	if rec.kind != "permission" {
		t.Errorf("recalled kind = %q, want permission", rec.kind)
	}
	if rec.sessionID != "s1" {
		t.Errorf("recalled sessionID = %q, want s1", rec.sessionID)
	}
	if rec.permission == nil || rec.permission.Resource != "example.com" {
		t.Errorf("recalled permission detail lost: %+v", rec.permission)
	}
	if rec.permission.Action != string(permission.ActionNetwork) {
		t.Errorf("recalled action = %q, want network", rec.permission.Action)
	}
}

func TestRecallMissesNeverMintedId(t *testing.T) {
	srv, _ := newIssue560Server(t)
	if _, ok := srv.approvals.recall("apr_bogus"); ok {
		t.Fatal("recall should miss an id that was never minted")
	}
}

func TestRecallRingEvictsOldestPastCap(t *testing.T) {
	srv, _ := newIssue560Server(t)

	// Fill the ring exactly to capacity.
	first := mintPermissionApproval(srv, "s0", "h0")
	srv.approvals.remove(first)
	for i := 1; i < recentCap; i++ {
		id := mintPermissionApproval(srv, "s", "h")
		srv.approvals.remove(id)
	}
	if _, ok := srv.approvals.recall(first); !ok {
		t.Fatalf("first approval should still be recallable at exactly cap=%d", recentCap)
	}

	// One more eviction pushes the oldest out.
	extra := mintPermissionApproval(srv, "sx", "hx")
	srv.approvals.remove(extra)
	if _, ok := srv.approvals.recall(first); ok {
		t.Fatalf("oldest approval should have been evicted once the ring exceeded cap=%d", recentCap)
	}
	if _, ok := srv.approvals.recall(extra); !ok {
		t.Fatal("newest approval should be recallable after an eviction")
	}

	// The ring never exceeds cap.
	if got := len(srv.approvals.recent); got != recentCap {
		t.Fatalf("recent ring size = %d, want cap %d", got, recentCap)
	}
	if got := len(srv.approvals.recentOrder); got != recentCap {
		t.Fatalf("recentOrder length = %d, want cap %d", got, recentCap)
	}
}

func TestRemoveIsIdempotentOnRecallOrder(t *testing.T) {
	srv, _ := newIssue560Server(t)
	id := mintPermissionApproval(srv, "s1", "example.com")

	srv.approvals.remove(id)
	srv.approvals.remove(id) // a second remove must not duplicate the order entry.

	count := 0
	for _, x := range srv.approvals.recentOrder {
		if x == id {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("id appeared %d times in recentOrder after double-remove, want 1", count)
	}
}

// --- late-arrival Decide over HTTP ----------------------------------------

func TestDecideLateAlwaysPersistsAndSuppressesFuturePrompts(t *testing.T) {
	srv, _ := newIssue560Server(t)
	id := mintPermissionApproval(srv, "s1", "example.com")
	srv.approvals.remove(id) // simulate the timeout/race that removed it first.

	ps := srv.g.GetPermissionService()

	// Sanity: before the late grant, example.com is not yet authorized.
	if err := ps.CheckWithContext(permission.RequestContext{}, permission.ActionNetwork, "example.com", "https://example.com/"); err == nil {
		t.Fatal("example.com should require a prompt before the late grant")
	}

	res := decideOverHTTP(t, srv, id, "always")
	if res.Code != http.StatusOK {
		t.Fatalf("late always-decide status = %d, want 200 (idempotent reconcile); body=%q", res.Code, res.Body)
	}
	if got := res.statusField(t); got != "late" {
		t.Fatalf("late-decide status field = %q, want \"late\"", got)
	}

	// Criterion 1: the host is now allowed (every path), with no re-prompt.
	if err := ps.CheckWithContext(permission.RequestContext{}, permission.ActionNetwork, "example.com", "https://example.com/deep/path"); err != nil {
		t.Fatalf("example.com not allowed after late always-grant: %v", err)
	}
}

func TestDecideLateAlwaysDenyPersistsDeny(t *testing.T) {
	srv, _ := newIssue560Server(t)
	id := mintPermissionApproval(srv, "s1", "evil.example")
	srv.approvals.remove(id)

	res := decideOverHTTP(t, srv, id, "always_deny")
	if res.Code != http.StatusOK || res.statusField(t) != "late" {
		t.Fatalf("late always_deny-decide = %d %q, want 200 \"late\"", res.Code, res.Body)
	}
	ps := srv.g.GetPermissionService()
	if err := ps.CheckWithContext(permission.RequestContext{}, permission.ActionNetwork, "evil.example", "https://evil.example/"); err == nil {
		t.Fatal("always_deny should persist a deny, but the host was allowed")
	}
}

func TestDecideLateNonStickyDoesNotPersist(t *testing.T) {
	srv, _ := newIssue560Server(t)
	id := mintPermissionApproval(srv, "s1", "example.com")
	srv.approvals.remove(id)

	res := decideOverHTTP(t, srv, id, "allow")
	if res.Code != http.StatusOK || res.statusField(t) != "late" {
		t.Fatalf("late allow-decide = %d %q, want 200 \"late\"", res.Code, res.Body)
	}
	// A one-shot allow must NOT broaden to a sticky grant.
	ps := srv.g.GetPermissionService()
	if err := ps.CheckWithContext(permission.RequestContext{}, permission.ActionNetwork, "example.com", "https://example.com/"); err == nil {
		t.Fatal("a late one-shot allow was incorrectly persisted as a sticky grant")
	}
}

func TestDecideLateEditReviewDoesNotPersist(t *testing.T) {
	srv, _ := newIssue560Server(t)
	id := srv.approvals.alloc("edit_review", "s1", "root", nil, &editReviewDetail{Path: "a.go", Op: "edit", Diff: "-x\n+y"})
	srv.approvals.remove(id)

	res := decideOverHTTP(t, srv, id, "approve_all")
	if res.Code != http.StatusOK || res.statusField(t) != "late" {
		t.Fatalf("late edit-review decide = %d %q, want 200 \"late\"", res.Code, res.Body)
	}
	// Nothing to persist for an edit review; the response is purely idempotent.
	// (No permission state to assert beyond the benign 200 already checked.)
}

func TestDecideEvictedIdStillReturns404(t *testing.T) {
	srv, _ := newIssue560Server(t)
	id := mintPermissionApproval(srv, "s1", "example.com")
	srv.approvals.remove(id)
	// Push it out of the recall ring.
	for i := 0; i < recentCap; i++ {
		x := mintPermissionApproval(srv, "s", "h")
		srv.approvals.remove(x)
	}

	res := decideOverHTTP(t, srv, id, "always")
	if res.Code != http.StatusNotFound {
		t.Fatalf("evicted id decide = %d, want 404 (so the client retries then surfaces); body=%q", res.Code, res.Body)
	}
}

func TestDecideRacedDoubleConsumerReturns200Not409(t *testing.T) {
	srv, _ := newIssue560Server(t)
	id := mintPermissionApproval(srv, "s1", "example.com")

	// First POST resolves it (sends a decision to the buffered channel).
	first := decideOverHTTP(t, srv, id, "allow")
	if first.Code != http.StatusOK || first.statusField(t) != "resolved" {
		t.Fatalf("first decide = %d %q, want 200 \"resolved\"", first.Code, first.Body)
	}
	// A racing second POST finds the approval still pending but already delivered
	// (resolve returns false). This used to be a 409; it must now be a benign 200.
	second := decideOverHTTP(t, srv, id, "allow")
	if second.Code != http.StatusOK {
		t.Fatalf("raced decide = %d, want 200 (idempotent); body=%q", second.Code, second.Body)
	}
	if got := second.statusField(t); got != "resolved" {
		t.Fatalf("raced decide status = %q, want \"resolved\"", got)
	}
}

// --- host-scoping + restart (criteria 2 & 3) ------------------------------

func TestLateGrantSurvivesRestartAndIsHostScoped(t *testing.T) {
	srv, home := newIssue560Server(t)
	id := mintPermissionApproval(srv, "s1", "example.com")
	srv.approvals.remove(id)
	if res := decideOverHTTP(t, srv, id, "always"); res.Code != http.StatusOK {
		t.Fatalf("late always-decide = %d, want 200", res.Code)
	}

	// Simulate a daemon restart: a fresh permission.Service loading the same dir.
	reloaded := permission.New(filepath.Join(home, ".gogent"))

	// Criterion 2: the grant survived the restart.
	if err := reloaded.CheckWithContext(permission.RequestContext{}, permission.ActionNetwork, "example.com", "https://example.com/anything"); err != nil {
		t.Fatalf("always-grant did not survive restart: %v", err)
	}
	// Criterion 3: host-scoping preserved — a different host still asks (denies
	// here because the reloaded service has no prompter).
	if err := reloaded.CheckWithContext(permission.RequestContext{}, permission.ActionNetwork, "other.com", "https://other.com/"); err == nil {
		t.Fatal("granting example.com must not authorize other.com")
	}
}

// --- audit trail (R5) ------------------------------------------------------

func TestDecideLateAlwaysFiresAuditSink(t *testing.T) {
	srv, _ := newIssue560Server(t)
	id := mintPermissionApproval(srv, "s_audit", "example.com")
	srv.approvals.remove(id)

	type auditEntry struct {
		action   permission.Action
		resource string
		allowed  bool
		session  string
	}
	var (
		mu      sync.Mutex
		entries []auditEntry
	)
	ps := srv.g.GetPermissionService()
	ps.SetAuditSink(func(rc permission.RequestContext, action permission.Action, resource string, allowed bool) {
		mu.Lock()
		defer mu.Unlock()
		entries = append(entries, auditEntry{action, resource, allowed, rc.SessionID})
	})

	if res := decideOverHTTP(t, srv, id, "always"); res.Code != http.StatusOK {
		t.Fatalf("late always-decide = %d, want 200", res.Code)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want exactly 1 (late grant must be on-record, not double)", len(entries))
	}
	e := entries[0]
	if e.action != permission.ActionNetwork || e.resource != "example.com" {
		t.Errorf("audit entry = {%s %s}, want {network example.com}", e.action, e.resource)
	}
	if !e.allowed {
		t.Error("an always-grant should be audited as allowed=true")
	}
	if e.session != "s_audit" {
		t.Errorf("audit session = %q, want the recalled session id s_audit", e.session)
	}
}

func TestDecideLateAlwaysDenyFiredAuditAsDenied(t *testing.T) {
	srv, _ := newIssue560Server(t)
	id := mintPermissionApproval(srv, "s_deny", "example.com")
	srv.approvals.remove(id)

	var (
		mu      sync.Mutex
		allowed []bool
	)
	srv.g.GetPermissionService().SetAuditSink(func(rc permission.RequestContext, action permission.Action, resource string, a bool) {
		mu.Lock()
		defer mu.Unlock()
		allowed = append(allowed, a)
	})

	if res := decideOverHTTP(t, srv, id, "always_deny"); res.Code != http.StatusOK {
		t.Fatalf("late always_deny-decide = %d, want 200", res.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(allowed) != 1 || allowed[0] {
		t.Fatalf("always_deny audit allowed = %v, want [false]", allowed)
	}
}
