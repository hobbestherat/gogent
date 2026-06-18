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
	nextNum    int
	handlers   Handlers
	sidebar    *sidebar
	monolog    *tv.Layer
	quit       context.CancelFunc
	windowConfig config.WindowConfig
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
		// Use default window config (resizable and minimizable by default)
		windowConfig: config.WindowConfig{
			Resizable:   true,
			Minimizable: true,
			MinWidth:    50,
			MinHeight:   12,
		},
	}
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
	w.mu.Unlock()
	sessionItems := []*tv.MenuItem{
		tv.NewMenuItem("&New Session", func() { w.NewSession() }).
			WithShortcut("Ctrl+N", tui.KeyRune, 'n', true),
		tv.NewMenuItem("Ne&xt Session", func() { w.cycle(1) }).
			WithShortcut("Ctrl+]", tui.KeyRune, ']', true),
		tv.NewMenuItem("&Close Session", func() { w.CloseActive() }).
			WithShortcut("Ctrl+W", tui.KeyRune, 'w', true),
	}
	if len(order) > 0 {
		sessionItems = append(sessionItems, tv.NewMenuItem("----------", nil))
		for _, id := range order {
			id := id
			sessionItems = append(sessionItems, tv.NewMenuItem(titles[id], func() { w.Focus(id) }))
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
	return []*tv.MenuItem{
		tv.NewMenuItem("&Sub-agents…", func() { w.showSettingsDialog() }).
			WithShortcut("Ctrl+,", tui.KeyRune, ',', true),
		tv.NewMenuItem("&Models…", func() { w.showModelEditor() }),
		tv.NewMenuItem("S&kills…", func() { w.showSkillsDialog() }),
		tv.NewMenuItem("----------", nil),
		tv.NewMenuItem("Mode: "+mode, func() { w.showSettingsDialog() }),
		tv.NewMenuItem("Recursive: "+recursive, func() { w.showSettingsDialog() }),
	}
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
	w.mu.Unlock()
	w.desktop.AddLayer(sw.layer)
	if w.sidebar != nil {
		w.sidebar.addSession(id, title)
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

// CloseActive closes the top-most session window.
func (w *Workbench) CloseActive() {
	w.mu.Lock()
	id := w.activeIDLocked()
	w.mu.Unlock()
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
	})
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
	ctx, cancel := context.WithCancel(context.Background())
	w.quit = cancel
	// Re-open any persisted sessions (crash recovery / continuation).
	if w.handlers.Restore != nil {
		for _, rs := range w.handlers.Restore() {
			w.AdoptSession(rs)
		}
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
	err := w.desktop.Run(ctx)
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
