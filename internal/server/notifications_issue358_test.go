package server

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"gogent/internal/agent"
	"gogent/internal/notify"
)

const (
	notificationTitleIssue358 = "Watcher finished"
	notificationBodyIssue358  = "nightly check passed"
)

func TestNotificationSinkIssue358ConnectedClientGetsSSEAndNoLocalFallback(t *testing.T) {
	srv := testNotificationServerIssue358()
	sub, unsub := srv.hub.subscribeGlobal()
	defer unsub()

	var localCalls int
	sink := srv.NotificationSink(func(reason string) bool {
		return reason == "watcher"
	}, func(title, body string) {
		localCalls++
	})

	sink("watcher", notificationTitleIssue358, notificationBodyIssue358)

	te := recvNotificationIssue358(t, sub)
	if te.notif.Title != notificationTitleIssue358 {
		t.Fatalf("title = %q, want %q", te.notif.Title, notificationTitleIssue358)
	}
	if te.notif.Body != notificationBodyIssue358 {
		t.Fatalf("body = %q, want %q", te.notif.Body, notificationBodyIssue358)
	}
	if te.notif.Reason != "watcher" {
		t.Fatalf("reason = %q, want watcher", te.notif.Reason)
	}
	if te.notif.Timestamp != "2026-06-24T12:34:56Z" {
		t.Fatalf("timestamp = %q, want fixed RFC3339 timestamp", te.notif.Timestamp)
	}
	if localCalls != 0 {
		t.Fatalf("local fallback calls = %d, want 0 while global client is subscribed", localCalls)
	}
}

func TestNotificationSinkIssue358FullConnectedSubscriberFallsBackAndReplays(t *testing.T) {
	srv := testNotificationServerIssue358()
	sub, unsub := srv.hub.subscribeGlobal()
	defer unsub()

	for i := 0; i < cap(sub); i++ {
		srv.hub.deliver("busy-session", agent.SessionEvent{Type: agent.SessionEventThinking, Step: i})
	}

	var mu sync.Mutex
	var local []string
	sink := srv.NotificationSink(func(reason string) bool {
		return reason == string(notify.ReasonWatcher)
	}, func(title, body string) {
		mu.Lock()
		defer mu.Unlock()
		local = append(local, title+": "+body)
	})

	sink(string(notify.ReasonWatcher), notificationTitleIssue358, notificationBodyIssue358)

	mu.Lock()
	if len(local) != 1 || local[0] != notificationTitleIssue358+": "+notificationBodyIssue358 {
		t.Fatalf("local fallback calls = %#v, want fallback when every connected subscriber buffer is full", local)
	}
	mu.Unlock()

	unsub()
	replay, replayUnsub := srv.hub.subscribeGlobal()
	defer replayUnsub()

	te := recvNotificationIssue358(t, replay)
	if te.notif.Title != notificationTitleIssue358 || te.notif.Body != notificationBodyIssue358 {
		t.Fatalf("replayed full-buffer notification = %+v, want original notification", *te.notif)
	}
}

