package ui

import (
	"strings"
	"testing"
)

// hasSessionWindow reports whether a TUI window is open for sessionID.
func hasSessionWindow(w *Workbench, sessionID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sessions[sessionID] != nil
}

// topLayerIsConfirm reports whether the top-most layer is a confirm/message
// dialog (the "not open yet" fallback openWatcherSession shows).
func topLayerIsConfirm(w *Workbench) bool {
	top := w.desktop.TopLayer()
	return top != nil && top.Name == "confirm-dialog"
}

// TestOpenWatcherSessionEmptyNoop covers the empty-id guard: nothing is opened and
// no fallback dialog appears.
func TestOpenWatcherSessionEmptyNoop(t *testing.T) {
	w := newTestWorkbench(t)
	w.app.Resize(120, 40)
	w.openWatcherSession("")
	if topLayerIsConfirm(w) {
		t.Error("empty id should be a silent no-op, not a confirm dialog")
	}
}

// TestOpenWatcherSessionRaisesOpen covers the already-open path: the session's
// window is raised (becomes the top layer), no adoption or confirm.
func TestOpenWatcherSessionRaisesOpen(t *testing.T) {
	w := newTestWorkbench(t)
	w.app.Resize(120, 40)
	w.openWindow("watcher:emailer", "watcher:emailer")
	w.openWindow("other", "other")
	w.Focus("other")

	w.openWatcherSession("watcher:emailer")
	if top := w.desktop.TopLayer(); top != w.sessions["watcher:emailer"].layer {
		t.Error("an already-open watcher session should be raised to the top")
	}
}

// TestOpenWatcherSessionAdoptsFromDisk covers the new adopt path (fixes round 1):
// a free-running watcher whose window is NOT open but whose transcript is persisted
// is opened by loading it through the Saved Sessions path, rather than no-opping.
func TestOpenWatcherSessionAdoptsFromDisk(t *testing.T) {
	w := newTestWorkbench(t)
	w.app.Resize(120, 40)

	var openedFile string
	w.SetHandlers(Handlers{
		ListSavedSessions: func() []SessionMeta {
			return []SessionMeta{{ID: "watcher:emailer", Title: "watcher:emailer", File: "/x/emailer.json"}}
		},
		OpenSavedSession: func(file string, readOnly bool) (RestoredSession, bool) {
			openedFile = file
			return RestoredSession{ID: "watcher:emailer", Title: "watcher:emailer",
				Messages: []ChatMessage{{Role: "user", Content: "[Watcher fired]"}}}, true
		},
	})

	if hasSessionWindow(w, "watcher:emailer") {
		t.Fatal("precondition: watcher session must not be open")
	}
	w.openWatcherSession("watcher:emailer")

	if openedFile != "/x/emailer.json" {
		t.Errorf("OpenSavedSession was called with %q, want the watcher's saved file", openedFile)
	}
	if !hasSessionWindow(w, "watcher:emailer") {
		t.Error("the watcher session should have been adopted into an open window")
	}
	if topLayerIsConfirm(w) {
		t.Error("a successful adopt must not also show the not-open fallback confirm")
	}
}

// TestOpenWatcherSessionNotFoundConfirms covers the fallback: a watcher session
// that is neither open nor on disk reports where it will appear (a confirm),
// rather than failing silently — and opens no window.
func TestOpenWatcherSessionNotFoundConfirms(t *testing.T) {
	w := newTestWorkbench(t)
	w.app.Resize(120, 40)
	w.SetHandlers(Handlers{
		ListSavedSessions: func() []SessionMeta { return nil }, // not on disk
		OpenSavedSession:  func(string, bool) (RestoredSession, bool) { return RestoredSession{}, false },
	})

	w.openWatcherSession("watcher:never-fired")
	if hasSessionWindow(w, "watcher:never-fired") {
		t.Error("no window should open for a session that is neither live nor saved")
	}
	if !topLayerIsConfirm(w) {
		t.Error("a not-open, not-saved watcher session should surface the explanatory confirm")
	}
}

