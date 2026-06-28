package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hobbestherat/webapi"

	"gogent/internal/gogent"
)

// fakeLogStreamer is a test LogStreamer: controllable snapshot history plus a
// live channel the test pushes records onto.
type fakeLogStreamer struct {
	hist []LogRecord
	live chan LogRecord
}

func (f fakeLogStreamer) Snapshot() []LogRecord { return f.hist }
func (f fakeLogStreamer) Subscribe() (<-chan LogRecord, func()) {
	return f.live, func() {}
}

// fakeEventStream collects the SSE events a Producer sends, signalling each via
// the sent channel so a test can deterministically wait for N events.
type fakeEventStream struct {
	ctx  context.Context
	sent chan webapi.SSEvent
}

func (s *fakeEventStream) Send(ev webapi.SSEvent) error {
	s.sent <- ev
	return nil
}
func (s *fakeEventStream) Comment(string) error     { return nil }
func (s *fakeEventStream) Context() context.Context { return s.ctx }

func newFakeStream(ctx context.Context) *fakeEventStream {
	return &fakeEventStream{ctx: ctx, sent: make(chan webapi.SSEvent, 8)}
}

func TestLogStream_PrimesHistoryThenStreamsLive(t *testing.T) {
	t.Parallel()
	live := make(chan LogRecord, 4)
	src := fakeLogStreamer{
		hist: []LogRecord{
			{Time: time.Unix(1, 0).UTC(), Level: "INFO", Text: "old1"},
			{Time: time.Unix(2, 0).UTC(), Level: "WARN", Text: "old2"},
		},
		live: live,
	}
	srv := NewServer(gogent.NewGogent(t.TempDir()), Options{Logs: src})

	resp, err := eventsSvc{s: srv}.LogStream(httptest.NewRequest(http.MethodGet, "/api/logs/stream", nil))
	if err != nil {
		t.Fatalf("LogStream: %v", err)
	}
	esr, ok := resp.(*webapi.EventStreamResponse)
	if !ok {
		t.Fatalf("LogStream returned %T, want *EventStreamResponse", resp)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stream := newFakeStream(ctx)
	done := make(chan error, 1)
	go func() { done <- esr.Producer(stream) }()

	// One live record after the two primed history records.
	live <- LogRecord{Time: time.Unix(3, 0), Level: "ERROR", Text: "new"}

	got := make([]webapi.SSEvent, 0, 3)
	for len(got) < 3 {
		select {
		case ev := <-stream.sent:
			got = append(got, ev)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for events; got %d", len(got))
		}
	}
	cancel()
	<-done

	wantText := []string{"old1", "old2", "new"} // history oldest-first, then live
	for i, want := range wantText {
		if got[i].Name != logSSEEventName {
			t.Fatalf("event %d name = %q, want %q", i, got[i].Name, logSSEEventName)
		}
		if !strings.Contains(string(got[i].Data), want) {
			t.Fatalf("event %d data = %q, want it to contain %q", i, got[i].Data, want)
		}
	}
	// The level and the RFC3339Nano time are carried in the JSON payload.
	if !strings.Contains(string(got[2].Data), `"level":"ERROR"`) {
		t.Fatalf("live event level not encoded: %q", got[2].Data)
	}
	if !strings.Contains(string(got[0].Data), "1970-01-01T00:00:01") {
		t.Fatalf("history time not RFC3339Nano-encoded: %q", got[0].Data)
	}
}

// With no log source (embedded mode) the producer must not spin or error — it
// simply holds the connection open until the client goes away.
func TestLogStream_NilSourceHoldsUntilContextDone(t *testing.T) {
	t.Parallel()
	srv := NewServer(gogent.NewGogent(t.TempDir()), Options{}) // Logs nil

	resp, err := eventsSvc{s: srv}.LogStream(httptest.NewRequest(http.MethodGet, "/api/logs/stream", nil))
	if err != nil {
		t.Fatalf("LogStream: %v", err)
	}
	esr := resp.(*webapi.EventStreamResponse)

	ctx, cancel := context.WithCancel(context.Background())
	stream := newFakeStream(ctx)
	done := make(chan error, 1)
	go func() { done <- esr.Producer(stream) }()

	cancel() // unblock the "<-Context().Done()" the nil branch waits on
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("nil-source producer returned %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("nil-source producer did not return when the client disconnected")
	}
}

// A slow/stalled subscriber (a full live channel the producer cannot send to)
// must not be able to wedge the producer's history path: the snapshot is sent
// first; if the client never drains, history sends block but that is the
// client's own connection, not the daemon. This test just confirms the wired
// endpoint + auth over loopback and that history is flushed.
func TestLogStream_HTTPStreamsHistoryOverLoopback(t *testing.T) {
	t.Parallel()
	src := fakeLogStreamer{
		hist: []LogRecord{{Time: time.Unix(1, 0), Level: "INFO", Text: "loopback-history"}},
		live: make(chan LogRecord), // never delivers; client will time out reading
	}
	srv := NewServer(gogent.NewGogent(t.TempDir()), Options{Password: "x", Logs: src})
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpSrv.URL+"/api/logs/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("log stream request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d over loopback, want 200 (auth should pass)", resp.StatusCode)
	}

	// The producer flushes the history frame immediately; read until we see it or
	// the context deadline closes the body.
	br := bufio.NewReader(resp.Body)
	var saw string
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !strings.Contains(saw, "loopback-history") {
		line, err := br.ReadString('\n')
		saw += line
		if err != nil {
			break
		}
	}
	if !strings.Contains(saw, "event: log") {
		t.Fatalf("response did not carry an SSE log frame: %q", saw)
	}
	if !strings.Contains(saw, "loopback-history") {
		t.Fatalf("response did not stream the primed history: %q", saw)
	}
}

func TestLogsEndpoint_RegisteredAuthRequired(t *testing.T) {
	t.Parallel()
	srv := NewServer(gogent.NewGogent(t.TempDir()), Options{Password: "x"})
	api := srv.buildAPI()
	for _, ep := range api.Endpoints {
		if ep.Path == "/logs/stream" {
			if ep.Method != http.MethodGet {
				t.Fatalf("/logs/stream method = %v, want GET", ep.Method)
			}
			if ep.AuthLevel != webapi.AuthRequired {
				t.Fatalf("/logs/stream AuthLevel = %v, want AuthRequired (reuse existing auth)", ep.AuthLevel)
			}
			return
		}
	}
	t.Fatal("/logs/stream endpoint is not registered in the API table")
}

// logSSE encodes the canonical level strings and the distinct "log" event name a
// remote client discriminates on.
func TestLogSSE_NameAndPayload(t *testing.T) {
	t.Parallel()
	ev := logSSE(LogRecord{Time: time.Unix(0, 0).UTC(), Level: "WARN", Text: "hi"})
	if ev.Name != "log" {
		t.Fatalf("event name = %q, want log", ev.Name)
	}
	body := string(ev.Data)
	for _, want := range []string{`"level":"WARN"`, `"text":"hi"`, `"time":"1970-01-01T00:00:00Z"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("payload %q missing %s", body, want)
		}
	}
}
