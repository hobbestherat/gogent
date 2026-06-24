package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestIssue358DaemonStatusReportsUserFacingLiveSessionCount(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	srv.g.NewSession("default")
	srv.g.NewSession("user-a")
	srv.g.NewSession("watcher:nightly")
	srv.g.NewSession("user-b")

	rec := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/daemon/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var got daemonStatusView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode daemon status: %v", err)
	}
	if got.PID <= 0 {
		t.Fatalf("pid = %d, want current process pid", got.PID)
	}
	if got.LiveSessions != 2 {
		t.Fatalf("live_sessions = %d, want 2 user-facing sessions", got.LiveSessions)
	}
	if got.Watchers != 0 {
		t.Fatalf("watchers = %d, want 0 with watcher engine not started", got.Watchers)
	}
}
