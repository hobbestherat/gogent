package ui

import (
	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// showNotificationsDialog opens the modal notification-settings dialog
// (issue #59). The top group toggles the delivery channels (master switch, bell,
// OSC desktop notification, native OS notifier); the lower group toggles which
// events trigger a notification and whether the focused session is skipped.
// Values are written back only on OK.
func (w *Workbench) showNotificationsDialog() {
	if w.handlers.GetNotifyConfig == nil || w.handlers.SetNotifyConfig == nil {
		w.showConfirm("Settings", "Notification settings are unavailable.", nil)
		return
	}
	cur := w.handlers.GetNotifyConfig()

	// A fixed-form dialog (two groups of checkboxes plus an OK/Cancel row; no
	// scrolling, no growing content), so it is PINNED to its content footprint rather
	// than left to fill the 80%×85% box (issue #317). PreferredW=54 fits the longest
	// label ("Nati&ve (notify-send / terminal-notifier)", 41 cells) without clipping on
	// any terminal wide enough to honour it; MaxW caps it so it never balloons to 160;
	// MinW keeps it usable on a small terminal. The height is pinned at 18: the last
	// toggle is at Y=13 and the button row at height-3. The spec is static, so the
	// dialog.Fit re-resolve of the frame on resize stays correct (issue #299).
	spec := tv.DialogSpec{MinW: 50, MaxW: 58, PreferredW: 54, MinH: 18, MaxH: 18}
	x, y, width, height := w.dialogRect(spec)

	dialog := tv.NewDialog("Notifications", x, y, width, height)
	applyWindowShadow(dialog.Window) // honour the NoShadow theme setting (issue #215)
	dialog.Window.ShowClose = false

	// styleCheck colours a checkbox for the dialog background.
	styleCheck := func(cb *tv.Checkbox) *tv.Checkbox {
		cb.FG = tv.DefaultTheme.DialogFG
		cb.BG = tv.DefaultTheme.DialogBG
		return cb
	}

	channelsLabel := dialogLabel("Delivery:", tv.Rect{X: 2, Y: 1, W: width - 4, H: 1})
	enabled := styleCheck(tv.NewCheckbox("&Enabled", tv.Rect{X: 4, Y: 2, W: width - 8, H: 1}, nil))
	bell := styleCheck(tv.NewCheckbox("&Bell (terminal \\a)", tv.Rect{X: 4, Y: 3, W: width - 8, H: 1}, nil))
	desktop := styleCheck(tv.NewCheckbox("&Desktop (OSC 9 / 777)", tv.Rect{X: 4, Y: 4, W: width - 8, H: 1}, nil))
	native := styleCheck(tv.NewCheckbox("Nati&ve (notify-send / terminal-notifier)", tv.Rect{X: 4, Y: 5, W: width - 8, H: 1}, nil))

	eventsLabel := dialogLabel("Notify on:", tv.Rect{X: 2, Y: 7, W: width - 4, H: 1})
	complete := styleCheck(tv.NewCheckbox("Task &complete", tv.Rect{X: 4, Y: 8, W: width - 8, H: 1}, nil))
	errorEv := styleCheck(tv.NewCheckbox("&Errors", tv.Rect{X: 4, Y: 9, W: width - 8, H: 1}, nil))
	approval := styleCheck(tv.NewCheckbox("&Approval prompts", tv.Rect{X: 4, Y: 10, W: width - 8, H: 1}, nil))
	clarify := styleCheck(tv.NewCheckbox("&Clarification (CLARIFY)", tv.Rect{X: 4, Y: 11, W: width - 8, H: 1}, nil))
	suppress := styleCheck(tv.NewCheckbox("Skip when session is &focused", tv.Rect{X: 4, Y: 13, W: width - 8, H: 1}, nil))

	enabled.SetChecked(cur.Enabled)
	bell.SetChecked(cur.Bell)
	desktop.SetChecked(cur.Desktop)
	native.SetChecked(cur.Native)
	complete.SetChecked(cur.OnComplete)
	errorEv.SetChecked(cur.OnError)
	approval.SetChecked(cur.OnApproval)
	clarify.SetChecked(cur.OnClarify)
	suppress.SetChecked(cur.SuppressWhenFocused)

	dialog.Window.AddContent(channelsLabel)
	dialog.Window.AddContent(enabled)
	dialog.Window.AddContent(bell)
	dialog.Window.AddContent(desktop)
	dialog.Window.AddContent(native)
	dialog.Window.AddContent(eventsLabel)
	dialog.Window.AddContent(complete)
	dialog.Window.AddContent(errorEv)
	dialog.Window.AddContent(approval)
	dialog.Window.AddContent(clarify)
	dialog.Window.AddContent(suppress)

	var layer *tv.Layer
	apply := func() {
		cfg := config.NotifyConfig{
			Enabled:             enabled.IsChecked(),
			Bell:                bell.IsChecked(),
			Desktop:             desktop.IsChecked(),
			Native:              native.IsChecked(),
			OnComplete:          complete.IsChecked(),
			OnError:             errorEv.IsChecked(),
			OnApproval:          approval.IsChecked(),
			OnClarify:           clarify.IsChecked(),
			SuppressWhenFocused: suppress.IsChecked(),
		}
		w.handlers.SetNotifyConfig(cfg) // persists + updates the live notifier
		w.desktop.RemoveLayer(layer)
		w.rebuildMenu()
	}
	cancel := func() { w.desktop.RemoveLayer(layer) }

	ok := newButton("OK", tv.Rect{X: width - 24, Y: height - 3, W: 9, H: 1}, apply)
	cancelBtn := newButton("Cancel", tv.Rect{X: width - 13, Y: height - 3, W: 10, H: 1}, cancel)
	dialog.Window.AddContent(ok)
	dialog.Window.AddContent(cancelBtn)

	// Escape anywhere in the dialog cancels without applying.
	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			cancel()
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("notifications-dialog", dialog)
	w.desktop.AddLayer(layer)
	dialog.Fit(spec) // re-resolve the rect when the terminal is resized (issue #299)
	w.desktop.SetFocus(enabled)
}
