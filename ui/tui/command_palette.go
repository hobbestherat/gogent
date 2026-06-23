package ui

import (
	"fmt"
	"sort"
	"strings"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// command is one entry in the central command/keybinding table — the single
// source of truth shared by the command palette and the keybinding help overlay
// (issue #60). The palette offers every entry that carries an action and passes
// its optional availability predicate; the help overlay lists every visible
// entry grouped by category, so the cheatsheet can never drift from the real
// bindings.
type command struct {
	category string // help-overlay grouping, e.g. "Session"
	name     string // human description shown in both views
	keys     string // key hint shown in both views ("" when unbound)
	// actionID ties this entry to its turbotui BindingRegistry binding (issue #269,
	// phase 4a). When set and the binding resolves, commands() overwrites keys with
	// the registry's real chord (registry.BindingFor(actionID).Chord.String()) so the
	// palette and cheatsheet can never drift from the live binding. Empty leaves the
	// keys hint as built in the table (slash commands and menu accelerators). The two
	// composite hints that name more than one chord ("Ctrl+F, /" and "Ctrl+K, :") also
	// leave actionID empty, but build their variable half from chordFor so a phase-4b
	// rebind of the '/' or ':' chord still shows through (issue #269).
	actionID tv.ActionID
	run      func()      // palette action; nil marks a reference-only binding
	enabled  func() bool // availability predicate; nil means always available
}

// Action identifiers (issue #269, phase 4a). These are the opaque keys gogent shares
// with turbotui's BindingRegistry: the transcript-context keys are registered at
// ScopeFocus (scoped to a session window's transcript) and the help/palette keys at
// ScopeFallthrough. They are the contract a tester targets, so they are stable
// strings, not generated.
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
)

// visible reports whether the command should appear in the help overlay: a
// command gated behind an unwired handler is hidden so the cheatsheet only lists
// things that actually work.
func (c command) visible() bool {
	return c.enabled == nil || c.enabled()
}

// available reports whether the command should be offered in the palette: it
// must carry an action and be visible in the current configuration.
func (c command) available() bool {
	return c.run != nil && c.visible()
}

