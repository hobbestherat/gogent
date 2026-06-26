package ui

import (
	"fmt"
	"testing"

	"gogent/internal/config"
)

// Regression coverage for issue #478: starting/stopping the local daemon from the
// Daemon menu must show exactly ONE daemon dialog at a time — a "Migrating…"
// progress modal while the handoff runs, replaced by a single result dialog
// (success or error) on completion. The bug was two stacked dialogs (progress
// never dismissed) because showConfirm AddLayer-appends and no reference to the
// progress layer was kept.
//
// Design criteria under test:
//   (1) goal — exactly one daemon dialog at a time for Start and Stop.
//   (2) usability — the progress modal is shown during the handoff (replaced, not
//       deleted) and a single result shows on success AND error.
//   (3) no regressions — showConfirm still builds a dismissible "confirm-dialog"
//       modal after the newMessageLayer refactor; dismiss is idempotent; double
//       Start is guarded.
//   (4) holistic — turbotui is untouched; the fix mirrors the disconnect-modal
//       tracked-layer idiom; layers are named distinctly for testability.

// assertNoDaemonDialogStackedBeneath pops the current top layer and asserts no
// daemon dialog ("daemon-progress"/"confirm-dialog") lurks beneath it. Desktop
// exposes no layer enumeration (only Add/Remove/RemoveTopLayer/TopLayer), so
// popping the result and inspecting the new top is the only way to catch the #478
// stacking bug — a progress dialog buried under the result would surface here.
// Destructive: call as the final assertion on a throwaway workbench.
func assertNoDaemonDialogStackedBeneath(t *testing.T, w *Workbench) {
	t.Helper()
	w.desktop.RemoveTopLayer()
	if top := w.desktop.TopLayer(); top != nil && (top.Name == "daemon-progress" || top.Name == "confirm-dialog") {
		t.Fatalf("a daemon dialog %q is stacked beneath the result — issue #478 not fixed (dialogs must replace, not stack)", top.Name)
	}
}

// assertSingleDaemonResultDialog asserts the post-handoff steady state: the
// progress field is cleared, a single result dialog is on top, and (via the
// destructive peek) no daemon dialog is stacked beneath it.
func assertSingleDaemonResultDialog(t *testing.T, w *Workbench) {
	t.Helper()
	if w.daemonHandoffLayer != nil {
		t.Fatalf("w.daemonHandoffLayer still set after handoff; progress dialog was not dismissed")
	}
	if got := topLayerName(w); got != "confirm-dialog" {
		t.Fatalf("top layer = %q, want %q (a single result dialog)", got, "confirm-dialog")
	}
	assertNoDaemonDialogStackedBeneath(t, w)
}

// startHandoffWorkbench builds a headless workbench with the daemon menu handlers
// wired. start is the StartDaemon behaviour (nil leaves it unwired, disabling the
// menu item); mode is the reported DaemonMode. A blocking start (receive from a
// channel) lets a test inspect the in-flight progress modal before completion.
func startHandoffWorkbench(t *testing.T, mode DaemonMode, start, stop func() error) *Workbench {
	t.Helper()
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	w.SetHandlers(Handlers{
		DaemonMode:  func() DaemonMode { return mode },
		StartDaemon: start,
		StopDaemon:  stop,
	})
	return w
}

// TestIssue478StartShowsProgressThenSingleResultOnSuccess is the core regression:
// while the handoff is in flight the "daemon-progress" modal is on top (feedback
// preserved), and on success it is REPLACED by exactly one result dialog — never
// both at once. (criteria 1, 2)
func TestIssue478StartShowsProgressThenSingleResultOnSuccess(t *testing.T) {
	release := make(chan struct{})
	w := startHandoffWorkbench(t, DaemonModeEmbedded, func() error { <-release; return nil }, nil)

	w.startDaemonFromMenu()

	// In-flight: the progress modal is up and blocking, and the field tracks it.
	if w.daemonHandoffLayer == nil {
		t.Fatal("daemonHandoffLayer not set while handoff is in flight")
	}
	if got := topLayerName(w); got != "daemon-progress" {
		t.Fatalf("in-flight top layer = %q, want daemon-progress (progress must show during the readiness window)", got)
	}
	if !w.daemonHandoffLayer.Modal || !w.daemonHandoffLayer.AcceptInput {
		t.Fatalf("progress layer modal/acceptInput = %v/%v, want a blocking modal", w.daemonHandoffLayer.Modal, w.daemonHandoffLayer.AcceptInput)
	}

	close(release) // let the handoff complete
	drainPostedEventually(t, w)

	assertSingleDaemonResultDialog(t, w)
}

