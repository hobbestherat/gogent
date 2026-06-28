package ui

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gogent/internal/agent"
)

// These tests cover the client side of the remote log interlace (issue #562):
// APIClient.StreamLogs decodes only "log" SSE frames (ignoring keep-alives,
// notifications and malformed JSON) and reuses the existing auth/transport, and
// RemoteClient.StreamLogsTo reconnects silently and stops on context cancel.

// logSSEServer serves the given raw SSE frames then closes the stream (so the
// client's channel ends).
func logSSEServer(t *testing.T, frames ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
}

func TestStreamLogs_DecodesOnlyLogFrames(t *testing.T) {
	frames := []string{
		"event: log\ndata: {\"time\":\"1970-01-01T00:00:01Z\",\"level\":\"INFO\",\"text\":\"one\"}\n\n",
		"event: notification\ndata: {\"x\":1}\n\n", // wrong event name → ignored
		"event: log\ndata: {bad json}\n\n",         // malformed → skipped, stream continues
		"event: log\ndata: {\"time\":\"1970-01-01T00:00:02Z\",\"level\":\"ERROR\",\"text\":\"two\"}\n\n",
		": keepalive\n\n", // comment → ignored
	}
	srv := logSSEServer(t, frames...)
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ch, err := client.StreamLogs(ctx)
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	var got []LogRecordDTO
	for rec := range ch {
		got = append(got, rec)
	}
	if len(got) != 2 {
		t.Fatalf("decoded %d records, want 2 (notification/malformed/comment ignored): %+v", len(got), got)
	}
	if got[0].Text != "one" || got[0].Level != "INFO" {
		t.Errorf("record 0 = %+v", got[0])
	}
	if got[1].Text != "two" || got[1].Level != "ERROR" {
		t.Errorf("record 1 = %+v", got[1])
	}
}

func TestStreamLogs_Non2xxReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "upstream broke")
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	if _, err := client.StreamLogs(context.Background()); err == nil {
		t.Fatal("StreamLogs returned nil error for a 500 response")
	}
}

func TestStreamLogs_ContextCancelClosesChannel(t *testing.T) {
	// A stream that never sends and never closes on its own, so only the client's
	// ctx cancellation (aborting the in-flight body) can end it. The handler also
	// honours a stop channel so the deferred server Close can never hang on a
	// blocked handler.
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Flush the headers so the client's http.Do returns and begins reading the
		// (never-arriving) body; without this Do blocks on the status line.
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	defer func() {
		close(stop) // release the handler regardless of connection state
		srv.Close()
	}()

	client, err := NewAPIClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := client.StreamLogs(ctx)
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("channel delivered a record after ctx cancel; want it closed")
		}
	case <-time.After(3 * time.Second):
		// Closing the Logs window cancels this ctx expecting the daemon stream to
		// stop; a channel that never closes leaks the StreamLogs goroutine (and,
		// via it, StreamLogsTo) for the life of the attach session.
		t.Fatal("StreamLogs channel did not close after ctx cancel (stream not aborted)")
	}
}

// StreamLogsTo reconnects silently on failure and must stop promptly when its
// context is cancelled (no leaked goroutine holding a daemon stream).
func TestStreamLogsTo_StopsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // every attempt fails → backoff loop
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	rc := NewRemoteClient(client, func(string, agent.SessionEvent) {}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		rc.StreamLogsTo(ctx, func(LogRecordDTO) {})
		close(done)
	}()

	// Let it fail an attempt and enter the backoff wait, then cancel.
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("StreamLogsTo did not stop after ctx cancel (leaked goroutine)")
	}
}

