package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ModelConfig represents a single model configuration
type ModelConfig struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	// APIType selects the provider conventions used to talk to this backend
	// ("openai" for any OpenAI-compatible server, "zai" for the Z.AI platform).
	// Empty defaults to "openai".
	APIType     string  `json:"api_type,omitempty"`
	Endpoint    string  `json:"endpoint"`
	Model       string  `json:"model"`
	APIKey      string  `json:"api_key,omitempty"`
	Temperature float32 `json:"temperature"`
	TopP        float32 `json:"top_p,omitempty"`
	// MaxTokens is the per-request output cap sent as the API's max_tokens field.
	// It bounds only the model's response length, never the conversation size.
	MaxTokens int `json:"max_tokens"`
	// ContextWindow is the model's input context window in tokens — the budget
	// the whole conversation (system prompt + transcript + tool results) must fit
	// within. It drives context-compaction thresholds and is deliberately
	// separate from MaxTokens: a sane output cap (e.g. 4096) must not be mistaken
	// for the context window. Leave unset (0) to fall back to
	// ContextWindowOrDefault's conservative default.
	ContextWindow int `json:"context_window,omitempty"`
	// ReasoningEffort, when set, is forwarded as the reasoning_effort request
	// parameter for providers that support it (OpenAI o-series / GPT-5, Z.AI
	// GLM). Recognized values are provider-specific — commonly
	// minimal|low|medium|high, plus none|max|xhigh on Z.AI GLM-5.2. Setting it
	// also marks the model as a reasoning model, which switches the output-token
	// parameter to max_completion_tokens and drops the (rejected) temperature on
	// OpenAI reasoning tiers. Empty omits the parameter.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// EffortOptions lists the reasoning_effort values this model accepts, taken
	// from models.dev reasoning_options (type "effort"). It drives the
	// per-session effort selector (issue #177): its options are
	// ["(default)"] + EffortOptions, where "(default)" means "no override — use
	// this model's ReasoningEffort". Empty => no effort control: the selector is
	// greyed out for this model (a toggle-type reasoning, reasoning:false, or a
	// provider without supportsReasoningEffort).
	EffortOptions []string `json:"effort_options,omitempty"`
	// Thinking toggles chain-of-thought reasoning on providers that expose an
	// explicit switch (Z.AI GLM-4.5+, sent as thinking:{type:enabled|disabled}).
	// nil leaves the parameter unset (provider default); a non-nil value forces
	// it on/off and, like ReasoningEffort, marks the model as a reasoning model.
	Thinking *bool `json:"thinking,omitempty"`
	Free     bool  `json:"free"`
}

// IsReasoningModel reports whether the config opts into any reasoning control
// (reasoning effort or an explicit thinking toggle). It is the signal that the
// model expects the reasoning-model request encoding (max_completion_tokens,
// temperature handling) rather than the legacy chat-completions shape.
func (m *ModelConfig) IsReasoningModel() bool {
	if m == nil {
		return false
	}
	return m.ReasoningEffort != "" || m.Thinking != nil
}

// defaultContextWindow is the conservative built-in context window assumed when a
// ModelConfig leaves ContextWindow unset. It errs small so a long session on a
// model with an unknown window compacts sooner rather than overflowing; set
// context_window explicitly for models with larger windows.
const defaultContextWindow = 32768

// ContextWindowOrDefault returns the configured input context window, or the
// conservative built-in default when unset (<=0). Compaction thresholds are
// calibrated against this value, never against MaxTokens (the output cap).
func (m *ModelConfig) ContextWindowOrDefault() int {
	if m == nil || m.ContextWindow <= 0 {
		return defaultContextWindow
	}
	return m.ContextWindow
}

// SubAgentExecutionModel selects how sub-agents are run.
type SubAgentExecutionModel string

const (
	// SubAgentOneShotModel runs sub-agents as blocking tool calls that must
	// finish with SUCCESS/FAILURE. This is the stable default.
	SubAgentOneShotModel SubAgentExecutionModel = "one_shot"
	// SubAgentInteractiveModel launches sub-agents as asynchronous workers that
	// return an id immediately and may ask the coordinator for clarification.
	// Experimental.
	SubAgentInteractiveModel SubAgentExecutionModel = "interactive"
)