// TestIssue478StartShowsSingleResultOnError asserts the error path also dismisses
// the progress modal and shows a single result dialog. (criteria 1, 2)
func TestIssue478StartShowsSingleResultOnError(t *testing.T) {
	w := startHandoffWorkbench(t, DaemonModeEmbedded, func() error { return fmt.Errorf("socket bind refused") }, nil)

	w.startDaemonFromMenu()
	drainPostedEventually(t, w)

	// The error branch must dismiss the progress modal and show one result dialog.
	if w.daemonHandoffLayer != nil {
		t.Fatal("progress dialog was not dismissed on the error path")
	}
	assertSingleDaemonResultDialog(t, w)
}

// TestIssue478StopShowsProgressThenSingleResultOnSuccess mirrors Start for the
// daemon->embedded (Stop) handoff: one progress modal, replaced by one result.
// (criteria 1, 2)
func TestIssue478StopShowsProgressThenSingleResultOnSuccess(t *testing.T) {
	release := make(chan struct{})
	w := startHandoffWorkbench(t, DaemonModeAttachedLocal, nil, func() error { <-release; return nil })

	w.stopDaemonFromMenu()

	if got := topLayerName(w); got != "daemon-progress" {
		t.Fatalf("in-flight top layer = %q, want daemon-progress", got)
	}

	close(release)
	drainPostedEventually(t, w)

	assertSingleDaemonResultDialog(t, w)
}

// TestIssue478StopShowsSingleResultOnError mirrors the Start error path for Stop.
// (criteria 1, 2)
func TestIssue478StopShowsSingleResultOnError(t *testing.T) {
	w := startHandoffWorkbench(t, DaemonModeAttachedLocal, nil, func() error { return fmt.Errorf("daemon did not exit") })

	w.stopDaemonFromMenu()
	drainPostedEventually(t, w)

	if w.daemonHandoffLayer != nil {
		t.Fatal("progress dialog was not dismissed on the Stop error path")
	}
	assertSingleDaemonResultDialog(t, w)
}

// TestIssue478DoubleStartIsGuarded asserts a second Start while one is in flight is
// a no-op: it does not push a second progress modal (the guard mirrors the
// disconnect modal's double-show guard). (criteria 2, 3)
func TestIssue478DoubleStartIsGuarded(t *testing.T) {
	release := make(chan struct{})
	w := startHandoffWorkbench(t, DaemonModeEmbedded, func() error { <-release; return nil }, nil)

	w.startDaemonFromMenu()
	first := w.daemonHandoffLayer
	if first == nil {
		t.Fatal("first Start did not show a progress modal")
	}

	// A second Start while the first is in flight must be a guarded no-op: no new
	// layer is pushed, the tracked layer is unchanged.
	w.startDaemonFromMenu()
	if w.daemonHandoffLayer != first {
		t.Fatalf("second Start changed the tracked progress layer (guard failed); got %v, want %v", w.daemonHandoffLayer, first)
	}
	if top := w.desktop.TopLayer(); top != first {
		t.Fatalf("second Start pushed a second progress modal on top (guard failed); top = %v, want the original %v", top, first)
	}

	close(release)
	drainPostedEventually(t, w)
	assertSingleDaemonResultDialog(t, w)
}