// commands returns the central command/keybinding table. It is rebuilt on each
// call so the availability predicates reflect the live handler wiring and active
// session. The entries are listed grouped by category (Session, Transcript,
// Config, App); both the palette and the help overlay rely on that grouping.
func (w *Workbench) commands() []command {
	avail := func(ok bool) func() bool { return func() bool { return ok } }
	h := w.handlers
	toggle := func(k eventKind) func() {
		return func() { w.transcriptDo(func(m *transcriptModel) { m.toggleKind(k) }) }
	}
	// sessionCmd runs a client-side slash command against the active session, then
	// repaints so its transcript note shows (issue #201): the palette is how /stop,
	// /clearqueue and /markdown become discoverable.
	sessionCmd := func(cmd string) func() {
		return func() {
			w.withActiveTranscript(func(sw *SessionWindow) { sw.handleSlashCommand(cmd) })
			w.desktop.Redraw()
		}
	}
	cmds := []command{
		// Session lifecycle and arrangement.
		{category: "Session", name: "New session", keys: "Ctrl+N", run: func() { w.NewSession() }},
		{category: "Session", name: "Next session", keys: "Ctrl+]", run: func() { w.cycle(1) }},
		{category: "Session", name: "Previous session", run: func() { w.cycle(-1) }},
		{category: "Session", name: "Close session", keys: "Ctrl+W", run: w.CloseActive},
		{category: "Session", name: "Close other sessions", run: func() { w.CloseOthers(w.ActiveID()) }},
		{category: "Session", name: "Close all sessions", run: w.CloseAll},
		{category: "Session", name: "Rename session", run: func() { w.RenameSession(w.ActiveID()) }},
		{category: "Session", name: "Pin / unpin session", run: func() { w.TogglePin(w.ActiveID()) }},
		{category: "Session", name: "Move session up", run: func() { w.MoveSession(w.ActiveID(), -1) }},
		{category: "Session", name: "Move session down", run: func() { w.MoveSession(w.ActiveID(), 1) }},
		{category: "Session", name: "Switch model", run: w.focusActiveModel},
		// Running-turn / queue controls and per-session toggles (issue #201): the
		// buttons cover the common path, but surfacing the commands here keeps the
		// keyboard equivalents discoverable.
		{category: "Session", name: "Stop turn", keys: "/stop", run: sessionCmd("/stop")},
		{category: "Session", name: "Clear queued message", keys: "/clearqueue", run: sessionCmd("/clearqueue")},
		{category: "Session", name: "Set / show goal (supervisor)", keys: "/goal", run: w.editActiveGoal},
		{category: "Session", name: "Toggle Markdown rendering", keys: "/markdown", run: sessionCmd("/markdown")},
		{category: "Session", name: "Export transcript (Markdown)", run: func() { w.exportActive("md") },
			enabled: avail(h.GetTranscript != nil)},
		{category: "Session", name: "Export transcript (JSON)", run: func() { w.exportActive("json") },
			enabled: avail(h.GetTranscript != nil)},
		{category: "Session", name: "Saved sessions browser", run: w.showSessionsDialog,
			enabled: avail(h.ListSavedSessions != nil)},

		// Window arrangement (issue #241): tile or maximize every open window across
		// the work area. Mirrored on the View menu with the same Ctrl+Shift chords.
		{category: "Window", name: "Tile vertically", keys: "Ctrl+Shift+V",
			run: func() { w.arrange(tv.TileRows) }},
		{category: "Window", name: "Tile horizontally", keys: "Ctrl+Shift+H",
			run: func() { w.arrange(tv.TileColumns) }},
		{category: "Window", name: "Tile grid", keys: "Ctrl+Shift+G",
			run: func() { w.arrange(tv.TileGrid) }},
		{category: "Window", name: "Cascade windows", keys: "Ctrl+Shift+D",
			run: func() { w.arrange(tv.TileCascade) }},
		{category: "Window", name: "Maximize all windows", keys: "Ctrl+Shift+M",
			run: w.maximizeAll},

		// Transcript view controls. The single-letter keys only fire while a
		// transcript is focused; listing them here is exactly the cheatsheet the
		// help overlay exists to provide.
		{category: "Transcript", name: "Find in transcript", keys: "Ctrl+F, " + displayChord(w.chordFor(actionTranscriptFind)),
			run: func() { w.withActiveTranscript((*SessionWindow).promptFind) }},
		{category: "Transcript", name: "Show all (clear filter)", keys: "Esc", actionID: actionTranscriptShowAll,
			run: func() { w.transcriptDo((*transcriptModel).showAll) }},
		{category: "Transcript", name: "Toggle messages", keys: "a", actionID: actionTranscriptToggleMsg, run: toggle(kindAssistant)},
		{category: "Transcript", name: "Toggle tool calls", keys: "t", actionID: actionTranscriptToggleTool, run: toggle(kindTool)},
		{category: "Transcript", name: "Toggle thinking", keys: "r", actionID: actionTranscriptToggleThink, run: toggle(kindThinking)},
		{category: "Transcript", name: "Toggle errors", keys: "e", actionID: actionTranscriptToggleErr, run: toggle(kindError)},
		{category: "Transcript", name: "Fold all", keys: "f", actionID: actionTranscriptFoldAll,
			run: func() { w.transcriptDo(func(m *transcriptModel) { m.setFold(true) }) }},
		{category: "Transcript", name: "Unfold all", keys: "u", actionID: actionTranscriptUnfoldAll,
			run: func() { w.transcriptDo(func(m *transcriptModel) { m.setFold(false) }) }},
		{category: "Transcript", name: "Copy last answer", keys: "y", actionID: actionTranscriptCopyAnswer,
			run: func() { w.withActiveTranscript((*SessionWindow).copyLastAnswer) }},
		{category: "Transcript", name: "Copy last code block",
			run: func() { w.withActiveTranscript((*SessionWindow).copyLastCode) }},

		// Configuration browsers and editors.
		{category: "Config", name: "Sub-agent settings", keys: "Ctrl+,", run: w.showSettingsDialog,
			enabled: avail(h.GetSettings != nil && h.SetSettings != nil)},
		{category: "Config", name: "Models", run: w.showModelEditor},
		{category: "Config", name: "Resources (tools & skills)", run: w.showResourcesDialog},
		{category: "Config", name: "Statistics", run: w.showStatisticsDialog,
			enabled: avail(h.GetStatistics != nil)},
		{category: "Config", name: "Notifications", run: w.showNotificationsDialog,
			enabled: avail(h.GetNotifyConfig != nil && h.SetNotifyConfig != nil)},
		{category: "Config", name: "Theme editor", run: w.showThemeEditor,
			enabled: avail(h.GetTheme != nil && h.SetTheme != nil)},

		// Application-wide actions. The palette itself is reference-only (you are
		// already in it); the sidebar pin and help/quit are runnable.
		{category: "App", name: "Pin / unpin sidebar", run: w.ToggleSidebarPin},
		{category: "App", name: "Command palette", keys: "Ctrl+K, " + displayChord(w.chordFor(actionCommandPalette))},
		{category: "App", name: "Keybinding help", keys: "?", actionID: actionHelpOverlay, run: w.showHelpOverlay},
		{category: "App", name: "Customize keybindings", run: w.showKeybindingCustomizer},
		{category: "App", name: "Quit", keys: "Ctrl+Q", run: w.confirmQuit},
	}
	// Derive the key hint from the live registry for every entry that names an action
	// (issue #269, phase 4a): the binding is the single source of truth, so the
	// palette and cheatsheet can never drift from what the key actually does. Entries
	// without an actionID (slash commands, menu accelerators) keep their table keys;
	// the two composite hints already folded their live chord in above via chordFor.
	for i := range cmds {
		if cmds[i].actionID == "" {
			continue
		}
		if disp, ok := w.chordDisplay(cmds[i].actionID); ok {
			cmds[i].keys = disp
		}
	}
	return cmds
}

// chordDisplay returns the conventional display string for the chord bound to
// actionID in the desktop's scoped BindingRegistry (issue #269, phase 4a), and false
// when there is no desktop yet or no binding carries the action. Callers fall back to
// the hardcoded keys hint on false, so a workbench built without a desktop (e.g. the
// zero value used by unit tests) renders the catalog's default hints unchanged.
func (w *Workbench) chordDisplay(id tv.ActionID) (string, bool) {
	if w.desktop == nil {
		return "", false
	}
	reg := w.desktop.ScopedBindings()
	if reg == nil {
		return "", false
	}
	if b, ok := reg.BindingFor(id); ok {
		return displayChord(b.Chord), true
	}
	return "", false
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
func filterCommands(cmds []command, query string) []command {
	type scored struct {
		cmd   command
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
	out := make([]command, len(matches))
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
func formatCommandRow(c command) string {
	row := padName(c.name, commandRowNameWidth)
	if c.keys != "" {
		row += "  " + c.keys
	}
	return strings.TrimRight(row, " ")
}

// helpText renders the keybinding cheatsheet from the visible commands, grouped
// under their category headers in table order. It is pure so the overlay's body
// can be unit-tested without a live desktop.
func helpText(cmds []command) string {
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
func availableCommandCount(cmds []command) int {
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
	all := w.commands()
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
		c, ok := n.Data.(command)
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
	help := helpText(w.commands())
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
