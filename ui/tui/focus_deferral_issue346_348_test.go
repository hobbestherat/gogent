package ui

import (
	"reflect"
	"sync"
	"testing"
	"time"
	"unsafe"

	"gogent/internal/gogent"
	"gogent/internal/permission"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

func exportedField[T any](t *testing.T, owner any, name string) *T {
	t.Helper()
	field := reflect.ValueOf(owner).Elem().FieldByName(name)
	if !field.IsValid() {
		t.Fatalf("%T no longer has field %q; update the test harness", owner, name)
	}
	return reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Interface().(*T)
}

func drainPosted(t *testing.T, w *Workbench) int {
	t.Helper()
	total := 0
	for {
		mu := exportedField[sync.Mutex](t, w.app, "postMu")
		mu.Lock()
		queue := exportedField[[]func()](t, w.app, "postQueue")
		batch := append([]func(){}, (*queue)...)
		*queue = (*queue)[:0]
		mu.Unlock()

		if len(batch) == 0 {
			return total
		}
		total += len(batch)
		for _, fn := range batch {
			fn()
		}
	}
}

func drainPostedEventually(t *testing.T, w *Workbench) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if drainPosted(t, w) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for a desktop.Post callback")
		}
		time.Sleep(time.Millisecond)
	}
}

func dispatchType(t *testing.T, w *Workbench, ev tui.TypeEvent) {
	t.Helper()
	handlers := append([]func(tui.TypeEvent){}, *exportedField[[]func(tui.TypeEvent)](t, w.app, "typeHandlers")...)
	if len(handlers) == 0 {
		t.Fatal("app has no type handlers; desktop input dispatch is not wired")
	}
	for _, handler := range handlers {
		handler(ev)
	}
}

func setDesktopLastInputAt(t *testing.T, d *tv.Desktop, at time.Time) {
	t.Helper()
	*exportedField[time.Time](t, d, "lastInputAt") = at
}

func issue346PermissionRequest() permission.Request {
	return permission.Request{
		Action:   permission.ActionShell,
		Detail:   "echo hello",
		Context:  permission.RequestContext{SessionID: "s", Agent: "root"},
		Resource: "echo hello",
	}
}

func issue346ReviewRequest() gogent.EditReviewRequest {
	return gogent.EditReviewRequest{
		SessionID: "s",
		AgentID:   "root",
		Op:        "edit",
		Path:      "main.go",
		Diff:      "--- a/main.go\n+++ b/main.go\n@@\n-old\n+new",
	}
}

func TestBackgroundPermissionModalDefersUntilTypingIdle(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "Session")
	w.desktop.SetFocus(sw.input)

	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	w.desktop.SetClock(func() time.Time { return now })
	setDesktopLastInputAt(t, w.desktop, now)
	if !w.desktop.RecentlyTyped(typingIdleThreshold) {
		t.Fatal("setup failed: desktop should report recent typing")
	}
	t.Cleanup(w.stopDeferredTimer)

	shown := false
	w.presentBackgroundModal(func() {
		shown = true
		showPermissionDialog(w.desktop, issue346PermissionRequest(), "Session", func(permission.Decision) {})
	})

	if shown {
		t.Fatal("permission dialog was shown while RecentlyTyped was true")
	}
	if top := w.desktop.TopLayer(); top == nil || top.Name == "permission-dialog" {
		t.Fatalf("permission dialog reached the layer stack while typing: top=%v", top)
	}
	if !sw.input.Component.Focused() {
		t.Fatal("deferred permission dialog stole focus from the session input")
	}
	if w.deferredModal == nil {
		t.Fatal("deferred permission modal was dropped instead of queued")
	}

	now = now.Add(typingIdleThreshold)
	if w.desktop.RecentlyTyped(typingIdleThreshold) {
		t.Fatal("setup failed: typing signal should have decayed at the idle threshold")
	}
	w.maybeShowDeferredModal()

	if !shown {
		t.Fatal("permission dialog was not shown after typing went idle")
	}
	if top := w.desktop.TopLayer(); top == nil || top.Name != "permission-dialog" {
		t.Fatalf("top layer = %v, want permission-dialog after idle", top)
	}
	if sw.input.Component.Focused() {
		t.Fatal("session input should lose focus only once the deferred dialog is actually shown")
	}
}