// TestIssue478DismissDaemonHandoffProgressIsIdempotent pins the dismiss helper's
// contract: it is a no-op when nothing is in flight and safe to call twice.
// (criterion 3)
func TestIssue478DismissDaemonHandoffProgressIsIdempotent(t *testing.T) {
	w := startHandoffWorkbench(t, DaemonModeEmbedded, func() error { return nil }, nil)

	w.dismissDaemonHandoffProgress() // nothing in flight: must not panic
	if w.daemonHandoffLayer != nil {
		t.Fatal("dismiss when idle should leave the field nil")
	}

	w.daemonHandoffLayer = w.showProgress("Start daemon", "migrating")
	if w.daemonHandoffLayer == nil {
		t.Fatal("showProgress did not return a layer")
	}
	w.dismissDaemonHandoffProgress()
	if w.daemonHandoffLayer != nil {
		t.Fatal("first dismiss did not clear the field")
	}
	w.dismissDaemonHandoffProgress() // second call: no-op, must not panic
	if top := w.desktop.TopLayer(); top != nil && top.Name == "daemon-progress" {
		t.Fatalf("progress layer still on desktop after dismiss")
	}
}

// TestIssue478ShowProgressIsNamedBlockingModal pins the interim modal's identity
// and blocking flags, mirroring the disconnect-modal assertions. (criteria 2, 4)
func TestIssue478ShowProgressIsNamedBlockingModal(t *testing.T) {
	w := startHandoffWorkbench(t, DaemonModeEmbedded, func() error { return nil }, nil)

	layer := w.showProgress("Start daemon", "Migrating…")
	if layer == nil {
		t.Fatal("showProgress returned nil")
	}
	if layer.Name != "daemon-progress" {
		t.Fatalf("progress layer name = %q, want daemon-progress", layer.Name)
	}
	if !layer.Modal || !layer.AcceptInput {
		t.Fatalf("progress layer modal/acceptInput = %v/%v, want blocking modal", layer.Modal, layer.AcceptInput)
	}
	if top := w.desktop.TopLayer(); top != layer {
		t.Fatalf("progress layer is not on top; top = %v", top)
	}
	// Programmatic dismissal removes it.
	w.desktop.RemoveLayer(layer)
	if top := w.desktop.TopLayer(); top != nil && top.Name == "daemon-progress" {
		t.Fatal("progress layer remained after RemoveLayer")
	}
}

// TestIssue478ShowConfirmRefactorPreservesDialogContract guards the newMessageLayer
// extraction: every showConfirm variant still creates a dismissible "confirm-dialog"
// modal on top (the refactor is the regression surface — it is shared by every
// dialog). (criterion 3)
func TestIssue478ShowConfirmRefactorPreservesDialogContract(t *testing.T) {
	w := startHandoffWorkbench(t, DaemonModeEmbedded, func() error { return nil }, nil)

	// Informational (nil callback → OK button): a single confirm-dialog modal.
	w.showConfirm("Title", "informational", nil)
	if got := topLayerName(w); got != "confirm-dialog" {
		t.Fatalf("informational showConfirm top layer = %q, want confirm-dialog", got)
	}
	top := w.desktop.TopLayer()
	if top == nil || !top.Modal || !top.AcceptInput {
		t.Fatalf("informational dialog modal/acceptInput invalid: %+v", top)
	}
	w.desktop.RemoveLayer(top)
	if got := topLayerName(w); got == "confirm-dialog" {
		t.Fatal("informational dialog was not removed by RemoveLayer")
	}

	// Yes/No (non-nil callback): still a single confirm-dialog modal.
	w.showConfirm("Title", "are you sure?", func(bool) {})
	if got := topLayerName(w); got != "confirm-dialog" {
		t.Fatalf("yes/no showConfirm top layer = %q, want confirm-dialog", got)
	}
	if top := w.desktop.TopLayer(); top == nil || top.Name != "confirm-dialog" {
		t.Fatalf("yes/no dialog not on top: %+v", top)
	}
}