// SubAgentConfig captures the user-facing execution-model settings for
// sub-agents (see the "Settings" section of the sub-agent design).
type SubAgentConfig struct {
	// ExecutionModel is "one_shot" (default) or "interactive" (experimental).
	ExecutionModel SubAgentExecutionModel `json:"execution_model"`
	// AllowRecursive permits spawned sub-agents to themselves spawn sub-agents.
	AllowRecursive bool `json:"allow_recursive"`
	// MaxSubAgents caps how many sub-agents a single agent may spawn. <=0 means
	// use the built-in default (see MaxSubAgentsOrDefault).
	MaxSubAgents int `json:"max_subagents"`
	// MaxDepth bounds how deeply sub-agents may recursively spawn further
	// sub-agents. <=0 means use the built-in default (see MaxDepthOrDefault).
	MaxDepth int `json:"max_depth"`
	// MaxConcurrent caps how many sub-agent loops may run concurrently across the
	// whole process, independent of the structural per-parent/per-depth limits
	// which otherwise compose multiplicatively (MaxSubAgents ^ MaxDepth). It is
	// the backpressure that stops a deep fan-out from thundering against the
	// backend (issue #23). <=0 means use the built-in default (see
	// MaxConcurrentOrDefault).
	MaxConcurrent int `json:"max_concurrent"`
	// TokenBudget caps the cumulative tokens (prompt + completion) a single
	// sub-agent may spend before it stops gracefully with a BUDGET_EXCEEDED
	// result. It bounds the cost of a sub-agent that would otherwise loop to the
	// step limit with no token ceiling (issue #28). Zero (the default) leaves
	// sub-agents unbounded, preserving prior behavior — it is opt-in.
	TokenBudget int `json:"token_budget,omitempty"`
}

// defaultMaxSubAgents, defaultMaxDepth and defaultMaxConcurrent are the
// conservative built-in limits applied when the config leaves the corresponding
// field unset (<=0).
const (
	defaultMaxSubAgents  = 4
	defaultMaxDepth      = 3
	defaultMaxConcurrent = 8
)

// MaxSubAgentsOrDefault returns the configured sub-agent fan-out limit, or the
// built-in default when unset.
func (c SubAgentConfig) MaxSubAgentsOrDefault() int {
	if c.MaxSubAgents <= 0 {
		return defaultMaxSubAgents
	}
	return c.MaxSubAgents
}

// MaxDepthOrDefault returns the configured recursion-depth limit, or the
// built-in default when unset.
func (c SubAgentConfig) MaxDepthOrDefault() int {
	if c.MaxDepth <= 0 {
		return defaultMaxDepth
	}
	return c.MaxDepth
}

// MaxConcurrentOrDefault returns the configured global cap on concurrently
// running sub-agents, or the built-in default when unset.
func (c SubAgentConfig) MaxConcurrentOrDefault() int {
	if c.MaxConcurrent <= 0 {
		return defaultMaxConcurrent
	}
	return c.MaxConcurrent
}

// IsOneShot reports whether the configured execution model is one-shot. An empty
// (unset) value defaults to one-shot, the stable mode.
func (c SubAgentConfig) IsOneShot() bool {
	return c.ExecutionModel != SubAgentInteractiveModel
}

// DefaultSubAgentConfig returns the conservative defaults: one-shot agents, no
// recursion. One-shot stays the default until the interactive model is proven.
func DefaultSubAgentConfig() SubAgentConfig {
	return SubAgentConfig{
		ExecutionModel: SubAgentOneShotModel,
		AllowRecursive: false,
		MaxSubAgents:   defaultMaxSubAgents,
		MaxDepth:       defaultMaxDepth,
		MaxConcurrent:  defaultMaxConcurrent,
	}
}

// TimeoutConfig holds the configurable timeouts (in seconds) for the major
// blocking operations gogent performs. A zero value means "use the built-in
// default" via the *OrDefault accessors.
type TimeoutConfig struct {
	// ModelSeconds bounds a single model HTTP request.
	ModelSeconds int `json:"model_seconds"`
	// ToolSeconds bounds a single tool/shell execution.
	ToolSeconds int `json:"tool_seconds"`
	// SubAgentSeconds bounds how long a sub-agent may run before timing out.
	SubAgentSeconds int `json:"subagent_seconds"`
}

// Built-in timeout defaults (seconds) used when a field is left unset (<=0).
const (
	defaultModelTimeoutSec    = 300
	defaultToolTimeoutSec     = 300
	defaultSubAgentTimeoutSec = 300
)

// ModelSecondsOrDefault, ToolSecondsOrDefault and SubAgentSecondsOrDefault
// return the configured timeout or the built-in default when unset.
func (t TimeoutConfig) ModelSecondsOrDefault() int {
	if t.ModelSeconds <= 0 {
		return defaultModelTimeoutSec
	}
	return t.ModelSeconds
}

func (t TimeoutConfig) ToolSecondsOrDefault() int {
	if t.ToolSeconds <= 0 {
		return defaultToolTimeoutSec
	}
	return t.ToolSeconds
}