func TestBackgroundReviewModalDefersUntilTypingIdle(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "Session")
	w.desktop.SetFocus(sw.input)

	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	w.desktop.SetClock(func() time.Time { return now })
	setDesktopLastInputAt(t, w.desktop, now)
	t.Cleanup(w.stopDeferredTimer)

	w.presentBackgroundModal(func() {
		showReviewDialog(w.desktop, issue346ReviewRequest(), "Session", func(gogent.EditReviewDecision) {})
	})

	if top := w.desktop.TopLayer(); top == nil || top.Name == "review-dialog" {
		t.Fatalf("review dialog reached the layer stack while typing: top=%v", top)
	}
	if !sw.input.Component.Focused() {
		t.Fatal("deferred review dialog stole focus from the session input")
	}

	now = now.Add(typingIdleThreshold + time.Nanosecond)
	w.maybeShowDeferredModal()

	if top := w.desktop.TopLayer(); top == nil || top.Name != "review-dialog" {
		t.Fatalf("top layer = %v, want review-dialog after idle", top)
	}
}

func TestAskPermissionPostedPathDefersAndDrainsOnSubmit(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "Session")
	w.desktop.SetFocus(sw.input)

	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	w.desktop.SetClock(func() time.Time { return now })
	setDesktopLastInputAt(t, w.desktop, now)
	t.Cleanup(w.stopDeferredTimer)

	done := make(chan permission.Decision, 1)
	go func() {
		done <- w.AskPermission(issue346PermissionRequest())
	}()

	drainPostedEventually(t, w) // badge update + prompt presentation post
	drainPosted(t, w)

	if top := w.desktop.TopLayer(); top == nil || top.Name == "permission-dialog" {
		t.Fatalf("permission dialog should not be shown while typing through AskPermission; top=%v", top)
	}
	if !sw.input.Component.Focused() {
		t.Fatal("AskPermission stole focus from the session input while deferred")
	}
	if w.deferredModal == nil {
		t.Fatal("AskPermission did not leave a deferred modal queued")
	}
	select {
	case got := <-done:
		t.Fatalf("AskPermission returned before the deferred dialog resolved: %v", got)
	default:
	}

	sw.input.SetText("send this first")
	sw.submitFn()

	top := w.desktop.TopLayer()
	if top == nil || top.Name != "permission-dialog" {
		t.Fatalf("permission dialog did not drain immediately on submit; top=%v", top)
	}
	if w.deferredModal != nil {
		t.Fatal("deferred modal was not cleared after submit-drain")
	}

	if top.Root.OnTypeFn == nil || !top.Root.OnTypeFn(top.Root, tui.TypeEvent{Key: tui.KeyEscape}) {
		t.Fatal("permission dialog did not handle Escape dismissal")
	}
	select {
	case got := <-done:
		if got != permission.DecisionDeny {
			t.Fatalf("AskPermission after Escape = %v, want deny", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AskPermission did not return after the dialog resolved")
	}
}

func TestReviewEditPostedPathDefersAndShowsAfterIdle(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "Session")
	w.desktop.SetFocus(sw.input)

	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	w.desktop.SetClock(func() time.Time { return now })
	setDesktopLastInputAt(t, w.desktop, now)
	t.Cleanup(w.stopDeferredTimer)

	done := make(chan gogent.EditReviewDecision, 1)
	go func() {
		done <- w.ReviewEdit(issue346ReviewRequest())
	}()

	drainPostedEventually(t, w)
	drainPosted(t, w)

	if top := w.desktop.TopLayer(); top == nil || top.Name == "review-dialog" {
		t.Fatalf("review dialog should not be shown while typing through ReviewEdit; top=%v", top)
	}
	if !sw.input.Component.Focused() {
		t.Fatal("ReviewEdit stole focus from the session input while deferred")
	}
	if w.deferredModal == nil {
		t.Fatal("ReviewEdit did not leave a deferred modal queued")
	}

	now = now.Add(typingIdleThreshold + time.Nanosecond)
	w.maybeShowDeferredModal()

	top := w.desktop.TopLayer()
	if top == nil || top.Name != "review-dialog" {
		t.Fatalf("review dialog did not show after typing went idle; top=%v", top)
	}
	if top.Root.OnTypeFn == nil || !top.Root.OnTypeFn(top.Root, tui.TypeEvent{Key: tui.KeyEscape}) {
		t.Fatal("review dialog did not handle Escape dismissal")
	}
	select {
	case got := <-done:
		if got != gogent.EditReject {
			t.Fatalf("ReviewEdit after Escape = %v, want reject", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReviewEdit did not return after the dialog resolved")
	}
}

func TestBackgroundDialogsHonorEnterGrace(t *testing.T) {
	for _, tc := range []struct {
		name      string
		open      func(*Workbench, func())
		wantLayer string
	}{
		{
			name:      "permission",
			wantLayer: "permission-dialog",
			open: func(w *Workbench, resolved func()) {
				showPermissionDialog(w.desktop, issue346PermissionRequest(), "Session", func(permission.Decision) {
					resolved()
				})
			},
		},
		{
			name:      "review",
			wantLayer: "review-dialog",
			open: func(w *Workbench, resolved func()) {
				showReviewDialog(w.desktop, issue346ReviewRequest(), "Session", func(gogent.EditReviewDecision) {
					resolved()
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestWorkbench(t)
			sw := w.openWindow("s", "Session")
			w.desktop.SetFocus(sw.input)
			now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
			w.desktop.SetClock(func() time.Time { return now })

			resolved := 0
			tc.open(w, func() { resolved++ })

			top := w.desktop.TopLayer()
			if top == nil || top.Name != tc.wantLayer {
				t.Fatalf("top layer = %v, want %s", top, tc.wantLayer)
			}
			if !top.Modal {
				t.Fatalf("%s layer is not modal", tc.wantLayer)
			}
			if top.NoEnterGrace {
				t.Fatalf("%s opted out of modal Enter grace", tc.wantLayer)
			}
			if top.ArmedAt().IsZero() {
				t.Fatalf("%s was not armed when added to the desktop", tc.wantLayer)
			}
			if w.desktop.EnterGrace() <= 0 {
				t.Fatal("desktop Enter grace is disabled; background dialogs cannot benefit from turbotui #347")
			}

			now = top.ArmedAt().Add(w.desktop.EnterGrace() - time.Nanosecond)
			dispatchType(t, w, tui.TypeEvent{Key: tui.KeyEnter})
			if resolved != 0 {
				t.Fatalf("Enter inside grace resolved %s", tc.wantLayer)
			}
			if stillTop := w.desktop.TopLayer(); stillTop == nil || stillTop.Name != tc.wantLayer {
				t.Fatalf("%s closed during Enter grace; top=%v", tc.wantLayer, stillTop)
			}

			now = top.ArmedAt().Add(w.desktop.EnterGrace())
			dispatchType(t, w, tui.TypeEvent{Key: tui.KeyEnter})
			if resolved != 1 {
				t.Fatalf("Enter after grace resolved %s %d times, want 1", tc.wantLayer, resolved)
			}
			if stillTop := w.desktop.TopLayer(); stillTop != nil && stillTop.Name == tc.wantLayer {
				t.Fatalf("%s stayed open after Enter outside grace", tc.wantLayer)
			}
		})
	}
}

func TestBackgroundDialogsEscapeDuringEnterGrace(t *testing.T) {
	for _, tc := range []struct {
		name      string
		open      func(*Workbench, func())
		wantLayer string
	}{
		{
			name:      "permission",
			wantLayer: "permission-dialog",
			open: func(w *Workbench, resolved func()) {
				showPermissionDialog(w.desktop, issue346PermissionRequest(), "Session", func(permission.Decision) {
					resolved()
				})
			},
		},
		{
			name:      "review",
			wantLayer: "review-dialog",
			open: func(w *Workbench, resolved func()) {
				showReviewDialog(w.desktop, issue346ReviewRequest(), "Session", func(gogent.EditReviewDecision) {
					resolved()
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestWorkbench(t)
			sw := w.openWindow("s", "Session")
			w.desktop.SetFocus(sw.input)
			now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
			w.desktop.SetClock(func() time.Time { return now })

			resolved := 0
			tc.open(w, func() { resolved++ })
			top := w.desktop.TopLayer()
			if top == nil || top.Name != tc.wantLayer {
				t.Fatalf("top layer = %v, want %s", top, tc.wantLayer)
			}

			now = top.ArmedAt().Add(w.desktop.EnterGrace() - time.Nanosecond)
			dispatchType(t, w, tui.TypeEvent{Key: tui.KeyEscape})
			if resolved != 1 {
				t.Fatalf("Escape during grace resolved %s %d times, want 1", tc.wantLayer, resolved)
			}
			if stillTop := w.desktop.TopLayer(); stillTop != nil && stillTop.Name == tc.wantLayer {
				t.Fatalf("%s stayed open after Escape during grace", tc.wantLayer)
			}
		})
	}
}

func TestModalCloseRestoresSessionInputFocus(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(*Workbench)
		top  string
	}{
		{
			name: "permission",
			top:  "permission-dialog",
			open: func(w *Workbench) {
				showPermissionDialog(w.desktop, issue346PermissionRequest(), "Session", func(permission.Decision) {})
			},
		},
		{
			name: "review",
			top:  "review-dialog",
			open: func(w *Workbench) {
				showReviewDialog(w.desktop, issue346ReviewRequest(), "Session", func(gogent.EditReviewDecision) {})
			},
		},
		{
			name: "confirm",
			top:  "confirm-dialog",
			open: func(w *Workbench) {
				w.showConfirm("Confirm", "Continue?", func(bool) {})
			},
		},
		{
			name: "input",
			top:  "input-dialog",
			open: func(w *Workbench) {
				w.showInputDialog("Rename", "&Title:", "Session", func(string, bool) {})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestWorkbench(t)
			sw := w.openWindow("s", "Session")
			w.desktop.SetFocus(sw.input)
			if !sw.input.Component.Focused() {
				t.Fatal("setup failed: session input should hold focus before the modal opens")
			}

			tc.open(w)
			top := w.desktop.TopLayer()
			if top == nil || top.Name != tc.top {
				t.Fatalf("top layer = %v, want %s", top, tc.top)
			}
			if sw.input.Component.Focused() {
				t.Fatalf("%s did not take modal focus on open", tc.top)
			}

			if top.Root.OnTypeFn == nil || !top.Root.OnTypeFn(top.Root, tui.TypeEvent{Key: tui.KeyEscape}) {
				t.Fatalf("%s did not handle Escape dismissal", tc.top)
			}
			if stillTop := w.desktop.TopLayer(); stillTop != nil && stillTop.Name == tc.top {
				t.Fatalf("%s still on top after Escape", tc.top)
			}
			if !sw.input.Component.Focused() {
				t.Fatalf("session input did not regain focus after %s closed", tc.top)
			}
		})
	}
}

func TestModalCloseFallsBackWhenPriorInputIsNoLongerFocusable(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "Session")
	w.desktop.SetFocus(sw.input)

	showPermissionDialog(w.desktop, issue346PermissionRequest(), "Session", func(permission.Decision) {})
	top := w.desktop.TopLayer()
	if top == nil || top.Name != "permission-dialog" {
		t.Fatalf("top layer = %v, want permission-dialog", top)
	}

	sw.input.Component.Enabled = false
	if top.Root.OnTypeFn == nil || !top.Root.OnTypeFn(top.Root, tui.TypeEvent{Key: tui.KeyEscape}) {
		t.Fatal("permission dialog did not handle Escape dismissal")
	}
	if sw.input.Component.Focused() {
		t.Fatal("disabled prior input regained focus after modal close")
	}
	if focused := findFocusedComponent(sw.layer.Root); focused == sw.input.Component {
		t.Fatal("focus restore kept a stale disabled input focused")
	}
}
