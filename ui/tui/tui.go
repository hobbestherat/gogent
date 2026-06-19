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
	"gogent/internal/clipboard"
	"gogent/internal/config"
	"gogent/internal/gogent"
	"gogent/internal/notify"
	"gogent/internal/stats"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// The UI's colour palette lives in theme.go (the colorXxx and chromeXxx
// variables), loaded from config and installed via ApplyTheme.

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
	// GetBudget / SetBudget read and persist the per-session token-budget
	// configuration that drives the status-bar budget alert (issue #63).
	// SetBudget also pushes the new config into the workbench so the next status
	// refresh reflects it. May be nil.
	GetBudget func() config.BudgetConfig
	SetBudget func(config.BudgetConfig)
	// GetReviewEdits / SetReviewEdits read and persist whether write/edit
	// operations are gated behind the interactive diff-review approval (issue
	// #64). May be nil.
	GetReviewEdits func() bool
	SetReviewEdits func(bool)
	// GetTheme / SetTheme read and persist the TUI colour palette (issue #103).
	// SetTheme also re-applies the resolved palette to the live UI so the change
	// takes effect without a restart. May be nil, in which case the Theme editor
	// is hidden.
	GetTheme func() config.ThemeConfig
	SetTheme func(config.ThemeConfig)
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
	// GetTools returns the registered tools with their enabled state and usage
	// stats so the Resources browser can list, inspect and toggle them.
	// SetToolEnabled toggles whether a tool is advertised to the model and
	// executable. Both may be nil.
	GetTools       func() []ToolInfo
	SetToolEnabled func(name string, enabled bool)
	// GetStatistics returns the detailed statistics report for the Statistics
	// view (issue #57): per-session, per-tool, per-skill and per-model
	// breakdowns of the counters gogent already collects. May be nil.
	GetStatistics func() stats.Report
	// Restore returns sessions to re-open at startup (crash/continuation
	// recovery). May be nil.
	Restore func() []RestoredSession
	// ListSavedSessions returns index-only metadata for every persisted session
	// for the Sessions browser (issue #58). It must not replay transcripts. May
	// be nil, in which case the browser is hidden.
	ListSavedSessions func() []SessionMeta
	// OpenSavedSession loads one persisted session by its index file path
	// (SessionMeta.File) for the Sessions browser (issue #58). When continue_
	// is true the backend adopts it so subsequent sends append; otherwise it is
	// loaded read-only for analysis. It returns the session and ok=false when it
	// could not be loaded. May be nil.
	OpenSavedSession func(file string, continueSession bool) (RestoredSession, bool)
	// LoadLayout returns the persisted workbench layout (sidebar order, titles,
	// pin states and window bounds) to re-apply after sessions are restored. May
	// be nil, in which case the desktop starts with its default arrangement.
	LoadLayout func() gogent.Layout
	// SaveLayout persists the current workbench layout so it survives a restart.
	// Best-effort: the handler should not block the UI on a write failure. May
	// be nil, in which case layout changes are kept only for the current run.
	SaveLayout func(gogent.Layout)
	// ListWorkspaceFiles returns workspace-relative file paths offered by the
	// @-file mention completer (issue #46): typing "@" in a session's input lists
	// these so a file can be attached to the message for precise, cheap context
	// steering. May be nil, in which case the completer never finds candidates.
	ListWorkspaceFiles func() []string
	// ReadWorkspaceFile returns the contents of a workspace file so an @-mention is
	// expanded into attached context in the message sent to the model (issue #46),
	// with ok=false when the path cannot be read (a typo, a directory, outside the
	// workspace). May be nil, in which case @-mentions are sent verbatim for the
	// model to read themselves.
	ReadWorkspaceFile func(path string) (content string, ok bool)
	// OnUndo reverts the last turn's file mutations for a session (issue #41),
	// returning a human-readable summary. May be nil (the /undo command then
	// reports the feature as unavailable).
	OnUndo func(sessionID string) (summary string, err error)
	// OnRewind reverts the last n turns (n <= 0 reverts all) for a session (issue
	// #41), returning a human-readable summary. May be nil.
	OnRewind func(sessionID string, turns int) (summary string, err error)
}

// RestoredSession describes a session to be re-opened from persisted state.
type RestoredSession struct {
	ID       string
	Title    string
	Messages []ChatMessage
}

