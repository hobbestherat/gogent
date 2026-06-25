package ui

import (
	"fmt"
	"sort"
	"strings"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// action is one entry in the unified command/keybinding catalog — the single source
// of truth shared by the command palette, the '?' cheatsheet, the menu bar's shortcut
// hints, and the desktop BindingRegistry (issue #401, merging the old commands() and
// keybindActions() tables). Four consumers project from actions(): the palette offers
// every entry whose run is non-nil and whose availability predicate passes; the
// cheatsheet lists every visible entry grouped by category; the menu bar tags its
// rebindable items with the matching actionID and reads their shortcut display from
// chordFor; and rebuildBindings registers every entry that carries an actionID.
//
// There is deliberately NO Target field: Target gates a ScopeFocus binding to one
// window's transcript and is only meaningful at per-window registration time (see
// SessionWindow.registerTranscriptBindings), not on this static catalog.
type action struct {
	category string // cheatsheet grouping, e.g. "Session"
	name     string // human description shown in palette and cheatsheet
	// keys is the display hint shown in the palette/cheatsheet. For a rebindable entry
	// (actionID != "") actions() overwrites it from chordFor so it can never drift from
	// the live binding; for a slash command or palette-only entry (actionID == "") it is
	// the literal hint built in the table (e.g. "/fork"), or "" when there is none.
	keys    string
	run     func()      // palette action; nil marks a reference-only / display-only entry
	enabled func() bool // availability predicate; nil means always available

	// actionID ties this entry to its BindingRegistry binding (issue #401). Empty marks
	// an entry that is not rebindable — slash commands (typed text, not keybindings) and
	// palette-only commands with no shortcut. A non-empty actionID is registered by
	// rebuildBindings (Global/Fallthrough once, Focus per window) and is the opaque key
	// the customizer rebinds and persists; it is the contract a tester targets.
	actionID tv.ActionID
	scope    tv.Scope // dispatch scope of the binding (Global / Focus / Fallthrough)
	deflt    tv.Chord // built-in default chord (the binding when no override is recorded)
}

// Action identifiers (issue #401, extending #269). These are the opaque keys gogent
// shares with turbotui's BindingRegistry. Global chords (menu accelerators) fire before
// the focused widget; the transcript-context keys are ScopeFocus (scoped to a session
// window's transcript); the help/palette keys are ScopeFallthrough. They are the
// contract a tester targets, so they are stable strings, not generated.
const (
	actionTranscriptFind        tv.ActionID = "transcript.find"
	actionTranscriptShowAll     tv.ActionID = "transcript.showAll"
	actionTranscriptToggleMsg   tv.ActionID = "transcript.toggle.messages"
	actionTranscriptToggleTool  tv.ActionID = "transcript.toggle.tools"
	actionTranscriptToggleThink tv.ActionID = "transcript.toggle.thinking"
	actionTranscriptToggleErr   tv.ActionID = "transcript.toggle.errors"
	actionTranscriptFoldAll     tv.ActionID = "transcript.foldAll"
	actionTranscriptUnfoldAll   tv.ActionID = "transcript.unfoldAll"
	actionTranscriptCopyAnswer  tv.ActionID = "transcript.copyAnswer"

	actionHelpOverlay    tv.ActionID = "app.help"
	actionCommandPalette tv.ActionID = "app.commandPalette"

	// Global menu-accelerator actions promoted to rebindable bindings (issue #401):
	// they were hardcoded Ctrl/Ctrl+Shift menu accelerators owned by the menu bar, so a
	// customizer rebind could never reach them. They now register on the desktop registry
	// from chordFor and survive a menu rebuild.
	actionSessionNew           tv.ActionID = "session.new"
	actionSessionNext          tv.ActionID = "session.next"
	actionSessionClose         tv.ActionID = "session.close"
	actionAppQuit              tv.ActionID = "app.quit"
	actionConfigSubagents      tv.ActionID = "config.subagents"
	actionWindowTileVertical   tv.ActionID = "window.tileVertical"
	actionWindowTileHorizontal tv.ActionID = "window.tileHorizontal"
	actionWindowTileGrid       tv.ActionID = "window.tileGrid"
	actionWindowMaximizeAll    tv.ActionID = "window.maximizeAll"
	actionWindowCascade        tv.ActionID = "window.cascade"

	// Session operations promoted to rebindable bindings (issue #463): they were
	// palette-only (no actionID), so the customizer could not list them and the user
	// could not bind a key to e.g. "Rename session". Most have no conventional default
	// key and so ship with deflt: unboundChord — rebindable but bound to nothing until
	// the user assigns a chord (rebuildBindings/conflictHolder/persistence all skip the
	// sentinel, so an unbound default registers, collides and persists as nothing).
	actionSessionPrev        tv.ActionID = "session.prev"
	actionSessionCloseOthers tv.ActionID = "session.closeOthers"
	actionSessionCloseAll    tv.ActionID = "session.closeAll"
	actionSessionRename      tv.ActionID = "session.rename"
	actionSessionPin         tv.ActionID = "session.pin"
	actionSessionMoveUp      tv.ActionID = "session.moveUp"
	actionSessionMoveDown    tv.ActionID = "session.moveDown"
	actionSessionSwitchModel tv.ActionID = "session.switchModel"
	actionSessionExportMD    tv.ActionID = "session.exportMarkdown"
	actionSessionExportJSON  tv.ActionID = "session.exportJSON"
	actionSessionsBrowser    tv.ActionID = "session.browser"
	actionTranscriptCopyCode tv.ActionID = "transcript.copyCode"
)

// visible reports whether the action should appear in the help overlay: an action
// gated behind an unwired handler is hidden so the cheatsheet only lists things that
// actually work.
func (c action) visible() bool {
	return c.enabled == nil || c.enabled()
}

// available reports whether the action should be offered in the palette: it must
// carry a runnable action and be visible in the current configuration.
func (c action) available() bool {
	return c.run != nil && c.visible()
}

// rawActions returns the unified command/keybinding catalog (issue #401) WITHOUT the
// display-key projection. It is the lookup catalog: actionByID / keybindDefault /
// rebindable / rebuildBindings scan it, and crucially it does NOT call chordFor — so
// the chordFor -> keybindDefault -> actionByID lookup chain terminates here instead of
// recursing back through the projection. It is rebuilt on each call so the availability
// predicates reflect the live handler wiring and active session. Entries are grouped by
// category (Session, Window, Transcript, Config, App); the palette, the help overlay and
// the customizer all rely on that grouping. Display consumers call actions() (which adds
// the key hints); everything that only needs identity/scope/default uses rawActions().
func (w *Workbench) rawActions() []action {
	avail := func(ok bool) func() bool { return func() bool { return ok } }
	h := w.handlers
	toggle := func(k eventKind) func() {
		return func() { w.transcriptDo(func(m *transcriptModel) { m.toggleKind(k) }) }
	}
	// sessionCmd runs a client-side slash command against the active session, then
	// repaints so its transcript note shows (issue #201): the palette is how the
	// client-side slash commands (/stop, /clearqueue, /markdown, /undo, /rewind,
	// /plan, /act, /thinking) become discoverable (issues #201, #340).
	sessionCmd := func(cmd string) func() {
		return func() {
			w.withActiveTranscript(func(sw *SessionWindow) { sw.handleSlashCommand(cmd) })
			w.desktop.Redraw()
		}
	}
	cmds := []action{
		// Session lifecycle and arrangement.
		{category: "Session", name: "New session", run: func() { w.NewSession() },
			actionID: actionSessionNew, scope: tv.ScopeGlobal, deflt: tv.Chord{Rune: 'n', Ctrl: true}},
		{category: "Session", name: "Fork session", keys: "/fork", run: sessionCmd("/fork")},
		{category: "Session", name: "Next session", run: func() { w.cycle(1) },
			actionID: actionSessionNext, scope: tv.ScopeGlobal, deflt: tv.Chord{Rune: ']', Ctrl: true}},
		// Previous session mirrors Next; its default is Ctrl+P (not the issue's suggested
		// Ctrl+[, which a terminal delivers as Esc and tui.Deliverability rejects).
		{category: "Session", name: "Previous session", run: func() { w.cycle(-1) },
			actionID: actionSessionPrev, scope: tv.ScopeGlobal, deflt: tv.Chord{Rune: 'p', Ctrl: true}},
		{category: "Session", name: "Close session", run: w.CloseActive,
			actionID: actionSessionClose, scope: tv.ScopeGlobal, deflt: tv.Chord{Rune: 'w', Ctrl: true}},
		// Destructive close-many ops ship unbound by default so no key fires them unless
		// the user opts in; the customizer still lists them.
		{category: "Session", name: "Close other sessions", run: func() { w.CloseOthers(w.ActiveID()) },
			actionID: actionSessionCloseOthers, scope: tv.ScopeGlobal, deflt: unboundChord},
		{category: "Session", name: "Close all sessions", run: w.CloseAll,
			actionID: actionSessionCloseAll, scope: tv.ScopeGlobal, deflt: unboundChord},
		// Rename defaults to F2 (conventional, conflict-free, deliverable); the rest of
		// the per-session ops have no conventional key and ship unbound (issue #463).
		{category: "Session", name: "Rename session", run: func() { w.RenameSession(w.ActiveID()) },
			actionID: actionSessionRename, scope: tv.ScopeGlobal, deflt: tv.Chord{Key: tui.KeyF2}},
		{category: "Session", name: "Pin / unpin session", run: func() { w.TogglePin(w.ActiveID()) },
			actionID: actionSessionPin, scope: tv.ScopeGlobal, deflt: unboundChord},
		{category: "Session", name: "Move session up", run: func() { w.MoveSession(w.ActiveID(), -1) },
			actionID: actionSessionMoveUp, scope: tv.ScopeGlobal, deflt: unboundChord},
		{category: "Session", name: "Move session down", run: func() { w.MoveSession(w.ActiveID(), 1) },
			actionID: actionSessionMoveDown, scope: tv.ScopeGlobal, deflt: unboundChord},
		{category: "Session", name: "Switch model", run: w.focusActiveModel,
			actionID: actionSessionSwitchModel, scope: tv.ScopeGlobal, deflt: unboundChord},
		// Running-turn / queue controls and per-session toggles (issue #201): the
		// buttons cover the common path, but surfacing the commands here keeps the
		// keyboard equivalents discoverable.
		{category: "Session", name: "Stop turn", keys: "/stop", run: sessionCmd("/stop")},
		{category: "Session", name: "Clear queued message", keys: "/clearqueue", run: sessionCmd("/clearqueue")},
		{category: "Session", name: "Set / show goal (supervisor)", keys: "/goal", run: w.editActiveGoal},
		{category: "Session", name: "Toggle Markdown rendering", keys: "/markdown", run: sessionCmd("/markdown")},
		// The remaining client-side slash commands (issue #340): each was handled in
		// handleSlashCommand but missing from this table, so it was invisible to both
		// the palette and the ? cheatsheet. They reuse sessionCmd, so invoking the
		// palette entry runs the exact same path as typing the command. /rewind is
		// surfaced bare (revert every recorded turn — its documented default) to match
		// the toggle-style entries above; a numeric prompt would be a larger change.
		{category: "Session", name: "Undo last turn", keys: "/undo", run: sessionCmd("/undo")},
		{category: "Session", name: "Rewind turns", keys: "/rewind", run: sessionCmd("/rewind")},
		{category: "Session", name: "Toggle plan mode", keys: "/plan", run: sessionCmd("/plan")},
		{category: "Session", name: "Toggle YOLO mode", keys: "/yolo", run: sessionCmd("/yolo")},
		// /act is a no-op unless a plan is pending, so gate it on that state to keep the
		// palette honest — it mirrors approvePlan()'s own guard in session_window.go.
		{category: "Session", name: "Approve plan", keys: "/act", run: sessionCmd("/act"),
			enabled: w.activePlanPending},
		{category: "Session", name: "Toggle thinking stream", keys: "/thinking", run: sessionCmd("/thinking")},
		// Names shortened to "Export Markdown"/"Export JSON" (matching the Session menu)
		// so the customizer's 26-cell name column does not truncate them (issue #463).
		{category: "Session", name: "Export Markdown", run: func() { w.exportActive("md") },
			enabled:  avail(h.GetTranscript != nil),
			actionID: actionSessionExportMD, scope: tv.ScopeGlobal, deflt: unboundChord},
		{category: "Session", name: "Export JSON", run: func() { w.exportActive("json") },
			enabled:  avail(h.GetTranscript != nil),
			actionID: actionSessionExportJSON, scope: tv.ScopeGlobal, deflt: unboundChord},
		{category: "Session", name: "Saved sessions browser", run: w.showSessionsDialog,
			enabled:  avail(h.ListSavedSessions != nil),
			actionID: actionSessionsBrowser, scope: tv.ScopeGlobal, deflt: unboundChord},

		// Window arrangement (issue #241): tile or maximize every open window across
		// the work area. Mirrored on the View menu, which tags the same actionIDs.
		{category: "Window", name: "Tile vertically", run: func() { w.arrange(tv.TileRows) },
			actionID: actionWindowTileVertical, scope: tv.ScopeGlobal, deflt: tv.Chord{Rune: 'v', Ctrl: true, Shift: true}},
		{category: "Window", name: "Tile horizontally", run: func() { w.arrange(tv.TileColumns) },
			actionID: actionWindowTileHorizontal, scope: tv.ScopeGlobal, deflt: tv.Chord{Rune: 'h', Ctrl: true, Shift: true}},
		{category: "Window", name: "Tile grid", run: func() { w.arrange(tv.TileGrid) },
			actionID: actionWindowTileGrid, scope: tv.ScopeGlobal, deflt: tv.Chord{Rune: 'g', Ctrl: true, Shift: true}},
		{category: "Window", name: "Cascade windows", run: func() { w.arrange(tv.TileCascade) },
			actionID: actionWindowCascade, scope: tv.ScopeGlobal, deflt: tv.Chord{Rune: 'd', Ctrl: true, Shift: true}},
		{category: "Window", name: "Maximize all windows", run: w.maximizeAll,
			actionID: actionWindowMaximizeAll, scope: tv.ScopeGlobal, deflt: tv.Chord{Rune: 'm', Ctrl: true, Shift: true}},

		// Transcript view controls. The single-letter keys only fire while a
		// transcript is focused (ScopeFocus); listing them here is exactly the
		// cheatsheet the help overlay exists to provide.
		{category: "Transcript", name: "Find in transcript",
			run:      func() { w.withActiveTranscript((*SessionWindow).promptFind) },
			actionID: actionTranscriptFind, scope: tv.ScopeFocus, deflt: tv.Chord{Rune: '/'}},
		{category: "Transcript", name: "Show all (clear filter)",
			run:      func() { w.transcriptDo((*transcriptModel).showAll) },
			actionID: actionTranscriptShowAll, scope: tv.ScopeFocus, deflt: tv.Chord{Key: tui.KeyEscape}},
		{category: "Transcript", name: "Toggle messages", run: toggle(kindAssistant),
			actionID: actionTranscriptToggleMsg, scope: tv.ScopeFocus, deflt: tv.Chord{Rune: 'a'}},
		{category: "Transcript", name: "Toggle tool calls", run: toggle(kindTool),
			actionID: actionTranscriptToggleTool, scope: tv.ScopeFocus, deflt: tv.Chord{Rune: 't'}},
		{category: "Transcript", name: "Toggle thinking", run: toggle(kindThinking),
			actionID: actionTranscriptToggleThink, scope: tv.ScopeFocus, deflt: tv.Chord{Rune: 'r'}},
		{category: "Transcript", name: "Toggle errors", run: toggle(kindError),
			actionID: actionTranscriptToggleErr, scope: tv.ScopeFocus, deflt: tv.Chord{Rune: 'e'}},
		{category: "Transcript", name: "Fold all",
			run:      func() { w.transcriptDo(func(m *transcriptModel) { m.setFold(true) }) },
			actionID: actionTranscriptFoldAll, scope: tv.ScopeFocus, deflt: tv.Chord{Rune: 'f'}},
		{category: "Transcript", name: "Unfold all",
			run:      func() { w.transcriptDo(func(m *transcriptModel) { m.setFold(false) }) },
			actionID: actionTranscriptUnfoldAll, scope: tv.ScopeFocus, deflt: tv.Chord{Rune: 'u'}},
		{category: "Transcript", name: "Copy last answer",
			run:      func() { w.withActiveTranscript((*SessionWindow).copyLastAnswer) },
			actionID: actionTranscriptCopyAnswer, scope: tv.ScopeFocus, deflt: tv.Chord{Rune: 'y'}},
		{category: "Transcript", name: "Copy last code block",
			run:      func() { w.withActiveTranscript((*SessionWindow).copyLastCode) },
			actionID: actionTranscriptCopyCode, scope: tv.ScopeFocus, deflt: unboundChord},

		// Configuration browsers and editors.
		{category: "Config", name: "Sub-agent settings", run: w.showSettingsDialog,
			enabled:  avail(h.GetSettings != nil && h.SetSettings != nil),
			actionID: actionConfigSubagents, scope: tv.ScopeGlobal, deflt: tv.Chord{Rune: ',', Ctrl: true}},
		{category: "Config", name: "Models", run: w.showModelEditor},
		{category: "Config", name: "Resources (tools & skills)", run: w.showResourcesDialog},
		{category: "Config", name: "Statistics", run: w.showStatisticsDialog,
			enabled: avail(h.GetStatistics != nil)},
		{category: "Config", name: "Notifications", run: w.showNotificationsDialog,
			enabled: avail(h.GetNotifyConfig != nil && h.SetNotifyConfig != nil)},
		{category: "Config", name: "Theme editor", run: w.showThemeEditor,
			enabled: avail(h.GetTheme != nil && h.SetTheme != nil)},
		{category: "Config", name: "Edit commands…", run: w.showCommandsDialog,
			enabled: avail(h.ListCommands != nil)},

		// Application-wide actions. The command palette and help overlay are full
		// rebindable actions (issue #401): their run IS the registry handler for the
		// ':' / '?' fallthrough keys, so the menu OnSelect and the binding fire the same
		// closure.
		{category: "App", name: "Pin / unpin sidebar", run: w.ToggleSidebarPin},
		{category: "App", name: "Command palette", run: w.showCommandPalette,
			actionID: actionCommandPalette, scope: tv.ScopeFallthrough, deflt: tv.Chord{Rune: ':'}},
		{category: "App", name: "Keybinding help", run: w.showHelpOverlay,
			actionID: actionHelpOverlay, scope: tv.ScopeFallthrough, deflt: tv.Chord{Rune: '?'}},
		// Re-open the welcome/onboarding dialog on demand (issue #342): always
		// available, and because the ? cheatsheet renders from this same table it
		// appears there too. The dialog's checkbox doubles as the startup-preference
		// toggle, so this is also how a user re-enables the startup dialog.
		{category: "App", name: "Show welcome", run: w.showWelcomeDialog},
		{category: "App", name: "Customize keybindings", run: w.showKeybindingCustomizer},
		{category: "App", name: "Quit", run: w.confirmQuit,
			actionID: actionAppQuit, scope: tv.ScopeGlobal, deflt: tv.Chord{Rune: 'q', Ctrl: true}},
	}
	// Custom slash commands (issue #403): one palette entry per user-defined
	// command, rebuilt each call (rawActions is invoked fresh) so newly created or
	// deleted commands appear/disappear without a restart. Each runs the exact
	// dispatch path typing "/name" would, so a parameterless command expands and
	// sends; a parameterized one sends with empty/default bindings (or surfaces its
	// required-parameter error), matching handleSlashCommand.
	if h.ListCommands != nil {
		for _, c := range h.ListCommands() {
			slash := "/" + c.Name
			name := slash
			if c.Description != "" {
				name += " — " + c.Description
			}
			cmds = append(cmds, action{category: "Commands", name: name, keys: slash, run: sessionCmd(slash)})
		}
	}
	return cmds
}

// actions returns the catalog with each rebindable entry's display key hint projected
// from its current binding (issue #401): the override-or-default chord is the single
// source of truth, so the palette and cheatsheet can never drift from what the key
// actually does, and a rebind shows through without touching the registry. chordLabel
// renders a cleared (unbound) action as "—"; entries without an actionID (slash commands,
// palette-only) keep their literal table keys. This is the DISPLAY catalog — the palette
// and help overlay use it; identity/default lookups use rawActions() to avoid recursing
// through chordFor.
func (w *Workbench) actions() []action {
	cmds := w.rawActions()
	for i := range cmds {
		if cmds[i].actionID == "" {
			continue
		}
		cmds[i].keys = chordLabel(w.chordFor(cmds[i].actionID))
	}
	return cmds
}

// displayChord renders a chord as the keys the user actually presses, for the
// palette/cheatsheet hint. It is tv.Chord.String() except for a bare letter chord
// (a single ASCII letter with no modifier and no named key), which is shown
// lowercase: Chord.String() upper-cases letters so "Ctrl+R" reads naturally, but an
// unmodified transcript key like 'a' must display as the unshifted "a" the user
// presses, not a Shift-looking "A". This keeps the derived hint equivalent to the
// pre-4a hardcoded string (issue #269) while still tracking the real binding — a
// rebind to another letter, or to a modified chord, flows straight through.
func displayChord(c tv.Chord) string {
	if c.Key == tui.KeyUnknown && !c.Ctrl && !c.Shift && !c.Alt {
		if c.Rune >= 'a' && c.Rune <= 'z' {
			return string(c.Rune)
		}
		if c.Rune >= 'A' && c.Rune <= 'Z' {
			return string(c.Rune - 'A' + 'a')
		}
	}
	return c.String()
}

// activePlanPending reports whether the active session has a plan awaiting
// approval. It is the availability predicate for the palette's "/act" entry
// (issue #340) so the approve-plan command is only offered when there is
// actually a plan to approve — mirroring approvePlan()'s own guard in
// session_window.go. It reads sw.planPending under w.mu, matching how
// editActiveGoal looks up the active window, and is safe on an empty workbench.
func (w *Workbench) activePlanPending() bool {
	// actions() runs this predicate while filtering, so it must tolerate the
	// desktop-less zero-value Workbench unit tests build: ActiveID dereferences the
	// desktop, so without this guard the predicate would panic (cf. activePlanPending).
	if w.desktop == nil {
		return false
	}
	id := w.ActiveID()
	if id == "" {
		return false
	}
	w.mu.Lock()
	sw := w.sessions[id]
	w.mu.Unlock()
	return sw != nil && sw.planPending
}

// editActiveGoal is the palette's "Set / show goal" action (issue #201): it opens
// an input dialog seeded with the active session's current goal (so it both shows
// the goal and lets the user edit it), then applies the result through the same
// /goal path the typed command uses — a non-blank value sets the goal, a cleared
// field clears it. It is a no-op when no session is open, so the palette command is
// safe to invoke on an empty workbench.
func (w *Workbench) editActiveGoal() {
	id := w.ActiveID()
	if id == "" {
		return
	}
	w.mu.Lock()
	sw := w.sessions[id]
	w.mu.Unlock()
	// A read-only analysis window has no backend session, so a supervisor goal is
	// meaningless there (issue #201).
	if sw == nil || sw.readOnly {
		return
	}
	w.showInputDialog("Session Goal", "&Goal:", sw.goal, func(value string, ok bool) {
		if !ok {
			return
		}
		if v := strings.TrimSpace(value); v != "" {
			sw.handleGoalCommand(v)
		} else {
			sw.handleGoalCommand("clear")
		}
		w.desktop.Redraw()
	})
}

// focusActiveModel routes the keyboard to the focused session's model selector so
// the "Switch model" palette action lands the user on the dropdown (Enter/Space
// opens it). It no-ops for a read-only analysis window, which has no selector.
func (w *Workbench) focusActiveModel() {
	w.withActiveTranscript(func(sw *SessionWindow) {
		if sw.modelSelect == nil {
			return
		}
		w.desktop.SetFocus(sw.modelSelect)
		w.desktop.Redraw()
	})
}

// fuzzyScore reports whether pattern matches text as a case-insensitive
// subsequence and, when it does, a score where LOWER is a better match. The
// score penalises a late first match and gaps between matched runes, so a typed
// "ns" ranks "New session" ahead of "Close other sessions". An empty pattern
// matches everything with the best (zero) score.
func fuzzyScore(pattern, text string) (int, bool) {
	if strings.TrimSpace(pattern) == "" {
		return 0, true
	}
	p := strings.ToLower(strings.TrimSpace(pattern))
	t := []rune(strings.ToLower(text))
	score, ti, prev := 0, 0, -1
	for _, pr := range p {
		found := -1
		for ti < len(t) {
			if t[ti] == pr {
				found = ti
				break
			}
			ti++
		}
		if found < 0 {
			return 0, false
		}
		if prev < 0 {
			score += found // leading offset
		} else {
			score += found - prev - 1 // gap between matches
		}
		prev = found
		ti = found + 1
	}
	return score, true
}

// filterCommands returns the available commands fuzzy-matching query, best match
// first. Ties keep the table's (category-grouped) order, so an empty query lists
// every command in its natural grouping. The sort is stable and deterministic.
func filterCommands(cmds []action, query string) []action {
	type scored struct {
		cmd   action
		score int
	}
	matches := make([]scored, 0, len(cmds))
	for _, c := range cmds {
		if !c.available() {
			continue
		}
		if s, ok := fuzzyScore(query, c.name); ok {
			matches = append(matches, scored{c, s})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].score < matches[j].score
	})
	out := make([]action, len(matches))
	for i, m := range matches {
		out[i] = m.cmd
	}
	return out
}

