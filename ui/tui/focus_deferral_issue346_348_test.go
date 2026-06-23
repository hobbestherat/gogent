package ui

import (
	"reflect"
	"testing"
	"time"
	"unsafe"

	"gogent/internal/gogent"
	"gogent/internal/permission"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

func setDesktopLastInputAt(t *testing.T, d *tv.Desktop, at time.Time) {
	t.Helper()
	field := reflect.ValueOf(d).Elem().FieldByName("lastInputAt")
	if !field.IsValid() {
		t.Fatal("turbotui Desktop no longer has lastInputAt; update the typing-awareness test harness")
	}
	reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Set(reflect.ValueOf(at))
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

func TestBackgroundDialogsAreArmedForEnterGrace(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(*Workbench)
		want string
	}{
		{
			name: "permission",
			want: "permission-dialog",
			open: func(w *Workbench) {
				showPermissionDialog(w.desktop, issue346PermissionRequest(), "Session", func(permission.Decision) {})
			},
		},
		{
			name: "review",
			want: "review-dialog",
			open: func(w *Workbench) {
				showReviewDialog(w.desktop, issue346ReviewRequest(), "Session", func(gogent.EditReviewDecision) {})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestWorkbench(t)
			sw := w.openWindow("s", "Session")
			w.desktop.SetFocus(sw.input)

			tc.open(w)

			top := w.desktop.TopLayer()
			if top == nil || top.Name != tc.want {
				t.Fatalf("top layer = %v, want %s", top, tc.want)
			}
			if !top.Modal {
				t.Fatalf("%s layer is not modal", tc.want)
			}
			if top.NoEnterGrace {
				t.Fatalf("%s opted out of modal Enter grace", tc.want)
			}
			if top.ArmedAt().IsZero() {
				t.Fatalf("%s was not armed when added to the desktop", tc.want)
			}
			if w.desktop.EnterGrace() <= 0 {
				t.Fatal("desktop Enter grace is disabled; background dialogs cannot benefit from turbotui #347")
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
