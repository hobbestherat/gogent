package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gogent/internal/model"
)

// fakeBackend is a programmable httpBackend for exercising the handlers without
// a live model.
type fakeBackend struct {
	content string
	err     error
	stats   map[string]interface{}
	block   chan struct{} // when non-nil, SendMessage blocks until closed
	called  int
	mu      sync.Mutex
}

func (f *fakeBackend) SendMessage(ctx context.Context, message, modelName string) (*model.CompletionResponse, error) {
	f.mu.Lock()
	f.called++
	f.mu.Unlock()
	if f.block != nil {
		// Honor the request context so a client disconnect aborts the loop
		// instead of blocking forever (issue #24).
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return &model.CompletionResponse{Content: f.content}, nil
}

func (f *fakeBackend) Stats() map[string]interface{} { return f.stats }

func newTestServer(t *testing.T, b httpBackend, token string, shutdown func()) *httptest.Server {
	t.Helper()
	if shutdown == nil {
		shutdown = func() {}
	}
	return httptest.NewServer(newHTTPHandler(b, token, shutdown))
}

func TestHealthEndpoint(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, "", nil)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got["status"] != "healthy" {
		t.Fatalf("status field = %q, want healthy", got["status"])
	}
}

// TestMessageEncodesAwkwardContent is the core regression: model output
// containing quotes, newlines and backslashes must still yield valid JSON.
func TestMessageEncodesAwkwardContent(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"quotes", `He said "hello" to the world`},
		{"newlines", "line one\nline two\nline three"},
		{"backslashes", `C:\path\to\file and a \n literal`},
		{"json-looking", `{"nested":"value","arr":[1,2,3]}`},
		{"unicode", "emoji 🚀 and accents café"},
		{"control", "tab\tand\x00null"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, &fakeBackend{content: tc.content}, "", nil)
			defer srv.Close()

			resp, err := http.PostForm(srv.URL+"/message", map[string][]string{"message": {"hi"}})
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			var got struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("response is not valid JSON: %v", err)
			}
			if !got.Success {
				t.Fatalf("success = false, want true")
			}
			if got.Message != tc.content {
				t.Fatalf("message round-trip mismatch:\n got %q\nwant %q", got.Message, tc.content)
			}
		})
	}
}

func TestMessageErrorIsValidJSON(t *testing.T) {
	// An error string containing a quote must not corrupt the JSON envelope.
	srv := newTestServer(t, &fakeBackend{err: &jsonyError{`bad "input" \ here`}}, "", nil)
	defer srv.Close()

	resp, err := http.PostForm(srv.URL+"/message", map[string][]string{"message": {"hi"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("error response is not valid JSON: %v", err)
	}
	if got.Success {
		t.Fatalf("success = true, want false")
	}
	if got.Error != `bad "input" \ here` {
		t.Fatalf("error = %q, want round-trip", got.Error)
	}
}

type jsonyError struct{ msg string }

func (e *jsonyError) Error() string { return e.msg }

func TestMessageMethodAndValidation(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{content: "ok"}, "", nil)
	defer srv.Close()

	t.Run("GET rejected", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/message")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", resp.StatusCode)
		}
	})

	t.Run("missing message", func(t *testing.T) {
		resp, err := http.PostForm(srv.URL+"/message", map[string][]string{})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})
}

func TestMessageBodyTooLarge(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{content: "ok"}, "", nil)
	defer srv.Close()

	big := strings.Repeat("a", httpMaxRequestBody+1024)
	resp, err := http.PostForm(srv.URL+"/message", map[string][]string{"message": {big}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body cap)", resp.StatusCode)
	}
}

func TestMessageClientDisconnectAbandons(t *testing.T) {
	block := make(chan struct{})
	b := &fakeBackend{content: "slow", block: block}
	srv := newTestServer(t, b, "", nil)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/message",
		strings.NewReader("message=hi"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	errc := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		errc <- err
	}()

	// Let the handler start the (blocked) backend call, then cancel the client.
	deadline := time.After(2 * time.Second)
	for {
		b.mu.Lock()
		started := b.called > 0
		b.mu.Unlock()
		if started {
			break
		}
		select {
		case <-deadline:
			t.Fatal("backend never invoked")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("expected client request to error on cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after client disconnect")
	}
	close(block) // release the background goroutine
}

func TestStatusEndpoint(t *testing.T) {
	b := &fakeBackend{stats: map[string]interface{}{"tokens_in": 42}}
	srv := newTestServer(t, b, "", nil)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got struct {
		ToolLogs []string       `json:"tool_logs"`
		Stats    map[string]any `json:"stats"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got.ToolLogs == nil {
		t.Fatal("tool_logs should be a non-nil array")
	}
	if got.Stats["tokens_in"].(float64) != 42 {
		t.Fatalf("stats.tokens_in = %v, want 42", got.Stats["tokens_in"])
	}
}

func TestExitGuard(t *testing.T) {
	t.Run("GET rejected", func(t *testing.T) {
		var fired bool
		srv := newTestServer(t, &fakeBackend{}, "", func() { fired = true })
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/exit")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", resp.StatusCode)
		}
		if fired {
			t.Fatal("shutdown fired on rejected GET")
		}
	})

	t.Run("local POST allowed", func(t *testing.T) {
		shut := make(chan struct{}, 1)
		srv := newTestServer(t, &fakeBackend{}, "", func() { shut <- struct{}{} })
		defer srv.Close()

		// httptest serves on loopback, so this POST is a local caller.
		resp, err := http.Post(srv.URL+"/exit", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		select {
		case <-shut:
		case <-time.After(time.Second):
			t.Fatal("shutdown not invoked for local POST")
		}
	})
}

// TestExitGuardRemote drives the handler directly so we can spoof a non-local
// RemoteAddr, verifying the token gate.
func TestExitGuardRemote(t *testing.T) {
	cases := []struct {
		name       string
		token      string // configured server token
		header     string // X-Gogent-Token sent
		wantStatus int
		wantFired  bool
	}{
		{"no token configured, remote denied", "", "", http.StatusForbidden, false},
		{"token configured, missing header", "s3cret", "", http.StatusForbidden, false},
		{"token configured, wrong header", "s3cret", "nope", http.StatusForbidden, false},
		{"token configured, correct header", "s3cret", "s3cret", http.StatusOK, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var fired bool
			h := newHTTPHandler(&fakeBackend{}, tc.token, func() { fired = true })

			req := httptest.NewRequest(http.MethodPost, "/exit", nil)
			req.RemoteAddr = "203.0.113.7:54321" // non-loopback
			if tc.header != "" {
				req.Header.Set("X-Gogent-Token", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			// shutdown runs in a goroutine; give it a moment when expected.
			if tc.wantFired {
				deadline := time.After(time.Second)
				for !fired {
					select {
					case <-deadline:
						t.Fatal("shutdown not invoked")
					default:
						time.Sleep(time.Millisecond)
					}
				}
			} else if fired {
				t.Fatal("shutdown fired when it should not have")
			}
		})
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8080", true},
		{"[::1]:8080", true},
		{"203.0.113.7:54321", false},
		{"10.0.0.5:80", false},
		{"not-an-addr", false},
		{"localhost:80", false}, // hostnames aren't IPs; only parsed IPs count
	}
	for _, tc := range cases {
		if got := isLoopbackAddr(tc.addr); got != tc.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}
