package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gogent/internal/agent"
	"gogent/internal/gogent"
	"gogent/internal/permission"
)

func TestAPIClientSendMessageAndStreamEvents(t *testing.T) {
	type requestRecord struct {
		Method        string
		RequestURI    string
		Authorization string
		Body          sendMessageBody
	}

	records := make(chan requestRecord, 1)
	eventsStarted := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sessions/s/a b/messages":
			var body sendMessageBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode message body: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			records <- requestRecord{
				Method:        r.Method,
				RequestURI:    r.RequestURI,
				Authorization: r.Header.Get("Authorization"),
				Body:          body,
			}
			_ = json.NewEncoder(w).Encode(MessageDTO{Role: "assistant", Content: "ok"})
		case "/api/events":
			if got := r.Header.Get("Accept"); got != "text/event-stream" {
				t.Errorf("Accept = %q, want text/event-stream", got)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
				t.Errorf("Authorization = %q, want bearer token", got)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Error("response writer is not a flusher")
				return
			}
			close(eventsStarted)
			fmt.Fprint(w, "event: final\n")
			fmt.Fprint(w, `data: {"session_id":"s/a b","event":{"type":"final","text":"done","step":3}}`+"\n\n")
			flusher.Flush()
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "test-token")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}

	msg, err := client.SendMessage(context.Background(), "s/a b", "hello", "m1", "medium")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if msg.Content != "ok" {
		t.Fatalf("send response content = %q, want ok", msg.Content)
	}
	select {
	case rec := <-records:
		if rec.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", rec.Method)
		}
		if rec.RequestURI != "/api/sessions/s%2Fa%20b/messages" {
			t.Fatalf("RequestURI = %q, want escaped session path", rec.RequestURI)
		}
		if rec.Authorization != "Bearer test-token" {
			t.Fatalf("Authorization = %q, want bearer token", rec.Authorization)
		}
		if rec.Body.Message != "hello" || rec.Body.Model != "m1" || rec.Body.Effort != "medium" {
			t.Fatalf("body = %+v, want message/model/effort", rec.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not receive SendMessage request")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := client.StreamEvents(ctx)
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	select {
	case <-eventsStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("event stream did not start")
	}
	select {
	case ev := <-events:
		if ev.SessionID != "s/a b" {
			t.Fatalf("session id = %q, want escaped id round-tripped", ev.SessionID)
		}
		if ev.Event.Type != string(agent.SessionEventFinal) || ev.Event.Text != "done" || ev.Event.Step != 3 {
			t.Fatalf("event = %+v, want final/done/3", ev.Event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for streamed event")
	}
}

func TestAPIClientUnixSocketTransportHealth(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "daemon.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer os.Remove(sock)

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			t.Errorf("path = %q, want /api/health", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	})}
	defer srv.Close()
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			t.Errorf("Serve: %v", err)
		}
	}()

	client, err := NewAPIClient("unix://"+sock, "ignored-token")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	if err := client.Health(); err != nil {
		t.Fatalf("Health over unix socket: %v", err)
	}
}

func TestNewAPIClientRejectsInvalidConnectAddresses(t *testing.T) {
	cases := []string{
		"/tmp/daemon.sock",
		"unix://",
		"ssh://host",
	}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			if _, err := NewAPIClient(tc, ""); err == nil {
				t.Fatal("NewAPIClient succeeded, want error")
			}
		})
	}
}

func TestParseSSEToleratesCommentsMultilineAndTrailingEvent(t *testing.T) {
	input := strings.NewReader(strings.Join([]string{
		": keepalive",
		"event: final",
		"data: first",
		"data: second",
		"",
		"event: usage",
		"data: trailing",
	}, "\n"))

	var got []sseFrame
	for frame := range parseSSE(context.Background(), input) {
		got = append(got, frame)
	}
	if len(got) != 2 {
		t.Fatalf("frames = %d, want 2: %+v", len(got), got)
	}
	if got[0].name != "final" || got[0].data != "first\nsecond" {
		t.Fatalf("first frame = %+v, want multiline final", got[0])
	}
	if got[1].name != "usage" || got[1].data != "trailing" {
		t.Fatalf("second frame = %+v, want trailing usage", got[1])
	}
}

