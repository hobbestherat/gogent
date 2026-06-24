package ui

import (
	"strings"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// mentionCompleter drives the inline completion popup attached to a session
// window's input. It serves two token kinds: "@<partial>" workspace-file mentions
// (issue #46) and, on the first line, "/<partial>" custom slash commands (issue
// #403). ↑/↓ move the selection, Enter or Tab accepts the highlighted item, Esc
// dismisses, and typing re-filters. The input keeps keyboard focus throughout —
// the popup is a purely visual layer the completer drives by hand (the same
// keep-focus, forward-keys pattern the command palette uses), so accepting a
// completion never steals the caret from the message being typed.
type mentionCompleter struct {
	sw        *SessionWindow
	layer     *tv.Layer
	container *tv.VisualComponent
	list      *tv.Tree
	files     []string      // workspace file cache, refreshed each time the popup opens
	commands  []CommandInfo // custom-command cache, refreshed each time the popup opens
	// values holds the insertion text for each currently-rendered row, parallel to
	// the list's nodes; accept() inserts values[selected]. Labels (what is shown) and
	// values (what is inserted) differ for slash commands ("/name — desc" vs "/name ").
	values []string
	// spanStart/spanEnd are the rune-index bounds [start,end) of the active token on
	// the cursor line that accept() rewrites with the chosen row's value.
	spanStart int
	spanEnd   int
}

// completionItem is one popup row: label is shown, value is inserted (replacing
// the active token span) when the row is accepted.
type completionItem struct {
	label string
	value string
}

const (
	mentionPopupWidth = 50 // popup width in cells, clamped to the input width
	mentionMaxVisible = 8  // rows shown before the list scrolls
	maxMentionMatches = 50 // candidates kept after filtering
)

// newMentionCompleter creates the completer for a session window. It is always
// non-nil; when no ListWorkspaceFiles handler is wired the completer simply never
// finds candidates and stays dormant.
func newMentionCompleter(sw *SessionWindow) *mentionCompleter {
	return &mentionCompleter{sw: sw}
}

// active reports whether the popup is currently shown.
func (mc *mentionCompleter) active() bool { return mc.layer != nil }

// fetch pulls the current workspace file listing from the backend bridge, or nil
// when the feature is unwired.
func (mc *mentionCompleter) fetch() []string {
	if mc.sw.wb.handlers.ListWorkspaceFiles == nil {
		return nil
	}
	return mc.sw.wb.handlers.ListWorkspaceFiles()
}

// update recomputes the popup from the input's current line and cursor. It is
// called after every keystroke the input handles: it opens or refreshes the list
// while the cursor is inside a mention token and closes it otherwise. The file
// listing is (re)fetched only on open, so mid-mention typing just re-filters the
// cached slice.
func (mc *mentionCompleter) update() {
	in := mc.sw.input
	if in == nil || in.CursorY < 0 || in.CursorY >= len(in.Lines) {
		mc.hide()
		return
	}
	line := []rune(in.Lines[in.CursorY])
	// Slash-command completion takes priority on the first line when it begins with
	// '/' and the cursor is within the command word (issue #403).
	if items, end, ok := mc.slashMatches(line, in.CursorX, in.CursorY); ok {
		mc.spanStart, mc.spanEnd = 0, end
		mc.renderItems(items)
		return
	}
	start, query, ok := mentionToken(line, in.CursorX)
	if !ok {
		mc.hide()
		return
	}
	if !mc.active() {
		mc.files = mc.fetch()
	}
	matches := filterPaths(mc.files, query, maxMentionMatches)
	if len(matches) == 0 {
		mc.hide()
		return
	}
	mc.spanStart, mc.spanEnd = start, in.CursorX
	items := make([]completionItem, len(matches))
	for i, p := range matches {
		items[i] = completionItem{label: p, value: "@" + p + " "}
	}
	mc.renderItems(items)
}

// renderItems records each row's insertion value and renders the labels. It is the
// single funnel both token kinds use so accept() can map the selected row to its
// value via the parallel values slice.
func (mc *mentionCompleter) renderItems(items []completionItem) {
	labels := make([]string, len(items))
	mc.values = make([]string, len(items))
	for i, it := range items {
		labels[i] = it.label
		mc.values[i] = it.value
	}
	mc.render(labels)
}

// slashMatches returns the custom-command completions for a "/<partial>" token on
// the first line. ok is false unless the line starts with '/', the cursor is
// within the command word (not in the arguments) and at least one command matches.
// end is the rune index just past the command word, so accept() replaces the whole
// word (preserving any already-typed arguments after it).
func (mc *mentionCompleter) slashMatches(line []rune, cursorX, cursorY int) (items []completionItem, end int, ok bool) {
	if cursorY != 0 || len(line) == 0 || line[0] != '/' {
		return nil, 0, false
	}
	if mc.sw.wb == nil || mc.sw.wb.handlers.ListCommands == nil {
		return nil, 0, false
	}
	end = 1
	for end < len(line) && line[end] != ' ' {
		end++
	}
	if cursorX < 1 || cursorX > end {
		return nil, 0, false // cursor is in the arguments, not the command word
	}
	if !mc.active() {
		mc.commands = mc.sw.wb.handlers.ListCommands()
	}
	query := strings.ToLower(string(line[1:end]))
	for _, c := range mc.commands {
		if query != "" && !strings.Contains(strings.ToLower(c.Name), query) {
			continue
		}
		label := "/" + c.Name
		if c.Description != "" {
			label += " — " + c.Description
		}
		items = append(items, completionItem{label: label, value: "/" + c.Name + " "})
		if len(items) >= maxMentionMatches {
			break
		}
	}
	if len(items) == 0 {
		return nil, 0, false
	}
	return items, end, true
}

// render (re)builds the popup layer for the given matches, anchored above the
// input (or below it when there is no room above). The list is rebuilt every
// render so the selection resets to the top (best) match, which is what Enter
// accepts.
func (mc *mentionCompleter) render(matches []string) {
	rows := len(matches)
	if rows > mentionMaxVisible {
		rows = mentionMaxVisible
	}
	inAbs := mc.sw.input.Component.AbsoluteBounds()
	boxW := mentionPopupWidth
	if inAbs.W > 0 && boxW > inAbs.W {
		boxW = inAbs.W
	}
	boxH := rows + 2 // list rows plus the box border
	x := inAbs.X
	y := inAbs.Y - boxH
	if y < menuBarHeight {
		y = inAbs.Y + inAbs.H // no room above: drop below the input
	}

	if mc.container == nil {
		mc.container = tv.NewComponent(tv.Rect{})
		mc.container.DrawFn = func(c *tv.VisualComponent, surface tv.Surface) {
			abs := c.AbsoluteBounds()
			surface.Fill(abs, tui.Cell{Ch: ' ', FG: tv.DefaultTheme.DialogFG, BG: tv.DefaultTheme.DialogBG})
			surface.DrawBox(abs, tui.LineSingle, tv.DefaultTheme.DialogFG, tv.DefaultTheme.DialogBG)
		}
	}
	mc.container.SetBounds(tv.Rect{X: x, Y: y, W: boxW, H: boxH})

	list := tv.NewTree(tv.Rect{X: 1, Y: 1, W: boxW - 2, H: rows})
	list.FG = tv.DefaultTheme.ListFG
	list.BG = tv.DefaultTheme.ListBG
	list.SelFG, list.SelBG = selectionColorsFor(
		tv.DefaultTheme.ListFG, tv.DefaultTheme.ListBG,
		tv.DefaultTheme.SelectionFG, tv.DefaultTheme.SelectionBG)
	nodes := make([]*tv.TreeNode, len(matches))
	for i, p := range matches {
		nodes[i] = tv.NewTreeNode(p)
	}
	list.Roots = nodes
	for _, child := range mc.container.Children() {
		mc.container.RemoveChild(child)
	}
	mc.container.AddChild(list)
	mc.list = list

	if mc.layer == nil {
		mc.layer = tv.NewWindowLayer("mention-completer-"+mc.sw.id, mc.container)
		mc.sw.wb.desktop.AddLayer(mc.layer)
	}
	mc.sw.wb.desktop.Redraw()
}

// hide tears down the popup if it is showing and re-pins focus on the input.
// RemoveLayer re-evaluates focus against the new top layer, so the explicit
// SetFocus keeps the caret in the message the user is typing.
func (mc *mentionCompleter) hide() {
	if mc.layer == nil {
		return
	}
	mc.sw.wb.desktop.RemoveLayer(mc.layer)
	mc.layer = nil
	mc.list = nil
	mc.sw.wb.desktop.SetFocus(mc.sw.input)
}

// handleKey intercepts the navigation/accept/dismiss keys while the popup is
// shown, returning true when it consumed the event so the input does not also act
// on it (notably: Enter accepts a completion rather than submitting the message).
// Every other key — including plain runs and backspace — falls through to the
// input, which then triggers update to re-filter.
func (mc *mentionCompleter) handleKey(event tui.TypeEvent) bool {
	if !mc.active() {
		return false
	}
	switch event.Key {
	case tui.KeyUp, tui.KeyDown:
		if mc.list != nil {
			mc.list.Component.OnTypeFn(mc.list.Component, event)
			mc.sw.wb.desktop.Redraw()
		}
		return true
	case tui.KeyEnter, tui.KeyTab:
		mc.accept()
		return true
	case tui.KeyEscape:
		mc.hide()
		mc.sw.wb.desktop.Redraw()
		return true
	}
	return false
}

// accept replaces the active token span with the selected item's value (an
// "@<path> " mention or a "/<name> " slash command) and places the caret after it,
// then closes the popup. The trailing space lets the user keep typing (and ends
// the token so the completer does not immediately reopen). It is a no-op-then-close
// when there is no selection or the stored span no longer fits the line (e.g. after
// an intervening edit).
func (mc *mentionCompleter) accept() {
	if mc.list == nil {
		mc.hide()
		return
	}
	node := mc.list.Selected()
	in := mc.sw.input
	line := []rune(in.Lines[in.CursorY])
	if node == nil || mc.spanStart < 0 || mc.spanStart > mc.spanEnd || mc.spanEnd > len(line) {
		mc.hide()
		return
	}
	// Map the selected node to its insertion value via the parallel values slice
	// (label ≠ inserted text for slash commands). Fall back to the label when the
	// row index can't be resolved, which keeps mention completion correct.
	value := node.Label
	for i, n := range mc.list.Roots {
		if n == node && i < len(mc.values) {
			value = mc.values[i]
			break
		}
	}
	replacement := []rune(value)
	newLine := append([]rune{}, line[:mc.spanStart]...)
	newLine = append(newLine, replacement...)
	newLine = append(newLine, line[mc.spanEnd:]...)
	in.Lines[in.CursorY] = string(newLine)
	in.CursorX = mc.spanStart + len(replacement)
	mc.hide()
	mc.sw.wb.desktop.Redraw()
}