func (t TimeoutConfig) SubAgentSecondsOrDefault() int {
	if t.SubAgentSeconds <= 0 {
		return defaultSubAgentTimeoutSec
	}
	return t.SubAgentSeconds
}

// DefaultTimeoutConfig returns the built-in timeout defaults.
func DefaultTimeoutConfig() TimeoutConfig {
	return TimeoutConfig{
		ModelSeconds:    defaultModelTimeoutSec,
		ToolSeconds:     defaultToolTimeoutSec,
		SubAgentSeconds: defaultSubAgentTimeoutSec,
	}
}

// WindowConfig controls the appearance and behavior of session windows.
type WindowConfig struct {
	// Resizable allows users to drag the window corners to resize session windows.
	Resizable bool `json:"resizable"`
	// Minimizable allows users to collapse session windows to their title bar.
	Minimizable bool `json:"minimizable"`
	// Maximizable allows users to expand session windows to fill the available
	// desktop (minus the reserved "Sessions & Agents" sidebar) and restore them
	// back via the maximize/restore button in the title bar (issue #105).
	Maximizable bool `json:"maximizable"`
	// MinWidth is the minimum allowed width for a window.
	MinWidth int `json:"min_width"`
	// MinHeight is the minimum allowed height for a window.
	MinHeight int `json:"min_height"`
}

// NotifyConfig controls desktop/terminal notifications emitted when a long task
// finishes or a session needs attention (issue #59). Each delivery channel
// (bell, OSC desktop notification, native OS notifier) and each event type can
// be toggled independently.
type NotifyConfig struct {
	// Enabled is the master switch; when false no notification is emitted
	// regardless of the per-event toggles below.
	Enabled bool `json:"enabled"`
	// Channels. These are independent so a user can, say, take the bell but skip
	// the desktop popup.
	Bell    bool `json:"bell"`    // terminal bell (\a)
	Desktop bool `json:"desktop"` // OSC 9 / OSC 777 desktop notification
	Native  bool `json:"native"`  // shell out to notify-send / terminal-notifier
	// Per-event toggles. A notification fires only when its toggle is on.
	OnComplete bool `json:"on_complete"` // a task finished
	OnError    bool `json:"on_error"`    // a task errored
	OnApproval bool `json:"on_approval"` // a permission prompt needs an answer
	OnClarify  bool `json:"on_clarify"`  // a sub-agent asked a question (CLARIFY)
	// SuppressWhenFocused skips notifications whose originating session is the
	// currently focused window, so a session you are already watching does not
	// also ding. Approval prompts are never suppressed this way (they block the
	// agent and always need a response).
	SuppressWhenFocused bool `json:"suppress_when_focused"`
}

// DefaultNotifyConfig returns sensible defaults: notifications on for every
// event via the bell and OSC desktop sequences, with the native OS notifier off
// (it needs an external binary the user may not have installed).
func DefaultNotifyConfig() NotifyConfig {
	return NotifyConfig{
		Enabled:             true,
		Bell:                true,
		Desktop:             true,
		Native:              false,
		OnComplete:          true,
		OnError:             true,
		OnApproval:          true,
		OnClarify:           true,
		SuppressWhenFocused: false,
	}
}

// notifyPtr returns a pointer to n. It exists because Go does not allow taking
// the address of a function-call result, so GetDefaultConfig cannot spell
// &DefaultNotifyConfig() inline.
func notifyPtr(n NotifyConfig) *NotifyConfig { return &n }

// BudgetConfig holds the per-session usage budget that drives the context-window
// / status-bar budget alert (issue #63, the UI side of #28). A zero value
// (TokenBudget <= 0) disables budget alerting entirely, which is the default for
// existing configs that predate the setting — budget alerting is opt-in.
type BudgetConfig struct {
	// TokenBudget is the per-session cumulative token budget (prompt + completion
	// tokens) at/over which the status line raises a budget alert. Zero means no
	// budget; each session is compared against this figure independently.
	TokenBudget int `json:"token_budget,omitempty"`
	// WarnFraction (0..1) is the fraction of TokenBudget at which the gauge turns
	// amber before the limit is hit. <=0 falls back to the built-in default.
	WarnFraction float64 `json:"warn_fraction,omitempty"`
}

// RateLimitConfig governs how fast gogent issues model requests to the provider,
// process-wide. It is the throttle that keeps a wide sub-agent fan-out — or
// several cluster nodes firing at once — from stampeding a provider into 429s
// (issue #28). A zero value (RequestsPerMinute <= 0) disables throttling, which
// is the default so an older config.json without the key is unaffected.
type RateLimitConfig struct {
	// RequestsPerMinute is the sustained ceiling on model requests per minute
	// across all sessions. <=0 disables throttling.
	RequestsPerMinute int `json:"requests_per_minute,omitempty"`
	// Burst is how many requests may fire back-to-back before the per-minute rate
	// applies. <=0 falls back to RequestsPerMinute (a one-minute burst).
	Burst int `json:"burst,omitempty"`
}