// TestOpenWatcherSessionUnwiredConfirms covers the path where the Saved Sessions
// handlers are not wired at all: openWatcherSession must still degrade to the
// confirm rather than panicking on a nil handler.
func TestOpenWatcherSessionUnwiredConfirms(t *testing.T) {
	w := newTestWorkbench(t)
	w.app.Resize(120, 40)
	w.SetHandlers(Handlers{}) // no ListSavedSessions / OpenSavedSession

	w.openWatcherSession("watcher:x")
	if hasSessionWindow(w, "watcher:x") {
		t.Error("no window should open without the Saved Sessions handlers")
	}
	if !topLayerIsConfirm(w) {
		t.Error("missing Saved Sessions handlers should still show the explanatory confirm")
	}
}

// TestOpenWatcherSessionAdoptFailConfirms covers the OpenSavedSession-returns-false
// branch: the meta is listed but the load fails, so the fallback confirm is shown
// and no window opens.
func TestOpenWatcherSessionAdoptFailConfirms(t *testing.T) {
	w := newTestWorkbench(t)
	w.app.Resize(120, 40)
	w.SetHandlers(Handlers{
		ListSavedSessions: func() []SessionMeta {
			return []SessionMeta{{ID: "watcher:emailer", File: "/x/emailer.json"}}
		},
		OpenSavedSession: func(string, bool) (RestoredSession, bool) { return RestoredSession{}, false },
	})
	w.openWatcherSession("watcher:emailer")
	if hasSessionWindow(w, "watcher:emailer") {
		t.Error("a failed load must not leave a window open")
	}
	if !topLayerIsConfirm(w) {
		t.Error("a failed adopt should fall through to the explanatory confirm")
	}
}

// TestFreeRunningWatcherNoDuplicateAfterOpen is the adversarial regression for the
// adopt path (fixes round 1) interacting with the sidebar's ◷ watcher node.
//
// The issue wants a free-running watcher to appear as ONE top-level entry — the
// watcher:<name> session row, badged with the ◷ glyph. The sidebar adds a separate
// ◷ watcher node (refreshWatcherNodes), and once the user clicks Open Session the
// adopt path opens a *second* top-level node — a plain ○ watcher:emailer session
// row — for the very same thing. The tree then shows the watcher twice.
//
// This test asserts the desired single-entry behaviour; it fails while the two
// nodes coexist, documenting the duplicate.
func TestFreeRunningWatcherNoDuplicateAfterOpen(t *testing.T) {
	w := newTestWorkbench(t)
	w.app.Resize(120, 40)
	w.SetHandlers(Handlers{
		ListWatchers: func(id string) []WatcherInfo {
			if id == "" {
				return []WatcherInfo{{ID: "w1", Name: "emailer", Free: true, SessionID: "watcher:emailer", Enabled: true, Status: "idle"}}
			}
			return nil
		},
		ListSavedSessions: func() []SessionMeta {
			return []SessionMeta{{ID: "watcher:emailer", Title: "watcher:emailer", File: "/x/emailer.json"}}
		},
		OpenSavedSession: func(string, bool) (RestoredSession, bool) {
			return RestoredSession{ID: "watcher:emailer", Title: "watcher:emailer"}, true
		},
	})

	// The sidebar gains the free-running watcher's ◷ node.
	w.refreshWatcherNodes()
	// The user opens the watcher's session window (adopt path).
	w.openWatcherSession("watcher:emailer")
	// Reconcile again, as the live status tick would.
	w.refreshWatcherNodes()

	roots := 0
	for _, n := range w.sidebar.tree.Roots {
		if strings.Contains(n.Label, "watcher:emailer") {
			roots++
		}
	}
	if roots != 1 {
		t.Errorf("free-running watcher shows %d top-level rows labelled watcher:emailer, want 1 (duplicate session + ◷ watcher node)", roots)
	}
}
