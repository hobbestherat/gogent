package server

import (
	"testing"
	"time"

	"gogent/internal/config"
	"gogent/internal/gogent"
	"gogent/internal/permission"
)

func TestApprovalBridgeIssue358UnattendedWaitsPastConnectedTimeout(t *testing.T) {
	h := newHub()
	bridge := newApprovalBridge(h, 15*time.Millisecond, 250*time.Millisecond, time.Now)

	done := make(chan permission.Decision, 1)
	go func() {
		done <- bridge.AskPermission(permission.Request{
			Action:  permission.ActionShell,
			Context: permission.RequestContext{SessionID: "s1", Agent: "root"},
		})
	}()

	pending := waitForPendingIssue358(t, bridge, "permission")

	select {
	case dec := <-done:
		t.Fatalf("unattended approval resolved after connected timeout with %v; want still pending", dec)
	case <-time.After(60 * time.Millisecond):
	}

	if got := findPendingIssue358(bridge, pending.ID); got == nil {
		t.Fatalf("unattended approval %q was removed before unattended timeout", pending.ID)
	}

	if !bridge.resolve(pending.ID, decision{perm: permission.DecisionAllow}) {
		t.Fatalf("resolve returned false for still-pending unattended approval %q", pending.ID)
	}
	select {
	case dec := <-done:
		if dec != permission.DecisionAllow {
			t.Fatalf("decision after unattended wait = %v, want allow", dec)
		}
	case <-time.After(time.Second):
		t.Fatal("unattended approval did not unblock after explicit decision")
	}
}

func TestApprovalBridgeIssue358ConnectedClientKeepsShortAutoDeny(t *testing.T) {
	for _, tc := range []struct {
		name      string
		subscribe func(*hub) func()
	}{
		{
			name: "global_events_client",
			subscribe: func(h *hub) func() {
				_, unsubscribe := h.subscribeGlobal()
				return unsubscribe
			},
		},
		{
			name: "session_events_client",
			subscribe: func(h *hub) func() {
				_, unsubscribe := h.subscribeSession("s1")
				return unsubscribe
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHub()
			unsubscribe := tc.subscribe(h)
			defer unsubscribe()
			if got := h.clientCount(); got != 1 {
				t.Fatalf("clientCount = %d, want 1", got)
			}

			bridge := newApprovalBridge(h, 20*time.Millisecond, 500*time.Millisecond, time.Now)
			done := make(chan permission.Decision, 1)
			go func() {
				done <- bridge.AskPermission(permission.Request{
					Action:  permission.ActionShell,
					Context: permission.RequestContext{SessionID: "s1", Agent: "root"},
				})
			}()
			_ = waitForPendingIssue358(t, bridge, "permission")

			select {
			case dec := <-done:
				if dec != permission.DecisionDeny {
					t.Fatalf("connected unresponsive approval = %v, want deny", dec)
				}
			case <-time.After(150 * time.Millisecond):
				t.Fatal("connected unresponsive approval did not auto-deny at the short timeout")
			}
			if got := bridge.list(); len(got) != 0 {
				t.Fatalf("pending approvals after connected timeout = %d, want 0", len(got))
			}
		})
	}
}

func TestApprovalBridgeIssue358DisconnectWhilePendingSwitchesToUnattendedWait(t *testing.T) {
	h := newHub()
	_, unsubscribe := h.subscribeGlobal()
	if got := h.clientCount(); got != 1 {
		t.Fatalf("clientCount = %d, want 1", got)
	}
	bridge := newApprovalBridge(h, 80*time.Millisecond, 400*time.Millisecond, time.Now)

	done := make(chan permission.Decision, 1)
	go func() {
		done <- bridge.AskPermission(permission.Request{
			Action:  permission.ActionShell,
			Context: permission.RequestContext{SessionID: "s1", Agent: "root"},
		})
	}()
	pending := waitForPendingIssue358(t, bridge, "permission")

	time.Sleep(35 * time.Millisecond)
	unsubscribe()
	if got := h.clientCount(); got != 0 {
		t.Fatalf("clientCount after disconnect = %d, want 0", got)
	}

	select {
	case dec := <-done:
		t.Fatalf("approval resolved after disconnect with %v; want pending until unattended timeout or decision", dec)
	case <-time.After(130 * time.Millisecond):
	}
	if got := findPendingIssue358(bridge, pending.ID); got == nil {
		t.Fatalf("approval %q was removed after disconnect before unattended timeout", pending.ID)
	}

	if !bridge.resolve(pending.ID, decision{perm: permission.DecisionAllow}) {
		t.Fatalf("resolve returned false for disconnected pending approval %q", pending.ID)
	}
	select {
	case dec := <-done:
		if dec != permission.DecisionAllow {
			t.Fatalf("decision after disconnected wait = %v, want allow", dec)
		}
	case <-time.After(time.Second):
		t.Fatal("approval did not unblock after decision")
	}
}