// defaultBudgetWarnFraction is the fraction of the token budget at which the
// status gauge turns amber (the red "exceeded" alert fires at the full budget).
const defaultBudgetWarnFraction = 0.8

// WarnFractionOrDefault returns the configured warn fraction, or the built-in
// default when unset (<=0). Values outside (0,1] are clamped to the default.
func (b BudgetConfig) WarnFractionOrDefault() float64 {
	if b.WarnFraction <= 0 || b.WarnFraction > 1 {
		return defaultBudgetWarnFraction
	}
	return b.WarnFraction
}

// Auxiliary model roles. These name the lightweight, latency-sensitive, or
// high-volume tasks that may run on a smaller/cheaper "fast" model instead of
// the primary reasoning model. Use them with Config.ModelForRole.
const (
	RoleCompression       = "compression"
	RoleWebFetchSummarize = "web_fetch_summarize"
	RoleTitle             = "title"
	RoleJSONRepair        = "json_repair"
)

// FastModelRef is the sentinel a model_roles entry uses to point at the
// configured fast_model rather than naming a specific Models[] entry directly.
const FastModelRef = "fast_model"

// DiagnosticsConfig configures the `diagnostics` tool (issue #42), which runs the
// project's compiler/linter and returns structured errors. The zero value leaves
// the tool working out of the box with the Go default (`go vet ./...`), so an
// older config.json without a "diagnostics" key is unaffected; the fields only
// customize the command and how its output is classified.
type DiagnosticsConfig struct {
	// Command is the argument vector run to produce diagnostics. Empty defaults
	// to ["go", "vet", "./..."]. Use, e.g. ["go", "build", "./..."] to typecheck
	// only, or any linter that emits `path:line:col: message` lines.
	Command []string `json:"command,omitempty"`
	// WarningPattern, when set, is a regular expression tested against each
	// parsed message; a match marks the diagnostic a warning rather than an
	// error. Empty treats every diagnostic as an error.
	WarningPattern string `json:"warning_pattern,omitempty"`
}

// VerifyConfig configures the `verify` tool (issue #44), which runs the
// project's test command and returns structured pass/fail results. The zero
// value leaves the tool working out of the box with the Go default
// (`go test ./...`), so an older config.json without a "verify" key is
// unaffected; the field only customizes the command.
type VerifyConfig struct {
	// Command is the argument vector run as the test suite. Empty defaults to
	// ["go", "test", "./..."]. Use any command whose exit status signals suite
	// pass/fail; output is parsed for `go test`-style failures.
	Command []string `json:"command,omitempty"`
}

// MCPServerConfig declares one Model Context Protocol (MCP) server whose tools
// gogent surfaces through its own tool registry (issue #36). Transport selects
// the wire — "stdio" (default, a launched subprocess) or "http"/"streamable-http"
// — and the remaining fields are transport-specific. Launching a server is gated
// through the permission service, so adding an entry here advertises a server but
// does not silently run it.
type MCPServerConfig struct {
	Name string `json:"name"`
	// Transport is "stdio" (default) or "http"/"streamable-http".
	Transport string `json:"transport,omitempty"`
	// Command/Args/Env configure a stdio server.
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// URL/Headers configure an http server.
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Disabled skips this server entirely without removing its configuration.
	Disabled bool `json:"disabled,omitempty"`
}

// ThemeConfig selects and customises the TUI colour palette (issue #66). The
// zero value is the built-in "default" palette with colour enabled, so an older
// config.json without a "theme" key is unaffected. Colour is still disabled at
// runtime by the NO_COLOR env var or the --no-color flag regardless of this.
type ThemeConfig struct {
	// Name selects a built-in palette: "default" (the original colours),
	// "high-contrast" (a colourblind-safe, high-contrast preset; aliases:
	// "colorblind", "high_contrast"), or "dark" (a plain black-background dark
	// theme; aliases: "midnight", "black"). Empty means "default".
	Name string `json:"name,omitempty"`
	// NoColor disables all colour (terminal defaults only), the config-file
	// equivalent of the NO_COLOR env var / --no-color flag.
	NoColor bool `json:"no_color,omitempty"`
	// NoShadow fully disables drop shadows under windows, dialogs, menus and
	// buttons (issue #215). Default false keeps today's shadows; setting it true
	// renders a flat UI for terminals/fonts that draw the shadow cells poorly. It
	// is applied live via the theme-apply path, so toggling it needs no restart.
	NoShadow bool `json:"no_shadow,omitempty"`
	// Overrides recolours individual roles on top of the selected palette. Keys
	// are role names — user, agent, note, tool, result, info, error, the chrome
	// roles desktop_fg/desktop_bg/panel_fg/panel_bg/title/divider/accent, and
	// code_bg (the fenced-code block background).
	// Values are "#RRGGBB" hex, a decimal ANSI index ("0".."255"), or
	// "default"/"none" for the terminal default. Unknown keys/values are ignored.
	Overrides map[string]string `json:"overrides,omitempty"`
}

