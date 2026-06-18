// Package ui implements the multi-session terminal UI for gogent on top of the
// turbotui/turbotv Turbo-Vision-style toolkit.
//
// The UI is organised as a Workbench (the desktop, menu bar and background) that
// hosts any number of SessionWindows. Each session is an independent, draggable
// window with its own chat transcript, model selector and status line. The
// transcript uses turbotv's foldable TextView entries so the user can inspect
// (or hide) the agent's intermediate thoughts and the arguments/results of every
// tool call.
package ui

import (
	"context"
	"fmt"
	"gogent/internal/agent"
	"gogent/internal/config"
	"gogent/internal/gogent"
	"gogent/internal/notify"
	"os"
	"strings"
	"sync"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Colours used across the UI.
var (
	colorUser   = tui.ANSIColor(14) // bright cyan
	colorAgent  = tui.ANSIColor(10) // bright green
	colorNote   = tui.ANSIColor(8)  // dim grey (thoughts)
	colorTool   = tui.ANSIColor(11) // bright yellow (tool calls)
	colorResult = tui.ANSIColor(13) // magenta (tool results)
	colorInfo   = tui.ANSIColor(12) // bright blue
	colorError  = tui.ANSIColor(9)  // bright red
)

// Handlers wires the Workbench to the agent backend. All handlers may be nil.
type Handlers struct {
	// OnCreate is invoked (on the UI thread) when a new session window is
	// created, so the backend can build the matching core session and register
	// an observer that forwards events back via Workbench.EmitSessionEvent. The
	// title lets the backend persist a human-friendly session name.
	OnCreate func(sessionID, title string)
	// OnSend processes a user message for a session. It is called on a background
	// goroutine; progress is reported through EmitSessionEvent.
	OnSend func(sessionID, message, modelName string)
	// OnClose tears down the backend session when its window is closed.
	OnClose func(sessionID string)
	// GetSettings returns the current sub-agent execution-model settings so the
	// Settings menu can reflect them. May be nil.
	GetSettings func() config.SubAgentConfig
	// SetSettings applies updated sub-agent execution-model settings. May be nil.
	SetSettings func(config.SubAgentConfig)
	// GetTimeouts / SetTimeouts read and persist the model/tool/sub-agent
	// timeout configuration. May be nil.
	GetTimeouts func() config.TimeoutConfig
	SetTimeouts func(config.TimeoutConfig)
	// GetNotifyConfig / SetNotifyConfig read and persist the desktop/terminal
	// notification configuration (issue #59). SetNotifyConfig also pushes the new
	// config into the workbench's live notifier. May be nil.
	GetNotifyConfig func() config.NotifyConfig
	SetNotifyConfig func(config.NotifyConfig)
	// GetModels returns editable copies of the configured models; UpdateModel
	// persists changes to one model (matched by Name). May be nil.
	GetModels   func() []config.ModelConfig
	UpdateModel func(config.ModelConfig) error
	// ScanModels queries a backend (built from the given draft config's
	// api_type/endpoint/api_key) for the model ids it serves, so the editor can
	// swap the model-id text field for a dropdown. May be nil.
	ScanModels func(config.ModelConfig) ([]string, error)
	// GetTranscript returns a (sub-)agent's message transcript for the monologue
	// popup. May be nil.
	GetTranscript func(sessionID, agentID string) []ChatMessage
	// GetSkills returns the loaded skills with active state and usage stats.
	// SetSkillActive toggles whether a skill is active (offered to the model and
	// listed in the system prompt). Both may be nil.
	GetSkills      func() []SkillInfo
	SetSkillActive func(name string, active bool)
	// Restore returns sessions to re-open at startup (crash/continuation
	// recovery). May be nil.
	Restore func() []RestoredSession
	// LoadLayout returns the persisted workbench layout (sidebar order, titles,
	// pin states and window bounds) to re-apply after sessions are restored. May
	// be nil, in which case the desktop starts with its default arrangement.
	LoadLayout func() gogent.Layout
	// SaveLayout persists the current workbench layout so it survives a restart.
	// Best-effort: the handler should not block the UI on a write failure. May
	// be nil, in which case layout changes are kept only for the current run.
	SaveLayout func(gogent.Layout)
}

// RestoredSession describes a session to be re-opened from persisted state.
type RestoredSession struct {
	ID       string
	Title    string
	Messages []ChatMessage
}

// SkillInfo is a UI-facing view of a loaded skill and its usage stats.
type SkillInfo struct {
	Name        string
	Description string
	Active      bool
	Success     int
	Failure     int
	TotalCalls  int
}

// Workbench is the top-level multi-session TUI.
type Workbench struct {
	app        *tui.App
	desktop    *tv.Desktop
	models     []*config.ModelConfig
	modelNames []string
	mu         sync.Mutex
	sessions   map[string]*SessionWindow
	order      []string
	// pinned records favorite sessions (shown with a ★ marker and floated to the
	// top of the sidebar on pin). Kept as a set so the flag survives reorders.
	pinned   map[string]bool
	nextNum  int
	handlers Handlers
	sidebar  *sidebar
	monolog  *tv.Layer
	// shutdown is cancelled (via quit) when the UI loop stops. Background
	// goroutines blocked on a permission prompt select on it so they unblock
	// instead of leaking when the user quits. See AskPermission.
	shutdown context.Context
	quit     context.CancelFunc
	// promptMu serializes permission prompts so concurrent tool calls present
	// one modal at a time rather than stacking and clobbering focus.
	promptMu     sync.Mutex
	windowConfig config.WindowConfig
	// notify emits desktop/terminal notifications on terminal session events
	// (task complete, error, clarification, approval). It owns the terminal
	// output (os.Stdout); SetNotifyConfig keeps its config in sync with the
	// persisted setting.
	notify *notify.Notifier
}

// NewWorkbench creates the workbench and its desktop chrome.
func NewWorkbench(models []*config.ModelConfig) *Workbench {
	app, err := tui.New()
	if err != nil {
		panic(fmt.Sprintf("failed to initialize TUI: %v", err))
	}
	w := &Workbench{
		app:      app,
		desktop:  tv.NewDesktop(app),
		sessions: make(map[string]*SessionWindow),
		pinned:   make(map[string]bool),
		// Use default window config (resizable and minimizable by default)
		windowConfig: config.WindowConfig{
			Resizable:   true,
			Minimizable: true,
			MinWidth:    50,
			MinHeight:   12,
		},
		// Desktop/terminal notifications write their escape sequences to the same
		// terminal the TUI renders to. Defaults are used until the backend pushes
		// the persisted config in via SetNotifyConfig.
		notify: notify.New(config.DefaultNotifyConfig(), os.Stdout),
	}
	// Cancelled when the UI loop stops; see the shutdown field and Run.
	w.shutdown, w.quit = context.WithCancel(context.Background())
	w.SetModels(models)
	// Background layer: a filled desktop with a hint.
	bg := tv.NewComponent(tv.Rect{X: 0, Y: 0, W: app.Width(), H: app.Height()})
	bg.DrawFn = func(c *tv.VisualComponent, surface tv.Surface) {
		abs := c.AbsoluteBounds()
		surface.Fill(abs, tui.Cell{Ch: ' ', FG: tui.ANSIColor(7), BG: tui.ANSIColor(4)})
		hint := "Gogent - Session > New (Ctrl+N) to start.  Use the >/v markers in a transcript to fold thoughts & tool details."
		if abs.H > 2 {
			surface.WriteString(abs.X+2, abs.Y+abs.H-2, hint, tui.Cell{FG: tui.ANSIColor(15), BG: tui.ANSIColor(4)})
		}
	}
	w.desktop.AddLayer(tv.NewFullscreenLayer("background", bg))
	// Pinned right-hand sidebar showing sessions and their sub-agents.
	w.sidebar = newSidebar(w)
	w.sidebar.reposition(app.Width(), app.Height())
	w.desktop.AddLayer(w.sidebar.layer)
	// Keep the sidebar pinned to the right edge across terminal resizes.
	app.OnResize(func(tui.ResizeEvent) {
		w.sidebar.reposition(w.app.Width(), w.app.Height())
	})
	w.rebuildMenu()
	return w
}

// SetModels updates the list of available models offered in each session window.
func (w *Workbench) SetModels(models []*config.ModelConfig) {
	w.models = models
	w.modelNames = make([]string, len(models))
	for i, m := range models {
		name := m.DisplayName
		if name == "" {
			name = m.Name
		}
		w.modelNames[i] = name
	}
}

// modelByDisplayName returns the model config matching a select label.
func (w *Workbench) modelByDisplayName(name string) *config.ModelConfig {
	for _, m := range w.models {
		display := m.DisplayName
		if display == "" {
			display = m.Name
		}
		if display == name {
			return m
		}
	}
	return nil
}

// SetHandlers registers the backend callbacks.
func (w *Workbench) SetHandlers(h Handlers) {
	w.handlers = h
}

// SetNotifyConfig updates the live notification configuration (and so what the
// next emitted notification respects). The persisted copy is the backend's
// responsibility (the SetNotifyConfig handler); this only updates the in-process
// notifier.
func (w *Workbench) SetNotifyConfig(cfg config.NotifyConfig) {
	if w.notify != nil {
		w.notify.SetConfig(cfg)
	}
}

// rebuildMenu (re)constructs the menu bar, including the dynamic Session list.
func (w *Workbench) rebuildMenu() {
	w.mu.Lock()
	order := append([]string(nil), w.order...)
	titles := make(map[string]string, len(order))
	for _, id := range order {
		if s := w.sessions[id]; s != nil {
			titles[id] = s.title
		}
	}
	pinned := make(map[string]bool, len(w.pinned))
	for id, p := range w.pinned {
		pinned[id] = p
	}
	active := w.activeIDLocked()
	w.mu.Unlock()
	sessionItems := []*tv.MenuItem{
		tv.NewMenuItem("&New Session", func() { w.NewSession() }).
			WithShortcut("Ctrl+N", tui.KeyRune, 'n', true),
		tv.NewMenuItem("Ne&xt Session", func() { w.cycle(1) }).
			WithShortcut("Ctrl+]", tui.KeyRune, ']', true),
		tv.NewMenuItem("&Close Session", func() { w.CloseActive() }).
			WithShortcut("Ctrl+W", tui.KeyRune, 'w', true),
		tv.NewMenuItem("----------", nil),
		tv.NewMenuItem("Close &Others", func() { w.CloseOthers(w.ActiveID()) }),
		tv.NewMenuItem("Close Al&l", func() { w.CloseAll() }),
	}
	// Per-active-session operations (rename / pin / reorder) only make sense when
	// a window is open. They reflect the active session's current pin state.
	if active != "" {
		pinLabel := "&Pin Active"
		if pinned[active] {
			pinLabel = "Un&pin Active"
		}
		sessionItems = append(sessionItems,
			tv.NewMenuItem("----------", nil),
			tv.NewMenuItem("&Rename Active…", func() { w.RenameSession(w.ActiveID()) }),
			tv.NewMenuItem(pinLabel, func() { w.TogglePin(w.ActiveID()) }),
			tv.NewMenuItem("Move Active &Up", func() { w.MoveSession(w.ActiveID(), -1) }),
			tv.NewMenuItem("Move Active &Down", func() { w.MoveSession(w.ActiveID(), 1) }),
		)
	}
	if len(order) > 0 {
		sessionItems = append(sessionItems, tv.NewMenuItem("----------", nil))
		for _, id := range order {
			id := id
			label := titles[id]
			if pinned[id] {
				label = "★ " + label
			}
			sessionItems = append(sessionItems, tv.NewMenuItem(label, func() { w.Focus(id) }))
		}
	}
	bar := tv.NewMenuBar(tv.Rect{X: 0, Y: 0, W: w.app.Width(), H: 1},
		tv.NewSubMenu("&File",
			tv.NewMenuItem("E&xit", func() {
				w.confirmQuit()
			}).WithShortcut("Ctrl+Q", tui.KeyRune, 'q', true),
		),
		tv.NewSubMenu("&Session", sessionItems...),
		tv.NewSubMenu("&Config", w.settingsItems()...),
		tv.NewSubMenu("&Help",
			tv.NewMenuItem("&About", func() {
				tv.ShowConfirmYesNo(w.desktop, "Gogent",
					"Gogent multi-session TUI.\nEach session is its own window; fold thoughts & tool calls with the >/v markers.", nil)
			}),
		),
	)
	w.desktop.SetMenuBar(bar)
	w.desktop.Redraw()
}

// settingsItems builds the Settings submenu. The sub-agent execution-model
// settings live in a modal dialog built from the turbotv Checkbox widgets (see
// showSettingsDialog); the menu also surfaces a quick read-only summary of the
// current configuration so the active mode is visible at a glance.
func (w *Workbench) settingsItems() []*tv.MenuItem {
	if w.handlers.GetSettings == nil || w.handlers.SetSettings == nil {
		return []*tv.MenuItem{tv.NewMenuItem("(settings unavailable)", nil)}
	}
	cur := w.handlers.GetSettings()
	mode := "one-shot"
	if !cur.IsOneShot() {
		mode = "interactive"
	}
	recursive := "off"
	if cur.AllowRecursive {
		recursive = "on"
	}
	items := []*tv.MenuItem{
		tv.NewMenuItem("&Sub-agents…", func() { w.showSettingsDialog() }).
			WithShortcut("Ctrl+,", tui.KeyRune, ',', true),
		tv.NewMenuItem("&Models…", func() { w.showModelEditor() }),
		tv.NewMenuItem("S&kills…", func() { w.showSkillsDialog() }),
		tv.NewMenuItem("----------", nil),
		tv.NewMenuItem("Mode: "+mode, func() { w.showSettingsDialog() }),
		tv.NewMenuItem("Recursive: "+recursive, func() { w.showSettingsDialog() }),
	}
	// Notification settings (issue #59). Surfaced only when the backend wired the
	// accessors; a one-line summary mirrors the sub-agent mode/recursive lines.
	if w.handlers.GetNotifyConfig != nil && w.handlers.SetNotifyConfig != nil {
		nc := w.handlers.GetNotifyConfig()
		state := "off"
		if nc.Enabled {
			state = "on"
		}
		items = append(items,
			tv.NewMenuItem("----------", nil),
			tv.NewMenuItem("&Notifications…", func() { w.showNotificationsDialog() }),
			tv.NewMenuItem("Notifications: "+state, func() { w.showNotificationsDialog() }),
		)
	}
	return items
}
func (w *Workbench) confirmQuit() {
	tv.ShowConfirmYesNo(w.desktop, "Quit Gogent", "Are you sure you want to quit?", func(yes bool) {
		if yes && w.quit != nil {
			w.quit()
		}
	})
}

// NewSession creates a new session window and notifies the backend.
func (w *Workbench) NewSession() *SessionWindow {
	w.mu.Lock()
	w.nextNum++
	num := w.nextNum
	id := fmt.Sprintf("session-%d", num)
	title := fmt.Sprintf("Session %d", num)
	w.mu.Unlock()
	sw := w.openWindow(id, title)
	w.desktop.SetFocus(sw.input)
	if w.handlers.OnCreate != nil {
		w.handlers.OnCreate(id, title)
	}
	w.rebuildMenu()
	w.persistLayout()
	return sw
}

// AdoptSession re-opens a window for a session that already exists in the
// backend (e.g. restored from disk), pre-filling its transcript. OnCreate is
// still invoked so the backend can attach the live event observer; because the
// backend session already exists this does not create a duplicate.
func (w *Workbench) AdoptSession(rs RestoredSession) *SessionWindow {
	w.mu.Lock()
	if n := parseSessionNum(rs.ID); n > w.nextNum {
		w.nextNum = n
	}
	w.mu.Unlock()
	title := rs.Title
	if title == "" {
		title = rs.ID
	}
	sw := w.openWindow(rs.ID, title)
	renderTranscript(sw.history, rs.Messages)
	if w.handlers.OnCreate != nil {
		w.handlers.OnCreate(rs.ID, title)
	}
	w.rebuildMenu()
	return sw
}

// openWindow builds, registers and shows a session window with the given id and
// title. It is shared by NewSession and AdoptSession.
func (w *Workbench) openWindow(id, title string) *SessionWindow {
	w.mu.Lock()
	// Cascade windows so they don't perfectly overlap.
	offset := len(w.order) % 6
	avail := w.app.Width() - sidebarWidth
	if avail < 50 {
		avail = w.app.Width()
	}
	width := avail * 90 / 100
	height := (w.app.Height() - 1) * 85 / 100
	if width < 50 {
		width = avail - 2
	}
	if height < 12 {
		height = w.app.Height() - 2
	}
	x := 2 + offset*3
	y := 2 + offset*1
	sw := newSessionWindow(w, id, title, tv.Rect{X: x, Y: y, W: width, H: height})
	w.sessions[id] = sw
	w.order = append(w.order, id)
	pinned := w.pinned[id]
	w.mu.Unlock()
	w.desktop.AddLayer(sw.layer)
	if w.sidebar != nil {
		w.sidebar.addSession(id, title, pinned)
	}
	return sw
}

// parseSessionNum extracts the trailing number from a "session-N" id (0 if none)
// so restored ids don't collide with newly created ones.
func parseSessionNum(id string) int {
	i := strings.LastIndex(id, "-")
	if i < 0 || i+1 >= len(id) {
		return 0
	}
	n := 0
	for _, r := range id[i+1:] {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// Focus raises a session window to the top and focuses its input.
func (w *Workbench) Focus(id string) {
	w.mu.Lock()
	sw := w.sessions[id]
	w.mu.Unlock()
	if sw == nil {
		return
	}
	// Re-adding the layer moves it to the top of the z-stack.
	w.desktop.RemoveLayer(sw.layer)
	w.desktop.AddLayer(sw.layer)
	w.desktop.SetFocus(sw.input)
	w.desktop.Redraw()
}

// cycle moves focus to the next/previous session window.
func (w *Workbench) cycle(delta int) {
	w.mu.Lock()
	if len(w.order) == 0 {
		w.mu.Unlock()
		return
	}
	top := w.activeIDLocked()
	idx := 0
	for i, id := range w.order {
		if id == top {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(w.order)) % len(w.order)
	next := w.order[idx]
	w.mu.Unlock()
	w.Focus(next)
}

// activeIDLocked returns the id of the currently top-most session window.
// Caller must hold w.mu.
func (w *Workbench) activeIDLocked() string {
	top := w.desktop.TopLayer()
	for id, sw := range w.sessions {
		if sw.layer == top {
			return id
		}
	}
	if len(w.order) > 0 {
		return w.order[len(w.order)-1]
	}
	return ""
}

// ActiveID returns the id of the currently top-most session window, or "" when
// none is open. It is the thread-safe counterpart of activeIDLocked.
func (w *Workbench) ActiveID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.activeIDLocked()
}

// CloseActive closes the top-most session window.
func (w *Workbench) CloseActive() {
	id := w.ActiveID()
	if id != "" {
		w.CloseSession(id)
	}
}

// CloseSession removes a session window and notifies the backend.
func (w *Workbench) CloseSession(id string) {
	w.mu.Lock()
	sw := w.sessions[id]
	if sw == nil {
		w.mu.Unlock()
		return
	}
	delete(w.sessions, id)
	delete(w.pinned, id)
	next := w.order[:0]
	for _, existing := range w.order {
		if existing != id {
			next = append(next, existing)
		}
	}
	w.order = next
	w.mu.Unlock()
	w.desktop.RemoveLayer(sw.layer)
	if w.sidebar != nil {
		w.sidebar.removeSession(id)
	}
	if w.handlers.OnClose != nil {
		w.handlers.OnClose(id)
	}
	w.rebuildMenu()
	w.persistLayout()
	w.mu.Lock()
	last := ""
	if len(w.order) > 0 {
		last = w.order[len(w.order)-1]
	}
	w.mu.Unlock()
	if last != "" {
		w.Focus(last)
	}
}

// RenameSession opens a modal that lets the user edit a session's title. The
// new title is reflected in the window, sidebar and menu. Renaming is a UI
// concern: it never changes the session id or its transcript.
func (w *Workbench) RenameSession(id string) {
	w.mu.Lock()
	sw := w.sessions[id]
	current := ""
	if sw != nil {
		current = sw.title
	}
	w.mu.Unlock()
	if sw == nil {
		return
	}
	w.showInputDialog("Rename Session", "&Title:", current, func(value string, ok bool) {
		if !ok {
			return
		}
		if title := strings.TrimSpace(value); title != "" {
			w.SetSessionTitle(id, title)
		}
	})
}

// SetSessionTitle applies a new title to a session's window, sidebar node and
// menu, then persists the layout. Empty titles are ignored.
func (w *Workbench) SetSessionTitle(id, title string) {
	title = strings.TrimSpace(title)
	if title == "" {
		return
	}
	w.mu.Lock()
	sw := w.sessions[id]
	if sw != nil {
		sw.title = title
		sw.window.Title = title
	}
	w.mu.Unlock()
	if sw == nil {
		return
	}
	if w.sidebar != nil {
		w.sidebar.relabelSession(id, title, w.IsPinned(id))
	}
	w.rebuildMenu()
	w.persistLayout()
}

// IsPinned reports whether a session is marked as a favorite.
func (w *Workbench) IsPinned(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pinned[id]
}

// TogglePin flips a session's favorite state. Pinning also moves it to the front
// of the sidebar so favorites stay on top; unpinning leaves it in place. The
// state is reflected in the sidebar marker and menu, then persisted.
func (w *Workbench) TogglePin(id string) {
	w.mu.Lock()
	if _, ok := w.sessions[id]; !ok {
		w.mu.Unlock()
		return
	}
	nowPinned := !w.pinned[id]
	w.pinned[id] = nowPinned
	if nowPinned {
		w.orderToFrontLocked(id)
	}
	title := ""
	if sw := w.sessions[id]; sw != nil {
		title = sw.title
	}
	order := append([]string(nil), w.order...)
	w.mu.Unlock()
	if w.sidebar != nil {
		w.sidebar.reorder(order)
		w.sidebar.relabelSession(id, title, nowPinned)
	}
	w.rebuildMenu()
	w.persistLayout()
}

// orderToFrontLocked moves id to the front of w.order. Caller must hold w.mu.
func (w *Workbench) orderToFrontLocked(id string) {
	next := make([]string, 0, len(w.order))
	next = append(next, id)
	for _, existing := range w.order {
		if existing != id {
			next = append(next, existing)
		}
	}
	w.order = next
}

// MoveSession reorders a session within the sidebar by delta places (-1 toward
// the top, +1 toward the bottom), clamped to the list bounds, then persists the
// new order.
func (w *Workbench) MoveSession(id string, delta int) {
	if delta == 0 {
		return
	}
	w.mu.Lock()
	if _, ok := w.sessions[id]; !ok {
		w.mu.Unlock()
		return
	}
	idx := -1
	for i, existing := range w.order {
		if existing == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		w.mu.Unlock()
		return
	}
	next := idx + delta
	if next < 0 {
		next = 0
	}
	if next >= len(w.order) {
		next = len(w.order) - 1
	}
	if next == idx {
		w.mu.Unlock()
		return
	}
	w.order = append(w.order[:idx], w.order[idx+1:]...)
	w.order = append(w.order[:next], append([]string{id}, w.order[next:]...)...)
	order := append([]string(nil), w.order...)
	w.mu.Unlock()
	if w.sidebar != nil {
		w.sidebar.reorder(order)
	}
	w.rebuildMenu()
	w.persistLayout()
}

// CloseOthers closes every open session except id.
func (w *Workbench) CloseOthers(id string) {
	w.mu.Lock()
	others := make([]string, 0, len(w.order))
	for _, existing := range w.order {
		if existing != id {
			others = append(others, existing)
		}
	}
	w.mu.Unlock()
	for _, other := range others {
		w.CloseSession(other)
	}
}

// CloseAll closes every open session, then opens a single fresh window so the
// user always has somewhere to type.
func (w *Workbench) CloseAll() {
	w.mu.Lock()
	all := append([]string(nil), w.order...)
	w.mu.Unlock()
	for _, id := range all {
		w.CloseSession(id)
	}
	if len(all) > 0 {
		w.NewSession()
	}
}

// captureLayout snapshots the current sidebar order together with each session's
// title, pin state and window bounds/minimized flag into a Layout.
func (w *Workbench) captureLayout() gogent.Layout {
	w.mu.Lock()
	defer w.mu.Unlock()
	layout := gogent.Layout{Entries: make([]gogent.LayoutEntry, 0, len(w.order))}
	for _, id := range w.order {
		sw := w.sessions[id]
		if sw == nil {
			continue
		}
		bounds := sw.window.Component.Bounds
		layout.Entries = append(layout.Entries, gogent.LayoutEntry{
			ID:        id,
			Title:     sw.title,
			Pinned:    w.pinned[id],
			Minimized: sw.window.IsMinimized(),
			X:         bounds.X,
			Y:         bounds.Y,
			W:         bounds.W,
			H:         bounds.H,
		})
	}
	return layout
}

// persistLayout captures the current layout and writes it via the SaveLayout
// handler. It is best-effort and a no-op when the handler is unset, so layout
// changes are kept only for the current run when persistence is unavailable.
func (w *Workbench) persistLayout() {
	if w.handlers.SaveLayout == nil {
		return
	}
	w.handlers.SaveLayout(w.captureLayout())
}

// applyLayout restores titles, pin states and window bounds from a persisted
// layout onto already-open windows. Entries for sessions that no longer exist
// are ignored; the next persistLayout drops them (the layout is self-healing).
func (w *Workbench) applyLayout(layout gogent.Layout) {
	w.mu.Lock()
	if len(layout.Entries) == 0 {
		w.mu.Unlock()
		return
	}
	screenW, screenH := w.app.Width(), w.app.Height()
	// relabel captures each applied session's final title + pin so the sidebar
	// refresh runs without re-reading workbench state outside the lock.
	type relabel struct {
		id, title string
		pinned    bool
	}
	var relabels []relabel
	for _, e := range layout.Entries {
		sw := w.sessions[e.ID]
		if sw == nil {
			continue
		}
		if e.Title != "" {
			sw.title = e.Title
			sw.window.Title = e.Title
		}
		w.pinned[e.ID] = e.Pinned
		bounds := clampWindowRect(tv.Rect{X: e.X, Y: e.Y, W: e.W, H: e.H},
			screenW, screenH, sw.window.MinWidth, sw.window.MinHeight)
		sw.window.Component.SetBounds(bounds)
		if e.Minimized && !sw.window.IsMinimized() {
			sw.window.Minimize()
		}
		relabels = append(relabels, relabel{e.ID, sw.title, e.Pinned})
	}
	w.mu.Unlock()
	if w.sidebar != nil {
		for _, r := range relabels {
			w.sidebar.relabelSession(r.id, r.title, r.pinned)
		}
	}
	w.rebuildMenu()
}

// clampWindowRect keeps a restored window on-screen and at least minW×minH after
// a possible terminal resize since the layout was saved, so a layout from a
// larger terminal can't strand a window off-screen.
func clampWindowRect(r tv.Rect, screenW, screenH, minW, minH int) tv.Rect {
	if minW < 1 {
		minW = 1
	}
	if minH < 1 {
		minH = 1
	}
	if r.W < minW {
		r.W = minW
	}
	if r.H < minH {
		r.H = minH
	}
	if r.W > screenW && screenW > 0 {
		r.W = screenW
	}
	if r.H > screenH && screenH > 0 {
		r.H = screenH
	}
	if r.X < 0 {
		r.X = 0
	}
	if r.Y < 0 {
		r.Y = 0
	}
	if screenW > 0 && r.X+r.W > screenW {
		r.X = screenW - r.W
	}
	if screenH > 0 && r.Y+r.H > screenH {
		r.Y = screenH - r.H
	}
	if r.X < 0 {
		r.X = 0
	}
	if r.Y < 0 {
		r.Y = 0
	}
	return r
}

// orderByLayout returns the restored sessions reordered so that those recorded
// in the layout come first (in layout order), with any unknown sessions appended
// in their original order. This re-establishes the sidebar arrangement the user
// left behind without losing sessions added out-of-band.
func orderByLayout(restored []RestoredSession, layout gogent.Layout) []RestoredSession {
	if len(layout.Entries) == 0 || len(restored) <= 1 {
		return restored
	}
	byID := make(map[string]RestoredSession, len(restored))
	for _, rs := range restored {
		byID[rs.ID] = rs
	}
	seen := make(map[string]bool, len(restored))
	out := make([]RestoredSession, 0, len(restored))
	for _, e := range layout.Entries {
		if rs, ok := byID[e.ID]; ok && !seen[e.ID] {
			out = append(out, rs)
			seen[e.ID] = true
		}
	}
	for _, rs := range restored {
		if !seen[rs.ID] {
			out = append(out, rs)
		}
	}
	return out
}

// EmitSessionEvent forwards a core session event to the matching window. It is
// safe to call from any goroutine: the update is marshalled onto the UI thread.
func (w *Workbench) EmitSessionEvent(id string, ev agent.SessionEvent) {
	w.desktop.Post(func() {
		w.mu.Lock()
		sw := w.sessions[id]
		w.mu.Unlock()
		if ev.Type == agent.SessionEventSubAgent && w.sidebar != nil {
			w.sidebar.applySubAgent(id, ev)
		}
		if sw != nil {
			sw.apply(ev)
		}
		w.maybeNotify(id, ev)
	})
}

// eventNotification maps a session event to a notification reason and body. It
// returns ok=false for events that are not notification-worthy: non-terminal
// events, an error with no underlying error, or a sub-agent event that is not a
// "waiting for clarification" state. Pure so it can be unit tested.
func eventNotification(ev agent.SessionEvent) (reason notify.Reason, title, body string, ok bool) {
	switch ev.Type {
	case agent.SessionEventFinal:
		return notify.ReasonComplete, "Task complete", firstLine(ev.Text), true
	case agent.SessionEventError:
		if ev.Err == nil {
			return "", "", "", false
		}
		return notify.ReasonError, "Task error", firstLine(ev.Err.Error()), true
	case agent.SessionEventSubAgent:
		// A sub-agent in the "waiting" status has asked a CLARIFY question.
		if ev.Status != agent.StatusWaiting {
			return "", "", "", false
		}
		body := firstLine(ev.Result)
		if body == "" {
			body = firstLine(ev.Text)
		}
		return notify.ReasonClarify, "Clarification needed", body, true
	}
	return "", "", "", false
}

// maybeNotify fires a desktop/terminal notification for terminal session events
// (task complete, error, sub-agent clarification), gated by the notification
// config and the focused-session suppression. It runs on the UI thread (called
// from EmitSessionEvent) so the focus check is accurate.
func (w *Workbench) maybeNotify(id string, ev agent.SessionEvent) {
	if w.notify == nil {
		return
	}
	reason, title, body, ok := eventNotification(ev)
	if !ok {
		return
	}
	focused := w.ActiveID() == id
	if w.notify.ShouldNotify(reason, focused) {
		w.notify.Notify(title, body)
	}
}

// QuitFunc returns a function that requests the UI to shut down.
func (w *Workbench) QuitFunc() func() {
	return func() {
		if w.quit != nil {
			w.quit()
		}
	}
}

// Run starts the event loop. It blocks until the user quits.
func (w *Workbench) Run() error {
	// Ensure the shutdown context is cancelled however Run returns, so any
	// goroutine still blocked on a permission prompt unblocks (DecisionDeny)
	// instead of leaking once the event loop is gone.
	defer w.quit()
	// Persist the final desktop arrangement (including window moves/resizes the
	// user made this session) when the loop stops, so it is restored next launch.
	defer w.persistLayout()
	// Re-open any persisted sessions (crash recovery / continuation), then
	// re-apply the saved workbench layout (titles, pin/order, window bounds).
	if w.handlers.Restore != nil {
		restored := w.handlers.Restore()
		var layout gogent.Layout
		if w.handlers.LoadLayout != nil {
			layout = w.handlers.LoadLayout()
		}
		for _, rs := range orderByLayout(restored, layout) {
			w.AdoptSession(rs)
		}
		w.applyLayout(layout)
	}
	// Open an initial session so the user has somewhere to type.
	if len(w.order) == 0 {
		w.NewSession()
	}
	w.desktop.SetUnhandledKeyFn(func(event tui.TypeEvent) {
		if event.Key == tui.KeyRune && (event.Rune == 'c' || event.Rune == 'C') && event.Ctrl {
			w.confirmQuit()
		}
	})
	err := w.desktop.Run(w.shutdown)
	w.app.Close()
	return err
}

// childLines splits text into lines for foldable children, preserving structure.
func childLines(text string) []string {
	raw := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(raw) == 0 {
		return []string{""}
	}
	return raw
}

// notifyBodyMaxRunes caps a notification body so a long final answer or error
// does not produce an enormous popup; the first line is the useful signal.
const notifyBodyMaxRunes = 120

// firstLine returns the trimmed first line of s, capped at notifyBodyMaxRunes
// runes, for use as a one-line notification body.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return truncateRunes(s, notifyBodyMaxRunes)
}
