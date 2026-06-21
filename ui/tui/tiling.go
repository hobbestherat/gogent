package ui

import (
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Window tiling / auto-arrange (issue #241). The pure tiling geometry lives in the
// turbotv toolkit (TileRects/TileWindows); the session-aware concerns stay here:
// which windows to arrange, the work area they tile into, clearing gogent's own
// maximized bookkeeping, and persistence. Surfaced from the View menu and the
// command palette (see viewItems and commands).

// tileArea is the rectangle the arrange / maximize-all actions lay windows out in:
// the maximized-window area — the pinned work area (windowArea, issue #106) reduced
// to the rect a single maximized window fills (maximizedWindowRect, issue #105).
// Using it keeps tiled and maximized windows on one origin, below the menu bar and
// left of a pinned sidebar, and (being a subset of windowArea) keeps every tile
// inside the work area. Read on the UI thread.
func (w *Workbench) tileArea() tv.Rect {
	area := w.windowArea()
	return maximizedWindowRect(area.W, area.H)
}

// openWindows returns every open session window — live and read-only analysis
// windows (issue #58) alike — in sidebar order (w.order), paired with the underlying
// turbotv windows. Arranging in sidebar order keeps the result predictable and
// re-runnable. Caller must NOT hold w.mu.
func (w *Workbench) openWindows() (sws []*SessionWindow, wins []*tv.Window) {
	w.mu.Lock()
	defer w.mu.Unlock()
	sws = make([]*SessionWindow, 0, len(w.order))
	wins = make([]*tv.Window, 0, len(w.order))
	for _, id := range w.order {
		sw := w.sessions[id]
		if sw == nil {
			continue
		}
		sws = append(sws, sw)
		wins = append(wins, sw.window)
	}
	return sws, wins
}

// arrange tiles every open window across the work area using the given layout
// (TileRows / TileColumns / TileGrid). Windows are gathered in sidebar order and
// handed to tv.TileWindows, which un-minimizes each one and gives it explicit,
// non-overlapping bounds inside tileArea(). A tiled window is no longer maximized,
// so gogent's own maximized flag is cleared too (the toolkit clears its separate
// flag); the layout is then persisted and redrawn. A no-op when nothing is open.
func (w *Workbench) arrange(layout tv.TileLayout) {
	sws, wins := w.openWindows()
	if len(wins) == 0 {
		return
	}
	tv.TileWindows(layout, w.tileArea(), wins)
	for _, sw := range sws {
		sw.maximized = false
	}
	w.persistLayout()
	w.desktop.Redraw()
}

// maximizeAll expands every open window to the work area (tileArea), the bulk
// counterpart of the per-window title-bar maximize (issue #105). Each window records
// its pre-maximize bounds (unless already maximized) so a later per-window restore
// returns it, is marked maximized and sized to the area. Unlike the title-bar button
// this ignores the per-window maximizable gate so "Maximize All" always expands
// everything, matching how the tiling actions arrange all windows. Persists+redraws;
// a no-op when nothing is open.
func (w *Workbench) maximizeAll() {
	sws, _ := w.openWindows()
	if len(sws) == 0 {
		return
	}
	area := w.tileArea()
	for _, sw := range sws {
		win := sw.window
		// Un-minimize first, exactly as tv.TileWindows does for arrange (otherwise a
		// minimized window draws only its 1-row title bar and ignores the new bounds,
		// so "Maximize All" would silently skip it). Restore brings the content back
		// and gives Component.Bounds a real (pre-collapse) size to remember.
		if win.IsMinimized() {
			win.Restore()
		}
		if !sw.maximized {
			sw.preMaximizeBounds = win.Component.Bounds
		}
		sw.maximized = true
		win.Component.SetBounds(area)
	}
	w.persistLayout()
	w.desktop.Redraw()
}
