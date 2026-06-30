package gogent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gogent/internal/agent"
	"gogent/internal/config"
	"gogent/internal/model"
)

// hookRecorder collects the hook events fired during a test. NotifyHooks fans out
// synchronously, but the recorder is mutex-guarded so it is safe regardless.
type hookRecorder struct {
	mu     sync.Mutex
	events []HookEvent
}

func (r *hookRecorder) record(e HookEvent) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *hookRecorder) counts() map[HookEventType]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[HookEventType]int{}
	for _, e := range r.events {
		out[e.Type]++
	}
	return out
}

func (r *hookRecorder) first(t HookEventType) (HookEvent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Type == t {
			return e, true
		}
	}
	return HookEvent{}, false
}

func (r *hookRecorder) states() []agent.AgentState {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []agent.AgentState
	for _, e := range r.events {
		if e.Type == HookStateChange {
			out = append(out, e.State)
		}
	}
	return out
}

// newHookGogent builds a Gogent whose default model points at endpoint, isolated
// to temp directories so it touches neither the real home nor cwd.
func newHookGogent(t *testing.T, endpoint string) *Gogent {
	t.Helper()
	g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())
	g.config = &config.Config{
		DefaultModel: "test",
		Connections: []*config.ProviderConnection{
			{Name: "test-conn", APIType: "openai", Endpoint: endpoint},
		},
		ModelConfigs: []*config.ModelConfig{
			{Name: "test", Model: "test-model", Connection: "test-conn"},
		},
	}
	return g
}

// finalChatResponse is an OpenAI-style assistant message with no tool calls, so
// the task loop ends after a single round-trip.
func finalChatResponse(content string) map[string]interface{} {
	return map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       map[string]interface{}{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]interface{}{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18},
	}
}

// TestSendMessageFiresLifecycleHooks exercises a full successful turn and asserts
// the previously-inert lifecycle hooks now fire (issue #47): thinking/idle state
// transitions plus a response-complete event carrying the text and usage.
func TestSendMessageFiresLifecycleHooks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(finalChatResponse("done"))
	}))
	defer server.Close()

	g := newHookGogent(t, server.URL+"/v1/chat/completions")
	g.NewSession("s1")

	rec := &hookRecorder{}
	g.AddHook("rec", rec.record)

	resp, err := g.SendMessageToSessionWithModel(context.Background(), "s1", "root", "hello", "")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if resp == nil || resp.Content != "done" {
		t.Fatalf("unexpected response: %+v", resp)
	}

	fired := rec.counts()
	if fired[HookResponseComplete] == 0 {
		t.Errorf("expected HookResponseComplete to fire, got %v", fired)
	}
	if fired[HookError] != 0 {
		t.Errorf("did not expect HookError on a successful turn, got %v", fired)
	}

	// HookResponseComplete carries the final text, usage and identity.
	rc, ok := rec.first(HookResponseComplete)
	if !ok {
		t.Fatal("no HookResponseComplete recorded")
	}
	if rc.Response != "done" {
		t.Errorf("HookResponseComplete.Response = %q, want \"done\"", rc.Response)
	}
	if rc.Usage == nil || rc.Usage.TotalTokens != 18 {
		t.Errorf("HookResponseComplete.Usage = %+v, want total 18", rc.Usage)
	}
	if rc.SessionID != "s1" || rc.AgentID != "root" {
		t.Errorf("HookResponseComplete identity = %s/%s, want s1/root", rc.SessionID, rc.AgentID)
	}

	// State transitioned to thinking for the turn and back to idle at the end.
	states := rec.states()
	if len(states) < 2 || states[0] != agent.StateThinking || states[len(states)-1] != agent.StateIdle {
		t.Errorf("state transitions = %v, want thinking...idle", states)
	}
}

// TestSendMessageFiresErrorHook asserts a failing model turn fires HookError
// (and still restores idle state) instead of HookResponseComplete.
func TestSendMessageFiresErrorHook(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 400 is non-retryable, so the connection fails fast without backoff.
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer server.Close()

	g := newHookGogent(t, server.URL+"/v1/chat/completions")
	g.NewSession("s2")

	rec := &hookRecorder{}
	g.AddHook("rec", rec.record)

	if _, err := g.SendMessageToSessionWithModel(context.Background(), "s2", "root", "hello", ""); err == nil {
		t.Fatal("expected an error from the failing model server")
	}

	fired := rec.counts()
	if fired[HookError] == 0 {
		t.Errorf("expected HookError to fire, got %v", fired)
	}
	if fired[HookResponseComplete] != 0 {
		t.Errorf("did not expect HookResponseComplete on an error turn, got %v", fired)
	}
	// Idle is restored even on the error path: thinking then idle.
	if states := rec.states(); len(states) < 2 || states[len(states)-1] != agent.StateIdle {
		t.Errorf("state transitions = %v, want to end idle", states)
	}
	he, _ := rec.first(HookError)
	if he.Error == nil || he.Error.Message == "" {
		t.Errorf("HookError.Error should carry a message, got %+v", he.Error)
	}
}

// TestCompressionBridgesToHook verifies the model session's compaction callback
// is bridged to HookCompression on the registered hooks (issue #47).
func TestCompressionBridgesToHook(t *testing.T) {
	g := newHookGogent(t, "http://unused.test/v1/chat/completions")
	us := g.NewSession("s3")

	rec := &hookRecorder{}
	g.AddHook("rec", rec.record)

	root := us.GetAgent("root")
	if root == nil || root.ThoughtTrain == nil {
		t.Fatal("root agent/model session missing")
	}
	root.ThoughtTrain.ApplyCompressedTranscript([]model.Message{
		{Role: model.RoleUser, Content: "summary"},
	})

	he, ok := rec.first(HookCompression)
	if !ok {
		t.Fatalf("expected HookCompression to fire, got %v", rec.counts())
	}
	if he.Compression == nil {
		t.Error("HookCompression should carry CompressionInfo, got nil")
	}
	if he.SessionID != "s3" || he.AgentID != "root" {
		t.Errorf("HookCompression identity = %s/%s, want s3/root", he.SessionID, he.AgentID)
	}
}

// TestBridgeModelEvent table-checks the CallbackEvent -> HookEvent mapping,
// including that an unrecognized event type is dropped.
func TestBridgeModelEvent(t *testing.T) {
	tests := []struct {
		name string
		in   model.CallbackEventType
		want HookEventType
		fire bool
	}{
		{"token", model.EventTokenReceived, HookTokenReceived, true},
		{"response", model.EventResponseComplete, HookResponseComplete, true},
		{"error", model.EventError, HookError, true},
		{"compression", model.EventCompression, HookCompression, true},
		{"unknown", model.CallbackEventType("nonsense"), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())
			rec := &hookRecorder{}
			g.AddHook("rec", rec.record)

			g.bridgeModelEvent("sid", "aid", model.CallbackEvent{Type: tt.in})

			if !tt.fire {
				if len(rec.events) != 0 {
					t.Errorf("expected no hook for %q, got %v", tt.in, rec.counts())
				}
				return
			}
			ev, ok := rec.first(tt.want)
			if !ok {
				t.Fatalf("expected %s to fire for %q", tt.want, tt.in)
			}
			if ev.SessionID != "sid" || ev.AgentID != "aid" {
				t.Errorf("identity = %s/%s, want sid/aid", ev.SessionID, ev.AgentID)
			}
		})
	}
}