// SessionMeta is the UI-facing, index-only view of one persisted session for the
// Sessions browser (issue #58): enough to list, search and pick a session
// without loading its transcript. File is the index path the browser hands back
// to OpenSavedSession to open or continue the session.
type SessionMeta struct {
	ID        string
	Title     string
	CreatedAt string
	Turns     int
	Messages  int
	TokensIn  int
	TokensOut int
	Model     string
	File      string
}

// SkillInfo is a UI-facing view of a loaded skill and its usage stats.
type SkillInfo struct {
	Name        string
	Description string
	Active      bool
	Success     int
	Failure     int
	TotalCalls  int
	// Content holds the raw SKILL.md text for the Resources browser preview.
	Content string
	// Path is the on-disk SKILL.md location, shown in the Resources browser detail.
	Path string
}

// ToolInfo is a UI-facing view of a registered tool and its usage stats. The
// input schema is pre-serialized to indented JSON by the backend so the UI does
// not depend on the tool package.
type ToolInfo struct {
	Name        string
	Description string
	InputSchema string
	Enabled     bool
	Invocations int
	LastUsed    string // human-readable, empty when never used
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
	pinned  map[string]bool
	nextNum int
	// nextAnalysis assigns unique ids to read-only analysis windows opened from
	// the Sessions browser (issue #58), kept separate from nextNum so synthetic
	// "analysis-N" ids never collide with backend "session-N" ids.
	nextAnalysis int
	handlers     Handlers
	sidebar      *sidebar
	// sidebarPinned reserves the right-hand sidebar strip as a hard window
	// boundary when true (the default): windows are dragged, resized, maximized,
	// created and restored within the area left of the "Sessions & Agents" panel
	// so it is never covered (issue #106). Toggling it off restores free,
	// overlapping windows. Read/written on the UI thread, like the sidebar's own
	// approval state, so the geometry helpers below read it without the lock.
	sidebarPinned bool
	monolog       *tv.Layer
	// shutdown is cancelled (via quit) when the UI loop stops. Background
	// goroutines blocked on a permission prompt select on it so they unblock
	// instead of leaking when the user quits. See AskPermission.
	shutdown context.Context
	quit     context.CancelFunc
	// promptMu serializes permission prompts so concurrent tool calls present
	// one modal at a time rather than stacking and clobbering focus.
	promptMu sync.Mutex
	// approvals counts in-flight permission prompts per session id so the
	// requesting session's sidebar node shows a "needs approval" badge and the
	// sidebar header shows a global indicator while any prompt is waiting (issue
	// #55). Guarded by w.mu; the badge refresh is marshalled onto the UI thread.
	approvals    map[string]int
	windowConfig config.WindowConfig
	// budget holds the per-session token-budget configuration used by every
	// session window's status line for budget alerting (issue #63). It is an
	// atomic.Value (storing config.BudgetConfig) so the read path refreshStatus
	// takes on the UI thread — including from within a window's LayoutFn, which
	// can run while the workbench lock is held (applyLayout) — never contends on
	// w.mu and so cannot self-deadlock.
	budget atomic.Value
	// notify emits desktop/terminal notifications on terminal session events
	// (task complete, error, clarification, approval). It owns the terminal
	// output (os.Stdout); SetNotifyConfig keeps its config in sync with the
	// persisted setting.
	notify *notify.Notifier
	// clipboard copies yanked text to the system clipboard (OSC 52 plus a native
	// fallback), writing the OSC sequence to os.Stdout like the notifier (issue
	// #62).
	clipboard *clipboard.Board
	// statsRefresh coalesces Overall-panel recomputations: a burst of session
	// events arms it once and rapid follow-ups Reset it, so the panel refreshes
	// at most ~250ms after the burst settles instead of once per event (issues
	// #22 / #53). Lazily created and only touched on the UI thread; its AfterFunc
	// goroutine just Posts the refresh back to the UI thread.
	statsRefresh *time.Timer
}

