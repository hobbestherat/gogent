package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gogent/internal/watcher"
)

// ModelConfig represents a single model configuration
type ModelConfig struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	// APIType selects the provider conventions used to talk to this backend
	// ("openai" for any OpenAI-compatible server, "zai" for the Z.AI platform).
	// Empty defaults to "openai".
	APIType  string `json:"api_type,omitempty"`
	Endpoint string `json:"endpoint"`
	Model    string `json:"model"`
	APIKey   string `json:"api_key,omitempty"`
	// Project and Location target a Google Vertex AI deployment (api_type
	// "vertex"). Project is the GCP project ID; Location is the region (e.g.
	// "us-central1", or the special "global"). They build the Vertex endpoint URL
	// when Endpoint is left empty, and are unused by every other provider.
	// Vertex authenticates with Application Default Credentials, so no API key is
	// needed (see model.ADCRoundTripper).
	Project     string  `json:"project,omitempty"`
	Location    string  `json:"location,omitempty"`
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
	// CacheTTL selects the Anthropic prompt-cache breakpoint lifetime. "" or "5m"
	// (the default) use the 5-minute ephemeral cache; "1h" uses the 1-hour cache
	// (a 2× write premium, worthwhile only across idle/resume gaps >5min — see
	// issue #545); "off"/"none" disables client-side cache_control entirely. It is
	// honored only by Anthropic and Claude-on-Vertex (api_type anthropic /
	// vertex-anthropic); providers that cache automatically ignore it.
	CacheTTL string `json:"cache_ttl,omitempty"`
}

// AnthropicCacheTTL returns the normalized prompt-cache directive resolved from
// CacheTTL: "" (the default 5-minute ephemeral cache), "1h" (the 1-hour cache),
// or "off" (disable client-side cache_control). Unknown/typo values fall back to
// "" so a misconfiguration never silently disables caching or 400s the request —
// the same conservative-default posture as ContextWindowOrDefault.
func (m *ModelConfig) AnthropicCacheTTL() string {
	if m == nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(m.CacheTTL)) {
	case "1h":
		return "1h"
	case "off", "none", "disabled":
		return "off"
	default: // "", "5m", or anything unrecognized → default 5-minute ephemeral
		return ""
	}
}

// GeminiCacheTTL returns the explicit-cache lifetime for native Gemini (Vertex
// vertex-native), as a Gemini "ttl" duration string (e.g. "3600s"), resolved
// from the SAME CacheTTL knob the Anthropic path uses (issue #545). Unlike
// Anthropic — where caching is on by default — Gemini explicit context caching
// is OPT-IN: an explicit CachedContent resource is billable storage, so it must
// never be created unless the user asked. So an EMPTY CacheTTL means OFF here
// (returns ""), and only a positive duration ("1h", "5m", "30m", "<N>s", …)
// enables it. "off"/"none"/unrecognized also return "" (disabled). Issue #547.
func (m *ModelConfig) GeminiCacheTTL() string {
	if m == nil {
		return ""
	}
	raw := strings.ToLower(strings.TrimSpace(m.CacheTTL))
	if raw == "" || raw == "off" || raw == "none" || raw == "disabled" {
		return ""
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return "" // unparseable/non-positive → disabled (never create a resource)
	}
	// Gemini wants a duration string with a seconds suffix (fractional allowed,
	// but whole seconds suffice for cache lifetimes).
	return strconv.FormatInt(int64(d.Seconds()), 10) + "s"
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
	// SubAgentBothModel exposes BOTH the blocking (spawn_subagent) and the
	// asynchronous fire-and-forget (launch_agent family) coordination tools in the
	// same session, letting the agent pick blocking delegation for "I need all
	// results now" and fire-and-forget for "kick this off and keep working"
	// (issue #284). This is the default so async delegation is reachable without a
	// mode switch.
	SubAgentBothModel SubAgentExecutionModel = "both"
)

