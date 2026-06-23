package ui

import (
	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// welcomeBody is the onboarding text shown by the welcome dialog (issue #341). It
// is a concise orientation — the essentials a new user needs to start and to
// discover the rest — not a manual. The slash commands and keybindings listed
// here are the same ones surfaced by the Ctrl+K palette and the ? cheatsheet, so
// the dialog points at gogent's own discovery surfaces rather than duplicating
// them. See docs/usage-tui.md for the full command reference.
const welcomeBody = `gogent is an autonomous coding agent that runs in your workspace. Here are the
essentials to get started.

Key actions
  Ctrl+K      Command palette — run any action by name
  Ctrl+N      New session
  Ctrl+F, /   Find in transcript
  ?           Keybinding cheatsheet
  Ctrl+Q      Quit

Useful slash commands (type in the input box)
  /plan       Research & propose a plan without making changes
  /act        Approve and execute the pending plan
  /undo       Undo the last turn
  /rewind     Rewind N turns
  /stop       Stop the running turn
  /goal       Set a supervisor goal for the session
  /thinking   Toggle live chain-of-thought streaming

Tips
  • Open the command palette (Ctrl+K) to discover everything gogent can do.
  • Press ? at any time for the full keybinding cheatsheet.
  • Re-open this dialog from the palette ("Show welcome") or Help → Welcome.
`

// welcomeVChrome is the welcome dialog's non-body vertical cost: 2 borders + 1
// gap + 1 checkbox row + 1 button row. Height = bodyLineCount + welcomeVChrome
// shows the whole orientation without dead space, mirroring helpVChrome's role in
// the help overlay (issues #299, #309). It must track the bodyH math in
// showWelcomeDialog (bodyH = height - 5).
const welcomeVChrome = 5

// showWelcomeDialog opens the modal welcome/onboarding dialog (issue #341). It is
// shown once on startup (gated on the GetShowWelcome preference) and is
// re-openable on demand from the command palette and Help menu (issue #342).
//
// The "Don't show this on startup again" checkbox mirrors the persisted
// preference: it starts checked exactly when the startup dialog is currently
// disabled (GetShowWelcome() == false). Closing the dialog persists the new state
// via SetShowWelcome (checked -> false) ONLY when the user actually flipped the
// checkbox, so the dialog doubles as a bidirectional toggle for the startup
// preference — a user who re-opened it from the palette can both suppress and
// re-enable the startup dialog from here.
//
// Merely viewing and dismissing the dialog without touching the checkbox persists
// nothing: that honours issue #341's "if unchecked, do nothing / leaves
// ShowWelcome unchanged" rule and, crucially, never rewrites an older config that
// omitted the key from nil/unset to an explicit value just because the first-run
// dialog was dismissed.
//
// It is robust to a missing handler pair: with no SetShowWelcome the checkbox is
// inert (closing persists nothing) and with no GetShowWelcome it defaults to
// "shown" (unchecked). Esc and the title-bar [x] dismiss it exactly like the
// command palette, so it never blocks the UI.
func (w *Workbench) showWelcomeDialog() {
	// Read-only body + one checkbox + one button, so cap the height to the content
	// (+ chrome) like the help overlay rather than filling the percentage box
	// (issues #299, #309). The widest body line (~68 cells) drives PreferredW;
	// MaxW caps it and MinW keeps it usable on a small terminal.
	bodyLines := textLineCount(welcomeBody)
	spec := tv.DialogSpec{MinW: 56, MaxW: 80, PreferredW: 74, MinH: 16, MaxH: bodyLines + welcomeVChrome}
	x, y, width, height := w.dialogRect(spec)

	var layer *tv.Layer
	// startChecked is the checkbox's initial state, which mirrors the persisted
	// preference (checked == startup dialog currently disabled). A nil GetShowWelcome
	// leaves it unchecked ("shown"). closeFn compares the final state against this to
	// persist only a real change.
	startChecked := w.handlers.GetShowWelcome != nil && !w.handlers.GetShowWelcome()
	// closeFn persists the new preference — but ONLY when the user actually flipped
	// the checkbox (and the handler is wired) — before removing the layer, so opting
	// out / back in takes effect immediately while a no-op dismissal changes nothing.
	// It is captured by the [x] button, Esc, and the Close button so every dismissal
	// path behaves identically.
	var dontShow *tv.Checkbox
	closeFn := func() {
		if w.handlers.SetShowWelcome != nil && dontShow != nil && dontShow.IsChecked() != startChecked {
			// Checked means "don't show on startup", i.e. ShowWelcome=false.
			w.handlers.SetShowWelcome(!dontShow.IsChecked())
		}
		w.desktop.RemoveLayer(layer)
	}
	dialog := newCloseableDialog("Welcome to gogent", x, y, width, height, closeFn)

	bodyH := height - 5 // title border + checkbox row + button row + bottom margin
	if bodyH < 3 {
		bodyH = 3
	}
	body := tv.NewTextView(welcomeBody, tv.Rect{X: 2, Y: 1, W: width - 4, H: bodyH})
	body.FG = tv.DefaultTheme.DialogFG
	body.BG = tv.DefaultTheme.DialogBG
	body.ScrollToTop() // orientation is read top-down (issue #174)
	dialog.Window.AddContent(body)

	dontShow = tv.NewCheckbox("&Don't show this on startup again",
		tv.Rect{X: 2, Y: height - 3, W: width - 4, H: 1}, nil)
	dontShow.FG = tv.DefaultTheme.DialogFG
	dontShow.BG = tv.DefaultTheme.DialogBG
	// Mirror the current persisted state: checked when the startup dialog is off, so
	// a re-opened dialog shows the truth and lets the user flip it back.
	dontShow.SetChecked(startChecked)
	dialog.Window.AddContent(dontShow)

	dialog.Window.AddContent(newButton("Close",
		tv.Rect{X: width - 11, Y: height - 2, W: 9, H: 1}, closeFn))

	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			closeFn()
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("welcome-dialog", dialog)
	w.desktop.AddLayer(layer)
	dialog.Fit(spec) // re-resolve the rect when the terminal is resized (issue #299)
	w.desktop.SetFocus(dontShow)
}