func TestRemoteClientStartDeliversSSEToSink(t *testing.T) {
	ready := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/events" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		close(ready)
		fmt.Fprint(w, "event: usage\n")
		fmt.Fprint(w, `data: {"session_id":"sess-1","event":{"type":"usage","stats":{"turns":2,"tokens_in":11,"tokens_out":7,"tool_calls":1,"context_tokens":18,"context_window":100}}}`+"\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	received := make(chan agent.SessionEvent, 1)
	rc := NewRemoteClient(client, func(sessionID string, ev agent.SessionEvent) {
		if sessionID != "sess-1" {
			t.Errorf("session id = %q, want sess-1", sessionID)
		}
		received <- ev
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer rc.Close()

	if err := rc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("stream was not opened")
	}
	select {
	case ev := <-received:
		if ev.Type != agent.SessionEventUsage {
			t.Fatalf("event type = %q, want usage", ev.Type)
		}
		if ev.Stats.Turns != 2 || ev.Stats.TokensIn != 11 || ev.Stats.TokensOut != 7 || ev.Stats.ToolCalls != 1 {
			t.Fatalf("stats = %+v, want streamed usage stats", ev.Stats)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for sink event")
	}
}

func TestRemoteHandlersMapCallsToHTTPRequests(t *testing.T) {
	type requestRecord struct {
		Method     string
		RequestURI string
		Body       map[string]any
	}
	records := make(chan requestRecord, 16)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		records <- requestRecord{Method: r.Method, RequestURI: r.RequestURI, Body: body}
		switch {
		case r.URL.Path == "/api/sessions":
			_ = json.NewEncoder(w).Encode(SessionDTO{ID: "sess/1", Title: "Title", Live: true, PrimaryModel: "m1"})
		case r.URL.Path == "/api/sessions/sess/1/messages":
			_ = json.NewEncoder(w).Encode(MessageDTO{Role: "assistant", Content: "ok"})
		case strings.HasSuffix(r.URL.Path, "/undo"):
			_ = json.NewEncoder(w).Encode(map[string]string{"result": "undone"})
		case strings.HasSuffix(r.URL.Path, "/rewind"):
			_ = json.NewEncoder(w).Encode(map[string]string{"result": "rewound"})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	rc := NewRemoteClient(client, nil, nil)
	h := rc.Handlers()

	h.OnCreate("sess/1", "Title")
	h.OnStop("sess/1")
	h.OnInject("sess/1", "note")
	if got, err := h.OnUndo("sess/1"); err != nil || got != "undone" {
		t.Fatalf("OnUndo = %q, %v; want undone, nil", got, err)
	}
	if got, err := h.OnRewind("sess/1", 3); err != nil || got != "rewound" {
		t.Fatalf("OnRewind = %q, %v; want rewound, nil", got, err)
	}
	h.OnSetPlanMode("sess/1", true)
	h.OnSend("sess/1", "hello", "m1", "high")

	want := []struct {
		method string
		uri    string
	}{
		{http.MethodPost, "/api/sessions"},
		{http.MethodPost, "/api/sessions/sess%2F1/stop"},
		{http.MethodPost, "/api/sessions/sess%2F1/inject"},
		{http.MethodPost, "/api/sessions/sess%2F1/undo"},
		{http.MethodPost, "/api/sessions/sess%2F1/rewind"},
		{http.MethodPut, "/api/sessions/sess%2F1/plan-mode"},
		{http.MethodPost, "/api/sessions/sess%2F1/messages"},
	}
	for _, w := range want {
		select {
		case got := <-records:
			if got.Method != w.method || got.RequestURI != w.uri {
				t.Fatalf("request = %s %s, want %s %s", got.Method, got.RequestURI, w.method, w.uri)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s %s", w.method, w.uri)
		}
	}
}

func TestRemoteHandlersExposeSavedSessionBrowser(t *testing.T) {
	client, err := NewAPIClient("http://127.0.0.1:1", "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	h := NewRemoteClient(client, nil, nil).Handlers()
	if h.ListSavedSessions == nil {
		t.Fatal("ListSavedSessions is nil; remote TUI cannot browse daemon sessions")
	}
	if h.OpenSavedSession == nil {
		t.Fatal("OpenSavedSession is nil; remote TUI cannot open daemon sessions")
	}
}

type recordingApprover struct {
	permissions chan permission.Request
	edits       chan gogent.EditReviewRequest
}

func (a *recordingApprover) AskPermission(req permission.Request) permission.Decision {
	a.permissions <- req
	return permission.DecisionAlways
}

func (a *recordingApprover) ReviewEdit(req gogent.EditReviewRequest) gogent.EditReviewDecision {
	a.edits <- req
	return gogent.EditApproveAll
}

func TestRemoteClientPollApprovalsPostsDecisions(t *testing.T) {
	var mu sync.Mutex
	decisions := make(map[string]string)
	approvalList := []ApprovalDTO{
		{
			ID:        "perm-1",
			Kind:      "permission",
			SessionID: "sess",
			AgentID:   "root",
			Permission: &PermissionDetail{
				Action:   "read",
				Resource: "README.md",
				Detail:   "need context",
			},
		},
		{
			ID:        "edit-1",
			Kind:      "edit_review",
			SessionID: "sess",
			AgentID:   "root",
			EditReview: &EditReviewDetail{
				Path: "main.go",
				Op:   "write",
				Diff: "--- old\n+++ new",
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/approvals":
			mu.Lock()
			pending := append([]ApprovalDTO(nil), approvalList...)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(pending)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/approvals/") && strings.HasSuffix(r.URL.Path, "/decision"):
			parts := strings.Split(r.URL.Path, "/")
			id := parts[3]
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode decision: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			decisions[id] = body["decision"]
			if len(decisions) == 2 {
				approvalList = nil
			}
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	approver := &recordingApprover{
		permissions: make(chan permission.Request, 1),
		edits:       make(chan gogent.EditReviewRequest, 1),
	}
	rc := NewRemoteClient(client, nil, approver)
	rc.pollEvery = 10 * time.Millisecond
	go rc.pollApprovals()
	defer rc.Close()

	select {
	case req := <-approver.permissions:
		if req.Action != permission.Action("read") || req.Resource != "README.md" || req.Context.SessionID != "sess" {
			t.Fatalf("permission request = %+v, want read README.md in sess", req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("permission approval was not presented")
	}
	select {
	case req := <-approver.edits:
		if req.Path != "main.go" || req.Op != "write" || req.SessionID != "sess" {
			t.Fatalf("edit request = %+v, want main.go write in sess", req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("edit approval was not presented")
	}

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		permDecision := decisions["perm-1"]
		editDecision := decisions["edit-1"]
		mu.Unlock()
		if permDecision == "always" && editDecision == "approve_all" {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("decisions = %#v, want perm always and edit approve_all", decisions)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}