func TestNotificationSinkIssue358NoClientUsesLocalFallbackAndReplaysOnReconnect(t *testing.T) {
	srv := testNotificationServerIssue358()

	var mu sync.Mutex
	var local []string
	sink := srv.NotificationSink(func(string) bool { return true }, func(title, body string) {
		mu.Lock()
		defer mu.Unlock()
		local = append(local, title+": "+body)
	})

	sink("complete", "Agent done", "summary")

	mu.Lock()
	if len(local) != 1 || local[0] != "Agent done: summary" {
		t.Fatalf("local fallback calls = %#v, want one daemon-local notification", local)
	}
	mu.Unlock()

	sub, unsub := srv.hub.subscribeGlobal()
	defer unsub()

	te := recvNotificationIssue358(t, sub)
	if te.notif.Title != "Agent done" || te.notif.Body != "summary" || te.notif.Reason != "complete" {
		t.Fatalf("replayed notification = %+v, want original notification", *te.notif)
	}

	select {
	case extra := <-sub:
		t.Fatalf("ring was not drained after reconnect; got extra frame %+v", extra)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestNotificationRingIssue358BoundedToLast50OldestDropped(t *testing.T) {
	srv := testNotificationServerIssue358()
	sink := srv.NotificationSink(func(string) bool { return true }, nil)

	for i := 0; i < notificationRingSize+7; i++ {
		sink("watcher", fmt.Sprintf("notification-%02d", i), "body")
	}

	sub, unsub := srv.hub.subscribeGlobal()
	defer unsub()

	var got []string
	for i := 0; i < notificationRingSize; i++ {
		got = append(got, recvNotificationIssue358(t, sub).notif.Title)
	}
	if len(got) != notificationRingSize {
		t.Fatalf("replay count = %d, want %d", len(got), notificationRingSize)
	}
	if got[0] != "notification-07" {
		t.Fatalf("first replay = %q, want oldest retained notification-07", got[0])
	}
	if got[len(got)-1] != "notification-56" {
		t.Fatalf("last replay = %q, want newest notification-56", got[len(got)-1])
	}

	select {
	case extra := <-sub:
		t.Fatalf("ring replay exceeded bound %d; extra frame %+v", notificationRingSize, extra)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestNotificationSinkIssue358GateSuppressesEveryDeliveryPath(t *testing.T) {
	srv := testNotificationServerIssue358()
	sub, unsub := srv.hub.subscribeGlobal()
	defer unsub()

	var gateReasons []string
	var localCalls int
	sink := srv.NotificationSink(func(reason string) bool {
		gateReasons = append(gateReasons, reason)
		return false
	}, func(title, body string) {
		localCalls++
	})

	sink("watcher", "disabled", "should not appear")

	if len(gateReasons) != 1 || gateReasons[0] != "watcher" {
		t.Fatalf("gate reasons = %#v, want exactly watcher", gateReasons)
	}
	if localCalls != 0 {
		t.Fatalf("local fallback calls = %d, want 0 for disabled reason", localCalls)
	}
	select {
	case te := <-sub:
		t.Fatalf("disabled notification reached SSE subscriber: %+v", te)
	case <-time.After(25 * time.Millisecond):
	}

	replay, replayUnsub := srv.hub.subscribeGlobal()
	defer replayUnsub()
	select {
	case te := <-replay:
		t.Fatalf("disabled notification was buffered for replay: %+v", te)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestAgentCompletionIssue358EmitsNotificationForReplay(t *testing.T) {
	srv := testNotificationServerIssue358()
	obs := srv.hub.sessionObserver("sess-1")

	obs(agent.SessionEvent{Type: agent.SessionEventFinal, Text: "done"})

	sub, unsub := srv.hub.subscribeGlobal()
	defer unsub()

	te := recvNotificationIssue358(t, sub)
	if te.notif.Reason != string(notify.ReasonComplete) {
		t.Fatalf("notification reason = %q, want %q", te.notif.Reason, notify.ReasonComplete)
	}
	if te.notif.SessionID != "sess-1" {
		t.Fatalf("notification session_id = %q, want sess-1", te.notif.SessionID)
	}
	if te.notif.Title == "" || te.notif.Body == "" {
		t.Fatalf("notification should include a user-visible title and body, got %+v", *te.notif)
	}
}

func TestNotificationSinkIssue358NilGateAllowsAndNilLocalDoesNotPanic(t *testing.T) {
	srv := testNotificationServerIssue358()

	sink := srv.NotificationSink(nil, nil)
	sink("watcher", "unattended", "no local notifier installed")

	sub, unsub := srv.hub.subscribeGlobal()
	defer unsub()

	te := recvNotificationIssue358(t, sub)
	if te.notif.Title != "unattended" {
		t.Fatalf("title = %q, want unattended", te.notif.Title)
	}
}

func testNotificationServerIssue358() *Server {
	fixed := time.Date(2026, 6, 24, 12, 34, 56, 0, time.UTC)
	return &Server{
		hub: newHub(),
		now: func() time.Time { return fixed },
	}
}

func recvNotificationIssue358(t *testing.T, sub <-chan taggedEvent) taggedEvent {
	t.Helper()
	select {
	case te := <-sub:
		if te.notif == nil {
			t.Fatalf("received non-notification frame: %+v", te)
		}
		return te
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for notification frame")
	}
	return taggedEvent{}
}