// Config represents the full configuration
type Config struct {
	DefaultModel string `json:"default_model"`
	// FastModel optionally names a Models[] entry (by Name) used for auxiliary
	// tasks (compression, summarization, JSON repair, …). Empty means auxiliary
	// tasks run on the primary model, preserving prior behavior.
	FastModel string `json:"fast_model,omitempty"`
	// ModelRoles maps an auxiliary role (see the Role* constants) to either the
	// "fast_model" sentinel (FastModelRef) or a specific Models[] name. A role
	// absent from this map defaults to the fast model when one is configured,
	// otherwise to the primary model.
	ModelRoles   map[string]string `json:"model_roles,omitempty"`
	ModelConfigs []*ModelConfig    `json:"models"`
	SubAgents    SubAgentConfig    `json:"sub_agents"`
	Timeouts     TimeoutConfig     `json:"timeouts"`
	Window       WindowConfig      `json:"window"`
	// Budget holds the per-session token-budget settings that drive the
	// status-bar budget alert (issue #63). A zero value disables alerting, so an
	// older config.json without a "budget" key simply leaves the feature off.
	Budget BudgetConfig `json:"budget"`
	// RateLimit governs the process-wide model request rate (issue #28). A zero
	// value disables throttling, so an older config.json without the key keeps the
	// prior unthrottled behavior.
	RateLimit RateLimitConfig `json:"rate_limit,omitempty"`
	// Notify holds the desktop/terminal notification settings. It is a pointer
	// so a config.json that predates the setting (missing the "notify" key)
	// resolves to the built-in defaults rather than a zero "everything off"
	// value — see NotifyConfig.
	Notify *NotifyConfig `json:"notify,omitempty"`
	// ReviewEdits gates every write/edit behind an interactive diff-review
	// approval before it is applied (issue #64). It is opt-in: the zero value
	// (false) preserves the prior behavior of applying edits immediately, so an
	// older config.json without the key is unaffected.
	ReviewEdits bool `json:"review_edits,omitempty"`
	// MCPServers lists the Model Context Protocol servers whose tools are added
	// to the registry at startup (issue #36). Empty (the default) leaves MCP off,
	// so an older config.json without the key is unaffected.
	MCPServers []MCPServerConfig `json:"mcp_servers,omitempty"`
	// Diagnostics configures the `diagnostics` tool (issue #42). The zero value
	// keeps the Go default, so an older config.json without the key is unaffected.
	Diagnostics DiagnosticsConfig `json:"diagnostics,omitempty"`
	// Verify configures the `verify` tool (issue #44). The zero value keeps the
	// Go default (`go test ./...`), so an older config.json without the key is
	// unaffected.
	Verify VerifyConfig `json:"verify,omitempty"`
	// Theme selects and customises the TUI colour palette (issue #66). The zero
	// value is the coloured "default" palette, so an older config.json without the
	// key is unaffected.
	Theme ThemeConfig `json:"theme,omitempty"`
	// Experimental gates opt-in, not-yet-default behaviours (issue #170). The zero
	// value leaves every experimental feature off, so an older config.json without
	// the key behaves exactly as before.
	Experimental ExperimentalConfig `json:"experimental,omitempty"`
	// Supervisor tunes the harness-level supervisor (issue #172): the bounded
	// idle-watchdog that re-prompts a session toward a persisted /goal. It only
	// takes effect when Experimental.Supervisor is enabled; the zero value resolves
	// to the built-in defaults via the *OrDefault accessors.
	Supervisor SupervisorConfig `json:"supervisor,omitempty"`
	// MaxSteps caps how many model round-trips (steps/turns) an agent loop may take
	// before it stops, preventing runaway loops while still letting a session run
	// longer than the historical fixed bound (issue #249). The single value governs
	// every loop in the session — the root task loop AND every sub-agent /
	// interactive-agent loop it spawns — so setting it to 0 makes those nested loops
	// unbounded too, matching the pre-#249 behaviour where the same fixed cap applied
	// everywhere. It is a pointer so an absent "max_steps" key is distinguishable
	// from an explicit 0:
	//   nil (unset) -> the built-in default (DefaultMaxSteps, 25), preserving prior
	//                  behaviour;
	//   0           -> UNLIMITED ("yolo") — the loop is bounded only by its other
	//                  stop conditions (final answer, token budget, cancellation);
	//   N > 0       -> cap at N steps.
	// Any non-positive value (0 or, defensively, a negative typo) means unlimited.
	// Resolve it through MaxStepsOrDefault rather than reading the pointer directly.
	MaxSteps *int `json:"max_steps,omitempty"`
}

