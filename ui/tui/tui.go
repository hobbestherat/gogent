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
	"gogent/internal/modelsdev"
	"gogent/internal/notify"
	"gogent/internal/stats"
	"log"
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
	// goroutine; progress is reported through EmitSessionEvent. effort is the
	// per-session reasoning-effort override (issue #177): empty means "no override
	// — use the selected model config's reasoning_effort".
	OnSend func(sessionID, message, modelName, effort string)
	// OnFork is invoked (on the UI thread) when a /fork creates a new session
	// window that should continue from a copy of an existing session's full
	// conversation history (issue #349). The backend forks parentSessionID into a
	// new peer session under newSessionID (deep-copying the transcript) and
	// registers an observer that forwards events via EmitSessionEvent, exactly as
	// OnCreate does for a fresh session. The title lets the backend persist a
	// human-friendly name. May be nil, in which case /fork is a no-op.
	OnFork func(parentSessionID, newSessionID, title string)
	// OnClose tears down the backend session when its window is closed.
	OnClose func(sessionID string)
	// OnRename notifies the backend that a session was renamed so it can persist
	// the new title to the session index (issue #272). May be nil.
	OnRename func(sessionID, title string)
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
	// GetShowWelcome / SetShowWelcome read and persist whether the startup
	// welcome/onboarding dialog is shown (issues #339/#341/#342). GetShowWelcome
	// gates the startup trigger and seeds the dialog's "Don't show this on startup
	// again" checkbox; SetShowWelcome persists the opt-out (false) or re-enable
	// (true) when the dialog is closed. Both may be nil, in which case the dialog
	// is still re-openable from the palette/Help menu but never auto-shows and the
	// checkbox is inert.
	GetShowWelcome func() bool
	SetShowWelcome func(bool)
	// GetTheme / SetTheme read and persist the TUI colour palette (issue #103).
	// SetTheme also re-applies the resolved palette to the live UI so the change
	// takes effect without a restart. May be nil, in which case the Theme editor
	// is hidden.
	GetTheme func() config.ThemeConfig
	SetTheme func(config.ThemeConfig)
	// GetSavedThemes / SetSavedThemes read and persist the user's named custom
	// themes (issue #462) — copy-and-modify palettes saved alongside the read-only
	// built-ins. GetSavedThemes returns a working copy the editor may mutate;
	// SetSavedThemes only persists (the live palette is applied via SetTheme when a
	// saved theme is made active). Both may be nil, in which case the editor shows
	// the built-ins only and hides the Save As/Delete actions.
	GetSavedThemes func() []config.NamedTheme
	SetSavedThemes func([]config.NamedTheme)
	// GetKeybindings / SetKeybindings read and persist the keyboard-shortcut
	// overrides (issue #269). SetKeybindings only persists; the customizer applies
	// each rebind to the live registry itself so the new key works without a
	// restart. May be nil, in which case overrides are not persisted (the
	// customizer still applies them live for the session).
	GetKeybindings func() config.KeybindingsConfig
	SetKeybindings func(config.KeybindingsConfig)
	// GetModels returns editable copies of the configured models; UpdateModel
	// persists changes to one model (matched by Name). May be nil.
	GetModels   func() []config.ModelConfig
	UpdateModel func(config.ModelConfig) error
	// ScanModels queries a backend (built from the given draft config's
	// api_type/endpoint/api_key) for the model ids it serves, so the editor can
	// swap the model-id text field for a dropdown. May be nil.
	ScanModels func(config.ModelConfig) ([]string, error)
	// AddModel creates a NEW model config (the "Add from catalog" flow), distinct
	// from UpdateModel which only replaces an existing entry. May be nil, in which
	// case the catalog affordance is hidden.
	AddModel func(config.ModelConfig) error
	// RemoveModel deletes a configured model by Name (the unified Models… dialog's
	// Remove action). It enforces the removal policy server-/core-side: removing
	// the default while other models remain, or a model in use by a live session,
	// returns an error the dialog surfaces; removing the last model is allowed and
	// yields the empty-list state. May be nil (Remove then hidden/disabled).
	RemoveModel func(name string) error
	// GetModelCatalog returns the models.dev catalog (cached on disk with a TTL).
	// force=true revalidates/refreshes. May be nil (catalog affordance hidden). It
	// may perform a live network fetch, so callers MUST invoke it off the UI thread
	// and pass a context they can cancel to abort an in-flight fetch.
	GetModelCatalog func(ctx context.Context, force bool) (modelsdev.Catalog, error)
	// GetDefaultModel / SetDefaultModel read and persist the default model used
	// for new sessions (issue #296). SetDefaultModel validates the name against the
	// configured models. May be nil (the editor then hides the default control).
	GetDefaultModel func() string
	SetDefaultModel func(string) error
	// GetTranscript returns a (sub-)agent's message transcript for the monologue
	// popup and for the deferred lazy-load on focus (issue #517). The return value
	// encodes success vs failure: a successful load is always non-nil (an empty
	// transcript is a non-nil empty slice), while nil signals the fetch FAILED.
	// Consumers rely on this distinction — nil leaves shown content/placeholders
	// intact and retries; a non-nil empty result clears them. May be nil (the field
	// itself, meaning the capability is unavailable).
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
	// OnSetPlanMode toggles plan mode for a session (issue #43). May be nil.
	OnSetPlanMode func(sessionID string, on bool)
	// OnSetYoloMode toggles yolo mode for a session (issue #356): auto-approve
	// permissions (except hard-deny guardrails) and remove the step cap. May be nil.
	OnSetYoloMode func(sessionID string, on bool)
	// OnApprovePlan executes a session's pending plan with the full tool set
	// (issue #43). It runs on a background goroutine; progress flows back as
	// session events. May be nil.
	OnApprovePlan func(sessionID string)
	// OnStop cancels a session's in-flight turn (issue #170). It drives the same
	// cancellation the loop already supports (agent.Cancel); the window discards
	// any queued message when the user stops, rather than auto-firing it. May be
	// nil, in which case the /stop command reports the feature as unavailable.
	OnStop func(sessionID string)
	// OnInject hands the current input text to a running session for mid-turn
	// injection at the next turn boundary (issue #170, phase 2). It is the agent-side
	// path behind the per-message Interject button (issue #201): the running turn
	// sees the text as a clarification before its next step. Enter/Queue still use
	// the window's drain-on-idle path instead. May be nil, in which case the
	// Interject button reports the feature as unavailable.
	OnInject func(sessionID, message string)
	// SupervisorEnabled reports whether the harness-level supervisor (issue #172)
	// is enabled (experimental.supervisor). When false the idle watchdog never
	// runs, so a session's /goal is purely informational. May be nil, treated as
	// false (supervisor off).
	SupervisorEnabled func() bool
	// SupervisorMaxNudges returns the bound on consecutive supervisor nudges for a
	// single idle session before the watchdog gives up and notifies the user
	// (issue #172). May be nil; a non-positive value resolves to a built-in
	// default in the window.
	SupervisorMaxNudges func() int
	// OnSupervisorCheck runs the supervisor's completion check for a session: is
	// goal satisfied given the conversation so far (issue #172)? It runs the cheap
	// todo short-circuit and, when needed, a single lightweight model judge on the
	// backend, returning done=true when the goal is met. It is called on a
	// background goroutine from the idle watchdog so a model judge does not block
	// the UI; an errored check is treated as "not done". May be nil, in which case
	// the supervisor never fires.
	OnSupervisorCheck func(sessionID, goal string) (done bool, err error)
	// StreamThinking queries or sets live thinking-token streaming for a session
	// (issue #217): set==nil queries the current state; otherwise it applies *set.
	// It returns the resulting state. It backs the /thinking command's live toggle.
	// May be nil, in which case the command reports the feature as unavailable.
	StreamThinking func(sessionID string, set *bool) bool
	// --- Watchers (issue #329 Phase 4) ---
	// ListWatchers returns the watchers visible to sessionID: every free-running
	// watcher plus that session's own attached watchers (sessionID "" yields the
	// free-running ones only). It backs the Watchers dialog, the /watcher list
	// command and the sidebar's watcher nodes. May be nil, in which case the
	// Watchers dialog and menu item are hidden and the sidebar renders no watcher
	// nodes.
	ListWatchers func(sessionID string) []WatcherInfo
	// CreateWatcher registers a new watcher from cfg and reports the created
	// watcher (or an error). sessionID is the calling session, used as the owning
	// session for an attached watcher. May be nil.
	CreateWatcher func(cfg WatcherConfig, sessionID string) (WatcherInfo, error)
	// EnableWatcher / DisableWatcher drive a watcher (by id or name) to a specific
	// schedule state: enabling re-arms it, disabling stops future fires (a running
	// fire finishes). Both back the dialog's Enable/Disable buttons and the
	// /watcher enable|disable commands. May be nil.
	EnableWatcher  func(idOrName string) error
	DisableWatcher func(idOrName string) error
	// RunWatcher fires a watcher (by id or name) immediately, ignoring its
	// schedule and enabled state. Backs the dialog Run button and /watcher run.
	// May be nil.
	RunWatcher func(idOrName string) error
	// StopWatcher cancels a watcher's in-flight fire (by id or name) without
	// stopping its schedule. Backs the dialog Stop button and /watcher stop. May
	// be nil.
	StopWatcher func(idOrName string) error
	// DeleteWatcher unregisters a watcher (by id or name) entirely. Backs the
	// dialog Delete button. May be nil.
	DeleteWatcher func(idOrName string) error
	// --- Custom slash commands (issue #403) ---
	// ListCommands returns every user-defined custom command. It backs the command
	// editor list, the palette entries and the slash-completion popup. May be nil,
	// in which case the Commands editor/menu and custom-command dispatch are absent.
	ListCommands func() []CommandInfo
	// OnSendCommand sends an expanded custom-command prompt applying its
	// per-invocation overrides (issue #403): model selects the turn's model; a
	// non-empty agent or subtask=true routes the prompt through a spawned sub-agent
	// (the agent value names it) whose result is surfaced in the transcript. It is
	// the override-aware counterpart of OnSend; when nil the dispatch path falls back
	// to OnSend (model applied, agent/subtask ignored), so a backend that has not
	// wired it degrades gracefully. Called on a background goroutine.
	OnSendCommand func(sessionID, message, model, agent string, subtask bool, effort string)
	// ReservedCommandNames returns the built-in command names a custom command may
	// not shadow — the backend's single source of truth (command.ReservedNames). The
	// editor's collision check and the dispatch guard consult it so the reserved set
	// is not hard-coded in two places. May be nil, in which case the UI falls back to
	// its local mirror.
	ReservedCommandNames func() map[string]bool
	// GetCustomCommand resolves one command by name for runtime dispatch (the
	// handleSlashCommand fallthrough). A non-nil error means "no such custom
	// command" — the caller then sends the raw text to the model unchanged, so a
	// custom command never shadows a built-in. May be nil.
	GetCustomCommand func(name string) (CommandDef, error)
	// CreateCommand / UpdateCommand persist a command, enforcing collision and
	// shape validation in the backend and returning a descriptive error the editor
	// shows inline. Create stamps version 1; Update appends a new version. May be nil.
	CreateCommand func(def CommandDef) error
	UpdateCommand func(def CommandDef) error
	// DeleteCommand removes a command and its history. May be nil.
	DeleteCommand func(name string) error
	// GetCommandHistory returns a command's append-only version history (oldest
	// first) for the history/diff browser. May be nil.
	GetCommandHistory func(name string) ([]CommandVersion, error)
	// RestoreCommandVer restores version v of a command, itself recorded as a new
	// version. May be nil.
	RestoreCommandVer func(name string, v int) error
	// --- Daemon attach lifecycle (issue #358 §6) ---
	// DaemonMode reports the TUI's current attachment mode (embedded, attached to
	// the local daemon, or attached to a remote --connect daemon), so the Daemon
	// menu can offer the right actions. May be nil, in which case the whole Daemon
	// menu is hidden (the build has no daemon wiring, or the user is in a context
	// where the handoff does not apply).
	DaemonMode func() DaemonMode
	// StartDaemon performs the embedded->daemon handoff: persist live state, spawn
	// the local daemon, switch the Workbench Handlers to the remote (HTTP/SSE)
	// implementation and shut the embedded core down. It blocks until the handoff
	// is complete (or fails) and is called on a background goroutine. It is only
	// invoked when DaemonMode reports embedded. May be nil (the menu item is then
	// disabled).
	StartDaemon func() error
	// StopDaemon performs the daemon->embedded handoff: ask the local daemon to
	// persist and shut down, build an embedded core, RestoreSessions and switch the
	// Handlers back to the in-process implementation. It blocks until complete and
	// is called on a background goroutine. It operates on the LOCAL daemon only and
	// is only invoked when DaemonMode reports attached-local. May be nil.
	StopDaemon func() error
	// DaemonStatusInfo returns a snapshot for the "Daemon status" dialog (running,
	// pid, uptime, address, transport, live session/watcher counts, MCP servers).
	// It may perform a blocking round-trip to the daemon and is called on a
	// background goroutine. May be nil, in which case the status item is hidden.
	DaemonStatusInfo func() (DaemonStatusReport, error)
	// ConnectionLabel returns the terse remote target shown in the menu-bar status
	// indicator when DaemonMode reports attached-remote, e.g. "ssh:user@host" (or a
	// bare "host:port" for a plain --connect). It is cheap and synchronous (no
	// round-trip) so it can be read on the UI thread on every menu rebuild/resize,
	// unlike DaemonStatusInfo. It returns "" for embedded/attached-local (the
	// indicator derives those labels from DaemonMode alone). May be nil.
	ConnectionLabel func() string
	// ReconnectAddress returns the verbatim --connect argument for the current
	// attached-remote daemon (e.g. "ssh://user@host", "host:port"), so the
	// daemon-aware quit dialog can show a copy-pasteable "Re-attach later with:
	// gogent --connect {addr}" line (issue #503). Unlike reconnectHost (a display
	// label from hostLabel, which drops the scheme/user) this is a real connect
	// string. It is cheap and synchronous (a closure over the attach address),
	// mirroring ConnectionLabel. It returns "" for embedded/attached-local; may be
	// nil (the re-attach line is then omitted).
	ReconnectAddress func() string
}

