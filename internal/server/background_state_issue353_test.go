package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gogent/internal/agent"
	"gogent/internal/gogent"
	"gogent/internal/model"
)

func TestSessionToViewReportsBackgroundOnlyWhenRootIdle(t *testing.T) {
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		writeServerTestFinal(w, "SUCCESS: done")
	}))
	defer backend.Close()

	home := t.TempDir()
	g := gogent.NewGogentWithWorkspace(home, home)
	conn := model.NewModelConnection()
	conn.SetURL(backend.URL)
	sess := model.NewModelSession("session_test", conn)
	root := agent.NewAgent("agent_test", sess)
	us := g.CreateUserSession("session_test", root)
	us.SetSubAgentLimiter(agent.NewSubAgentLimiter(1))

	if _, err := us.LaunchBackgroundSubAgent("agent_test", "bg", "background task"); err != nil {
		t.Fatalf("launch background sub-agent: %v", err)
	}
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("background worker did not reach model request")
	}

	root.SetState(agent.StateIdle)
	if got := sessionToView(g, "session_test", us, "Session").State; got != "background" {
		t.Fatalf("idle root with background work state = %q, want background", got)
	}
	root.SetState(agent.StateThinking)
	if got := sessionToView(g, "session_test", us, "Session").State; got != "thinking" {
		t.Fatalf("thinking root with background work state = %q, want thinking", got)
	}
	root.SetState(agent.StateWaitingForTool)
	if got := sessionToView(g, "session_test", us, "Session").State; got != "waiting" {
		t.Fatalf("waiting root with background work state = %q, want waiting", got)
	}

	root.SetState(agent.StateIdle)
	close(release)
	waitUntilNoBackground(t, us)
	if got := sessionToView(g, "session_test", us, "Session").State; got != "idle" {
		t.Fatalf("state after background completion = %q, want idle", got)
	}
}

func waitUntilNoBackground(t *testing.T, us *agent.UserSession) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if !us.HasBackgroundWork() {
			return
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatal("timed out waiting for background work to finish")
		}
	}
}

func writeServerTestFinal(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"` + content + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
}