// DefaultMaxSteps is the built-in per-turn step (model round-trip) cap applied
// when Config.MaxSteps is left unset (nil). It matches gogent's historical fixed
// limit so an older config.json without a "max_steps" key behaves exactly as
// before (issue #249).
const DefaultMaxSteps = 25

// intPtr returns a pointer to v. It exists because Go does not allow taking the
// address of a constant/literal inline, so GetDefaultConfig cannot spell
// &DefaultMaxSteps directly.
func intPtr(v int) *int { return &v }

// MaxStepsOrDefault returns the effective per-turn step cap. An unset value (nil)
// yields the built-in DefaultMaxSteps; a configured value is returned verbatim,
// where 0 (and any non-positive value) means UNLIMITED — callers must treat a
// non-positive result as "no step cap" rather than "stop immediately".
func (c *Config) MaxStepsOrDefault() int {
	if c == nil || c.MaxSteps == nil {
		return DefaultMaxSteps
	}
	return *c.MaxSteps
}

// ExperimentalConfig collects opt-in features that are off by default (issue
// #170). Keeping them under one block makes the experimental surface explicit
// and easy to find; a missing "experimental" key resolves to the zero value
// (everything off).
type ExperimentalConfig struct {
	// Supervisor, when true, enables the harness-level idle watchdog (issue #172):
	// on each busy→idle transition, if a session has a /goal set, a cheap completion
	// check decides whether the goal is met and, if not, nudges the session to
	// continue — bounded by Supervisor.MaxNudges. Off by default, so a config
	// without the key keeps the previous (no-supervisor) behaviour exactly.
	Supervisor bool `json:"supervisor,omitempty"`
	// StreamThinking, when true, streams the model's chain-of-thought (reasoning)
	// tokens live into the transcript and folds the thinking entry once each turn's
	// thinking completes (issue #217). Off by default, so a config without the key
	// keeps the previous behaviour (reasoning shown only as a post-turn foldable
	// thought, if at all). It can also be toggled live with the /thinking command.
	StreamThinking bool `json:"stream_thinking,omitempty"`
}

// SupervisorConfig tunes the harness-level supervisor (issue #172). It is only
// consulted when ExperimentalConfig.Supervisor is enabled; a zero value resolves
// to the built-in defaults via the *OrDefault accessors.
type SupervisorConfig struct {
	// MaxNudges bounds how many consecutive supervisor nudges a single idle
	// session may receive before the supervisor gives up and surfaces a note to
	// the user. Zero (the default) resolves to defaultSupervisorMaxNudges. A real
	// (non-supervisor) user message resets the budget.
	MaxNudges int `json:"max_nudges,omitempty"`
}

// defaultSupervisorMaxNudges is the built-in nudge budget applied when
// SupervisorConfig.MaxNudges is left unset (<=0).
const defaultSupervisorMaxNudges = 3

// MaxNudgesOrDefault returns the configured consecutive-nudge budget, or the
// built-in default when unset (<=0).
func (c SupervisorConfig) MaxNudgesOrDefault() int {
	if c.MaxNudges <= 0 {
		return defaultSupervisorMaxNudges
	}
	return c.MaxNudges
}

// NotifyConfig returns the effective notification configuration, substituting
// DefaultNotifyConfig when none is configured (a nil pointer, e.g. an older
// config.json without a "notify" block). This keeps the feature on by default
// for existing users while still letting them opt out by setting enabled:false.
func (c *Config) NotifyConfig() NotifyConfig {
	if c == nil || c.Notify == nil {
		return DefaultNotifyConfig()
	}
	return *c.Notify
}

// SetNotifyConfig records the notification configuration.
func (c *Config) SetNotifyConfig(n NotifyConfig) {
	if c == nil {
		return
	}
	c.Notify = &n
}

