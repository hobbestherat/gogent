package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gogent/internal/agent"
	"gogent/internal/config"
)

func TestIssue358DaemonMenuItemsAreModeAware(t *testing.T) {
	tests := []struct {
		name       string
		mode       DaemonMode
		start      func() error
		stop       func() error
		wantLabels []string
		wantEn     []bool
	}{
		{
			name:       "embedded offers enabled start and status",
			mode:       DaemonModeEmbedded,
			start:      func() error { return nil },
			wantLabels: []string{"&Start daemon", "", "Daemon stat&us…"},
			wantEn:     []bool{true, false, true},
		},
		{
			name:       "embedded start is visible but disabled without handler",
			mode:       DaemonModeEmbedded,
			wantLabels: []string{"&Start daemon", "", "Daemon stat&us…"},
			wantEn:     []bool{false, false, true},
		},
		{
			name:       "local attach offers stop and status",
			mode:       DaemonModeAttachedLocal,
			stop:       func() error { return nil },
			wantLabels: []string{"S&top daemon", "", "Daemon stat&us…"},
			wantEn:     []bool{true, false, true},
		},
		{
			name:       "remote attach disables local start stop",
			mode:       DaemonModeAttachedRemote,
			start:      func() error { return nil },
			stop:       func() error { return nil },
			wantLabels: []string{"Start/Stop (local daemon only)", "", "Daemon stat&us…"},
			wantEn:     []bool{false, false, true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
			w.SetHandlers(Handlers{
				DaemonMode:       func() DaemonMode { return tc.mode },
				StartDaemon:      tc.start,
				StopDaemon:       tc.stop,
				DaemonStatusInfo: func() (DaemonStatusReport, error) { return DaemonStatusReport{Mode: tc.mode}, nil },
			})

			items := w.daemonItems()
			if len(items) != len(tc.wantLabels) {
				t.Fatalf("items = %d, want %d", len(items), len(tc.wantLabels))
			}
			for i, item := range items {
				if item.Label != tc.wantLabels[i] {
					t.Errorf("item[%d].Label = %q, want %q", i, item.Label, tc.wantLabels[i])
				}
				if item.Enabled != tc.wantEn[i] {
					t.Errorf("item[%d].Enabled = %v, want %v", i, item.Enabled, tc.wantEn[i])
				}
			}
		})
	}
}

func TestIssue358FormatDaemonStatusIncludesLiveCountsAndHostMode(t *testing.T) {
	got := formatDaemonStatus(DaemonStatusReport{
		Mode:         DaemonModeAttachedRemote,
		Running:      true,
		Transport:    "tcp",
		Address:      "example.test:8080",
		PID:          4242,
		Uptime:       "1h2m3s",
		LiveSessions: 3,
		Watchers:     2,
		MCPServers:   []string{"fs", "git"},
	})

	for _, want := range []string{
		"Mode: attached to remote daemon",
		"State: running",
		"Transport: tcp",
		"Address: example.test:8080",
		"PID: 4242",
		"Uptime: 1h2m3s",
		"Live sessions: 3",
		"Watchers: 2",
		"MCP servers: fs, git",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status missing %q in:\n%s", want, got)
		}
	}

	stopped := formatDaemonStatus(DaemonStatusReport{
		Mode:      DaemonModeEmbedded,
		Running:   false,
		Transport: "embedded (in-process)",
		Note:      "Use Start daemon.",
	})
	if !strings.Contains(stopped, "State: stopped") || !strings.Contains(stopped, "Live sessions: 0") || !strings.Contains(stopped, "Use Start daemon.") {
		t.Fatalf("embedded stopped status missing expected details:\n%s", stopped)
	}
}

func TestIssue358APIClientDaemonStatusEndpoint(t *testing.T) {
	var method, path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		if r.URL.Path != "/api/daemon/status" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(DaemonStatusDTO{
			PID:           99,
			UptimeSeconds: 12,
			LiveSessions:  4,
			Watchers:      5,
			MCPServers:    []string{"a", "b"},
		})
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	got, err := client.DaemonStatus()
	if err != nil {
		t.Fatalf("DaemonStatus: %v", err)
	}
	if method != http.MethodGet || path != "/api/daemon/status" || auth != "Bearer tok" {
		t.Fatalf("request = %s %s auth %q, want GET /api/daemon/status with bearer", method, path, auth)
	}
	if got.PID != 99 || got.UptimeSeconds != 12 || got.LiveSessions != 4 || got.Watchers != 5 || strings.Join(got.MCPServers, ",") != "a,b" {
		t.Fatalf("status = %+v, want decoded live counts", got)
	}
}