// SubAgentConfig captures the user-facing execution-model settings for
// sub-agents (see the "Settings" section of the sub-agent design).
type SubAgentConfig struct {
	// ExecutionModel is "both" (default — expose blocking and async tools),
	// "one_shot" (blocking only) or "interactive" (async only, experimental).
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

// IsOneShot reports whether the active model is one-shot ONLY (no async tools).
// An empty (unset) value defaults to one-shot, the stable mode. The "both" and
// "interactive" models are NOT one-shot. This drives the prompt-branch and the
// UI mode label; tool exposure is decided by the Exposes* accessors below.
func (c SubAgentConfig) IsOneShot() bool {
	return c.ExecutionModel != SubAgentInteractiveModel && c.ExecutionModel != SubAgentBothModel
}

// ExposesOneShotTools reports whether the blocking spawn_subagent tool should be
// available. True for one_shot, both, and the empty (unset) default — false only
// when the session is restricted to the interactive model.
func (c SubAgentConfig) ExposesOneShotTools() bool {
	return c.ExecutionModel != SubAgentInteractiveModel
}

// ExposesInteractiveTools reports whether the asynchronous launch_agent family
// should be available. True for interactive and both; false for one_shot and the
// empty default. Combined with ExposesOneShotTools this lets a single session
// expose both styles at once (issue #284).
func (c SubAgentConfig) ExposesInteractiveTools() bool {
	return c.ExecutionModel == SubAgentInteractiveModel || c.ExecutionModel == SubAgentBothModel
}

// DefaultSubAgentConfig returns the shipped defaults: BOTH delegation styles
// available (issue #284) so fire-and-forget delegation is reachable without a
// mode switch, with no recursion. One-shot semantics are unchanged — spawn_subagent
// still blocks; "both" merely also exposes the async launch_agent family.
func DefaultSubAgentConfig() SubAgentConfig {
	return SubAgentConfig{
		ExecutionModel: SubAgentBothModel,
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

// DefaultUnattendedApprovalTimeout bounds how long a pending interactive
// approval (permission/edit-review) waits when NO client is connected to answer
// it, before the approval bridge applies its safe default (deny/reject). It is
// deliberately much longer than the connected-but-unresponsive ApprovalTimeout
// (5 min) so a daemon's long watcher turns survive a transient TUI disconnect
// rather than being auto-denied (issue #358 §8). Confirmed v1 default: 1h.
const DefaultUnattendedApprovalTimeout = time.Hour

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

// UnattendedApprovalTimeoutOrDefault returns the configured unattended approval
// timeout, or the built-in default (DefaultUnattendedApprovalTimeout, 1h) when
// it is left unset (<=0). See the Config field for what it bounds (issue #358).
func (c *Config) UnattendedApprovalTimeoutOrDefault() time.Duration {
	if c == nil || c.UnattendedApprovalTimeout <= 0 {
		return DefaultUnattendedApprovalTimeout
	}
	return c.UnattendedApprovalTimeout
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
	OnWatcher  bool `json:"on_watcher"`  // a free-running watcher fire completed (issue #329)
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
		OnWatcher:           true,
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

// LSPServerConfig declares one Language Server Protocol (LSP) server gogent can
// launch and route source files to (the LSP support design). All per-language
// knowledge lives here as data: routing is by file extension, the workspace root
// is found by walking up for RootMarkers, and the wire languageId is resolved per
// file from Languages/Language. A single generic client serves every server; the
// client never learns which language it is talking to. Launching a server is
// gated through the permission service (ActionLSP), so adding an entry advertises
// a server but does not silently run it.
type LSPServerConfig struct {
	Name string `json:"name"`
	// Language is the default LSP languageId (e.g. "go", "rust", "python").
	Language string `json:"language,omitempty"`
	// Languages optionally overrides the languageId per file extension (leading dot
	// included), e.g. ".tsx" -> "typescriptreact", for one process that serves
	// several languageIds.
	Languages map[string]string `json:"languages,omitempty"`
	// Extensions is the routing key (leading dot included).
	Extensions []string `json:"extensions,omitempty"`
	// Command/Args/Env launch the stdio server subprocess.
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// RootMarkers name files that mark a project root, searched for by walking up
	// from the file (e.g. ["go.work", "go.mod"]). Empty falls back to the gogent
	// workspace root.
	RootMarkers []string `json:"root_markers,omitempty"`
	// InitOptions feeds the initialize request's initializationOptions.
	InitOptions map[string]any `json:"initialization_options,omitempty"`
	// Settings answers workspace/configuration pulls and seeds
	// workspace/didChangeConfiguration.
	Settings map[string]any `json:"settings,omitempty"`
	// AllowedCommands scopes the higher-risk workspace/executeCommand action; an
	// empty list means no command may run.
	AllowedCommands []string `json:"allowed_commands,omitempty"`
	// Disabled skips this server entirely without removing its configuration.
	Disabled bool `json:"disabled,omitempty"`
}

// DefaultLSPServers returns the built-in LSP server configuration: a single
// `gopls` entry so Go works with zero config when gopls is on PATH. Adding
// another language (rust-analyzer, pyright, ...) is a config entry, not code.
func DefaultLSPServers() []LSPServerConfig {
	return []LSPServerConfig{
		{
			Name:        "gopls",
			Language:    "go",
			Extensions:  []string{".go"},
			Command:     "gopls",
			Args:        []string{"serve"},
			RootMarkers: []string{"go.work", "go.mod"},
		},
	}
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
	// SavedName, when non-empty, records that this (active) theme is the user's
	// saved theme of that name (issue #462). It lets the theme editor re-select the
	// matching saved entry on reopen so a later Save writes back to that saved entry
	// rather than to the parent built-in. It is empty for a plain built-in
	// selection. It is metadata only — ResolveTheme/applyOverrides ignore it — and
	// is NOT stored inside a NamedTheme.Theme (a saved theme does not point at
	// itself; the entry's own Name is the identity). Empty by default, so an older
	// config.json without the key is unaffected.
	SavedName string `json:"saved_name,omitempty"`
}

// NamedTheme is a user-saved theme (issue #462): a display name plus the theme
// config it stores. The stored Theme.Name points at the built-in palette the saved
// theme was cloned from (so paletteByName/ResolveTheme still resolve a base
// palette) and Theme.Overrides carry the customisations. Built-in palettes stay
// hardcoded and read-only; a NamedTheme is the only mutable, user-authored theme.
// The stored Theme does not set SavedName — the entry's own Name is its identity.
type NamedTheme struct {
	Name  string      `json:"name"`
	Theme ThemeConfig `json:"theme"`
}

// KeybindingsConfig holds the user's keyboard-shortcut overrides (issues #269, #401).
// Overrides maps an opaque turbotui actionID (e.g. "session.new",
// "transcript.toggle.thinking", "app.commandPalette") to a chord spec string the TUI
// layer parses — "Ctrl+Shift+R", "b", "Esc", "F1", "/", or "none" to unbind. Since #401
// this covers the global menu accelerators (New/Next/Close session, the tiling keys,
// Sub-agents, Quit) as well as the transcript and overlay keys. Only actions rebound
// away from their built-in default are recorded, so a pristine install persists nothing
// and an unknown key/value is ignored on load. The value strings are opaque to this
// package (like ThemeConfig.Overrides' colour specs): the tui layer owns parse/format,
// keeping config decoupled from turbotui.
type KeybindingsConfig struct {
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
	// UnattendedApprovalTimeout bounds how long a pending interactive approval
	// (permission/edit-review) waits when NO client is connected to answer it,
	// before the approval bridge denies/rejects it as a safe default. It applies
	// only to the unattended case: when a client IS connected, the shorter
	// connected-but-unresponsive ApprovalTimeout governs instead (issue #358 §8).
	// Zero (the default for an older config.json without the key) means "use the
	// built-in default" — DefaultUnattendedApprovalTimeout (1h) — via
	// UnattendedApprovalTimeoutOrDefault.
	UnattendedApprovalTimeout time.Duration `json:"unattended_approval_timeout,omitempty"`
	Window                    WindowConfig  `json:"window"`
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
	// LSPServers lists the Language Server Protocol servers gogent can launch to
	// answer the lsp_* tools (the LSP support design). The default ships a single
	// `gopls` entry so Go works with zero config when gopls is on PATH; a missing
	// server command is skipped with a warning. Servers are launched lazily and
	// permission-gated (ActionLSP), so listing one does not silently run it.
	LSPServers []LSPServerConfig `json:"lsp_servers,omitempty"`
	// Theme selects and customises the TUI colour palette (issue #66). The zero
	// value is the coloured "default" palette, so an older config.json without the
	// key is unaffected.
	Theme ThemeConfig `json:"theme,omitempty"`
	// SavedThemes lists the user's named custom themes (issue #462): copy-and-modify
	// palettes saved alongside the read-only built-ins. Each is a parent built-in
	// plus colour overrides; the editor lists them in its preset dropdown and Theme
	// (above) may point at one via SavedName. Empty (the default) means no custom
	// themes, so an older config.json without the key is unaffected.
	SavedThemes []NamedTheme `json:"saved_themes,omitempty"`
	// Keybindings customises the TUI keyboard shortcuts (issue #269). The zero
	// value leaves every action at its built-in default, so an older config.json
	// without the key behaves exactly as before.
	Keybindings KeybindingsConfig `json:"keybindings,omitempty"`
	// Experimental gates opt-in, not-yet-default behaviours (issue #170). The zero
	// value leaves every experimental feature off, so an older config.json without
	// the key behaves exactly as before.
	Experimental ExperimentalConfig `json:"experimental,omitempty"`
	// Supervisor tunes the harness-level supervisor (issue #172): the bounded
	// idle-watchdog that re-prompts a session toward a persisted /goal. It only
	// takes effect when Experimental.Supervisor is enabled; the zero value resolves
	// to the built-in defaults via the *OrDefault accessors.
	Supervisor SupervisorConfig `json:"supervisor,omitempty"`
	// Watchers tunes the scheduled-watcher engine (issue #329): the global
	// concurrency cap and the default overlap policy. It only takes effect when
	// Experimental.Watchers is enabled; the zero value resolves to the built-in
	// defaults via the *OrDefault accessors. Watcher definitions themselves are
	// NOT here — they live in ~/.gogent/watchers.json (see WatcherStore).
	Watchers WatchersConfig `json:"watchers,omitempty"`
	// MaxSteps caps how many model round-trips (steps/turns) an agent loop may take
	// before it stops, preventing runaway loops while still letting a session run
	// longer than the historical fixed bound (issue #249). The single value governs
	// every loop in the session — the root task loop AND every sub-agent /
	// interactive-agent loop it spawns — so setting it to 0 makes those nested loops
	// unbounded too, matching the pre-#249 behaviour where the same fixed cap applied
	// everywhere. It is a pointer so an absent "max_steps" key is distinguishable
	// from an explicit 0:
	//   nil (unset) -> the built-in default (DefaultMaxSteps, 100; issue #449);
	//                  the same as a config that predates the key;
	//   0           -> UNLIMITED ("yolo") — the loop is bounded only by its other
	//                  stop conditions (final answer, token budget, cancellation);
	//   N > 0       -> cap at N steps.
	// Any non-positive value (0 or, defensively, a negative typo) means unlimited.
	// Resolve it through MaxStepsOrDefault rather than reading the pointer directly.
	MaxSteps *int `json:"max_steps,omitempty"`
	// ShowWelcome controls whether the startup welcome/onboarding dialog is shown
	// (issues #339/#341/#342). It is a pointer for the same reason MaxSteps is:
	// "unset in an older config.json" (nil) must be distinguishable from an explicit
	// false, so a current user who has never seen the dialog still gets it. nil is
	// treated as true ("show"); GetDefaultConfig sets it to true so a freshly written
	// config documents the setting. Resolve it through the nil-tolerant
	// Gogent.GetShowWelcome accessor rather than reading the pointer directly.
	ShowWelcome *bool `json:"show_welcome,omitempty"`
	// Yolo, when true, enables yolo mode globally at startup (issue #356): the
	// per-task step cap is removed (every session runs unlimited steps) and any
	// permission request that would otherwise prompt is auto-approved. It never
	// bypasses the rules.json hard-deny guardrails (issue #355), the token budget,
	// cancellation, or the audit trail. Off by default, so an older config.json
	// without the key behaves exactly as before. The --yolo CLI flag overrides
	// this, and the TUI /yolo command toggles it per session.
	Yolo bool `json:"yolo,omitempty"`
}

// DefaultMaxSteps is the built-in per-turn step (model round-trip) cap applied
// when Config.MaxSteps is left unset (nil). It was originally gogent's historical
// fixed limit (25, issue #249); issue #449 raised it to 100 so realistic
// multi-step tasks complete before the cap interrupts them, while the cap still
// bounds a runaway loop. The #249 mechanism is unchanged — nil ⇒ this default,
// 0 ⇒ unlimited, N>0 ⇒ cap N — only the default value moved.
const DefaultMaxSteps = 100

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
	// Watchers, when true, enables scheduled "Watcher" agent actions (issue #329):
	// recurring tasks that fire on their own cadence and run a full agent loop.
	// Off by default, so a config without the key never starts watcher goroutines
	// and existing users see no behaviour change. Free-running watcher definitions
	// live in ~/.gogent/watchers.json; only the tuning block lives in config.json
	// (see WatchersConfig). The startup gate is also subject to ActionWatcher.
	Watchers bool `json:"watchers,omitempty"`
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

// WatchersConfig is the tuning block for the scheduled-watcher engine (issue
// #329). It lives under the "watchers" key in config.json and holds ONLY global
// tuning — never watcher definitions, which live in ~/.gogent/watchers.json (see
// WatcherStore). It is consulted only when Experimental.Watchers is enabled; a
// zero value resolves to the built-in defaults via the *OrDefault accessors.
type WatchersConfig struct {
	// MaxConcurrent bounds how many watcher fires may run concurrently across all
	// watchers. <=0 (the default) resolves to defaultWatcherMaxConcurrent.
	MaxConcurrent int `json:"max_concurrent,omitempty"`
	// DefaultSkipIfRunning is the default overlap policy: when a watcher's
	// previous fire is still running at its next due time, surface the dropped
	// fire as "skipped". It is a pointer so an absent key defaults to true
	// (a plain bool could not express a default of true): nil → true.
	DefaultSkipIfRunning *bool `json:"default_skip_if_running,omitempty"`
}

// defaultWatcherMaxConcurrent is the built-in global concurrency cap applied when
// WatchersConfig.MaxConcurrent is left unset (<=0).
const defaultWatcherMaxConcurrent = 4

// MaxConcurrentOrDefault returns the configured global watcher-fire concurrency
// cap, or the built-in default (4) when unset (<=0).
func (c WatchersConfig) MaxConcurrentOrDefault() int {
	if c.MaxConcurrent <= 0 {
		return defaultWatcherMaxConcurrent
	}
	return c.MaxConcurrent
}

// SkipIfRunningOrDefault returns the configured default overlap policy, or the
// built-in default (true) when unset (nil pointer).
func (c WatchersConfig) SkipIfRunningOrDefault() bool {
	if c.DefaultSkipIfRunning == nil {
		return true
	}
	return *c.DefaultSkipIfRunning
}

// WatcherStore is the on-disk shape of ~/.gogent/watchers.json: the free-running
// watcher definitions. It mirrors the per-feature-file precedent
// (workbench_layout.json) — loaded/saved atomically and leniently by
// internal/gogent. Attached (session-scoped) watchers are NOT stored here.
type WatcherStore struct {
	Items []WatcherConfig `json:"items,omitempty"`
}

// WatcherConfig defines a single free-running watcher (issue #329). The ID is a
// stable unique identifier generated on load when empty (hand-written configs may
// omit it); the Name is the editable display label. Schedule/Task/Model drive the
// recurring agent invocation; Output configures completion delivery.
type WatcherConfig struct {
	// ID is the stable unique identifier the manager keys by. Empty in a
	// hand-written file → generated on load (GenerateWatcherID) and persisted back.
	ID string `json:"id,omitempty"`
	// Name is the human-friendly display label (e.g. "daily-meeting-overview").
	// It is also the dedicated session name (watcher:<name>) for free-running
	// watchers.
	Name string `json:"name"`
	// Enabled gates whether the watcher's schedule is armed at startup. A disabled
	// watcher is registered but never fires until enabled.
	Enabled bool `json:"enabled,omitempty"`
	// Schedule is the recurring cadence (exactly one of every / daily_at).
	Schedule ScheduleConfig `json:"schedule"`
	// Task is the prompt the watcher runs through the agent loop on each fire.
	Task string `json:"task"`
	// Model is the model config name used for the fire; empty = the default model.
	Model string `json:"model,omitempty"`
	// ReportToSession decides the watcher's kind (issue #329 Phase 3). nil/omitted
	// = free-running: the watcher is a process-global resource fired into its own
	// dedicated watcher:<name> session and persisted to ~/.gogent/watchers.json. A
	// non-nil session id = attached: the watcher is session-scoped, fires into that
	// session's transcript, is visible only to it, dies with it, and is stored with
	// the session (NOT in watchers.json). The create_watcher tool defaults this to
	// the calling session so a conversational "watch X and report back here"
	// attaches to the conversation.
	ReportToSession *string `json:"report_to_session,omitempty"`
	// Output configures completion delivery (notification). nil = notify on.
	Output *WatcherOutput `json:"on_complete,omitempty"`
}

// WatcherOutput configures how a free-running watcher's completion is delivered.
// v1 only adds a notification; side-effecting delivery (email/webhook) is the
// agent's job via tools during the run.
type WatcherOutput struct {
	// Notify, when true, emits a completion notification (ReasonWatcher). The
	// Phase-1 manager calls the host notifier on a successful free-running fire.
	Notify bool `json:"notify,omitempty"`
}

// CommandStore is the on-disk shape of ~/.gogent/commands.json: the user-defined
// custom slash commands (issue #403). It mirrors the per-feature-file precedent
// (WatcherStore / watchers.json) — loaded and saved atomically and leniently by
// internal/gogent — keeping config.json free of potentially many command
// definitions. Global scope only for v1; per-project commands are a follow-up.
type CommandStore struct {
	Commands []CommandDef `json:"commands"`
}

// CommandDef is one custom slash command. The top-level fields always mirror the
// latest version's content; Versions is the append-only history (every save
// appends a snapshot and is kept forever). Name is the natural key and the token
// the user types (/<name>); it is validated to not collide with a built-in or
// another custom command.
type CommandDef struct {
	// Name is the command token typed after the slash (e.g. "create-component").
	Name string `json:"name"`
	// Description is the one-line summary shown in the editor, palette and the
	// slash-completion popup.
	Description string `json:"description,omitempty"`
	// Parameters are the declared named parameters, in binding (declaration) order.
	Parameters []CommandParam `json:"parameters,omitempty"`
	// Template is the prompt text with $name / ${name} placeholders that expand at
	// invocation and is sent to the agent as a normal user message.
	Template string `json:"template"`
	// Model overrides the session model for this invocation; "" = current model.
	Model string `json:"model,omitempty"`
	// Agent routes the expanded prompt to a named sub-agent; "" = current agent.
	Agent string `json:"agent,omitempty"`
	// Subtask, when true, forces the invocation through a sub-agent spawn.
	Subtask bool `json:"subtask,omitempty"`
	// Version is the current (latest) version number; create sets it to 1 and every
	// update/restore increments it.
	Version int `json:"version"`
	// Versions is the immutable, append-only history. Index order is chronological;
	// the last entry's content always equals the top-level fields.
	Versions []CommandVersion `json:"versions,omitempty"`
}

// CommandParam is one declared parameter of a custom command. Name is the
// placeholder identifier ($name / ${name}); Required gates the missing-value
// error; Default supplies the value when an optional parameter is omitted.
type CommandParam struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Default     string `json:"default,omitempty"`
}

// CommandVersion is an immutable snapshot of a command's content at one save. The
// history is append-only: updates and restores both append a fresh snapshot, so a
// command's full evolution is always recoverable.
type CommandVersion struct {
	Version    int            `json:"version"`
	Template   string         `json:"template"`
	Parameters []CommandParam `json:"parameters,omitempty"`
	Model      string         `json:"model,omitempty"`
	Agent      string         `json:"agent,omitempty"`
	Subtask    bool           `json:"subtask,omitempty"`
	SavedAt    string         `json:"saved_at"` // RFC3339
}

// ScheduleConfig is the JSON shape of a watcher schedule. Exactly one of Every
// (a duration like "5m") or DailyAt (an "HH:MM" wall-clock time, interpreted in
// Timezone) must be set; see Schedule for the validation rules.
type ScheduleConfig struct {
	// Every is an interval duration string ("5m", "1h", "30s"). Mutually
	// exclusive with DailyAt.
	Every string `json:"every,omitempty"`
	// DailyAt is a 24h "HH:MM" wall-clock time. Mutually exclusive with Every.
	DailyAt string `json:"daily_at,omitempty"`
	// Timezone is an IANA location name ("Europe/Zurich") used to resolve DailyAt;
	// empty = UTC. Ignored for Every.
	Timezone string `json:"timezone,omitempty"`
}

// Schedule parses the config into a watcher.Schedule or returns a validation
// error. Rules: exactly one of Every / DailyAt is set; Every parses via
// time.ParseDuration and must be strictly positive; DailyAt is "HH:MM" with
// Hour 0-23 and Min 0-59; Timezone (when non-empty) must resolve via
// time.LoadLocation (empty = UTC, DST handled by the Schedule itself).
func (s ScheduleConfig) Schedule() (watcher.Schedule, error) {
	every := strings.TrimSpace(s.Every)
	dailyAt := strings.TrimSpace(s.DailyAt)
	switch {
	case every != "" && dailyAt != "":
		return nil, fmt.Errorf("watcher schedule: set exactly one of every/daily_at, not both")
	case every == "" && dailyAt == "":
		return nil, fmt.Errorf("watcher schedule: set exactly one of every/daily_at")
	case every != "":
		d, err := time.ParseDuration(every)
		if err != nil {
			return nil, fmt.Errorf("watcher schedule: invalid every %q: %w", every, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("watcher schedule: every must be positive, got %q", every)
		}
		return watcher.IntervalSchedule{D: d}, nil
	default:
		hour, min, err := parseHHMM(dailyAt)
		if err != nil {
			return nil, err
		}
		loc := time.UTC
		if tz := strings.TrimSpace(s.Timezone); tz != "" {
			l, err := time.LoadLocation(tz)
			if err != nil {
				return nil, fmt.Errorf("watcher schedule: invalid timezone %q: %w", tz, err)
			}
			loc = l
		}
		return watcher.DailySchedule{Hour: hour, Min: min, Loc: loc}, nil
	}
}

// parseHHMM parses a 24h "HH:MM" string into hour/minute, validating the ranges.
func parseHHMM(v string) (hour, min int, err error) {
	parts := strings.Split(v, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("watcher schedule: invalid daily_at %q: want HH:MM", v)
	}
	hour, err = strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("watcher schedule: invalid daily_at hour in %q: want 00-23", v)
	}
	min, err = strconv.Atoi(parts[1])
	if err != nil || min < 0 || min > 59 {
		return 0, 0, fmt.Errorf("watcher schedule: invalid daily_at minute in %q: want 00-59", v)
	}
	return hour, min, nil
}

// GenerateWatcherID returns a fresh stable watcher identifier of the form
// "watcher-<8 hex chars>". It is generated from crypto/rand so ids never collide
// in practice; callers persist it so the id is stable across runs.
func GenerateWatcherID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is effectively impossible; fall back to a fixed
		// prefix so the caller still gets a non-empty, syntactically valid id.
		return "watcher-00000000"
	}
	return "watcher-" + hex.EncodeToString(b[:])
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
	// Local so its address can be taken for the ShowWelcome pointer field (issue
	// #339); a literal can't be addressed inline in the struct below.
	showWelcome := true
	return &Config{
		DefaultModel: "local-lan",
		SubAgents:    DefaultSubAgentConfig(),
		Timeouts:     DefaultTimeoutConfig(),
		Notify:       notifyPtr(DefaultNotifyConfig()),
		// Round-trip the default step cap explicitly so a freshly written
		// config.json documents the setting (issue #249); 0 here would mean
		// unlimited, so the default is the built-in DefaultMaxSteps (100, #449).
		MaxSteps: intPtr(DefaultMaxSteps),
		// Show the welcome/onboarding dialog by default (issue #339); the "Don't show
		// again" checkbox persists false to opt out.
		ShowWelcome: &showWelcome,
		// Ship Go support out of the box: when gopls is on PATH the lsp_* tools work
		// with zero config; a missing gopls is skipped with a warning.
		LSPServers: DefaultLSPServers(),
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
			{
				// Google Vertex AI via its OpenAI-compatible endpoint (api_type
				// "vertex"). It authenticates with Application Default Credentials,
				// so NO API key is used — run `gcloud auth application-default login`
				// or set GOOGLE_APPLICATION_CREDENTIALS. Fill in your GCP project and
				// a region (e.g. "us-central1"; "global" is also valid); leaving
				// Endpoint empty derives the URL from project/location.
				Name:        "vertex-gemini",
				DisplayName: "Vertex AI Gemini (ADC — set project/location)",
				APIType:     "vertex",
				Endpoint:    "",
				Project:     "",
				Location:    "",
				Model:       "google/gemini-2.5-flash",
				APIKey:      "",
				Temperature: 0.7,
				// models.dev: gemini-2.5-flash caps output at 64K.
				MaxTokens: 65536,
				// Gemini 2.5 Flash serves a ~1M input context window.
				ContextWindow: 1048576,
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