// commandRowNameWidth is the column the palette reserves for the command name
// before its key hint, so the hints line up in a tidy second column.
const commandRowNameWidth = 30

// formatCommandRow renders one palette row: the padded name followed by its key
// hint (when bound).
func formatCommandRow(c action) string {
	row := padName(c.name, commandRowNameWidth)
	if c.keys != "" {
		row += "  " + c.keys
	}
	return strings.TrimRight(row, " ")
}

// helpText renders the keybinding cheatsheet from the visible commands, grouped
// under their category headers in table order. It is pure so the overlay's body
// can be unit-tested without a live desktop.
func helpText(cmds []action) string {
	var b strings.Builder
	cur := ""
	for _, c := range cmds {
		if !c.visible() {
			continue
		}
		if c.category != cur {
			if cur != "" {
				b.WriteByte('\n')
			}
			b.WriteString(c.category)
			b.WriteByte('\n')
			cur = c.category
		}
		fmt.Fprintf(&b, "  %-12s %s\n", c.keys, c.name)
	}
	return b.String()
}

// paletteVChrome is the palette's non-list vertical cost: 2 borders + 1 search-box
// row + 1 gap + 1 hint row + 1 bottom pad. A height of itemCount + paletteVChrome
// shows every command without dead space, so it is the MaxH that keeps a short
// palette compact (issue #309). It must track the listH math in showCommandPalette
// (listH = height - listY - 3, listY = 3).
const paletteVChrome = 6

