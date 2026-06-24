package server

import (
	"testing"
	"time"

	"gogent/internal/gogent"
	"gogent/internal/permission"
)

// TestApprovalBridgePermissionRoundTrip exercises the async approval bridge end
// to end: a permission prompt blocks, surfaces as a pending approval, and is
// resolved by a decision — without a real HTTP round-trip. It proves the bridge
// adapts the blocking permission.Prompter to the async request/response model.
func TestApprovalBridgePermissionRoundTrip(t *testing.T) {
	h := newHub()
	bridge := newApprovalBridge(h, time.Minute, time.Minute, time.Now)

	prompted := make(chan struct{})
	var got approvalView
	go func() {
		// AskPermission blocks until a decision arrives; run it off the test
		// goroutine so we can observe the pending state and resolve it.
		dec := bridge.AskPermission(permission.Request{
			Action:   permission.ActionShell,
			Resource: "rm -rf /tmp/x",
			Detail:   "rm -rf /tmp/x",
			Context:  permission.RequestContext{SessionID: "s1", Agent: "root"},
		})
		// The decision should match what we resolve with below.
		if dec != permission.DecisionAllow {
			t.Errorf("decision = %v, want allow", dec)
		}
		close(prompted)
	}()

	// Wait for the approval to appear in the pending list.
	deadline := time.After(time.Second)
	for got.ID == "" {
		select {
		case <-deadline:
			t.Fatal("approval never became pending")
		default:
		}
		for _, p := range bridge.list() {
			if p.Kind == "permission" {
				got = p
			}
		}
		time.Sleep(time.Millisecond)
	}

	if got.Permission == nil || got.Permission.Action != "shell" {
		t.Fatalf("permission detail missing or wrong: %+v", got.Permission)
	}
	if got.SessionID != "s1" {
		t.Fatalf("session id = %q, want s1", got.SessionID)
	}

	// Resolve it with "allow".
	if !bridge.resolve(got.ID, decision{perm: permission.DecisionAllow}) {
		t.Fatal("resolve returned false for a pending approval")
	}

	select {
	case <-prompted:
	case <-time.After(time.Second):
		t.Fatal("AskPermission did not unblock after resolve")
	}
}

// TestApprovalBridgeEditReviewRoundTrip does the same for the edit-review gate,
// proving a ReviewEdit call blocks and is resolved with approve.
func TestApprovalBridgeEditReviewRoundTrip(t *testing.T) {
	h := newHub()
	bridge := newApprovalBridge(h, time.Minute, time.Minute, time.Now)

	resolved := make(chan gogent.EditReviewDecision, 1)
	go func() {
		resolved <- bridge.ReviewEdit(gogent.EditReviewRequest{
			SessionID: "s1", AgentID: "root", Path: "a.go", Op: "edit", Diff: "-old\n+new",
		})
	}()

	// Find the pending edit-review approval.
	var id string
	deadline := time.After(time.Second)
	for id == "" {
		select {
		case <-deadline:
			t.Fatal("edit review never became pending")
		default:
		}
		for _, p := range bridge.list() {
			if p.Kind == "edit_review" && p.EditReview != nil {
				id = p.ID
				if p.EditReview.Path != "a.go" {
					t.Fatalf("path = %q, want a.go", p.EditReview.Path)
				}
			}
		}
		time.Sleep(time.Millisecond)
	}

	bridge.resolve(id, decision{edit: gogent.EditApprove})
	select {
	case dec := <-resolved:
		if dec != gogent.EditApprove {
			t.Fatalf("edit decision = %v, want approve", dec)
		}
	case <-time.After(time.Second):
		t.Fatal("ReviewEdit did not unblock after resolve")
	}
}

// TestApprovalBridgeTimeoutDenies confirms the safe default: when no client
// resolves a prompt within the timeout, it denies (matching headless behavior).
func TestApprovalBridgeTimeoutDenies(t *testing.T) {
	h := newHub()
	bridge := newApprovalBridge(h, 20*time.Millisecond, 20*time.Millisecond, time.Now)

	dec := bridge.AskPermission(permission.Request{Action: permission.ActionShell})
	if dec != permission.DecisionDeny {
		t.Fatalf("timed-out prompt should deny, got %v", dec)
	}
}

// TestApprovalBridgeTimedOutEditRejects is the edit-review analog: timeout
// rejects (the safe default — a rejected edit is not written).
func TestApprovalBridgeTimedOutEditRejects(t *testing.T) {
	h := newHub()
	bridge := newApprovalBridge(h, 20*time.Millisecond, 20*time.Millisecond, time.Now)

	dec := bridge.ReviewEdit(gogent.EditReviewRequest{Path: "a.go"})
	if dec != gogent.EditReject {
		t.Fatalf("timed-out review should reject, got %v", dec)
	}
}

// TestDecisionParsing checks the wire-decision → typed-decision mapping.
func TestDecisionParsing(t *testing.T) {
	cases := []struct {
		in       string
		wantPerm permission.Decision
		wantEdit gogent.EditReviewDecision
	}{
		{"allow", permission.DecisionAllow, gogent.EditReject},
		{"always", permission.DecisionAlways, gogent.EditReject},
		{"always_deny", permission.DecisionAlwaysDeny, gogent.EditReject},
		{"deny", permission.DecisionDeny, gogent.EditReject},
		{"approve", permission.DecisionDeny, gogent.EditApprove},
		{"approve_all", permission.DecisionDeny, gogent.EditApproveAll},
		{"reject", permission.DecisionDeny, gogent.EditReject},
		{"unknown", permission.DecisionDeny, gogent.EditReject},
	}
	for _, c := range cases {
		if got := parsePermDecision(c.in); got != c.wantPerm {
			t.Errorf("parsePermDecision(%q) = %v, want %v", c.in, got, c.wantPerm)
		}
		if got := parseEditDecision(c.in); got != c.wantEdit {
			t.Errorf("parseEditDecision(%q) = %v, want %v", c.in, got, c.wantEdit)
		}
	}
}