// StreamLogsTo delivers records to the sink when the stream is healthy.
func TestStreamLogsTo_DeliversRecords(t *testing.T) {
	srv := logSSEServer(t,
		"event: log\ndata: {\"time\":\"1970-01-01T00:00:01Z\",\"level\":\"INFO\",\"text\":\"delivered\"}\n\n",
	)
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	rc := NewRemoteClient(client, func(string, agent.SessionEvent) {}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan LogRecordDTO, 16)
	done := make(chan struct{})
	go func() {
		// Non-blocking sink: StreamLogsTo reconnects and re-delivers, and its sink
		// runs synchronously — a blocking sink would deadlock it (ctx cancel cannot
		// interrupt a blocked sink call). Production's sink is the non-blocking
		// w.Post, so this mirrors reality.
		rc.StreamLogsTo(ctx, func(rec LogRecordDTO) {
			select {
			case got <- rec:
			default:
			}
		})
		close(done)
	}()

	select {
	case rec := <-got:
		if rec.Text != "delivered" {
			t.Fatalf("delivered = %+v", rec)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StreamLogsTo did not deliver the record")
	}
	cancel()
	<-done
}

// TestStreamLogsTo_ReconnectMustNotDuplicate validates the resume-cursor fix for
// the remote interlace (issue #562). The daemon's LogStream primes its Snapshot()
// on every connection, and StreamLogsTo reconnects silently on a stream end, so to
// avoid re-delivering history the client sends ?since=<last record's time> and the
// server skips records at or before it (internal/server/logs.go + StreamLogsSince).
// The mock below honours that contract like the real daemon, then closes after
// priming (mimicking a blip); across reconnects each history line must arrive once.
func TestStreamLogsTo_ReconnectMustNotDuplicate(t *testing.T) {
	// Fixed monotonic history the mock re-primes on every connection.
	history := []struct {
		time string
		text string
	}{
		{"1970-01-01T00:00:01Z", "hist-one"},
		{"1970-01-01T00:00:02Z", "hist-two"},
		{"1970-01-01T00:00:03Z", "hist-three"},
	}
	var reqCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqCount, 1)
		// Honour the resume cursor exactly as the real LogStream does.
		var since time.Time
		if raw := r.URL.Query().Get("since"); raw != "" {
			if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
				since = t
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, h := range history {
			rt, _ := time.Parse(time.RFC3339Nano, h.time)
			if !since.IsZero() && !rt.After(since) {
				continue // already delivered before this reconnect
			}
			frame := fmt.Sprintf("event: log\ndata: {\"time\":%q,\"level\":\"INFO\",\"text\":%q}\n\n", h.time, h.text)
			_, _ = io.WriteString(w, frame)
			if flusher != nil {
				flusher.Flush()
			}
		}
		// Returning closes the stream; the client treats it as a blip and reconnects.
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	rc := NewRemoteClient(client, func(string, agent.SessionEvent) {}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var delivered []string
	done := make(chan struct{})
	go func() {
		rc.StreamLogsTo(ctx, func(rec LogRecordDTO) {
			mu.Lock()
			delivered = append(delivered, rec.Text)
			mu.Unlock()
		})
		close(done)
	}()

	// Wait for at least one reconnect (2+ connections), then let any re-primed
	// batch arrive. backoff(1)=500ms gates the first reconnect.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&reqCount) < 2 {
		time.Sleep(25 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond)
	cancel()
	<-done

	if conns := atomic.LoadInt32(&reqCount); conns < 2 {
		t.Fatalf("reconnect did not occur (connections=%d); test inconclusive", conns)
	}

	mu.Lock()
	defer mu.Unlock()
	seen := make(map[string]int)
	for _, txt := range delivered {
		seen[txt]++
	}
	for txt, n := range seen {
		if n > 1 {
			t.Errorf("daemon line %q delivered %d× across reconnects — resume cursor failed to suppress it", txt, n)
		}
	}
	if t.Failed() {
		t.Fatalf("StreamLogsTo re-delivered history on reconnect: deliveries=%v", delivered)
	}
}

// StreamLogsSince must put the resume cursor on the wire (?since=…, URL-encoded)
// so the daemon can skip already-delivered history. The server echoes the decoded
// value back in the frame, so the test asserts a clean round-trip without sharing
// state across goroutines.
func TestStreamLogsSince_SendsResumeCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		since := r.URL.Query().Get("since")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		frame := fmt.Sprintf("event: log\ndata: {\"time\":\"1970-01-01T00:00:09Z\",\"level\":\"INFO\",\"text\":\"cursor=%s\"}\n\n", since)
		_, _ = io.WriteString(w, frame)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := client.StreamLogsSince(ctx, "1970-01-01T00:00:05Z")
	if err != nil {
		t.Fatalf("StreamLogsSince: %v", err)
	}
	select {
	case rec := <-ch:
		// The colons in the RFC3339Nano timestamp must survive URL encode→decode.
		if rec.Text != "cursor=1970-01-01T00:00:05Z" {
			t.Fatalf("resume cursor did not round-trip: got %q", rec.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StreamLogsSince did not deliver the frame")
	}
}

// StreamLogs (the since-less convenience wrapper) must not send a cursor, so a
// fresh view primes the whole snapshot.
func TestStreamLogs_OmitsSinceForFreshView(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		since := r.URL.Query().Get("since")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		frame := fmt.Sprintf("event: log\ndata: {\"time\":\"1970-01-01T00:00:01Z\",\"level\":\"INFO\",\"text\":\"since=%s\"}\n\n", since)
		_, _ = io.WriteString(w, frame)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := client.StreamLogs(ctx)
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	select {
	case rec := <-ch:
		if rec.Text != "since=" {
			t.Fatalf("fresh StreamLogs sent since=%q, want empty", rec.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StreamLogs did not deliver the frame")
	}
}