// helpVChrome is the help overlay's non-body vertical cost: 2 borders + 1 top pad +
// 1 hint/button row + 1 bottom pad. A height of lineCount + helpVChrome shows the
// whole cheatsheet without dead space, so it is the MaxH that keeps a short binding
// list compact (issue #309). It must track the bodyH math in showHelpOverlay
// (bodyH = height - 5).
const helpVChrome = 5

// availableCommandCount reports how many commands the palette will list with an
// empty query — every command that carries an action and is visible in the current
// configuration. It is the row count the palette's MaxH is keyed to.
func availableCommandCount(cmds []action) int {
	n := 0
	for _, c := range cmds {
		if c.available() {
			n++
		}
	}
	return n
}

// textLineCount reports how many display lines text occupies, counting a trailing
// newline as a line terminator rather than an extra empty line. helpText ends every
// line (including the last) with '\n', so this is the cheatsheet's row count.
func textLineCount(text string) int {
	if text == "" {
		return 0
	}
	n := strings.Count(text, "\n")
	if !strings.HasSuffix(text, "\n") {
		n++
	}
	return n
}

// showCommandPalette opens the fuzzy command palette (issue #60): a filterable
// list of every available action. Typing filters live; ↑/↓ move the selection
// and Enter runs it (closing the palette first so the action's own dialog is not
// buried under this modal). Esc dismisses.
// newCloseableDialog builds a modal dialog that carries a visible close
// affordance: the title-bar [x] button (ShowClose) is wired to closeFn so users
// are not left to guess that Esc dismisses it (issue #173). Callers still wire
// Esc through the dialog root for keyboard parity.
func newCloseableDialog(title string, x, y, width, height int, closeFn func()) *tv.Dialog {
	dialog := tv.NewDialog(title, x, y, width, height)
	applyWindowShadow(dialog.Window) // honour the NoShadow theme setting (issue #215)
	dialog.Window.ShowClose = true
	dialog.Window.OnClose = func(*tv.Window) { closeFn() }
	return dialog
}