// WatcherConfig is the UI-facing description of a watcher to create (issue #329
// Phase 4), kept free of internal/config so ui/tui stays decoupled from the
// backend types. ReportToSession nil makes a free-running watcher; a non-nil
// (possibly empty) value makes an attached watcher owned by that session.
type WatcherConfig struct {
	Name            string
	Task            string
	Model           string
	Every           string // interval form, e.g. "5m" (mutually exclusive with DailyAt)
	DailyAt         string // "HH:MM" form
	Timezone        string // IANA tz for DailyAt; "" means UTC
	ReportToSession *string
}

// CommandInfo is the list-row view of a custom command (issue #403): just enough
// to render the editor list, palette entries and the slash-completion popup
// without loading the full template/history. It is intentionally decoupled from
// internal/config so ui/tui stays free of the backend types.
type CommandInfo struct {
	Name        string
	Description string
	Version     int
}

// CommandDef is the UI-facing full custom command, mirroring config.CommandDef
// but kept independent of internal/config. The editor reads and writes this
// shape; the backend maps it to/from the persisted type.
type CommandDef struct {
	Name        string
	Description string
	Parameters  []CommandParam
	Template    string
	Model       string
	Agent       string
	Subtask     bool
	Version     int
	Versions    []CommandVersion
}

// CommandParam is one declared parameter of a custom command (UI-facing).
type CommandParam struct {
	Name        string
	Description string
	Required    bool
	Default     string
}

// CommandVersion is one immutable snapshot in a command's history (UI-facing).
type CommandVersion struct {
	Version    int
	Template   string
	Parameters []CommandParam
	Model      string
	Agent      string
	Subtask    bool
	SavedAt    string
}