// LoadConfig loads configuration from the default location
func LoadConfig(homeDir string) (*Config, error) {
	configDir := filepath.Join(homeDir, ".gogent")
	configPath := filepath.Join(configDir, "config.json")

	// Check if config file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		// Return default config
		return GetDefaultConfig(), nil
	}

	// Load config from file
	data, err := os.ReadFile(configPath) //nolint:gosec // reads caller-controlled config file path
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config: %v", err)
	}

	return &config, nil
}

// SaveConfig saves configuration to the default location
func SaveConfig(homeDir string, config *Config) error {
	configDir := filepath.Join(homeDir, ".gogent")
	if err := os.MkdirAll(configDir, 0750); err != nil {
		return fmt.Errorf("failed to create config directory: %v", err)
	}

	configPath := filepath.Join(configDir, "config.json")
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %v", err)
	}

	if err := os.WriteFile(configPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %v", err)
	}

	return nil
}

// EnvModelURL is the environment variable used to override gogent's default
// model endpoint at runtime, keeping environment-specific setups out of code.
const EnvModelURL = "GOGENT_MODEL_URL"

// FallbackModelURL is gogent's built-in default endpoint when neither the
// GOGENT_MODEL_URL env var nor a saved config provides one. This is the one
// place an environment-specific default lives — deliberately in the app's
// config layer, not in the reusable model connector.
const FallbackModelURL = "http://192.168.1.88:8080/v1/chat/completions"

// DefaultEndpoint returns gogent's default chat-completions endpoint, honoring
// the GOGENT_MODEL_URL environment variable and falling back to FallbackModelURL.
func DefaultEndpoint() string {
	if v := os.Getenv(EnvModelURL); v != "" {
		return v
	}
	return FallbackModelURL
}

// GetDefaultConfig returns the default configuration with free models
// Note: Most free models require an API key. Users should add their own API keys
// to ~/.gogent/config.json after setting up accounts.
//
// The default LAN endpoint honors the GOGENT_MODEL_URL environment variable (via
// DefaultEndpoint) so the same binary works across machines.
func GetDefaultConfig() *Config {
	return &Config{
		DefaultModel: "local-lan",
		SubAgents:    DefaultSubAgentConfig(),
		Timeouts:     DefaultTimeoutConfig(),
		Notify:       notifyPtr(DefaultNotifyConfig()),
		// Round-trip the default step cap explicitly so a freshly written
		// config.json documents the setting (issue #249); 0 here would mean
		// unlimited, so the default is the historical fixed bound.
		MaxSteps: intPtr(DefaultMaxSteps),
		Window: WindowConfig{
			Resizable:   true,
			Minimizable: true,
			Maximizable: true,
			MinWidth:    50,
			MinHeight:   12,
		},
		ModelConfigs: []*ModelConfig{
			{
				Name:        "local-lan",
				DisplayName: "Local LAN (env: GOGENT_MODEL_URL)",
				Endpoint:    DefaultEndpoint(),
				Model:       "default",
				APIKey:      "",
				Temperature: 0.7,
				MaxTokens:   262144,
				// Local llama.cpp-style servers expose a configurable context; set
				// this to the server's actual --ctx-size to calibrate compaction.
				ContextWindow: 262144,
				Free:          true,
			},
			{
				Name:        "qemu-host",
				DisplayName: "Qemu Host (10.0.2.2)",
				Endpoint:    "http://10.0.2.2:8080/v1/chat/completions",
				Model:       "default",
				APIKey:      "",
				Temperature: 0.7,
				MaxTokens:   262144,
				// Local llama.cpp-style servers expose a configurable context; set
				// this to the server's actual --ctx-size to calibrate compaction.
				ContextWindow: 262144,
				Free:          true,
			},
			{
				Name:        "localhost",
				DisplayName: "Localhost (127.0.0.1)",
				Endpoint:    "http://127.0.0.1:8080/v1/chat/completions",
				Model:       "default",
				APIKey:      "",
				Temperature: 0.7,
				MaxTokens:   262144,
				// Local llama.cpp-style servers expose a configurable context; set
				// this to the server's actual --ctx-size to calibrate compaction.
				ContextWindow: 262144,
				Free:          true,
			},
			{
				Name:        "groq-free",
				DisplayName: "Groq (API Key Required)",
				Endpoint:    "https://api.groq.com/openai/v1/chat/completions",
				Model:       "llama-3.3-70b-versatile",
				APIKey:      "",
				Temperature: 0.7,
				// models.dev: Groq caps llama-3.3-70b output at 32K.
				MaxTokens: 32768,
				// Llama 3.3 70B serves a 128K input context window.
				ContextWindow: 131072,
				Free:          false,
			},
			{
				Name:        "together-free",
				DisplayName: "Together (API Key Required)",
				Endpoint:    "https://api.together.xyz/v1/chat/completions",
				Model:       "meta-llama/Llama-3.3-70B-Instruct-Turbo",
				APIKey:      "",
				Temperature: 0.7,
				// models.dev: Together serves llama-3.3-70b with a 128K output cap.
				MaxTokens: 131072,
				// Llama 3.3 70B serves a 128K input context window.
				ContextWindow: 131072,
				Free:          false,
			},
			{
				// OpenRouter: api_type "openrouter" supplies the base URL and the
				// recommended HTTP-Referer / X-Title attribution headers; only an
				// API key (and model) are needed.
				Name:        "openrouter-free",
				DisplayName: "OpenRouter (gemma-3-27b-it)",
				APIType:     "openrouter",
				Endpoint:    "",
				Model:       "google/gemma-3-27b-it:free",
				APIKey:      "",
				Temperature: 0.7,
				// models.dev: gemma-3-27b-it caps output at 16K.
				MaxTokens: 16384,
				// Gemma 3 serves a 128K input context window.
				ContextWindow: 131072,
				Free:          false,
			},
			{
				// Z.AI: api_type "zai" supplies the base URL automatically, so
				// only an API key (and model) are needed. See https://docs.z.ai.
				Name:        "zai-glm",
				DisplayName: "Z.AI (API Key Required)",
				APIType:     "zai",
				Endpoint:    "",
				Model:       "glm-4.6",
				APIKey:      "",
				Temperature: 0.7,
				MaxTokens:   131072,
				// models.dev: GLM-4.6 serves a 200K input context window.
				ContextWindow: 204800,
				Free:          false,
			},
			{
				// Z.AI GLM-5.2 (coding plan). api_type "zai" supplies the auth,
				// max_tokens clamp and reasoning behaviour; the coding-plan models
				// live under a DIFFERENT base URL (…/api/coding/paas/v4) than the
				// general PaaS default, so set Endpoint explicitly. Only an API key
				// is then needed. See https://docs.z.ai/devpack.
				Name:        "zai-glm-5.2",
				DisplayName: "Z.AI GLM-5.2 (Coding Plan, API Key Required)",
				APIType:     "zai",
				Endpoint:    "https://api.z.ai/api/coding/paas/v4",
				Model:       "glm-5.2",
				APIKey:      "",
				Temperature: 0.7,
				// models.dev: glm-5.2 output cap (also Z.AI's clamp ceiling).
				MaxTokens: 131072,
				// GLM-5.2 serves a 1M input context window.
				ContextWindow: 1000000,
				// GLM-5.2 is a reasoning model; models.dev lists effort values
				// [high, max]. "high" gives strong coding reasoning without the
				// latency/cost of max. Safe on zai (keeps max_tokens/temperature).
				ReasoningEffort: "high",
				// models.dev reasoning_options for glm-5.2 (type "effort").
				EffortOptions: []string{"high", "max"},
				Free:          false,
			},
		},
	}
}