func (w *Workbench) showCommandPalette() {
	all := w.actions()
	// List-driven, so it benefits from width — keep it near the percentage default
	// (no PreferredW) with a 40×10 floor — but cap the height to the actual command
	// count (+ chrome) so a short palette does not fill 42 rows on a roomy terminal
	// (issues #299, #309).
	spec := tv.DialogSpec{MinW: 40, MinH: 10, MaxH: availableCommandCount(all) + paletteVChrome}
	x, y, width, height := w.dialogRect(spec)

	var layer *tv.Layer
	closeFn := func() { w.desktop.RemoveLayer(layer) }
	dialog := newCloseableDialog("Command Palette", x, y, width, height, closeFn)

	dialog.Window.AddContent(dialogLabel("Run:", tv.Rect{X: 2, Y: 1, W: 4, H: 1}))
	searchBox := tv.NewTextBox("", tv.Rect{X: 6, Y: 1, W: width - 8, H: 1})
	dialog.Window.AddContent(searchBox)

	listY := 3
	listH := height - listY - 3 // hint row + bottom margin + border
	if listH < 3 {
		listH = 3
	}
	list := tv.NewTree(tv.Rect{X: 2, Y: listY, W: width - 4, H: listH})
	list.FG = tv.DefaultTheme.ListFG
	list.BG = tv.DefaultTheme.ListBG
	list.SelFG, list.SelBG = selectionColorsFor(
		tv.DefaultTheme.ListFG, tv.DefaultTheme.ListBG,
		tv.DefaultTheme.SelectionFG, tv.DefaultTheme.SelectionBG)
	dialog.Window.AddContent(list)

	dialog.Window.AddContent(dialogLabel("Type to filter · ↑↓ move · Enter run · Esc close",
		tv.Rect{X: 2, Y: height - 2, W: width - 4, H: 1}))

	render := func() {
		items := filterCommands(all, searchBox.GetText())
		nodes := make([]*tv.TreeNode, 0, len(items))
		for i := range items {
			n := tv.NewTreeNode(formatCommandRow(items[i]))
			n.Data = items[i]
			nodes = append(nodes, n)
		}
		list.Roots = nodes
		w.desktop.Redraw()
	}

	runSelected := func() {
		n := list.Selected()
		if n == nil {
			return
		}
		c, ok := n.Data.(action)
		if !ok || c.run == nil {
			return
		}
		closeFn()
		c.run()
	}
	list.OnActivate = func(*tv.TreeNode) { runSelected() }

	// The search box keeps focus so the user types continuously; Enter runs the
	// selection and the vertical arrows drive the list underneath it (fzf-style).
	boxType := searchBox.Component.OnTypeFn
	searchBox.Component.OnTypeFn = func(c *tv.VisualComponent, event tui.TypeEvent) bool {
		switch event.Key {
		case tui.KeyEnter:
			runSelected()
			return true
		case tui.KeyUp, tui.KeyDown:
			list.Component.OnTypeFn(list.Component, event)
			w.desktop.Redraw()
			return true
		}
		handled := false
		if boxType != nil {
			handled = boxType(c, event)
		}
		render()
		return handled
	}

	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			closeFn()
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("command-palette", dialog)
	w.desktop.AddLayer(layer)
	dialog.Fit(spec) // re-resolve the rect when the terminal is resized (issue #299)
	render()
	w.desktop.SetFocus(searchBox)
}