func TestApprovalBridgeIssue358ReconnectGetsFreshConnectedTimeout(t *testing.T) {
	h := newHub()
	_, disconnect := h.subscribeGlobal()
	bridge := newApprovalBridge(h, 120*time.Millisecond, 600*time.Millisecond, time.Now)

	done := make(chan permission.Decision, 1)
	go func() {
		done <- bridge.AskPermission(permission.Request{
			Action:  permission.ActionShell,
			Context: permission.RequestContext{SessionID: "s1", Agent: "root"},
		})
	}()
	pending := waitForPendingIssue358(t, bridge, "permission")

	time.Sleep(85 * time.Millisecond)
	disconnect()
	time.Sleep(85 * time.Millisecond)
	_, disconnectAgain := h.subscribeGlobal()
	defer disconnectAgain()

	select {
	case dec := <-done:
		t.Fatalf("approval resolved before reconnect grace window with %v; want pending", dec)
	default:
	}

	time.Sleep(85 * time.Millisecond)
	select {
	case dec := <-done:
		t.Fatalf("approval reused pre-disconnect connected time and resolved with %v; want fresh connected timeout", dec)
	default:
	}

	if !bridge.resolve(pending.ID, decision{perm: permission.DecisionAllow}) {
		t.Fatalf("resolve returned false for reconnected pending approval %q", pending.ID)
	}
	select {
	case dec := <-done:
		if dec != permission.DecisionAllow {
			t.Fatalf("decision after reconnect grace = %v, want allow", dec)
		}
	case <-time.After(time.Second):
		t.Fatal("approval did not unblock after reconnect decision")
	}
}

func TestApprovalBridgeIssue358UnattendedPromptCanBeAnsweredAfterReconnect(t *testing.T) {
	h := newHub()
	bridge := newApprovalBridge(h, 20*time.Millisecond, 300*time.Millisecond, time.Now)

	done := make(chan permission.Decision, 1)
	go func() {
		done <- bridge.AskPermission(permission.Request{
			Action:  permission.ActionShell,
			Context: permission.RequestContext{SessionID: "s1", Agent: "root"},
		})
	}()

	pending := waitForPendingIssue358(t, bridge, "permission")
	time.Sleep(80 * time.Millisecond)

	select {
	case dec := <-done:
		t.Fatalf("unattended approval resolved before reconnect with %v; want pending", dec)
	default:
	}

	_, unsubscribe := h.subscribeGlobal()
	defer unsubscribe()
	if got := h.clientCount(); got != 1 {
		t.Fatalf("clientCount after reconnect = %d, want 1", got)
	}

	if !bridge.resolve(pending.ID, decision{perm: permission.DecisionAllow}) {
		t.Fatalf("resolve returned false for reconnected approval %q", pending.ID)
	}
	select {
	case dec := <-done:
		if dec != permission.DecisionAllow {
			t.Fatalf("decision after reconnect = %v, want allow", dec)
		}
	case <-time.After(time.Second):
		t.Fatal("approval did not unblock after reconnect decision")
	}
}

func TestApprovalBridgeIssue358UnattendedTimeoutStillSafetyDenies(t *testing.T) {
	h := newHub()
	bridge := newApprovalBridge(h, 20*time.Millisecond, 50*time.Millisecond, time.Now)

	done := make(chan permission.Decision, 1)
	go func() {
		done <- bridge.AskPermission(permission.Request{Action: permission.ActionShell})
	}()
	_ = waitForPendingIssue358(t, bridge, "permission")

	select {
	case dec := <-done:
		if dec != permission.DecisionDeny {
			t.Fatalf("unattended timeout decision = %v, want deny", dec)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("unattended approval did not deny at unattended safety timeout")
	}
}

func TestApprovalBridgeIssue358UnattendedDefaultAndServerPlumbing(t *testing.T) {
	if got := config.DefaultUnattendedApprovalTimeout; got != time.Hour {
		t.Fatalf("DefaultUnattendedApprovalTimeout = %v, want 1h", got)
	}
	if got := (&config.Config{}).UnattendedApprovalTimeoutOrDefault(); got != time.Hour {
		t.Fatalf("zero config unattended timeout = %v, want 1h", got)
	}
	custom := 37 * time.Minute
	if got := (&config.Config{UnattendedApprovalTimeout: custom}).UnattendedApprovalTimeoutOrDefault(); got != custom {
		t.Fatalf("custom config unattended timeout = %v, want %v", got, custom)
	}

	fixedNow := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	srv := NewServer(gogent.NewGogent(t.TempDir()), Options{
		ApprovalTimeout: 11 * time.Second,
		now:             func() time.Time { return fixedNow },
	})
	if got := srv.approvals.connectedTimeout; got != 11*time.Second {
		t.Fatalf("server connected approval timeout = %v, want 11s", got)
	}
	if got := srv.approvals.unattendedTimeout; got != time.Hour {
		t.Fatalf("server default unattended approval timeout = %v, want 1h", got)
	}

	srv = NewServer(gogent.NewGogent(t.TempDir()), Options{
		ApprovalTimeout:           11 * time.Second,
		UnattendedApprovalTimeout: custom,
		now:                       func() time.Time { return fixedNow },
	})
	if got := srv.approvals.unattendedTimeout; got != custom {
		t.Fatalf("server custom unattended approval timeout = %v, want %v", got, custom)
	}
}

func waitForPendingIssue358(t *testing.T, bridge *approvalBridge, kind string) approvalView {
	t.Helper()
	deadline := time.After(time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		for _, p := range bridge.list() {
			if p.Kind == kind {
				return p
			}
		}
		select {
		case <-deadline:
			t.Fatalf("%s approval never became pending", kind)
		case <-tick.C:
		}
	}
}

func findPendingIssue358(bridge *approvalBridge, id string) *approvalView {
	for _, p := range bridge.list() {
		if p.ID == id {
			cp := p
			return &cp
		}
	}
	return nil
}
