package ui

import (
	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// mentionCompleter drives the @-file mention popup attached to a session window's
// input (issue #46). When the cursor sits inside an "@<partial>" token it floats
// a small list of matching workspace files above the input; ↑/↓ move the
// selection, Enter or Tab accepts the highlighted path, Esc dismisses, and typing
// re-filters. The input keeps keyboard focus throughout — the popup is a purely
// visual layer the completer drives by hand (the same keep-focus, forward-keys
// pattern the command palette uses), so accepting a completion never steals the
// caret from the message being typed.
type mentionCompleter struct {
	sw        *SessionWindow
	layer     *tv.Layer
	container *tv.VisualComponent
	list      *tv.Tree
	files     []string // workspace file cache, refreshed each time the popup opens
	start     int      // rune index of '@' on the cursor line for the active token
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
	mc.start = start
	mc.render(matches)
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
	list.FG = tv.DefaultTheme.DialogFG
	list.BG = tv.DefaultTheme.DialogBG
	list.SelFG, list.SelBG = selectionColorsFor(
		tv.DefaultTheme.DialogFG, tv.DefaultTheme.DialogBG,
		tv.DefaultTheme.SelectionFG, tv.DefaultTheme.SelectionBG)
	nodes := make([]*tv.TreeNode, len(matches))
	for i, p := range matches {
		nodes[i] = tv.NewTreeNode(p)
	}
	list.Roots = nodes
	mc.container.Children = nil
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

// accept replaces the active "@<partial>" token with the selected "@<path> " and
// places the caret after it, then closes the popup. The trailing space lets the
// user keep typing (and ends the mention so the completer does not immediately
// reopen). It is a no-op-then-close when there is no selection or the stored token
// bounds no longer fit the line (e.g. after an intervening edit).
func (mc *mentionCompleter) accept() {
	if mc.list == nil {
		mc.hide()
		return
	}
	node := mc.list.Selected()
	in := mc.sw.input
	line := []rune(in.Lines[in.CursorY])
	if node == nil || mc.start < 0 || mc.start > in.CursorX || in.CursorX > len(line) {
		mc.hide()
		return
	}
	replacement := []rune("@" + node.Label + " ")
	newLine := append([]rune{}, line[:mc.start]...)
	newLine = append(newLine, replacement...)
	newLine = append(newLine, line[in.CursorX:]...)
	in.Lines[in.CursorY] = string(newLine)
	in.CursorX = mc.start + len(replacement)
	mc.hide()
	mc.sw.wb.desktop.Redraw()
}