// showHelpOverlay opens the keybinding cheatsheet (issue #60): a read-only,
// scrollable list of every binding grouped by context, rendered from the same
// command table that drives the palette so the two can never disagree. Esc or
// Close dismisses.
func (w *Workbench) showHelpOverlay() {
	help := helpText(w.actions())
	// Read-only and list-driven, so keep it near the percentage default width (no
	// PreferredW) with a 44×12 floor — but cap the height to the cheatsheet's line
	// count (+ chrome) so a short binding list does not fill 42 rows (issues #299,
	// #309).
	spec := tv.DialogSpec{MinW: 44, MinH: 12, MaxH: textLineCount(help) + helpVChrome}
	x, y, width, height := w.dialogRect(spec)

	var layer *tv.Layer
	closeFn := func() { w.desktop.RemoveLayer(layer) }
	dialog := newCloseableDialog("Keybindings", x, y, width, height, closeFn)

	bodyH := height - 5 // title border + hint/button row + bottom margin
	if bodyH < 3 {
		bodyH = 3
	}
	body := tv.NewTextView(help, tv.Rect{X: 2, Y: 1, W: width - 4, H: bodyH})
	body.FG = tv.DefaultTheme.DialogFG
	body.BG = tv.DefaultTheme.DialogBG
	// Help is read top-down, so open anchored at the first binding (issue #174).
	body.ScrollToTop()
	dialog.Window.AddContent(body)

	dialog.Window.AddContent(dialogLabel("↑↓/PgUp/PgDn scroll · Ctrl+K palette · Esc close",
		tv.Rect{X: 2, Y: height - 2, W: width - 30, H: 1}))

	// "Customize…" roots the keybinding customizer in the cheatsheet (issue #269): it
	// closes the read-only overlay and opens the editable customizer for the same
	// bindings. Close stays rightmost.
	dialog.Window.AddContent(newButton("&Customize…",
		tv.Rect{X: width - 27, Y: height - 2, W: 14, H: 1},
		func() { closeFn(); w.showKeybindingCustomizer() }))
	dialog.Window.AddContent(newButton("Close",
		tv.Rect{X: width - 11, Y: height - 2, W: 9, H: 1}, closeFn))

	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			closeFn()
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("help-overlay", dialog)
	w.desktop.AddLayer(layer)
	dialog.Fit(spec) // re-resolve the rect when the terminal is resized (issue #299)
	w.desktop.SetFocus(body)
}
