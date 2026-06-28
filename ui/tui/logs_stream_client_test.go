package ui

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