// ListModelNames returns a list of all model names for UI
func (c *Config) ListModelNames() []string {
	names := make([]string, len(c.ModelConfigs))
	for i, model := range c.ModelConfigs {
		names[i] = model.Name
	}
	return names
}

// GetModelConfig returns a model config by name
func (c *Config) GetModelConfig(name string) *ModelConfig {
	for _, model := range c.ModelConfigs {
		if model.Name == name {
			return model
		}
	}
	return nil
}

// primaryModel returns the configured default model, falling back to the first
// configured model when the default name is unknown.
func (c *Config) primaryModel() *ModelConfig {
	if c == nil {
		return nil
	}
	if m := c.GetModelConfig(c.DefaultModel); m != nil {
		return m
	}
	if len(c.ModelConfigs) > 0 {
		return c.ModelConfigs[0]
	}
	return nil
}

// ModelForRole resolves which model serves a given auxiliary role. An explicit
// model_roles entry takes precedence: the "fast_model" sentinel selects the
// configured fast model and any other value names a Models[] entry. With no
// explicit mapping, the role uses the fast model when one is configured,
// otherwise the primary model. Resolution always falls back to the primary
// model when a referenced model cannot be found, so a missing or misspelled
// reference degrades to current behavior rather than breaking the task.
func (c *Config) ModelForRole(role string) *ModelConfig {
	if c == nil {
		return nil
	}
	primary := c.primaryModel()
	fast := c.GetModelConfig(c.FastModel)

	if target, ok := c.ModelRoles[role]; ok {
		switch target {
		case "":
			return primary
		case FastModelRef:
			if fast != nil {
				return fast
			}
		default:
			if m := c.GetModelConfig(target); m != nil {
				return m
			}
		}
		return primary
	}

	if fast != nil {
		return fast
	}
	return primary
}
