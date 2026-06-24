package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestSendRejectsNewTurnWhileAsyncSpawnRunsInBackground(t *testing.T) {
	childArrived := make(chan struct{})
	releaseChild := make(chan struct{})
	var childOnce sync.Once
	var mu sync.Mutex
	toolIssued := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []map[string]interface{} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		isChild := requestMessagesContain(req.Messages, "background task from server send") && !requestHasRole(req.Messages, "tool")
		mu.Lock()
		shouldIssueTool := !toolIssued && !isChild
		if shouldIssueTool {
			toolIssued = true
		}
		mu.Unlock()
		switch {
		case isChild:
			childOnce.Do(func() { close(childArrived) })
			select {
			case <-releaseChild:
			case <-r.Context().Done():
			}
			writeServerTestFinal(w, "SUCCESS: background done")
		case shouldIssueTool:
			writeServerTestToolCall(w, "call_spawn", "spawn_subagent", `{"async":true,"name":"bg","task":"background task from server send"}`)
		default:
			writeServerTestFinal(w, "root final")
		}
	}))
	defer backend.Close()
	defer close(releaseChild)

	t.Setenv("GOGENT_MODEL_URL", backend.URL+"/chat/completions")
	srv := NewServer(gogent.NewGogent(t.TempDir()), Options{Password: "x"})

	create := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/sessions", strings.NewReader(`{"title":"s","persisted":false}`)))
	if create.Code != http.StatusOK {
		t.Fatalf("create status = %d; body=%s", create.Code, create.Body.String())
	}
	var created sessionView
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created session: %v", err)
	}

	first := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/sessions/"+created.ID+"/messages", strings.NewReader(`{"message":"start async work"}`)))
	if first.Code != http.StatusOK {
		t.Fatalf("first send status = %d; body=%s", first.Code, first.Body.String())
	}
	select {
	case <-childArrived:
	case <-time.After(3 * time.Second):
		t.Fatal("async background worker did not start")
	}

	second := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/sessions/"+created.ID+"/messages", strings.NewReader(`{"message":"second turn while background runs"}`)))
	if second.Code != http.StatusConflict {
		t.Fatalf("second send while background runs status = %d, want 409; body=%s", second.Code, second.Body.String())
	}
}

func TestStreamRejectsConcurrentTurnWhileForegroundRuns(t *testing.T) {
	firstModelArrived := make(chan struct{})
	releaseFirstModel := make(chan struct{})
	var mu sync.Mutex
	modelCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		modelCalls++
		call := modelCalls
		mu.Unlock()
		if call == 1 {
			close(firstModelArrived)
			select {
			case <-releaseFirstModel:
			case <-r.Context().Done():
			}
		}
		writeServerTestFinal(w, "root final")
	}))
	defer backend.Close()
	defer close(releaseFirstModel)

	t.Setenv("GOGENT_MODEL_URL", backend.URL+"/chat/completions")
	srv := NewServer(gogent.NewGogent(t.TempDir()), Options{Password: "x"})
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	createResp, err := http.Post(httpSrv.URL+"/api/sessions", "application/json", strings.NewReader(`{"title":"s","persisted":false}`))
	if err != nil {
		t.Fatalf("create session request: %v", err)
	}
	defer createResp.Body.Close()
	var created sessionView
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatalf("decode created session: %v", err)
	}

	streamReq, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/sessions/"+created.ID+"/messages/stream", strings.NewReader(`{"message":"stream turn"}`))
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	streamReq.Header.Set("Content-Type", "application/json")
	streamResp, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatalf("start stream request: %v", err)
	}
	defer streamResp.Body.Close()
	if streamResp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", streamResp.StatusCode)
	}
	select {
	case <-firstModelArrived:
	case <-time.After(3 * time.Second):
		t.Fatal("stream foreground model request did not start")
	}

	secondResp, err := http.Post(httpSrv.URL+"/api/sessions/"+created.ID+"/messages", "application/json", bytes.NewBufferString(`{"message":"second turn while stream runs"}`))
	if err != nil {
		t.Fatalf("second send request: %v", err)
	}
	defer secondResp.Body.Close()
	body, _ := io.ReadAll(secondResp.Body)
	if secondResp.StatusCode != http.StatusConflict {
		t.Fatalf("second send while stream foreground runs status = %d, want 409; body=%s", secondResp.StatusCode, body)
	}
}

func TestStopEndpointCancelsAsyncBackgroundSubAgents(t *testing.T) {
	arrived := make(chan struct{})
	release := make(chan struct{})
	var arrivedOnce sync.Once
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrivedOnce.Do(func() { close(arrived) })
		select {
		case <-release:
			writeServerTestFinal(w, "SUCCESS: released")
		case <-r.Context().Done():
		}
	}))
	defer backend.Close()
	defer close(release)

	home := t.TempDir()
	g := gogent.NewGogentWithWorkspace(home, home)
	conn := model.NewModelConnection()
	conn.SetURL(backend.URL)
	sess := model.NewModelSession("session_test", conn)
	root := agent.NewAgent("root", sess)
	us := g.CreateUserSession("session_test", root)
	us.SetSubAgentLimiter(agent.NewSubAgentLimiter(1))
	srv := NewServer(g, Options{Password: "x"})

	if _, err := us.LaunchBackgroundSubAgent("root", "bg", "cancel me"); err != nil {
		t.Fatalf("launch background sub-agent: %v", err)
	}
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("background worker did not reach model request")
	}
	if !us.HasBackgroundWork() {
		t.Fatal("precondition failed: session should have background work")
	}

	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/sessions/session_test/stop", nil))
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("stop status = %d, want 200 or 204; body=%s", rec.Code, rec.Body.String())
	}
	waitUntilNoBackground(t, us)
	if us.HasBackgroundWork() {
		t.Fatal("background work still running after /stop")
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
	_ = json.NewEncoder(w).Encode(finalResponseMap(content))
}

func writeServerTestToolCall(w http.ResponseWriter, id, name, args string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index": 0,
			"message": map[string]interface{}{
				"role": "assistant",
				"tool_calls": []map[string]interface{}{{
					"id":   id,
					"type": "function",
					"function": map[string]interface{}{
						"name":      name,
						"arguments": args,
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]interface{}{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
	})
}

func requestMessagesContain(messages []map[string]interface{}, needle string) bool {
	for _, m := range messages {
		if content, ok := m["content"].(string); ok && strings.Contains(content, needle) {
			return true
		}
	}
	return false
}

func requestHasRole(messages []map[string]interface{}, role string) bool {
	for _, m := range messages {
		if got, ok := m["role"].(string); ok && got == role {
			return true
		}
	}
	return false
}