// NewWorkbench creates the workbench and its desktop chrome.
func NewWorkbench(models []*config.ModelConfig) *Workbench {
	app, err := tui.New()
	if err != nil {
		panic(fmt.Sprintf("failed to initialize TUI: %v", err))
	}
	w := &Workbench{
		app:           app,
		desktop:       tv.NewDesktop(app),
		sessions:      make(map[string]*SessionWindow),
		pinned:        make(map[string]bool),
		sidebarPinned: true,
		// Use default window config (resizable, minimizable and maximizable by default)
		windowConfig: config.WindowConfig{
			Resizable:   true,
			Minimizable: true,
			Maximizable: true,
			MinWidth:    50,
			MinHeight:   12,
		},
		// Desktop/terminal notifications write their escape sequences to the same
		// terminal the TUI renders to. Defaults are used until the backend pushes
		// the persisted config in via SetNotifyConfig.
		notify: notify.New(config.DefaultNotifyConfig(), os.Stdout),
		// Clipboard writes OSC 52 to the same terminal (SSH-safe) and pipes to a
		// native utility when one is available.
		clipboard: clipboard.New(os.Stdout),
	}
	// Cancelled when the UI loop stops; see the shutdown field and Run.
	w.shutdown, w.quit = context.WithCancel(context.Background())
	w.SetModels(models)
	// Background layer: a filled desktop with a hint.
	bg := tv.NewComponent(tv.Rect{X: 0, Y: 0, W: app.Width(), H: app.Height()})
	bg.DrawFn = func(c *tv.VisualComponent, surface tv.Surface) {
		abs := c.AbsoluteBounds()
		surface.Fill(abs, tui.Cell{Ch: ' ', FG: chromeDesktopFG, BG: chromeDesktopBG})
		hint := "Gogent - Session > New (Ctrl+N) to start.  Use the >/v markers in a transcript to fold thoughts & tool details."
		if abs.H > 2 {
			surface.WriteString(abs.X+2, abs.Y+abs.H-2, hint, tui.Cell{FG: chromeTitle, BG: chromeDesktopBG})
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

// longestModelNameWidth returns the display width (in cells) of the widest model
// name offered in the window-header selector, used to size that dropdown so the
// active name is not truncated (issue #108).
func (w *Workbench) longestModelNameWidth() int {
	max := 0
	for _, n := range w.modelNames {
		if l := runeLen(n); l > max {
			max = l
		}
	}
	return max
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

// SetBudgetConfig updates the live token-budget configuration used by the
// session status lines for budget alerting (issue #63). The persisted copy is the
// backend's responsibility (the SetBudget handler); this only updates the
// in-process value read by refreshStatus.
func (w *Workbench) SetBudgetConfig(b config.BudgetConfig) {
	w.budget.Store(b)
}

// budgetConfig returns the current token-budget configuration, or the zero value
// (alerting off) before the first SetBudgetConfig. Lock-free so it is safe to
// call from within a window's LayoutFn while the workbench lock is held.
func (w *Workbench) budgetConfig() config.BudgetConfig {
	if v := w.budget.Load(); v != nil {
		return v.(config.BudgetConfig)
	}
	return config.BudgetConfig{}
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
	// A read-only analysis window (issue #58) has no backend session, so the
	// per-active operations (rename/pin/reorder/export) do not apply to it.
	activeRO := active != "" && w.sessions[active] != nil && w.sessions[active].readOnly
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
	// The Sessions browser (issue #58) is surfaced only when the backend wires
	// the listing handler; otherwise the item would lead nowhere.
	if w.handlers.ListSavedSessions != nil {
		sessionItems = append(sessionItems,
			tv.NewMenuItem("----------", nil),
			tv.NewMenuItem("Saved &Sessions…", func() { w.showSessionsDialog() }),
		)
	}
	// Per-active-session operations (rename / pin / reorder) only make sense when
	// a live window is open. They reflect the active session's current pin state.
	if active != "" && !activeRO {
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
			tv.NewMenuItem("----------", nil),
			tv.NewMenuItem("Export &Markdown…", func() { w.exportActive("md") }),
			tv.NewMenuItem("Export &JSON…", func() { w.exportActive("json") }),
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
		tv.NewSubMenu("&View", w.viewItems()...),
		tv.NewSubMenu("&Config", w.settingsItems()...),
		tv.NewSubMenu("&Help",
			tv.NewMenuItem("Command &Palette…", func() { w.showCommandPalette() }).
				WithShortcut("Ctrl+K", tui.KeyRune, 'k', true),
			tv.NewMenuItem("&Keybindings (?)…", func() { w.showHelpOverlay() }),
			tv.NewMenuItem("----------", nil),
			tv.NewMenuItem("&About", func() {
				w.showConfirm("Gogent",
					"Gogent multi-session TUI.\nEach session is its own window; fold thoughts & tool calls with the >/v markers.\nPress ? for the keybinding cheatsheet or Ctrl+K for the command palette.", nil)
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
		tv.NewMenuItem("&Resources…", func() { w.showResourcesDialog() }),
	}
	// Statistics is surfaced only when the backend wires the report handler.
	if w.handlers.GetStatistics != nil {
		items = append(items, tv.NewMenuItem("S&tatistics…", func() { w.showStatisticsDialog() }))
	}
	items = append(items,
		tv.NewMenuItem("----------", nil),
		tv.NewMenuItem("Mode: "+mode, func() { w.showSettingsDialog() }),
		tv.NewMenuItem("Recursive: "+recursive, func() { w.showSettingsDialog() }),
	)
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
	// Theme editor (issue #103). Surfaced only when the backend wired the
	// accessors so the palette can be edited and persisted live.
	if w.handlers.GetTheme != nil && w.handlers.SetTheme != nil {
		items = append(items,
			tv.NewMenuItem("----------", nil),
			tv.NewMenuItem("T&heme…", func() { w.showThemeEditor() }),
		)
	}
	return items
}

// RefreshTheme re-applies the active palette to the long-lived UI chrome after a
// theme change: the desktop background, sidebar and menu bar all read the
// package-level colour variables at draw time, so rebuilding the menu and
// repainting is enough to recolour them without a restart (issue #103).
func (w *Workbench) RefreshTheme() {
	w.rebuildMenu() // also repaints the desktop
}

// viewItems builds the View submenu: find-in-transcript, the event-type filter
// toggles, fold/unfold and yank-to-clipboard — all acting on the active
// session's transcript — plus the sidebar pin toggle. The transcript operations
// are also available from the keyboard while the transcript is focused ('/',
// a/t/r/e, f/u, y, Esc); the menu makes them discoverable.
func (w *Workbench) viewItems() []*tv.MenuItem {
	pinLabel := "Pin &Sidebar"
	if w.IsSidebarPinned() {
		pinLabel = "Unpin &Sidebar"
	}
	return []*tv.MenuItem{
		tv.NewMenuItem("&Find…", func() { w.withActiveTranscript((*SessionWindow).promptFind) }).
			WithShortcut("Ctrl+F", tui.KeyRune, 'f', true),
		tv.NewMenuItem("Show &All", func() { w.transcriptDo(func(m *transcriptModel) { m.showAll() }) }),
		tv.NewMenuItem("----------", nil),
		tv.NewMenuItem("Toggle &Messages", func() { w.transcriptDo(func(m *transcriptModel) { m.toggleKind(kindAssistant) }) }),
		tv.NewMenuItem("Toggle &Tool Calls", func() { w.transcriptDo(func(m *transcriptModel) { m.toggleKind(kindTool) }) }),
		tv.NewMenuItem("Toggle Thi&nking", func() { w.transcriptDo(func(m *transcriptModel) { m.toggleKind(kindThinking) }) }),
		tv.NewMenuItem("Toggle &Errors", func() { w.transcriptDo(func(m *transcriptModel) { m.toggleKind(kindError) }) }),
		tv.NewMenuItem("----------", nil),
		tv.NewMenuItem("F&old All", func() { w.transcriptDo(func(m *transcriptModel) { m.setFold(true) }) }),
		tv.NewMenuItem("&Unfold All", func() { w.transcriptDo(func(m *transcriptModel) { m.setFold(false) }) }),
		tv.NewMenuItem("----------", nil),
		tv.NewMenuItem("Cop&y Last Answer", func() {
			w.withActiveTranscript((*SessionWindow).copyLastAnswer)
		}),
		tv.NewMenuItem("Copy Last &Code Block", func() {
			w.withActiveTranscript((*SessionWindow).copyLastCode)
		}),
		tv.NewMenuItem("----------", nil),
		tv.NewMenuItem(pinLabel, func() { w.ToggleSidebarPin() }),
	}
}

// withActiveTranscript runs fn against the currently active session window, if
// any. It is the shared lookup behind the View menu actions.
func (w *Workbench) withActiveTranscript(fn func(*SessionWindow)) {
	id := w.ActiveID()
	if id == "" {
		return
	}
	w.mu.Lock()
	sw := w.sessions[id]
	w.mu.Unlock()
	if sw != nil {
		fn(sw)
	}
}

// transcriptDo applies a transcript-model mutation to the active session and
// repaints.
func (w *Workbench) transcriptDo(fn func(*transcriptModel)) {
	w.withActiveTranscript(func(sw *SessionWindow) {
		fn(sw.transcript)
		w.desktop.Redraw()
	})
}

// copyToClipboard writes text to the system clipboard via the workbench's board
// (OSC 52 plus a native utility fallback). No-op when no board is configured.
func (w *Workbench) copyToClipboard(text string) {
	if w.clipboard != nil {
		w.clipboard.Copy(text)
	}
}

// sessionTitle returns a session's window title, or "" when it is unknown.
func (w *Workbench) sessionTitle(id string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if sw := w.sessions[id]; sw != nil {
		return sw.title
	}
	return ""
}

// exportActive renders the active session's transcript in the given format
// ("md" or "json") and writes it to ~/.gogent, then confirms with the path (or an
// error). It reads the same backend transcript data the restored-session and
// monologue views use, so the export reflects the full conversation rather than
// the capped live view (issue #62).
func (w *Workbench) exportActive(format string) {
	if w.handlers.GetTranscript == nil {
		w.showConfirm("Export Session", "Export is unavailable.", nil)
		return
	}
	id := w.ActiveID()
	if id == "" {
		return
	}
	msgs := w.handlers.GetTranscript(id, "root")
	path, err := writeTranscriptExport(msgs, w.sessionTitle(id), format)
	label := "Markdown"
	if format == "json" {
		label = "JSON"
	}
	msg := "Wrote " + label + " to:\n" + path
	if err != nil {
		msg = label + " export failed:\n" + err.Error()
	}
	w.showConfirm("Export Session", msg, nil)
}

func (w *Workbench) confirmQuit() {
	w.showConfirm("Quit Gogent", "Are you sure you want to quit?", func(yes bool) {
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
	sw.restore(rs.Messages)
	if w.handlers.OnCreate != nil {
		w.handlers.OnCreate(rs.ID, title)
	}
	w.rebuildMenu()
	return sw
}

// OpenAnalysisSession opens a saved transcript in a read-only analysis window
// (issue #58): it renders the conversation with the full search/filter/fold/yank
// toolkit but has no input or live backend session, so several can sit open
// side-by-side for comparison. It is fed by the Sessions browser's read-only
// load and uses a synthetic "analysis-N" id that never collides with a backend
// session.
func (w *Workbench) OpenAnalysisSession(rs RestoredSession) *SessionWindow {
	w.mu.Lock()
	w.nextAnalysis++
	id := fmt.Sprintf("analysis-%d", w.nextAnalysis)
	w.mu.Unlock()
	title := rs.Title
	if title == "" {
		title = rs.ID
	}
	sw := w.openWindowAny(id, title, true)
	sw.restore(rs.Messages)
	w.rebuildMenu()
	return sw
}

// openWindow builds, registers and shows a live session window with the given id
// and title. It is the entry point shared by NewSession and AdoptSession.
func (w *Workbench) openWindow(id, title string) *SessionWindow {
	return w.openWindowAny(id, title, false)
}

// openWindowAny is the core window builder shared by live sessions (readOnly
// false) and the read-only analysis windows (readOnly true, issue #58) opened
// from the Sessions browser.
func (w *Workbench) openWindowAny(id, title string, readOnly bool) *SessionWindow {
	w.mu.Lock()
	// Cascade windows so they don't perfectly overlap. New windows open in the
	// area left of the sidebar (with a fallback to the full width on a terminal
	// too narrow to spare it); dragging/resizing/maximizing then keep them there
	// via the pinned window area (issue #106).
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
	sw := newSessionWindow(w, id, title, tv.Rect{X: x, Y: y, W: width, H: height}, readOnly)
	w.sessions[id] = sw
	w.order = append(w.order, id)
	pinned := w.pinned[id]
	w.mu.Unlock()
	w.desktop.AddLayer(sw.layer)
	if w.sidebar != nil {
		w.sidebar.addSession(id, title, pinned)
	}
	// The Overall panel's session count changed; refresh it so the count is right
	// on the immediate repaint rather than the next coalesced event refresh.
	w.refreshOverall()
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
	// The Overall panel's "model"/"api" rows follow the focused session (issue
	// #107); refresh before the redraw below so the new model shows immediately.
	w.refreshOverall()
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
	// Dismiss a still-open @-mention popup so closing mid-completion leaves no
	// orphaned layer (issue #46).
	if sw.completer != nil {
		sw.completer.hide()
	}
	w.desktop.RemoveLayer(sw.layer)
	if w.sidebar != nil {
		w.sidebar.removeSession(id)
	}
	// The Overall panel's session/sub-agent counts changed; refresh before the
	// repaint below so the closed session's figures drop immediately.
	w.refreshOverall()
	// A read-only analysis window (issue #58) has no live backend session, so
	// closing it tears down nothing on the backend and persists no layout.
	if !sw.readOnly {
		if w.handlers.OnClose != nil {
			w.handlers.OnClose(id)
		}
		w.persistLayout()
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

// IsSidebarPinned reports whether the "Sessions & Agents" sidebar boundary is
// enforced (windows kept left of it). It is on by default (issue #106).
func (w *Workbench) IsSidebarPinned() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sidebarPinned
}

// windowArea returns the desktop rectangle session windows are constrained to:
// the full screen when the sidebar is unpinned, or the screen minus the reserved
// right-hand sidebar strip when it is pinned (issue #106). Dragging, resizing,
// maximizing, creating and restoring a window all stay within it so a pinned
// sidebar is never covered. On a desktop too narrow to spare the sidebar the full
// width is used (mirroring maximizedWindowRect). Read on the UI thread.
func (w *Workbench) windowArea() tv.Rect {
	sw, sh := w.app.Width(), w.app.Height()
	if w.sidebarPinned && sw > sidebarWidth {
		sw -= sidebarWidth
	}
	return tv.Rect{X: 0, Y: 0, W: sw, H: sh}
}

// ToggleSidebarPin flips whether the sidebar boundary constrains windows (issue
// #106). When pinning, every open window is clamped into the now-reserved area so
// none is left covering the sidebar; unpinning leaves windows where they are and
// restores free dragging. The View menu label and command palette reflect the new
// state via rebuildMenu.
func (w *Workbench) ToggleSidebarPin() {
	w.mu.Lock()
	w.sidebarPinned = !w.sidebarPinned
	pinned := w.sidebarPinned
	windows := make([]*SessionWindow, 0, len(w.sessions))
	for _, sw := range w.sessions {
		windows = append(windows, sw)
	}
	w.mu.Unlock()
	if pinned {
		for _, sw := range windows {
			sw.clampToWindowArea()
		}
	}
	w.rebuildMenu()
	w.desktop.Redraw()
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
		// Read-only analysis windows (issue #58) are ephemeral views with no
		// backend session, so they are not part of the persisted layout.
		if sw.readOnly {
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
	// Restored windows are clamped to the pinned window area (issue #106) so a
	// layout saved while overlapping the sidebar does not cover it once applied.
	area := w.windowArea()
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
			area.W, area.H, sw.window.MinWidth, sw.window.MinHeight)
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

// clampWindowRect keeps a window inside the given screenW×screenH area and at
// least minW×minH. Callers pass the workbench's window area (Workbench.windowArea,
// which excludes a pinned sidebar — issue #106) so a window can be dragged,
// resized or restored up to the sidebar boundary but never past it; the same
// call keeps a restored window on-screen after a terminal resize since the layout
// was saved, so a layout from a larger terminal can't strand a window off-screen.
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
		// An event may have moved the aggregate (usage tokens/requests, a sub-agent
		// spawn, an error); coalesce the Overall-panel refresh rather than paying
		// for one per event (issue #53 / redraw note in #22).
		w.scheduleOverallRefresh()
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
	// Stop the coalesced Overall-panel refresh timer so it cannot Post after the
	// loop is gone (a stray Post on a stopped desktop is benign, but stopping is
	// tidy). Lazily created, so guard nil.
	defer func() {
		if w.statsRefresh != nil {
			w.statsRefresh.Stop()
		}
	}()
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
		if event.Key != tui.KeyRune || event.Alt {
			return
		}
		if event.Ctrl {
			if event.Rune == 'c' || event.Rune == 'C' {
				w.confirmQuit()
			}
			return
		}
		// '?' and ':' open the help overlay and command palette respectively, but
		// only when they reach the desktop unconsumed — i.e. focus is on a
		// transcript/sidebar, not a text input where they are literal characters.
		switch event.Rune {
		case '?':
			w.showHelpOverlay()
		case ':':
			w.showCommandPalette()
		}
	})
	// Live-status ticker: while any session is generating, refresh its status
	// line once a second so the elapsed timer and throughput tick. Idle
	// workbenches do no work (and no redraw) per tick. Stops with the UI loop.
	go w.runStatusTicker(w.shutdown)
	// Populate the Overall panel's first frame from the current aggregate so it
	// is not blank before the first session event arrives.
	w.refreshOverall()
	err := w.desktop.Run(w.shutdown)
	w.app.Close()
	return err
}

// overallRefreshCoalesce bounds how often the Overall panel recomputes its
// aggregate during a burst of session events: events arriving faster than this
// collapse into a single recomputation, so a fast event stream cannot thrash the
// sidebar redraw (the TUI redraw note in issue #22; the panel itself is #53).
const overallRefreshCoalesce = 250 * time.Millisecond

// scheduleOverallRefresh (re)arms a single coalesced refresh of the Overall
// panel. The first session event after an idle period arms the timer; rapid
// follow-up events Reset it, so only one refresh fires ~250ms after the burst
// settles. It is a no-op when there is no sidebar or statistics handler. All
// callers run on the UI thread (event Post callbacks, the status ticker, session
// open/close), so the lazy creation and Reset are single-threaded; the AfterFunc
// goroutine only Posts back to the UI thread.
func (w *Workbench) scheduleOverallRefresh() {
	if w.sidebar == nil || w.handlers.GetStatistics == nil {
		return
	}
	if w.statsRefresh == nil {
		w.statsRefresh = time.AfterFunc(overallRefreshCoalesce, func() {
			w.desktop.Post(func() {
				w.refreshOverall()
				w.desktop.Redraw()
			})
		})
		return
	}
	w.statsRefresh.Reset(overallRefreshCoalesce)
}

// refreshOverall rebuilds the Overall panel from a fresh Statistics report and
// the focused session's active model config. It only updates the sidebar's stored
// aggregate; the caller owns the redraw (mirrors SessionWindow's refreshStatus
// contract). No-op without a sidebar or statistics handler. Runs on the UI thread.
func (w *Workbench) refreshOverall() {
	if w.sidebar == nil || w.handlers.GetStatistics == nil {
		return
	}
	w.sidebar.refreshOverallStats(w.handlers.GetStatistics(), w.activeModelConfig())
}

// activeModelConfig returns the model config selected in the focused (top-most)
// session window, or nil when no session is open or its model is unknown. It
// drives the Overall panel's "model" / "api" rows (issue #107). Runs on the UI
// thread (drawn from Focus / event / ticker refresh paths).
func (w *Workbench) activeModelConfig() *config.ModelConfig {
	w.mu.Lock()
	defer w.mu.Unlock()
	sw := w.sessions[w.activeIDLocked()]
	if sw == nil {
		return nil
	}
	return sw.selectedModelConfig()
}

// statusTickInterval is how often the live-status ticker refreshes busy
// sessions' status lines (the elapsed timer / throughput). One second matches
// the precision the status line shows.
const statusTickInterval = time.Second

// runStatusTicker refreshes the status lines of sessions that are currently
// generating once per tick, so the elapsed timer and tokens/sec advance live
// (issue #63). It stops when ctx is cancelled (the UI loop shutting down). The
// per-session busy check and redraw run on the UI thread via Post, so an idle
// workbench performs no redraws.
func (w *Workbench) runStatusTicker(ctx context.Context) {
	ticker := time.NewTicker(statusTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.desktop.Post(w.tickBusyStatuses)
		}
	}
}

// tickBusyStatuses refreshes every generating session's status line (so the
// elapsed timer and throughput advance) and redraws only when at least one
// session is busy — an idle workbench is left untouched. Runs on the UI thread.
func (w *Workbench) tickBusyStatuses() {
	w.mu.Lock()
	busy := make([]*SessionWindow, 0, len(w.sessions))
	for _, sw := range w.sessions {
		if sw.busy {
			busy = append(busy, sw)
		}
	}
	w.mu.Unlock()
	if len(busy) == 0 {
		return
	}
	for _, sw := range busy {
		sw.refreshStatus()
	}
	// While work is in flight the aggregate keeps moving (tokens stream in), so
	// refresh the Overall panel on the same 1s tick as the status lines.
	w.refreshOverall()
	w.desktop.Redraw()
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