func TestIssue358RemoteReconnectIsJumpToPresentAndNotReplay(t *testing.T) {
	type stream struct {
		ch chan GlobalEventDTO
	}

	var mu sync.Mutex
	streams := make([]stream, 0, 2)
	var eventGets int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/events":
			mu.Lock()
			eventGets++
			ch := make(chan GlobalEventDTO, 8)
			streams = append(streams, stream{ch: ch})
			mu.Unlock()

			w.Header().Set("Content-Type", "text/event-stream")
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Error("response writer is not a flusher")
				return
			}
			flusher.Flush()
			for {
				select {
				case ge, ok := <-ch:
					if !ok {
						return
					}
					b, _ := json.Marshal(ge)
					fmt.Fprintf(w, "data: %s\n\n", b)
					flusher.Flush()
				case <-r.Context().Done():
					return
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}

	var eventsMu sync.Mutex
	var seen []string
	rc := NewRemoteClient(client, func(sessionID string, ev agent.SessionEvent) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		seen = append(seen, sessionID+":"+ev.Text)
	}, nil)
	rec := &issue358Reconnector{}
	rc.SetReconnector(rec)
	rc.backoff = func(int) time.Duration { return time.Millisecond }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer rc.Close()

	waitForIssue358(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(streams) == 1
	}, "initial SSE subscription")

	mu.Lock()
	first := streams[0].ch
	mu.Unlock()
	first <- GlobalEventDTO{SessionID: "s1", Event: EventDTO{Type: string(agent.SessionEventThinking), Text: "before"}}
	close(first)

	waitForIssue358(t, func() bool { return rec.lostAttempts() >= 1 }, "disconnect notification")
	waitForIssue358(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(streams) == 2
	}, "replacement SSE subscription")
	waitForIssue358(t, func() bool { return rec.restoredCount() == 1 }, "reconnect notification")

	mu.Lock()
	second := streams[1].ch
	gets := eventGets
	mu.Unlock()
	second <- GlobalEventDTO{SessionID: "s1", Event: EventDTO{Type: string(agent.SessionEventFinal), Text: "present"}}

	waitForIssue358(t, func() bool {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		return len(seen) == 2
	}, "event after reconnect")

	eventsMu.Lock()
	got := append([]string(nil), seen...)
	eventsMu.Unlock()
	if strings.Join(got, ",") != "s1:before,s1:present" {
		t.Fatalf("events = %v, want only live first-stream event then new-stream present event", got)
	}
	if gets != 2 {
		t.Fatalf("event stream opens = %d, want 2", gets)
	}
}

func TestIssue358BackoffScheduleAndRetryNow(t *testing.T) {
	if got := []time.Duration{backoffFor(1), backoffFor(2), backoffFor(3), backoffFor(4), backoffFor(5), backoffFor(99)}; fmt.Sprint(got) != "[500ms 1s 2s 5s 10s 10s]" {
		t.Fatalf("backoff schedule = %v, want 500ms 1s 2s 5s capped at 10s", got)
	}

	rc := NewRemoteClient(nil, nil, nil)
	rc.RetryNow()
	rc.RetryNow()
	select {
	case <-rc.retryNow:
	default:
		t.Fatal("RetryNow did not enqueue a wakeup")
	}
	select {
	case <-rc.retryNow:
		t.Fatal("RetryNow should coalesce wakeups in a size-1 channel")
	default:
	}
}

func TestIssue358DisconnectModalIsBlockingAndHostAware(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	retried := 0
	w.SetReconnectControls("remote.example:8080", func() { retried++ })

	w.showDisconnectModal()
	w.renderDisconnectBody(3)

	if w.disconnectLayer == nil {
		t.Fatal("disconnect modal was not shown")
	}
	if top := w.desktop.TopLayer(); top != w.disconnectLayer {
		t.Fatalf("top layer = %v, want disconnect modal", top)
	}
	if !w.disconnectLayer.Modal || !w.disconnectLayer.AcceptInput {
		t.Fatalf("disconnect layer modal/input = %v/%v, want blocking modal accepting only its controls", w.disconnectLayer.Modal, w.disconnectLayer.AcceptInput)
	}
	if w.disconnectLayer.Name != "daemon-disconnect" {
		t.Fatalf("disconnect layer name = %q", w.disconnectLayer.Name)
	}
	if w.disconnectBody == nil {
		t.Fatal("disconnect modal body missing")
	}

	w.renderDisconnectBody(4)
	w.dismissDisconnectModal()
	if w.disconnectLayer != nil || w.disconnectBody != nil {
		t.Fatal("disconnect modal was not fully dismissed")
	}
	if retried != 0 {
		t.Fatalf("retry callback fired without clicking Retry now: %d", retried)
	}
}

type issue358Reconnector struct {
	mu       sync.Mutex
	lost     []int
	restored int
}

func (r *issue358Reconnector) OnConnectionLost(attempt int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lost = append(r.lost, attempt)
}

func (r *issue358Reconnector) OnConnectionRestored() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.restored++
}

func (r *issue358Reconnector) lostAttempts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.lost)
}

func (r *issue358Reconnector) restoredCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.restored
}

func waitForIssue358(t *testing.T, ok func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
