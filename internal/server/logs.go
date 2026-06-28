package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/hobbestherat/webapi"
)

// LogRecord is one diagnostic line exposed over the API. It mirrors the diag
// ring's record but is defined here so internal/server never imports
// internal/diag — the daemon supplies an adapter (see cmd/daemon.go), keeping
// diag a leaf and respecting the import boundary the package already maintains
// for webapi's logger shim (see stdLogPrintf).
type LogRecord struct {
	Time  time.Time
	Level string // "INFO" | "WARN" | "ERROR"
	Text  string // already redacted by the diag path
}

// LogStreamer is the daemon-side source of diagnostic logs the server streams to
// remote clients (issue #562). It is implemented by an adapter over the diag
// ring. Snapshot primes a newly-connected client with recent history; Subscribe
// delivers live records on a buffered channel that drops on overflow (best-effort
// live tail) and returns an unsubscribe func the producer calls on disconnect.
type LogStreamer interface {
	Snapshot() []LogRecord
	Subscribe() (<-chan LogRecord, func())
}

// logSSEEventName is the SSE event name for a streamed log record. It is distinct
// from every agent SessionEvent type and from the notification frame name, so a
// client tells log frames apart by name alone.
const logSSEEventName = "log"

// LogStream handles GET /api/logs/stream — a live diagnostic-log stream (issue
// #562). It primes the client with the ring's recent history, then fans live
// records out as SSE until the client disconnects. With no log source wired
// (embedded mode) it streams nothing and ends when the client goes away.
func (svc eventsSvc) LogStream(r *http.Request) (interface{}, error) {
	src := svc.s.logs
	return &webapi.EventStreamResponse{
		Producer: func(stream webapi.EventStream) error {
			if src == nil {
				<-stream.Context().Done()
				return nil
			}
			for _, rec := range src.Snapshot() {
				if err := stream.Send(logSSE(rec)); err != nil {
					return fmt.Errorf("send log history: %w", err)
				}
			}
			sub, unsub := src.Subscribe()
			defer unsub()
			for {
				select {
				case <-stream.Context().Done():
					return nil
				case rec := <-sub:
					if err := stream.Send(logSSE(rec)); err != nil {
						return fmt.Errorf("send log record: %w", err)
					}
				}
			}
		},
	}, nil
}

// logRecordView is the JSON shape of a streamed log record. Time is RFC3339Nano
// so the client can render and (per-stream) order it.
type logRecordView struct {
	Time  string `json:"time"`
	Level string `json:"level"`
	Text  string `json:"text"`
}

// logSSE builds the SSE frame for one log record.
func logSSE(rec LogRecord) webapi.SSEvent {
	return webapi.SSEvent{
		Name: logSSEEventName,
		Data: marshalJSON(logRecordView{
			Time:  rec.Time.Format(time.RFC3339Nano),
			Level: rec.Level,
			Text:  rec.Text,
		}),
	}
}