// RestoredSession describes a session to be re-opened from persisted state.
type RestoredSession struct {
	ID       string
	Title    string
	Messages []ChatMessage
	// Model is the config name of the model the session was last using, so the
	// re-opened window preselects it in the model dropdown (issue #266). Empty
	// leaves the default selection.
	Model string
	// Deferred marks a session whose transcript was intentionally not fetched up
	// front to bound first-connect round-trips (issue #517). AdoptSession opens it
	// as a labelled shell (no transcript, no OnCreate) and the Workbench fetches its
	// transcript once, lazily, the first time the window is focused. Messages is
	// empty when Deferred is true.
	Deferred bool
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
	Model     string // stable config Name (lookup key)
	// ModelLabel/ModelID are the frozen display label + provider id captured when
	// the session was last saved (issue #389), so the Saved Sessions detail pane
	// shows the model the session actually ran on, unaffected by later config
	// edits. Empty for older index files predating the fields; formatSessionDetail
	// then falls back to the bare Model key.
	ModelLabel string
	ModelID    string
	File       string
	// Archived is true when the session's window was closed (its on-disk base is
	// "_session_archived"). The browser lists archived sessions too and marks them
	// so the user can tell a closed session from an open one (issue #325).
	Archived bool
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

// WatcherInfo is the UI-facing snapshot of one scheduled watcher (issue #329
// Phase 4), assembled by the backend from the watcher manager's state plus its
// stored config. It is deliberately self-contained so ui/tui does not depend on
// internal/watcher: the Watchers dialog and the sidebar render entirely from this
// view.
type WatcherInfo struct {
	ID   string
	Name string
	// Free reports the watcher's kind: true for a free-running (global) watcher
	// that owns its own watcher:<name> session and renders as a top-level sidebar
	// entry; false for an attached (session-scoped) watcher that renders as a
	// child of its target session.
	Free bool
	// TargetSession is the owning session id for an attached watcher, or "" for a
	// free-running watcher. It backs the dialog's Target column ("free" badge when
	// empty) and the sidebar's child-node placement.
	TargetSession string
	// SessionID is the session the watcher reports into and that the dialog's Open
	// Session button raises: TargetSession for attached watchers, watcher:<name>
	// for free-running ones.
	SessionID string
	Enabled   bool
	// Status is the lowercase status token ("idle"/"running"/"skipped"/"failed").
	Status string
	// Running is true while a fire is in flight, driving the live busy marker in
	// both the dialog list and the sidebar node (independent of Status so a test
	// can set it directly).
	Running    bool
	Task       string // configured task prompt (shown in the dialog detail pane)
	Schedule   string // human-readable schedule ("every 5m"), may be empty
	NextFire   string // formatted next-fire time, empty when none/disabled
	LastRun    string // formatted last-run time, empty when never run
	LastResult string
	LastError  string
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
	// deferredTranscripts is the set of restored windows whose transcript was not
	// fetched up front (issue #517): they opened as labelled shells and load their
	// transcript once, lazily, on first focus. An id is removed the moment its load
	// starts (in ensureTranscript), so focus can never trigger a second fetch.
	deferredTranscripts map[string]bool
	// pinned records favorite sessions (shown with a ★ marker and floated to the
	// top of the sidebar on pin). Kept as a set so the flag survives reorders.
	pinned  map[string]bool
	nextNum int
	// nextAnalysis assigns unique ids to read-only analysis windows opened from
	// the Sessions browser (issue #58), kept separate from nextNum so synthetic
	// "analysis-N" ids never collide with backend "session-N" ids.
	nextAnalysis int
	handlers     Handlers
	// keybindings holds the user's keyboard-shortcut overrides (issue #269): an
	// actionID -> chord map carrying ONLY actions rebound away from their catalog
	// default. rebuildBindings / registerTranscriptBindings consult it (via chordFor)
	// when registering, so a persisted override is applied the moment
	// a binding is registered; the customizer mutates it and persists via the
	// GetKeybindings/SetKeybindings handlers. Touched only on the UI thread, like the
	// rest of the workbench's binding state.
	keybindings map[tv.ActionID]tv.Chord
	sidebar     *sidebar
	// sidebarPinned reserves the right-hand sidebar strip as a hard window
	// boundary when true (the default): windows are dragged, resized, maximized,
	// created and restored within the area left of the "Sessions & Agents" panel
	// so it is never covered (issue #106). Toggling it off restores free,
	// overlapping windows. Read/written on the UI thread, like the sidebar's own
	// approval state, so the geometry helpers below read it without the lock.
	sidebarPinned bool
	// sidebarW is the live width (columns) of the right-hand sidebar strip (issue
	// #175). It is mutated by dragging the sidebar's left-edge divider, clamped to
	// [minSidebarWidth, screen-minWorkAreaWidth], persisted in the layout store and
	// restored on launch. Read/written on the UI thread, like sidebarPinned, so the
	// geometry helpers read it without the lock. Zero means "use the default".
	sidebarW int
	monolog  *tv.Layer
	// monologWindow is the open monologue popup's window, tracked alongside
	// monolog so a sidebar pin-on / width change can re-clamp it into the window
	// area (issue #319). turbotv's Layer.window is unexported with no accessor, so
	// the window is stored directly. Set in showAgentMonolog, cleared on close /
	// replace; nil when no monologue is open. Read/written on the UI thread.
	monologWindow *tv.Window
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
	approvals map[string]int
	// deferredModal holds a background-triggered modal (permission/review) whose
	// visual presentation is deferred while the user is typing in a session input
	// (issue #346), and deferredTimer is the re-check armed while it waits. Both are
	// loop-confined: touched only on the desktop event loop (directly or via Post),
	// so they need no lock. promptMu already serializes prompts one at a time, so at
	// most one presentation is ever pending.
	deferredModal func()
	deferredTimer *time.Timer
	// clarifyWaiting records which interactive sub-agents are currently blocked on
	// a CLARIFY question, keyed by the same sub-agent key applySubAgent uses
	// (agent id, or session/name when the id is empty). It collapses the raw
	// sub-agent event stream — which re-emits StatusWaiting on each CLARIFY round
	// but does not emit the resume in between — into balanced per-sub-agent
	// transitions, so the sidebar's per-session clarify reference count is bumped
	// once per waiting sub-agent and dropped once when it resolves (issue #207).
	// Entries are removed on a sub-agent's terminal event; sidebar.removeSession
	// also prunes a closed session's still-waiting sub-agents so none linger.
	// Touched only on the UI thread (from EmitSessionEvent / removeSession), like
	// the sidebar's own clarify state, so it needs no lock.
	clarifyWaiting map[string]bool
	windowConfig   config.WindowConfig
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
	// suppressEventNotify turns off maybeNotify's per-session-event notifications
	// when the TUI is attached to a daemon (issue #358 §9): the daemon delivers
	// completion/attention notifications over the wire as "notification" SSE frames
	// (surfaced via NotifyFromWire), so also notifying off the normal final/error
	// session events would double up. Embedded mode leaves it false. Guarded by mu.
	suppressEventNotify bool
	// statsRefresh coalesces Overall-panel recomputations: a burst of session
	// events arms it once and rapid follow-ups Reset it, so the panel refreshes
	// at most ~250ms after the burst settles instead of once per event (issues
	// #22 / #53). Lazily created and only touched on the UI thread; its AfterFunc
	// goroutine just Posts the refresh back to the UI thread.
	statsRefresh *time.Timer
	// layoutPersist coalesces layout-file writes during a sidebar-divider drag
	// (issue #320): the resize handler arms it once and each follow-up motion
	// Resets it, so a drag writes ~/.gogent/workbench_layout.json at most once,
	// shortly after the user stops moving, instead of synchronously on every
	// mouse-motion report. It mirrors statsRefresh: lazily created, only touched
	// on the UI thread, and its AfterFunc goroutine just Posts persistLayout back
	// to the UI thread. The final width is always captured regardless by Run's
	// shutdown defer w.persistLayout().
	layoutPersist *time.Timer
	// overallLifetime accumulates the Overall panel's token/request/error/cache-hit
	// figures over the whole gogent run (issue #232). The Statistics report sums only
	// the currently-open sessions, so without this a closed session's counters would
	// vanish from the panel; overallLifetime folds each fresh report and remembers
	// every session's last-known tally so the totals only ever grow. Touched only on
	// the UI thread (refreshOverall), like the rest of the Overall state.
	overallLifetime *lifetimeStats
	// undelivered counts window-needing session events (eventNeedsWindow) whose id
	// had no open window when deliverSessionEvent ran, so their apply (transcript or
	// status render) was lost. Sub-agent/todo events are excluded — the sidebar
	// services them regardless of the window — so a non-zero value means a real
	// render was dropped, a tripwire on the invariant that a live session keeps its
	// window for the whole turn. It stays zero in normal operation; a non-zero value
	// is the lifecycle regression that could let a final answer vanish with no trace
	// (issue #227). Guarded by w.mu.
	undelivered int
	// --- daemon disconnect modal (issue #358 §7) ---
	// reconnectHost labels the disconnect modal ("Connection to <host> lost");
	// reconnectRetry pokes the remote client's backoff for the "Retry now" button.
	// Both are set by SetReconnectControls when the TUI is attached to a daemon and
	// are read only on the UI thread.
	reconnectHost  string
	reconnectRetry func()
	// disconnectLayer is the live BLOCKING modal while the daemon connection is
	// down, nil when connected; disconnectBody is its message TextView, updated as
	// the reconnect attempt count climbs. Touched only on the UI thread (the
	// Reconnector callbacks marshal through Post), so they need no lock.
	disconnectLayer *tv.Layer
	disconnectBody  *tv.TextView
	// daemonHandoffLayer is the live interim "Migrating…" progress modal during a
	// Start/Stop daemon handoff, nil when none is in flight. The result dialog
	// replaces it (RemoveLayer) so the two never stack (issue #478). Start (embedded
	// only) and Stop (attached-local only) are mode-exclusive and the modal blocks
	// the menu, so one field suffices. Touched only on the UI thread.
	daemonHandoffLayer *tv.Layer
	// quitDialogLayer is the live daemon-aware quit confirmation, nil when none is
	// open (issue #503). It is tracked so the background daemon-status fetch can
	// enrich the body in place only while the same dialog is still up (and no-op if
	// the user already dismissed or acted). Touched only on the UI thread (open +
	// Post-ed enrichment callback).
	quitDialogLayer *tv.Layer
	// quitDialogBody is the quit dialog's body TextView while it is open, nil
	// otherwise — retained so the background status fetch can rewrite it in place
	// (issue #503), mirroring disconnectBody. Touched only on the UI thread.
	quitDialogBody *tv.TextView
	// menuBar is the live menu bar built by rebuildMenu, retained so the right-anchored
	// connection-status slot can be updated in place (refreshConnectionStatus) without a
	// full menu rebuild. Touched only on the UI thread.
	menuBar *tv.MenuBar
	// connPhase tracks the remote connection's transient state for the status indicator
	// (issue #500): healthy, just-dropped, or actively reconnecting. It is set from the
	// Reconnector hooks (OnConnectionLost/OnConnectionRestored) and read when deriving the
	// indicator. It only affects attached-remote mode — connectionIndicator forces healthy
	// otherwise. Touched only on the UI thread (the hooks marshal through Post).
	connPhase connPhase
}

// NewWorkbench creates the workbench and its desktop chrome.
func NewWorkbench(models []*config.ModelConfig) *Workbench {
	app := tui.New()
	w := &Workbench{
		app:                 app,
		desktop:             tv.NewDesktop(app),
		sessions:            make(map[string]*SessionWindow),
		deferredTranscripts: make(map[string]bool),
		keybindings:         make(map[tv.ActionID]tv.Chord),
		pinned:              make(map[string]bool),
		sidebarPinned:       true,
		sidebarW:            defaultSidebarWidth,
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
		// Process-lifetime accumulator for the Overall panel (issue #232).
		overallLifetime: newLifetimeStats(),
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
	// Re-sync the derived active-session state (sidebar highlight, TODO region,
	// Overall band) whenever the desktop raises a different window on its own —
	// notably click-to-raise, which the toolkit handles internally without routing
	// through Workbench.Focus (issue #304). Before this hook, clicking a background
	// session window while idle left the sidebar highlight stranded until the busy
	// ticker or a session event re-synced it.
	//
	// The desktop fires this on every top-layer change, including opening/closing a
	// dialog that sits above the windows — but those do not change the active
	// session (ActiveID falls back to the top-most session beneath), so we guard on
	// the resolved active session actually changing. sidebar.focused records the id
	// of the last session synced through refreshOverall (it always calls
	// focusSession(ActiveID())), so an unchanged ActiveID means there is nothing to
	// re-sync — skipping it keeps a dialog open from triggering a redundant
	// GetStatistics fetch. refreshOverall only updates stored state and mutates no
	// layers, so it cannot re-enter this hook; request a coalesced redraw to paint it.
	w.desktop.OnActiveLayerChange(func(*tv.Layer) {
		if w.sidebar == nil || w.ActiveID() == w.sidebar.focused {
			return
		}
		// AddLayer fires this hook synchronously, so a connect/restore burst that opens
		// N windows would otherwise run N synchronous GetStatistics fetches here — the
		// per-window Overall recompute issue #521 targets. Keep the cheap focus/TODO
		// tracking immediate (it also updates sidebar.focused so the guard above dedupes
		// the next fire) but defer the expensive recompute to the 250ms coalescer, so the
		// burst folds into one fetch ~250ms after it settles. Mirrors openWindowAny.
		w.sidebar.focusSession(w.ActiveID())
		w.scheduleOverallRefresh()
		w.desktop.RequestRedraw()
	})
	// Keep the sidebar pinned to the right edge across terminal resizes.
	app.OnResize(func(tui.ResizeEvent) {
		w.sidebar.reposition(w.app.Width(), w.app.Height())
	})
	w.rebuildMenu()
	w.rebuildBindings()
	return w
}

// SetModels updates the list of available models offered in each session window.
// Besides repointing the workbench cache and the sidebar's Overall selector, it
// propagates the refreshed options into every open session window's header
// dropdown so a model edit (Models… dialog → Save) takes effect live, without an
// app restart (issue #389). Each window snapshotted modelNames at creation and
// never repointed them; without this an edit leaves stale labels — and a
// DisplayName edit would make selectedModelName() resolve to "" (a silent
// fallback to the default model on the next send).
func (w *Workbench) SetModels(models []*config.ModelConfig) {
	// Capture each open window's currently-selected model by its STABLE config
	// Name before swapping the cache, resolving the live dropdown label against
	// the OLD model set (w.models is still the previous list here). Re-selecting
	// by Name (not label) below preserves the user's model across a DisplayName
	// edit, where the label itself changed.
	w.mu.Lock()
	windows := make([]*SessionWindow, 0, len(w.sessions))
	for _, sw := range w.sessions {
		windows = append(windows, sw)
	}
	w.mu.Unlock()
	selectedNames := make([]string, len(windows))
	for i, sw := range windows {
		selectedNames[i] = sw.selectedModelName()
	}

	w.models = models
	w.modelNames = make([]string, len(models))
	for i, m := range models {
		name := m.DisplayName
		if name == "" {
			name = m.Name
		}
		w.modelNames[i] = name
	}
	// Keep the Overall band's model selector in sync with the available models
	// (issue #191). No-op before the sidebar is built (initial SetModels in the
	// constructor runs first).
	if w.sidebar != nil {
		w.sidebar.rebuildModelOptions()
	}

	// Repopulate each open window's header dropdown from the refreshed names and
	// re-select the previously-chosen model by its stable Name (issue #389).
	for i, sw := range windows {
		if sw.modelSelect == nil {
			continue
		}
		// SetOptions preserves selection by old *label* and clamps to 0 when it is
		// gone; we override that immediately with a Name-based re-selection so a
		// changed DisplayName can't drop the user's model. SetOptions/SetSelected
		// don't fire OnChange, so rebuildEffortOptions is called explicitly (exactly
		// what the OnChange handler would have done) to refresh the effort choices.
		sw.modelSelect.SetOptions(w.modelNames)
		if idx := w.modelIndexByName(selectedNames[i]); idx >= 0 {
			sw.modelSelect.SetSelected(idx)
		}
		sw.rebuildEffortOptions()
	}
	if len(windows) > 0 {
		w.desktop.Redraw()
	}
}

// longestModelNameWidth returns the display width (in cells) of the widest model
// name offered in the window-header selector, used to size that dropdown so the
// active name is not truncated (issue #108).
func (w *Workbench) longestModelNameWidth() int {
	max := 0
	for _, n := range w.modelNames {
		if l := tui.StringWidth(n); l > max {
			max = l
		}
	}
	return max
}

// modelByIndex returns the model config at the given header-dropdown index, or
// nil when the index is out of range. The dropdown's option list is built
// positionally from w.models (option i labels models[i]), so the selected index
// is the unambiguous identity of the chosen model — selectedModelConfig resolves
// through this rather than the display label, keeping two models that share a
// DisplayName individually selectable (issue #389).
func (w *Workbench) modelByIndex(i int) *config.ModelConfig {
	if i < 0 || i >= len(w.models) {
		return nil
	}
	return w.models[i]
}

// SetHandlers registers the backend callbacks. It also refreshes the menu-bar
// connection-status indicator (issue #500) so a Handlers swap — e.g. attach setup
// installing the remote handlers, or a daemon handoff switching modes — is reflected
// immediately, without depending on a later rebuildMenu to reseed the slot.
// refreshConnectionStatus is nil-safe before the first menu build and must run on the
// UI thread; every SetHandlers caller already satisfies that (startup before the loop,
// or the handoff controller via Post).
func (w *Workbench) SetHandlers(h Handlers) {
	w.handlers = h
	w.refreshConnectionStatus()
}

// Post marshals fn onto the UI (event-loop) thread, so background goroutines —
// notably the daemon-handoff controller swapping Handlers, and the remote
// client's reconnect callbacks — mutate Workbench/desktop state without racing
// the render loop. It is a thin, exported wrapper over the desktop's own queue.
func (w *Workbench) Post(fn func()) { w.desktop.Post(fn) }

// RefreshMenu rebuilds the menu bar AND the binding registry from the current
// Handlers and state. The daemon-handoff controller calls it (on the UI thread)
// after swapping Handlers so the Daemon menu reflects the new attachment mode
// immediately. Rebuilding the bindings too keeps handler-gated Global accelerators
// (e.g. Ctrl+, Sub-agents, which rebuildBindings skips while its handler is unwired)
// in step with the new Handlers — the menu bar no longer registers them on SetMenuBar
// since #401, so RefreshMenu owns that re-registration.
func (w *Workbench) RefreshMenu() {
	w.rebuildBindings()
	w.rebuildMenu()
}

// SessionIDs returns the ids of the open, live (non-read-only) session windows.
// The handoff controller uses it to rewire each window's backend observer after a
// daemon->embedded switch, so live sessions keep streaming into their windows.
func (w *Workbench) SessionIDs() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	ids := make([]string, 0, len(w.order))
	for _, id := range w.order {
		if sw := w.sessions[id]; sw != nil && !sw.readOnly {
			ids = append(ids, id)
		}
	}
	return ids
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
		w.menuActionItem("&New Session", actionSessionNew),
		w.menuActionItem("Ne&xt Session", actionSessionNext),
		w.menuActionItem("&Close Session", actionSessionClose),
		tv.NewMenuItem("----------", nil),
		w.menuActionItem("Close &Others", actionSessionCloseOthers),
		w.menuActionItem("Close Al&l", actionSessionCloseAll),
	}
	// The Sessions browser (issue #58) is surfaced only when the backend wires
	// the listing handler; otherwise the item would lead nowhere.
	if w.handlers.ListSavedSessions != nil {
		sessionItems = append(sessionItems,
			tv.NewMenuItem("----------", nil),
			w.menuActionItem("Saved &Sessions…", actionSessionsBrowser),
		)
	}
	// The Watchers dialog (issue #329 Phase 4) is surfaced only when the backend
	// wires the watcher listing handler.
	if w.handlers.ListWatchers != nil {
		sessionItems = append(sessionItems,
			tv.NewMenuItem("&Watchers…", func() { w.showWatchersDialog() }),
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
			w.menuActionItem("&Rename Active…", actionSessionRename),
			w.menuActionItem(pinLabel, actionSessionPin),
			w.menuActionItem("Move Active &Up", actionSessionMoveUp),
			w.menuActionItem("Move Active &Down", actionSessionMoveDown),
			tv.NewMenuItem("----------", nil),
			w.menuActionItem("Export &Markdown…", actionSessionExportMD),
			w.menuActionItem("Export &JSON…", actionSessionExportJSON),
		)
		// Plan-mode approval (issue #43): surface it when the backend wires the
		// handler. The action reports when there is no plan to approve.
		if w.handlers.OnApprovePlan != nil {
			sessionItems = append(sessionItems,
				tv.NewMenuItem("----------", nil),
				tv.NewMenuItem("&Approve Plan", func() { w.approveActivePlan() }),
			)
		}
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
	editMenuItems := w.editItems()
	subMenus := []*tv.MenuItem{
		tv.NewSubMenu("&File",
			w.menuActionItem("E&xit", actionAppQuit),
		),
		tv.NewSubMenu("&Edit", editMenuItems...),
		tv.NewSubMenu("&Session", sessionItems...),
		tv.NewSubMenu("&View", w.viewItems()...),
		tv.NewSubMenu("&Config", w.settingsItems()...),
	}
	// The Daemon menu (issue #358 §6) appears only when the build wired the daemon
	// handoff handlers; embedded users who never opt in never see it. It is
	// right-aligned (issue #500) so it packs against the right edge, immediately left
	// of the connection-status indicator; the left navigation menus keep their order.
	if w.handlers.DaemonMode != nil {
		subMenus = append(subMenus, tv.NewSubMenu("&Daemon", w.daemonItems()...).AlignRight())
	}
	subMenus = append(subMenus,
		tv.NewSubMenu("&Help",
			w.menuActionItem("Command &Palette…", actionCommandPalette),
			tv.NewMenuItem("&Keybindings (?)…", func() { w.showHelpOverlay() }),
			tv.NewMenuItem("&Welcome…", func() { w.showWelcomeDialog() }),
			tv.NewMenuItem("----------", nil),
			tv.NewMenuItem("&About", func() {
				w.showConfirm("Gogent",
					"Gogent multi-session TUI.\nEach session is its own window; fold thoughts & tool calls with the >/v markers.\nPress ? for the keybinding cheatsheet or Ctrl+K for the command palette.", nil)
			}),
		),
	)
	bar := tv.NewMenuBar(tv.Rect{X: 0, Y: 0, W: w.app.Width(), H: 1}, subMenus...)
	applyMenuBarShadow(bar) // honour the NoShadow theme setting (issue #215)
	// Retain the bar and seed the right-anchored connection-status slot (issue #500) so
	// the first paint already shows the indicator. A fresh bar is built every rebuild, so
	// the slot must be reseeded here each time; refreshConnectionStatus updates it in place
	// between rebuilds (e.g. on disconnect/reconnect).
	w.menuBar = bar
	text, fg, bg := w.connectionIndicator()
	bar.SetStatus(text)
	bar.SetStatusColors(fg, bg)
	w.desktop.SetMenuBar(bar)
	// Coalesced redraw (issue #521): rebuildMenu only rebuilds the menu-bar model and
	// never precedes a blocking read, so it has no must-paint-now requirement. It runs
	// once per restored window on the connect/restore hot path (AdoptSession /
	// OpenAnalysisSession), so a synchronous Redraw here repainted the whole frame N
	// times during a burst. RequestRedraw defers to the run loop's single per-iteration
	// flush, and is serviced in every context this runs: the pre-loop restore folds into
	// Desktop.Run's initial compose+Apply (it composes the final state when the loop
	// starts, independent of the dirty flag), a reconnect (Post) into the post-drain
	// flush, and in-loop user actions (rename/pin/close/reorder) into the dispatch flush.
	w.desktop.RequestRedraw()
}

// connectionIndicator derives the menu-bar status string and its colours for the
// current daemon mode + remote target + transient connection phase (issue #500). It
// is cheap and synchronous (DaemonMode/ConnectionLabel are lock-free getters; no
// daemon round-trip), so it is safe to call on every rebuild/refresh on the UI thread.
// When no daemon wiring is present (DaemonMode nil) it reports "● embedded".
func (w *Workbench) connectionIndicator() (text string, fg, bg tui.Color) {
	mode := DaemonModeEmbedded
	if w.handlers.DaemonMode != nil {
		mode = w.handlers.DaemonMode()
	}
	label := ""
	if w.handlers.ConnectionLabel != nil {
		label = w.handlers.ConnectionLabel()
	}
	// The transient disconnect phase only applies to a remote attachment; force healthy
	// otherwise so a stale phase (e.g. left over after a Stop handoff) can never leak a ○
	// marker into embedded/local mode.
	phase := w.connPhase
	if mode != DaemonModeAttachedRemote {
		phase = connHealthy
	}
	text = daemonIndicatorText(mode, label, phase)
	fg, bg = daemonIndicatorColors(mode, phase)
	return text, fg, bg
}

// refreshConnectionStatus updates the menu bar's right-anchored status slot in place,
// without a full menu rebuild (issue #500). It is the light path used on remote
// disconnect/reconnect, where rebuilding the whole dynamic menu on every backoff tick
// would be wasteful. UI-thread only; a no-op before the first rebuildMenu.
func (w *Workbench) refreshConnectionStatus() {
	if w.menuBar == nil {
		return
	}
	text, fg, bg := w.connectionIndicator()
	w.menuBar.SetStatus(text)
	w.menuBar.SetStatusColors(fg, bg)
	w.desktop.RequestRedraw()
}

// menuActionItem builds a menu item for a rebindable catalog action (issue #401): it
// tags the item with the action's ActionID and renders a display-only shortcut hint
// from the action's current chord (chordFor), so the hint tracks a rebind on the next
// rebuildMenu. The item's OnSelect is the SAME catalog run that rebuildBindings
// registers for this ActionID, so a click and the keyboard accelerator fire one
// closure. The menu bar no longer registers accelerators from its tree (since #401 it
// is a view); the binding itself lives on the desktop registry via rebuildBindings.
func (w *Workbench) menuActionItem(label string, id tv.ActionID) *tv.MenuItem {
	a, ok := w.actionByID(id)
	if !ok {
		// The id is not in the catalog — a catalog/menu drift. Build a plain, inert item
		// (no run, no shortcut hint) so the drift is visible rather than masquerading as a
		// working item with a bogus zero-chord shortcut. Every id rebuildMenu passes is a
		// compile-time catalog constant, so this is defensive only.
		return tv.NewMenuItem(label, nil)
	}
	it := tv.NewMenuItem(label, a.run).WithActionID(id)
	c := w.chordFor(id)
	if c == unboundChord {
		it.Shortcut = &tv.MenuShortcut{Display: chordLabel(c)} // "—"
	} else {
		it.Shortcut = &tv.MenuShortcut{
			Display: displayChord(c),
			Key:     c.Key, Rune: c.Rune, Ctrl: c.Ctrl, Shift: c.Shift, Alt: c.Alt,
		}
	}
	return it
}

// settingsItems builds the Settings submenu. The sub-agent execution-model
// settings live in a modal dialog built from the turbotv Checkbox widgets (see
// showSettingsDialog); the menu also surfaces a quick read-only summary of the
// current configuration so the active mode is visible at a glance.
func (w *Workbench) settingsItems() []*tv.MenuItem {
	// Sub-agents is always present and tagged with its ActionID (issue #401): Ctrl+, is a
	// rebindable Global whose binding lives on the desktop registry, and showSettingsDialog
	// guards an unwired handler with a graceful "unavailable" message, so the menu entry
	// stays in step with the binding. The editors and summary lines that actually read the
	// settings accessors are gated below.
	if w.handlers.GetSettings == nil || w.handlers.SetSettings == nil {
		// Keep the (tagged) Sub-agents entry and the keybinding customizer — gated only on
		// its own handlers — but skip the entries that need the settings accessors.
		return append([]*tv.MenuItem{w.menuActionItem("&Sub-agents…", actionConfigSubagents)},
			w.keybindingsMenuItems()...)
	}
	cur := w.handlers.GetSettings()
	mode := "one-shot"
	switch {
	case cur.ExposesOneShotTools() && cur.ExposesInteractiveTools():
		mode = "both"
	case cur.ExposesInteractiveTools():
		mode = "interactive"
	}
	recursive := "off"
	if cur.AllowRecursive {
		recursive = "on"
	}
	items := []*tv.MenuItem{
		w.menuActionItem("&Sub-agents…", actionConfigSubagents),
		// A single unified "Models…" dialog is the one home for add (Catalog +
		// Empty-slot) / edit / remove / set-default (issue #509). It opens even with
		// zero configured models and works offline for the Empty-slot add, so it is
		// no longer gated on catalogReady() — the standalone "Add Model from
		// Catalog…" entry is gone, the catalog flow is now one of the Add paths
		// inside the dialog.
		tv.NewMenuItem("&Models…", func() { w.showModelsDialog() }),
	}
	items = append(items, tv.NewMenuItem("&Resources…", func() { w.showResourcesDialog() }))
	// Statistics is surfaced only when the backend wires the report handler.
	if w.handlers.GetStatistics != nil {
		items = append(items, tv.NewMenuItem("S&tatistics…", func() { w.showStatisticsDialog() }))
	}
	// Custom commands editor (issue #403), gated like the other handler-backed
	// items: shown only when the backend wired command management.
	if w.handlers.ListCommands != nil {
		items = append(items, tv.NewMenuItem("&Commands…", func() { w.showCommandsDialog() }))
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
	items = append(items, w.keybindingsMenuItems()...)
	return items
}

// keybindingsMenuItems returns the Config-menu entry for the keybinding customizer
// (issue #401), or nil when the keybinding handlers are unwired. It is gated solely on
// GetKeybindings/SetKeybindings — mirroring how Theme is gated on its own accessors —
// so the customizer is reachable whenever rebinds can be persisted, independent of the
// sub-agent settings handlers. A separator precedes it. Shared by both the normal and
// the settings-unavailable paths of settingsItems so the entry can't drift.
func (w *Workbench) keybindingsMenuItems() []*tv.MenuItem {
	if w.handlers.GetKeybindings == nil || w.handlers.SetKeybindings == nil {
		return nil
	}
	return []*tv.MenuItem{
		tv.NewMenuItem("----------", nil),
		tv.NewMenuItem("&Keybindings…", func() { w.showKeybindingCustomizer() }),
	}
}

// RefreshTheme re-applies the active palette to the whole live UI after a theme
// change, so the change takes effect without a restart (issues #103, #204). The
// desktop background, sidebar panel chrome and (rebuilt) menu bar read the active
// palette at draw time, so rebuilding the menu recolours the chrome; but the open
// session windows and the sidebar's tree / dropdown froze their colours at
// construction, so each is re-skinned in turn — every window's transcript
// re-rendered in the new palette and its window/widget chrome re-seeded (see
// SessionWindow.refreshTheme), and the sidebar's tree/dropdown reseeded (see
// sidebar.refreshTheme) — before a full desktop redraw. This
// covers the same regions a restart fixes, where ApplyTheme runs before the
// windows are built.
func (w *Workbench) RefreshTheme() {
	w.rebuildMenu() // rebuilds the menu bar from the new palette and repaints the desktop
	w.mu.Lock()
	windows := make([]*SessionWindow, 0, len(w.sessions))
	for _, sw := range w.sessions {
		windows = append(windows, sw)
	}
	w.mu.Unlock()
	for _, sw := range windows {
		sw.refreshTheme()
	}
	// The sidebar's panel chrome reads the package chrome vars at draw time, but its
	// session/agent/watcher tree and Overall-band dropdown froze their colours at
	// construction; reseed them so the whole sidebar follows the live switch too,
	// not just the surrounding panel fill (issue #379).
	if w.sidebar != nil {
		w.sidebar.refreshTheme()
	}
	w.desktop.Redraw()
}

// viewItems builds the View submenu: the event-type filter toggles, fold/unfold
// and yank-to-clipboard — all acting on the active session's transcript — plus
// the sidebar pin toggle. Find moved to the &Edit menu (issue #393). The
// transcript operations are also available from the keyboard while the transcript
// is focused ('/' for find, a/t/r/e, f/u, y, Esc); the menu makes them discoverable.
// editItems builds the &Edit submenu (issue #393): the clipboard operations
// (Copy/Cut/Paste) and transcript search (Find), giving them a discoverable home
// in the menu bar. Each item invokes the matching focused-widget path when
// selected (by click or accelerator) and is a graceful no-op when nothing is
// selectable.
//
// Since #401 the menu bar is a display-only view — it no longer registers
// accelerators from its tree — so WithShortcut here only renders the hint. The real
// key bindings live on the desktop registry: Ctrl+V is consumed by turbotui's native
// paste path; Ctrl+C/Ctrl+X are registered by registerClipboardBindings with handlers
// that decline when nothing was copied/cut (so Ctrl+C still reaches the quit-confirm
// tail), which is why they can be plain display hints here without the old
// always-consuming pitfall. Find is the transcript.find action (ScopeFocus '/'), so
// its item is tagged via menuActionItem and shows the live chord.
func (w *Workbench) editItems() []*tv.MenuItem {
	return []*tv.MenuItem{
		tv.NewMenuItem("&Copy", func() { w.copySelection() }).
			WithShortcut("Ctrl+C", tui.KeyRune, 'c', true),
		tv.NewMenuItem("Cu&t", func() { w.cutSelection() }).
			WithShortcut("Ctrl+X", tui.KeyRune, 'x', true),
		tv.NewMenuItem("&Paste", func() { w.pasteClipboard() }).
			WithShortcut("Ctrl+V", tui.KeyRune, 'v', true),
		tv.NewMenuItem("----------", nil),
		w.menuActionItem("&Find…", actionTranscriptFind),
	}
}

// copySelection copies the focused widget's selection (or all of its content when
// nothing is selected) to the clipboard, mirroring the native Ctrl+C copy path.
// It backs the Edit→Copy menu click; it is a graceful no-op when nothing is
// focused or selectable.
func (w *Workbench) copySelection() { w.desktop.CopyFocused() }

// cutSelection cuts the focused widget's selection to the clipboard, mirroring
// the native Ctrl+X path. It backs the Edit→Cut menu click; it is a graceful
// no-op when nothing is focused or selectable.
func (w *Workbench) cutSelection() { w.desktop.CutFocused() }

// pasteClipboard pastes the clipboard into the focused editable widget, mirroring
// the Ctrl+V / bracketed-paste path. It is a graceful no-op when nothing is
// focused or the clipboard cannot be read.
func (w *Workbench) pasteClipboard() { w.desktop.Paste() }

func (w *Workbench) viewItems() []*tv.MenuItem {
	pinLabel := "Pin &Sidebar"
	if w.IsSidebarPinned() {
		pinLabel = "Unpin &Sidebar"
	}
	return []*tv.MenuItem{
		// Find now lives in the &Edit menu (issue #393); View keeps the
		// transcript filtering/toggling actions.
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
		// Window arrangement (issue #241): tile or maximize every open window across
		// the work area in one action. These are rebindable Global actions (issue
		// #401): menuActionItem tags each with its actionID and shows the live chord,
		// while rebuildBindings registers the accelerator on the desktop registry so it
		// fires even when no menu is open (like Ctrl+N). They are also in the command
		// palette / help cheatsheet via Workbench.actions().
		w.menuActionItem("Tile &Vertically", actionWindowTileVertical),
		w.menuActionItem("Tile &Horizontally", actionWindowTileHorizontal),
		w.menuActionItem("Tile &Grid", actionWindowTileGrid),
		w.menuActionItem("Maximi&ze All", actionWindowMaximizeAll),
		// Cascade overlaps the windows in a diagonal stack so every title bar stays
		// visible while the front window stays large (issue #271). Ctrl+Shift+C would
		// clash with copy, so the default accelerator is Ctrl+Shift+D (diagonal).
		w.menuActionItem("Casca&de Windows", actionWindowCascade),
		tv.NewMenuItem("----------", nil),
		tv.NewMenuItem(pinLabel, func() { w.ToggleSidebarPin() }),
		// Secondary keyboard fallback for the sidebar (issue #314). The primary
		// controls now live on the panel itself: drag the left-edge divider (the ↔
		// grip) to resize, and click the header ▣/□ glyph to pin/unpin. The two
		// Widen/Narrow entries below are a separated, clearly-labelled fallback
		// group for terminals that do not report mouse drags (?1002h/?1003h off —
		// bare SSH, minimal terminals). The divider mechanics are unchanged; this is
		// just the no-mouse resize path.
		tv.NewSeparator(),
		tv.NewMenuItem("&Widen Sidebar (keyboard)", func() { w.nudgeSidebarWidth(+sidebarNudge) }),
		tv.NewMenuItem("Narro&w Sidebar (keyboard)", func() { w.nudgeSidebarWidth(-sidebarNudge) }),
		// Failed sub-agents are never auto-folded from the sidebar (issue #484); they
		// stay visible until the user clears them. This dismisses every failed
		// sub-agent of the active session at once — the click affordance, since the
		// sidebar tree's only per-row mouse target is the monologue-bound agent row
		// and the tree does not hold keyboard focus.
		tv.NewSeparator(),
		tv.NewMenuItem("Dismiss &Failed Sub-agents", func() { w.dismissFailedSubAgents() }),
	}
}

// dismissFailedSubAgents clears every failed sub-agent of the active session from
// the sidebar and redraws (issue #484). It is the menu action behind "Dismiss
// Failed Sub-agents"; ActiveID() is the canonical "session the user is viewing",
// matching the other View-menu actions. A no-op when there is no active session
// or no sidebar.
func (w *Workbench) dismissFailedSubAgents() {
	if w.sidebar == nil {
		return
	}
	id := w.ActiveID()
	if id == "" {
		return
	}
	w.sidebar.dismissFailed(id)
	w.desktop.Redraw()
}

// sidebarNudge is the column step used by the Widen/Narrow Sidebar commands (the
// keyboard fallback for the draggable divider, issue #175).
const sidebarNudge = 2

// nudgeSidebarWidth widens (delta>0) or narrows (delta<0) the sidebar by delta
// columns, clamped to the usual range. It is the keyboard/command fallback for
// the draggable divider (issue #175).
func (w *Workbench) nudgeSidebarWidth(delta int) {
	w.setSidebarWidth(w.sidebarWidth() + delta)
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

// copyToClipboard writes text to the system clipboard through turbotui's App,
// the single owner of the terminal output stream. Routing the yank path (the 'y'
// key, Copy Last Answer/Code) through App.CopyToClipboard serializes its OSC 52
// write under the same lock that guards frame flushes, so it can no longer
// interleave with rendering and corrupt the stream (issue #453). App.CopyToClipboard
// is SSH-aware (it skips the native fallback over SSH, which would target the
// remote host) and best-effort. No-op when the app is not configured.
func (w *Workbench) copyToClipboard(text string) {
	if w.app != nil {
		w.app.CopyToClipboard(text)
	}
}

// SessionTitle returns the window title for an open session id, or "" when the id
// is unknown. The handoff controller uses it to preserve a window's title when
// creating its session on the daemon during an embedded->daemon handoff, mirroring
// the title OnCreate carries on the auto-attach path. It delegates to the
// unexported sessionTitle so the lookup has a single source of truth, and is the
// public sibling of SessionIDs.
func (w *Workbench) SessionTitle(id string) string {
	return w.sessionTitle(id)
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

// approveActivePlan executes the active session's pending plan (issue #43). It
// marks the window busy for the executing turn and hands off to the backend's
// OnApprovePlan handler (which runs the loop on a background goroutine). It
// reports when there is no plan to approve rather than calling the handler.
func (w *Workbench) approveActivePlan() {
	id := w.ActiveID()
	if id == "" {
		return
	}
	w.mu.Lock()
	sw := w.sessions[id]
	w.mu.Unlock()
	if sw == nil || !sw.planPending {
		w.showConfirm("Approve Plan", "No plan is awaiting approval.\nUse /plan to plan a task first, then approve the result.", nil)
		return
	}
	sw.startApprovedTurn()
}

// confirmQuit is the single choke point for every confirmable quit gesture
// (Ctrl+C, File→Exit, the command-palette "Quit"). When daemon wiring is present
// it opens the daemon-aware quit dialog (issue #503), which tells the user what
// quitting will actually do and offers the relevant explicit handoff; without it
// (DaemonMode nil) it falls back to today's generic Yes/No confirmation so a build
// with no daemon wiring degrades gracefully. The disconnect modal's own "Quit"
// button deliberately bypasses this (it already states the daemon survives).
func (w *Workbench) confirmQuit() {
	if w.handlers.DaemonMode == nil {
		w.showConfirm("Quit Gogent", "Are you sure you want to quit?", func(yes bool) {
			if yes && w.quit != nil {
				w.quit()
			}
		})
		return
	}
	w.showQuitDialog()
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
	// Seed the dropdown to the configured default model (issue #306) so the first
	// send goes to it and the header shows it, rather than silently defaulting to
	// index 0. The backend's NewSession already builds on the default connection;
	// without this the TUI's selectedModelName() would disagree with the backend.
	// Mirrors AdoptSession's restore-model seeding (issue #266). SetSelected does
	// not fire OnChange, so this has no side effects. An empty/unknown default
	// yields idx -1 and leaves index 0 selected, matching the backend's fallback.
	if w.handlers.GetDefaultModel != nil && sw.modelSelect != nil {
		if idx := w.modelIndexByName(w.handlers.GetDefaultModel()); idx >= 0 {
			sw.modelSelect.SetSelected(idx)
			sw.rebuildEffortOptions()
		}
	}
	w.desktop.SetFocus(sw.input)
	if w.handlers.OnCreate != nil {
		w.handlers.OnCreate(id, title)
	}
	w.rebuildMenu()
	w.persistLayout()
	return sw
}

// ForkSession opens a new session window seeded with a copy of parentID's full
// conversation history and asks the backend to fork the parent into it (issue
// #349). The new session is a peer that diverges independently from the fork
// point — the parent window is left untouched. It mirrors NewSession but (a)
// pre-fills the window with the parent's transcript (via the GetTranscript
// handler, the same source the backend forks from), (b) inherits the parent
// window's selected model + effort so the fork "continues here" on the same
// backend, and (c) calls OnFork (which forks the transcript) instead of OnCreate.
// It is a no-op returning nil when the parent window is unknown, or when no
// OnFork handler is wired — without a backend fork the window would be a UI-only
// ghost with copied history and no session behind it, so both checks run before
// any window/session mutation (see Handlers.OnFork).
func (w *Workbench) ForkSession(parentID string) *SessionWindow {
	w.mu.Lock()
	parent := w.sessions[parentID]
	if parent == nil {
		w.mu.Unlock()
		return nil
	}
	// Without a backend fork handler a new window would be a UI-only ghost with
	// copied history and no session behind it, so /fork is a documented no-op
	// (see Handlers.OnFork). Bail before any window/session mutation and keep the
	// user on the parent they forked from.
	if w.handlers.OnFork == nil {
		w.mu.Unlock()
		w.desktop.SetFocus(parent.input)
		return nil
	}
	w.nextNum++
	num := w.nextNum
	id := fmt.Sprintf("session-%d", num)
	title := fmt.Sprintf("Session %d", num)
	w.mu.Unlock()

	sw := w.openWindow(id, title)
	// Pre-fill the window with the parent's history so the user sees the context
	// the fork carries. GetTranscript reads the live parent backend transcript —
	// the same source ForkSession copies — so the display and the forked session
	// agree.
	if w.handlers.GetTranscript != nil {
		sw.restore(w.handlers.GetTranscript(parentID, "root"))
	}
	// Inherit the parent window's selected model + effort (continue-here
	// semantics): seed the model dropdown, rebuild the effort options for it, then
	// preserve the parent's effort pick when the model still offers it. SetSelected
	// does not fire OnChange, so this has no side effects.
	if parent.modelSelect != nil && sw.modelSelect != nil {
		if idx := w.modelIndexByName(parent.selectedModelName()); idx >= 0 {
			sw.modelSelect.SetSelected(idx)
		}
		sw.rebuildEffortOptions()
		if eff := parent.selectedEffort(); eff != "" && sw.effortSelect != nil {
			for i, opt := range sw.effortSelect.Options {
				if opt == eff {
					sw.effortSelect.SetSelected(i)
					break
				}
			}
		}
	}
	w.desktop.SetFocus(sw.input)
	// OnFork is non-nil (checked at entry): hand the new window to the backend so
	// it forks parentID's transcript into the matching session.
	w.handlers.OnFork(parentID, id, title)
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
	// Duplicate-window guard (issue #518): if a window for this id is already open,
	// raise it and return the SAME window rather than opening a second one (which
	// would orphan the first on-screen and leave a single map entry → split-brain).
	// This mirrors openWatcherSession's already-open branch. We deliberately do NOT
	// reload rs.Messages here: AdoptSession is also reached via the Saved Sessions
	// "Continue" button, whose rs.Messages is a (possibly stale) file read, so a
	// reload could clobber a live transcript the user is typing into. The §7
	// reconnect jump-to-present reloads open windows inline in refreshAfterReconnect
	// and only routes new ids through AdoptSession, so it is unaffected.
	existing := w.sessions[rs.ID]
	w.mu.Unlock()
	if existing != nil {
		w.Focus(rs.ID)
		return existing
	}
	title := rs.Title
	if title == "" {
		title = rs.ID
	}
	sw := w.openWindow(rs.ID, title)
	// A deferred session (issue #517) opens as a labelled shell: its transcript is
	// fetched once on first focus (ensureTranscript), and OnCreate is deferred to
	// that same moment so first connect pays no up-front round-trip for it. Remote
	// live events still route to this window by id over the global SSE stream, so the
	// missing OnCreate does not lose streamed activity.
	if rs.Deferred {
		w.mu.Lock()
		w.deferredTranscripts[rs.ID] = true
		w.mu.Unlock()
		sw.markDeferred()
	} else {
		sw.restore(rs.Messages)
	}
	// Preselect the model the session was last using (issue #266) so the next
	// send goes to it and the dropdown shows it, rather than defaulting to index 0.
	// SetSelected does not fire OnChange, so this has no side effects.
	if idx := w.modelIndexByName(rs.Model); idx >= 0 && sw.modelSelect != nil {
		sw.modelSelect.SetSelected(idx)
		sw.rebuildEffortOptions()
	}
	if !rs.Deferred && w.handlers.OnCreate != nil {
		w.handlers.OnCreate(rs.ID, title)
	}
	w.rebuildMenu()
	return sw
}

// modelIndexByName returns the header-dropdown index whose backing config has the
// given stable config Name, or -1 when name is empty or unmatched. It matches on
// Name against w.models directly — which is positional with the dropdown options
// — so the lookup is unambiguous even when two models share a DisplayName.
// Re-deriving the index from the display label would collapse such duplicates
// onto the first match, dropping the user's actual model (issue #389).
func (w *Workbench) modelIndexByName(name string) int {
	if name == "" {
		return -1
	}
	for i, m := range w.models {
		if m != nil && m.Name == name {
			return i
		}
	}
	return -1
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
	// Collision tripwire (issue #518): this is the sole w.sessions[id]= write site,
	// so it is the last line of defense against a duplicate window. The callers
	// (NewSession, ForkSession, AdoptSession, OpenAnalysisSession) all generate or
	// guard ids that are unique here, so reaching this is a programming error
	// upstream. Return the existing window — keeping the *SessionWindow contract so
	// callers don't nil-panic and never orphaning the on-screen window — and log
	// rather than silently overwriting the map entry.
	if existing := w.sessions[id]; existing != nil {
		w.mu.Unlock()
		log.Printf("tui: openWindowAny: window for session %q already exists; returning existing (duplicate-open guard, issue #518)", id)
		return existing
	}
	// Cascade windows so they don't perfectly overlap. New windows open in the
	// area left of the sidebar (with a fallback to the full width on a terminal
	// too narrow to spare it); dragging/resizing/maximizing then keep them there
	// via the pinned window area (issue #106).
	offset := len(w.order) % 6
	avail := w.app.Width() - w.sidebarWidth()
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
	// Keep the sidebar's focus/TODO tracking current immediately (cheap: it only sets
	// the focused row and moves the tree highlight, with no statistics work), but defer
	// the expensive Overall recompute (GetStatistics + lifetime fold) via the 250ms
	// coalescer. A connect/restore burst opens many windows back-to-back, so folding N
	// per-window recomputes into one ~250ms after the burst settles avoids N full
	// aggregate rebuilds, matching how live session events refresh the panel
	// (deliverSessionEvent / issue #53). The window, its transcript and its sidebar row
	// are already on screen via AddLayer + addSession; only the Overall aggregate count
	// lags ≤250ms (issue #521). focusSession stays inline so the focus/TODO highlight is
	// not lost when no statistics handler is wired (scheduleOverallRefresh no-ops then,
	// whereas refreshOverall updates focus before its statistics guard).
	if w.sidebar != nil {
		w.sidebar.focusSession(w.ActiveID())
	}
	w.scheduleOverallRefresh()
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
	// Lazily load a deferred session's transcript the first time it is focused
	// (issue #517). This is the single user-driven focus chokepoint — cycle, the
	// session menu, the sidebar and watcher-open all route through it — while
	// construction-time focus uses desktop.SetFocus directly, so restoring the shells
	// never triggers a fetch.
	w.ensureTranscript(id)
	// The middle TODO region (issue #190) and the Overall panel's "model"/"api"
	// rows (issue #107) both follow the active session; refreshOverall resolves
	// both from the raised top window, so refresh before the redraw below.
	w.refreshOverall()
	w.desktop.Redraw()
}

// ensureTranscript fetches and renders a deferred window's transcript the first
// time it is needed (issue #517), then clears its deferred mark so it never fetches
// twice. It is a no-op for a window that was eagerly restored or already loaded.
//
// The work runs off the UI thread: a deferred window's transcript is one daemon
// round-trip (a fresh SSH channel over ssh://), and doing it synchronously inside
// Focus would freeze the whole UI on every first-focus — exactly the stall this
// change exists to remove. The fetched transcript is applied with reload (clear +
// restore) on the UI thread via desktop.Post, replacing the placeholder and any
// live deltas that streamed into the shell with the daemon's authoritative copy.
func (w *Workbench) ensureTranscript(id string) {
	w.mu.Lock()
	if !w.deferredTranscripts[id] {
		w.mu.Unlock()
		return
	}
	// Clear the mark before fetching so a concurrent or repeat focus cannot start a
	// second fetch (exactly-once).
	delete(w.deferredTranscripts, id)
	sw := w.sessions[id]
	w.mu.Unlock()
	if sw == nil || w.handlers.GetTranscript == nil {
		return
	}
	title := sw.title
	go func() {
		// The window may have been closed between Focus and now (deferred OnCreate is
		// async): re-check before issuing OnCreate so we don't resurrect a just-closed
		// session by re-creating it on the daemon.
		w.mu.Lock()
		alive := w.sessions[id] != nil
		w.mu.Unlock()
		if !alive {
			return
		}
		// OnCreate was deferred along with the transcript (AdoptSession); make the
		// daemon attach this session's observer now, before the first transcript read.
		if w.handlers.OnCreate != nil {
			w.handlers.OnCreate(id, title)
		}
		msgs := w.handlers.GetTranscript(id, "root")
		w.desktop.Post(func() {
			w.mu.Lock()
			sw := w.sessions[id]
			if sw != nil && msgs == nil {
				// A failed fetch (the handler returns nil only on error) leaves the
				// placeholder in place (reload is a no-op on nil); re-arm the deferred
				// flag so a refocus retries. A successful but empty transcript (non-nil)
				// is NOT re-armed: reload clears the placeholder to a genuine empty window.
				w.deferredTranscripts[id] = true
			}
			w.mu.Unlock()
			if sw != nil && !sw.readOnly {
				sw.reload(msgs)
			}
		})
	}()
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
//
// Teardown is a sequence of toolkit mutations (window layer, menu, sidebar
// node), and each one synchronously flushes a full frame to the terminal. To
// avoid the one-frame flash of a wrong session (issue #209), the session that
// becomes active after the close is decided up front and raised to the top of
// the z-stack *before* the closing layer is removed.
//
// The post-close active session is the tail of w.order (the last entry in the
// sidebar order) — the same session the close has always settled on; only when
// and how it is shown changes here. Raising it first (via Focus) makes it the
// top-most window for every subsequent frame, so removing the closed layer can
// only ever reveal the target — never the arbitrary z-stack neighbour that
// happened to sit directly beneath the closed window, whose ordering diverges
// from w.order as soon as the user clicks/cycles between sessions. The general
// "sidebar highlight follows focus" fix is the sibling issue #206; here Focus
// also moves the sidebar's Overall/TODO focus onto the target via refreshOverall.
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
	// Decide the post-close active session now, before any teardown, so the
	// choice never depends on the transient z-stack state the removal would
	// otherwise expose (issue #209).
	target := ""
	if len(w.order) > 0 {
		target = w.order[len(w.order)-1]
	}
	w.mu.Unlock()
	// Dismiss a still-open @-mention popup so closing mid-completion leaves no
	// orphaned layer (issue #46).
	if sw.completer != nil {
		sw.completer.hide()
	}
	// Raise the intended next session to the top *before* removing the closing
	// layer. From here target is the top-most window, so the RemoveLayer below
	// can only reveal target rather than the z-stack neighbour beneath the closed
	// window — eliminating the wrong-session flash (issue #209).
	if target != "" {
		w.Focus(target)
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
	// Select-on-open: the current name starts highlighted so the first keystroke
	// replaces it; an arrow / Home / End collapses the selection to edit in place
	// (issue #235).
	w.showInputDialog("Rename Session", "&Title:", current, func(value string, ok bool) {
		if !ok {
			return
		}
		if title := strings.TrimSpace(value); title != "" {
			w.SetSessionTitle(id, title)
		}
	}, withSelectAll())
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
	// Tell the backend so the new title reaches the session index (issue #272):
	// persistLayout only updates layout.json, which the Sessions browser does not
	// search. Without this the renamed session stays findable only by its old name.
	if w.handlers.OnRename != nil {
		w.handlers.OnRename(id, title)
	}
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

// sidebarWidth returns the live width of the right-hand sidebar strip (issue
// #175), clamped to the sensible range for the current terminal so a stale or
// out-of-bounds stored value can never strand the work area. A zero/unset width
// falls back to the default. Read on the UI thread.
func (w *Workbench) sidebarWidth() int {
	return w.clampSidebarWidth(w.sidebarW)
}

// clampSidebarWidth holds a requested sidebar width within [minSidebarWidth,
// max], where max keeps at least minWorkAreaWidth columns for the work area
// (issue #175). On a terminal too narrow to honour both, the work-area floor
// wins so the sidebar shrinks rather than swallowing the desktop. A zero/negative
// request is treated as the default width.
func (w *Workbench) clampSidebarWidth(req int) int {
	if req <= 0 {
		req = defaultSidebarWidth
	}
	if req < minSidebarWidth {
		req = minSidebarWidth
	}
	// Cap last so the work-area floor wins on a terminal too narrow to honour both
	// bounds: the sidebar shrinks below minSidebarWidth rather than swallowing the
	// desktop (and never goes negative on a vanishingly small terminal).
	max := w.app.Width() - minWorkAreaWidth
	if req > max {
		req = max
	}
	if req < 0 {
		req = 0
	}
	return req
}

// dragSidebarWidth recomputes the sidebar width from a drag X (the screen column
// the divider was dragged to) and applies it: the width is the distance from that
// column to the right edge (issue #175). It reflows the sidebar, re-clamps any
// pinned windows to the moved boundary and persists the new width. Runs on the UI
// thread (driven by the divider's OnClickFn).
func (w *Workbench) dragSidebarWidth(x int) {
	w.setSidebarWidth(w.app.Width() - x)
}

// setSidebarWidth stores a new (clamped) sidebar width, repositions the sidebar,
// clamps pinned windows into the new work area and persists the layout. It is a
// no-op when the clamped width is unchanged so a drag that hits a bound does not
// thrash the layout file. Runs on the UI thread (issue #175).
func (w *Workbench) setSidebarWidth(req int) {
	width := w.clampSidebarWidth(req)
	if width == w.sidebarWidth() {
		return
	}
	w.sidebarW = width
	if w.sidebar != nil {
		w.sidebar.reposition(w.app.Width(), w.app.Height())
	}
	if w.sidebarPinned {
		w.mu.Lock()
		windows := make([]*SessionWindow, 0, len(w.sessions))
		for _, sw := range w.sessions {
			windows = append(windows, sw)
		}
		w.mu.Unlock()
		for _, sw := range windows {
			sw.clampToWindowArea()
		}
		// The monologue popup is a bare *tv.Window with no *SessionWindow wrapper,
		// so it is not in w.sessions; re-clamp it directly so a width change pulls
		// it back inside the boundary too (issue #319).
		if w.monologWindow != nil {
			clampWindowToArea(w, w.monologWindow)
		}
	}
	// Persist and repaint via the coalesced paths (issue #320), matching the
	// toolkit's window-drag handling: a debounced layout write (no synchronous
	// disk I/O per motion event) and a dirty-flag redraw the run loop flushes once
	// per iteration after draining the input burst (RequestRedraw is idempotent).
	w.scheduleLayoutPersist()
	w.desktop.RequestRedraw()
}

// windowArea returns the desktop rectangle session windows are constrained to:
// the full screen when the sidebar is unpinned, or the screen minus the reserved
// right-hand sidebar strip when it is pinned (issue #106). Dragging, resizing,
// maximizing, creating and restoring a window all stay within it so a pinned
// sidebar is never covered. On a desktop too narrow to spare the sidebar the full
// width is used (mirroring maximizedWindowRect). The strip width is the live,
// draggable sidebar width (issue #175). Read on the UI thread.
func (w *Workbench) windowArea() tv.Rect {
	sw, sh := w.app.Width(), w.app.Height()
	if bw := w.sidebarWidth(); w.sidebarPinned && sw > bw {
		sw -= bw
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
		// The monologue popup has no *SessionWindow wrapper (it is not in
		// w.sessions), so pull it back inside the now-reserved area directly so
		// pinning the sidebar on does not leave it covering the panel (issue #319).
		if w.monologWindow != nil {
			clampWindowToArea(w, w.monologWindow)
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
	layout := gogent.Layout{
		Entries:      make([]gogent.LayoutEntry, 0, len(w.order)),
		SidebarWidth: w.sidebarW,
		OverallModel: w.sidebarOverallModel(),
	}
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
			Effort:    sw.selectedEffort(),
			Goal:      sw.goal,
			X:         bounds.X,
			Y:         bounds.Y,
			W:         bounds.W,
			H:         bounds.H,
		})
	}
	return layout
}

// sidebarOverallModel returns the model config name the Overall band is scoped to
// (issue #191), or "" for the aggregate view / before the sidebar exists. It is the
// value persisted in the layout so the selection survives a restart.
func (w *Workbench) sidebarOverallModel() string {
	if w.sidebar == nil {
		return ""
	}
	return w.sidebar.selectedOverallModel()
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
	// Restore the persisted sidebar width first (issue #175): it is independent of
	// the per-session entries (a layout may carry a width with no sessions), and
	// reposition + clamp below must already see the restored boundary. An
	// unset/0 width keeps the default. clampSidebarWidth guards out-of-range values
	// (e.g. a width saved on a wider terminal).
	if layout.SidebarWidth > 0 {
		w.sidebarW = w.clampSidebarWidth(layout.SidebarWidth)
		if w.sidebar != nil {
			w.sidebar.reposition(w.app.Width(), w.app.Height())
		}
	}
	// Restore the Overall band's per-model scope (issue #191). A model that no
	// longer exists falls back to the aggregate view inside setSelectedOverallModel.
	if w.sidebar != nil {
		w.sidebar.setSelectedOverallModel(layout.OverallModel)
	}
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
		// Restore the per-session reasoning-effort override (issue #177). It is a
		// no-op when the saved effort is not among the current model's options
		// (e.g. the model's effort set changed since the layout was written).
		sw.applyEffort(e.Effort)
		// Restore the per-session supervisor goal (issue #172) so the idle watchdog
		// resumes supervising the same objective after a restart.
		sw.goal = e.Goal
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
// safe to call from any goroutine: the update is marshalled onto the UI thread via
// desktop.Post, where deliverSessionEvent applies it.
func (w *Workbench) EmitSessionEvent(id string, ev agent.SessionEvent) {
	w.desktop.Post(func() { w.deliverSessionEvent(id, ev) })
}

// deliverSessionEvent applies a core session event to the matching window and
// refreshes the affected chrome. It is the body of the EmitSessionEvent post
// callback, split out so the live delivery seam — window lookup, apply, notify and
// the coalesced Overall refresh — can be driven directly under test: the desktop
// post-queue has no headless drain, so this method is the seam a test targets to
// assert that an event (notably a final answer) reaches the rendered transcript
// (issue #227). It must run on the UI thread. It returns whether a window received
// the event; a false result means the id had no open window — counted via
// noteUndeliveredEvent so the drop is observable instead of silent.
func (w *Workbench) deliverSessionEvent(id string, ev agent.SessionEvent) bool {
	w.mu.Lock()
	sw := w.sessions[id]
	pinned := w.pinned[id]
	title := ""
	if sw != nil {
		title = sw.title
	}
	w.mu.Unlock()
	if ev.Type == agent.SessionEventSubAgent && w.sidebar != nil {
		w.sidebar.applySubAgent(id, ev)
		// Persistent "needs input" badge on the owning session row (issue #207).
		// A sub-agent entering StatusWaiting has asked a CLARIFY question; leaving
		// it (resumed/completed/failed) resolves it. A session can host several
		// interactive sub-agents, so the badge is reference-counted per session
		// (sidebar.setClarify) and clears only when the last waiting sub-agent
		// resolves. The resume between two CLARIFY rounds is not emitted as a
		// sub-agent event, so collapse repeated same-state events per sub-agent
		// here, bumping the count once per waiting sub-agent and dropping it once
		// when that sub-agent leaves StatusWaiting — keeping the count balanced.
		key := ev.AgentID
		if key == "" {
			key = id + "/" + ev.Name
		}
		waiting := ev.Status == agent.StatusWaiting
		if w.clarifyWaiting == nil {
			w.clarifyWaiting = make(map[string]bool)
		}
		if waiting != w.clarifyWaiting[key] {
			if waiting {
				w.clarifyWaiting[key] = true
			} else {
				delete(w.clarifyWaiting, key)
			}
			w.sidebar.setClarify(id, title, pinned, waiting)
		}
	}
	if ev.Type == agent.SessionEventTodo && w.sidebar != nil {
		w.sidebar.applyTodo(id, ev.Todos)
	}
	delivered := sw != nil
	if delivered {
		sw.apply(ev)
		// Push the per-session idle/active marker (issue #236) on the event that
		// actually flips sw.busy, so the ●/○ updates at the transition instead of
		// waiting up to one statusTickInterval for the next tickBusyStatuses sweep:
		// a final/error clears it here the instant the turn ends, and the turn's first
		// streamed event confirms it set. sw.apply has just settled sw.busy for this
		// event, and sw.busy is UI-thread-only like the rest of this method, so the read
		// needs no lock. Relabel only on a real transition so the per-token event stream
		// does not rebuild the label each time; tickBusyStatuses stays the backstop and
		// still reconciles the all-idle edge.
		if w.sidebar != nil && w.sidebar.busy[id] != sw.busy {
			w.sidebar.setBusy(id, title, pinned, sw.busy)
		}
		// Push the background marker (◐, issue #353) on the same event-driven edge as
		// the busy marker, so the glyph flips the instant a SessionEventBackground
		// settles sw.background rather than waiting up to one tick for tickBusyStatuses.
		// sw.apply has just settled sw.background, which (like sw.busy) is UI-thread-only,
		// so the read needs no lock; relabel only on a real transition.
		if w.sidebar != nil && w.sidebar.background[id] != sw.background {
			w.sidebar.setBackground(id, title, pinned, sw.background)
		}
	} else if eventNeedsWindow(ev.Type) {
		// Only count a missing window as a dropped event when the window was actually
		// needed: sub-agent and todo events are fully serviced by the sidebar above,
		// independent of the window, so a missing window loses nothing for them and
		// counting them would cry wolf on legitimate windowless sidebar traffic.
		w.noteUndeliveredEvent(id, ev)
	}
	w.maybeNotify(id, ev)
	// An event may have moved the aggregate (usage tokens/requests, a sub-agent
	// spawn, an error); coalesce the Overall-panel refresh rather than paying
	// for one per event (issue #53 / redraw note in #22).
	w.scheduleOverallRefresh()
	return delivered
}

// eventNeedsWindow reports whether a session event's effect requires the session
// window — its transcript or status line, reached via SessionWindow.apply. Sub-agent
// and todo events are serviced entirely by the sidebar in deliverSessionEvent,
// independent of the window, so a missing window loses nothing for them; every other
// type renders through apply, so a missing window drops it. This keeps the
// undelivered tripwire precise (issue #227): a non-zero count means a render was
// actually lost, not that a windowless sidebar update happened to pass through.
func eventNeedsWindow(t agent.SessionEventType) bool {
	switch t {
	case agent.SessionEventSubAgent, agent.SessionEventTodo:
		return false
	default:
		return true
	}
}

// noteUndeliveredEvent records a session event that arrived for an id with no open
// window so the drop is observable instead of silent (issue #227). It is the one
// place deliverSessionEvent funnels the nil-window case through, so a test can
// assert on UndeliveredEventCount and a future log/metric has a single hook.
func (w *Workbench) noteUndeliveredEvent(id string, ev agent.SessionEvent) {
	w.mu.Lock()
	w.undelivered++
	w.mu.Unlock()
}

// UndeliveredEventCount returns how many window-needing session events reached
// deliverSessionEvent with no open window for their id, so their apply (transcript
// or status render) was lost. Sub-agent/todo events are excluded (the sidebar
// services them without a window). It stays zero in normal operation — a live
// session keeps its window for the whole turn — so a non-zero count signals the
// lifecycle regression that issue #227 traced a dropped final answer to.
func (w *Workbench) UndeliveredEventCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.undelivered
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
	// When attached to a daemon, completion notifications arrive over the wire as
	// "notification" SSE frames (NotifyFromWire); notifying off the session event
	// too would double up, so suppress this path (issue #358 §9).
	w.mu.Lock()
	suppressed := w.suppressEventNotify
	w.mu.Unlock()
	if suppressed {
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

// NotifyFromBackend delivers a backend-originated notification (a free-running
// watcher completion, issue #329) through the TUI's single notifier. It posts
// onto the UI thread via desktop.Post so the terminal escapes are written
// coordinated with the render loop — never interleaved mid-frame from a watcher
// goroutine — and reuses the one notifier instance (so there is no second,
// uncoordinated notification path). reason is the stable wire token ("watcher");
// a backend event has no owning window, so it is never focus-suppressed. A stray
// Post after the desktop has quit is benign.
func (w *Workbench) NotifyFromBackend(reason, title, body string) {
	w.desktop.Post(func() {
		if w.notify == nil {
			return
		}
		if w.notify.ShouldNotify(notify.Reason(reason), false) {
			w.notify.Notify(title, body)
		}
	})
}

// SetEventNotificationsSuppressed toggles maybeNotify's per-session-event
// notifications (issue #358 §9). The attach path sets it true so completion
// notifications come solely from the daemon's over-the-wire "notification" frames
// (NotifyFromWire) and are not also raised off the normal final/error session
// events — which would double up. Embedded mode leaves it false.
func (w *Workbench) SetEventNotificationsSuppressed(suppress bool) {
	w.mu.Lock()
	w.suppressEventNotify = suppress
	w.mu.Unlock()
}

// NotifyFromWire surfaces a daemon-emitted notification (issue #358 §9) on this
// (the TUI's) machine: a watcher/agent completion the daemon delivered as a
// "notification" SSE frame. It is gated by the local notify config and, when the
// notification carries an originating session id, by focus suppression — a
// completion for the active window is suppressed exactly as the in-process path
// would (sessionID "" means a windowless backend event, e.g. a free-running
// watcher, which is never focus-suppressed). Posted on the UI thread so the focus
// check is accurate and terminal writes stay coordinated with the render loop.
func (w *Workbench) NotifyFromWire(reason, title, body, sessionID string) {
	w.desktop.Post(func() {
		if w.notify == nil {
			return
		}
		focused := sessionID != "" && w.ActiveID() == sessionID
		if w.notify.ShouldNotify(notify.Reason(reason), focused) {
			w.notify.Notify(title, body)
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
		// Stop the coalesced layout-persist timer too (issue #320) so it cannot Post
		// after the loop is gone. The final width is already captured by the
		// defer w.persistLayout() above, which runs after this teardown. Lazily
		// created, so guard nil.
		if w.layoutPersist != nil {
			w.layoutPersist.Stop()
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
		// Skip ids already adopted this pass (issue #518): defends against a
		// duplicate id appearing twice in the restored list and against a
		// concurrent reconnect-triggered restore that already opened a window.
		// AdoptSession is itself idempotent on an open id, so this is a cheap,
		// explicit belt-and-suspenders that also documents the loop's intent.
		seen := make(map[string]bool, len(restored))
		for _, rs := range orderByLayout(restored, layout) {
			if seen[rs.ID] {
				continue
			}
			seen[rs.ID] = true
			w.AdoptSession(rs)
		}
		w.applyLayout(layout)
		// applyLayout fixes the z-order but does not focus, so resolve the landing
		// window afterwards and load its transcript if it happens to be a deferred
		// shell (issue #517) — the user must not land on a placeholder.
		if active := w.ActiveID(); active != "" {
			w.ensureTranscript(active)
		}
	}
	// Open an initial session so the user has somewhere to type.
	if len(w.order) == 0 {
		w.NewSession()
	}
	w.desktop.SetUnhandledKeyFn(func(event tui.TypeEvent) {
		// '?' and ':' are handled one step earlier, at the Fallthrough dispatch stage
		// (registered by rebuildBindings), which runs before this callback. Only Ctrl+C
		// remains here: it must stay at the unhandledKeyFn tail so it requests quit
		// confirmation only when no focused widget consumed it (the quit-only-when-
		// unconsumed rule).
		if event.Key != tui.KeyRune || event.Alt || !event.Ctrl {
			return
		}
		if event.Rune == 'c' || event.Rune == 'C' {
			w.confirmQuit()
		}
	})
	// Live-status ticker: while any session is generating, refresh its status
	// line once a second so the elapsed timer and throughput tick. Idle
	// workbenches do no work (and no redraw) per tick. Stops with the UI loop.
	go w.runStatusTicker(w.shutdown)
	// Populate the Overall panel's first frame from the current aggregate so it
	// is not blank before the first session event arrives.
	w.refreshOverall()
	// Show the welcome/onboarding dialog over the rendered UI on first run (issues
	// #341/#342), gated on the persisted preference. The handler itself resolves the
	// true/nil ("show") vs explicit-false ("skip") semantics; a nil handler means
	// the preference is unavailable, so the dialog is skipped (it can't be opted out
	// of persistently) rather than nagging every launch. The modal layer is added
	// before the loop starts so the main UI renders behind it, and it stays
	// re-openable from the palette and Help menu regardless of this preference.
	if w.handlers.GetShowWelcome != nil && w.handlers.GetShowWelcome() {
		w.showWelcomeDialog()
	}
	err := w.desktop.Run(w.shutdown)
	w.app.Close()
	if err != nil {
		return fmt.Errorf("run desktop: %w", err)
	}
	return nil
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
				// Desktop.Post already requests a coalesced redraw once the callback
				// returns, so a synchronous Redraw here is redundant; RequestRedraw lets
				// this refresh coalesce with any other posts drained in the same loop
				// iteration (issue #521).
				w.desktop.RequestRedraw()
			})
		})
		return
	}
	w.statsRefresh.Reset(overallRefreshCoalesce)
}

// layoutPersistCoalesce bounds how often the layout file is written during a
// sidebar-divider drag (issue #320): motion reports arriving faster than this
// collapse into a single write ~200ms after the drag settles, instead of an
// atomic temp-file write + rename per cell crossed.
const layoutPersistCoalesce = 200 * time.Millisecond

// scheduleLayoutPersist (re)arms a single coalesced layout persist. The first
// resize after an idle period arms the timer; rapid follow-ups (a drag) Reset it,
// so only one write fires ~200ms after the burst settles. It is a no-op when no
// SaveLayout handler is wired. All callers run on the UI thread (the divider's
// OnClickFn, the Widen/Narrow nudge), so the lazy creation and Reset are
// single-threaded; the AfterFunc goroutine only Posts persistLayout back to the
// UI thread. The final width is still captured even if the timer never fires:
// Run's deferred persistLayout writes it at shutdown. Mirrors scheduleOverallRefresh.
func (w *Workbench) scheduleLayoutPersist() {
	if w.handlers.SaveLayout == nil {
		return
	}
	if w.layoutPersist == nil {
		w.layoutPersist = time.AfterFunc(layoutPersistCoalesce, func() {
			w.desktop.Post(func() { w.persistLayout() })
		})
		return
	}
	w.layoutPersist.Reset(layoutPersistCoalesce)
}

// refreshOverall rebuilds the Overall panel from a fresh Statistics report and
// the focused session's active model config. It only updates the sidebar's stored
// aggregate; the caller owns the redraw (mirrors SessionWindow's refreshStatus
// contract). No-op without a sidebar or statistics handler. Runs on the UI thread.
func (w *Workbench) refreshOverall() {
	if w.sidebar == nil {
		return
	}
	// Both bottom regions follow the active top-most session — the Overall band's
	// model/api rows (issue #107) and the middle TODO region (issue #190). Resolve
	// the focus from the same source (the top window) so the two never diverge:
	// session creation, raise, cycle and close all funnel through here, so the
	// TODO region tracks the window the user is actually looking at without a
	// separate Focus hop. Done before the statistics guard so the TODO region
	// still follows focus even when no statistics handler is wired.
	w.sidebar.focusSession(w.ActiveID())
	if w.handlers.GetStatistics == nil {
		return
	}
	// The Overall band's model selector scopes every metric to one model (issue
	// #191). When a specific model is selected, the "model"/"api" rows describe that
	// model's backend; the aggregate "all models" view keeps following the focused
	// session's model (issue #107).
	selected := w.sidebar.selectedOverallModel()
	modelCfg := w.activeModelConfig()
	if selected != "" {
		if m := w.modelByName(selected); m != nil {
			modelCfg = m
		}
	}
	// Fold the live (open-session-only) report through the process-lifetime
	// accumulator so closing a session does not erase the tokens / requests / errors
	// it already burned (issue #232). filterPhantomSessions first drops the backend
	// "default" session, which has no TUI window, so the panel counts only what the
	// user sees (issue #278). The Statistics dialog consumes the same folded report
	// (issue #277).
	report := w.overallLifetime.fold(filterPhantomSessions(w.handlers.GetStatistics()))
	w.sidebar.refreshOverallStats(report, modelCfg, selected)
}

// modelByName returns the model config with the given config Name, or nil. Unlike
// modelByDisplayName it matches the stable name the Statistics report and the
// persisted layout key the Overall selector on (issue #191).
func (w *Workbench) modelByName(name string) *config.ModelConfig {
	for _, m := range w.models {
		if m.Name == name {
			return m
		}
	}
	return nil
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
	busyIDs := make(map[string]bool, len(w.sessions))
	bgIDs := make(map[string]bool, len(w.sessions))
	// active collects every session with ANY work in flight — a foreground turn OR
	// background sub-agents — so the status-line/Overall refresh below keeps ticking
	// the elapsed timer for a session that is only working in the background (#353).
	active := make([]*SessionWindow, 0, len(w.sessions))
	for id, sw := range w.sessions {
		if sw.busy {
			busyIDs[id] = true
		}
		if sw.background {
			bgIDs[id] = true
		}
		if sw.busy || sw.background {
			active = append(active, sw)
		}
	}
	w.mu.Unlock()
	// Reconcile the sidebar's per-session idle/active markers (issue #236) before the
	// all-idle early return, so the busy→idle transition that empties the set still
	// clears the last ● — and redraw once if any marker moved, since an otherwise
	// idle tick does no other work. The background marker (◐, issue #353) is
	// reconciled on the same tick from the parallel bgIDs set.
	redraw := w.sidebar != nil && w.sidebar.syncBusy(busyIDs)
	if w.sidebar != nil && w.sidebar.syncBackground(bgIDs) {
		redraw = true
	}
	// Expire finished sub-agents whose fold TTL has elapsed (issue #484) on the same
	// 1s sweep, before the all-idle early return so an otherwise-idle session still
	// folds (and repaints) once its completed agents age out. This is the single
	// ticker that drives every fold — no per-agent timers, no extra goroutine.
	if w.sidebar != nil && w.sidebar.tickFolds() {
		redraw = true
	}
	// Refresh the sidebar's watcher nodes on the same tick so a fire's busy marker,
	// a watcher created/deleted from the dialog or a tool, and a schedule change all
	// surface live (issue #329 Phase 4). It re-pulls the watcher list and reconciles
	// the nodes, reporting whether anything moved so an otherwise-idle tick still
	// redraws on a watcher transition.
	if w.refreshWatcherNodes() {
		redraw = true
	}
	if redraw {
		// tickBusyStatuses runs inside desktop.Post (runStatusTicker), which already
		// requests a coalesced redraw when the callback returns, so a synchronous Redraw
		// here is redundant and forces an extra mid-drain flush while deliverSessionEvent
		// posts drain alongside this tick. RequestRedraw lets the marker update coalesce
		// into the loop's single per-iteration flush (issue #521).
		w.desktop.RequestRedraw()
	}
	if len(active) == 0 {
		return
	}
	for _, sw := range active {
		sw.refreshStatus()
	}
	// While work is in flight the aggregate keeps moving (tokens stream in), so
	// refresh the Overall panel on the same 1s tick as the status lines. As above, the
	// enclosing desktop.Post already requests the coalesced redraw, so RequestRedraw is
	// the right call here (issue #521).
	w.refreshOverall()
	w.desktop.RequestRedraw()
}

// refreshWatcherNodes re-pulls the watcher list and reconciles the sidebar's
// watcher nodes (issue #329 Phase 4): free-running watchers as top-level entries
// and each session's attached watchers as children of its node. It returns
// whether the node set or any busy marker changed, so the caller redraws exactly
// on a watcher transition. It is a no-op (returning false) before the sidebar is
// built or when no ListWatchers handler is wired. Runs on the UI thread.
//
// The visibility API is per-session (ListWatchers(sessionID) yields free-running
// watchers plus that session's attached ones), so the free set is fetched once
// with the empty session id and each open session is queried for its own attached
// watchers — the local user's sidebar sees every session's watchers, unlike the
// session-private list_watchers tool.
func (w *Workbench) refreshWatcherNodes() bool {
	if w.sidebar == nil || w.handlers.ListWatchers == nil {
		return false
	}
	free := make([]WatcherInfo, 0)
	for _, info := range w.handlers.ListWatchers("") {
		if info.Free {
			free = append(free, info)
		}
	}
	w.mu.Lock()
	order := append([]string(nil), w.order...)
	w.mu.Unlock()
	attached := make(map[string][]WatcherInfo, len(order))
	for _, sid := range order {
		for _, info := range w.handlers.ListWatchers(sid) {
			if !info.Free && info.TargetSession == sid {
				attached[sid] = append(attached[sid], info)
			}
		}
	}
	return w.sidebar.setWatchers(free, attached)
}

// openWatcherSession raises — or, when necessary, opens — the session a watcher
// reports into (issue #329 Phase 4): the target session for an attached watcher,
// or the dedicated watcher:<name> session for a free-running one. It backs the
// Watchers dialog's Open Session button and the ◷ sidebar node's click/Enter.
//
// When the session already has an open window it is raised. A free-running
// watcher's window is usually NOT open (it renders as a ◷ node, not a session
// window), so when the session is not open it is adopted from its persisted
// transcript via the Saved Sessions load path — actually opening the watcher's
// session window rather than doing nothing. An empty id is a no-op; a session with
// neither an open window nor a saved transcript (e.g. a free-running watcher that
// has never fired) reports where it will appear instead of failing silently. Runs
// on the UI thread.
func (w *Workbench) openWatcherSession(sessionID string) {
	if sessionID == "" {
		return
	}
	w.mu.Lock()
	open := w.sessions[sessionID] != nil
	w.mu.Unlock()
	if open {
		w.Focus(sessionID)
		return
	}
	// Not open: adopt the watcher's persisted session so its live/past transcript is
	// inspectable, reusing the same continue-load path as the Saved Sessions browser.
	if w.handlers.ListSavedSessions != nil && w.handlers.OpenSavedSession != nil {
		for _, m := range w.handlers.ListSavedSessions() {
			if m.ID != sessionID {
				continue
			}
			if rs, ok := w.handlers.OpenSavedSession(m.File, true); ok {
				w.AdoptSession(rs)
				return
			}
			break
		}
	}
	w.showConfirm("Watchers",
		fmt.Sprintf("Session %q is not open yet — it appears once the watcher has fired.", sessionID), nil)
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
