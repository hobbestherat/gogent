# Configuration

gogent reads its configuration from `~/.gogent/config.json`. If that file is
absent, a built-in default configuration is used. A complete, annotated sample
lives at [`config.sample.json`](../config.sample.json) in the repository root.

You can edit the file by hand or through the TUI's **Config** menu dialogs. All
fields are optional unless noted; older configs that are missing a key simply
inherit the built-in default for that key.

> See also: [providers.md](providers.md) for `api_type` provider conventions,
> [tools-and-permissions.md](tools-and-permissions.md) for the permission model,
> and [usage-tui.md](usage-tui.md) for the in-app theme and notification editors.

---

## Top-level `Config`

| JSON tag | Type | Default | Description |
|---|---|---|---|
| `default_model` | string | `"local-lan"` | Name of the `models[]` entry used as the primary model. |
| `fast_model` | string | `""` | Optional name of a `models[]` entry for auxiliary tasks (compression, summarize, JSON repair). Empty ⇒ auxiliary tasks run on the primary model. |
| `model_roles` | map[string]string | `nil` | Maps an auxiliary role to the `"fast_model"` sentinel or a specific `models[]` name. Roles: `compression`, `web_fetch_summarize`, `title`, `json_repair`. A role absent from the map defaults to the fast model if one is configured, otherwise the primary. |
| `models` | []*ModelConfig | 8 built-ins | The list of model configurations. |
| `sub_agents` | SubAgentConfig | defaults | Sub-agent execution-model settings. |
| `timeouts` | TimeoutConfig | defaults | Timeouts (seconds) for model, tool, and sub-agent operations. |
| `window` | WindowConfig | resizable/min/max `true`; min 50×12 | Session window appearance and behavior. |
| `budget` | BudgetConfig | zero (disabled) | Per-session token-budget alert settings. |
| `rate_limit` | RateLimitConfig | zero (disabled) | Process-wide model request rate throttle. |
| `notify` | *NotifyConfig | defaults | Desktop/terminal notification settings. This is a pointer so a missing key yields the full defaults. |
| `review_edits` | bool | `false` | Gate every write/edit behind interactive diff-review approval. |
| `mcp_servers` | []MCPServerConfig | `nil` | MCP servers whose tools are added at startup. |
| `lsp_servers` | []LSPServerConfig | one `gopls` entry (`DefaultLSPServers`) | Language servers gogent can lazily launch to back the `lsp_*` tools. |
| `theme` | ThemeConfig | zero (`"default"` palette, color on) | TUI color palette. |
| `experimental` | ExperimentalConfig | zero (all off) | Opt-in, not-yet-default behaviors. |
| `supervisor` | SupervisorConfig | zero (defaults via `*OrDefault`) | Harness idle-watchdog tuning; only active when `experimental.supervisor` is on. |
| `max_steps` | *int | `100` (`DefaultMaxSteps`) | Per-turn model round-trip cap for every loop in a session. `nil` ⇒ 100; `0` ⇒ unlimited; `N>0` ⇒ cap N. See [max_steps](#max_steps) below. |

---

## `ModelConfig`

Each entry in `models[]` is a `ModelConfig`:

| JSON tag | Type | Default | Description |
|---|---|---|---|
| `name` | string | — | Unique identifier (**required**). |
| `display_name` | string | — | Human-readable label shown in the UI. |
| `api_type,omitempty` | string | `""` (⇒ `openai`) | Provider conventions: `openai`, `zai`, `anthropic` (alias `claude`), `openrouter`. Empty defaults to `openai`. |
| `endpoint` | string | — | Chat-completions endpoint URL. Empty for `zai`/`anthropic`/`openrouter` uses the built-in base URL. |
| `model` | string | — | Model identifier sent to the provider. |
| `api_key,omitempty` | string | `""` | API key. |
| `temperature` | float32 | `0.7` | Sampling temperature. |
| `top_p,omitempty` | float32 | `0` | Nucleus sampling `top_p`. |
| `max_tokens` | int | — | Per-request **output** cap (API `max_tokens`). Bounds response length only. |
| `context_window,omitempty` | int | `0` ⇒ 32768 | Input context window in tokens; drives compaction. Separate from `max_tokens`. |
| `reasoning_effort,omitempty` | string | `""` | `reasoning_effort` param (`minimal`/`low`/`medium`/`high`/`none`/`max`/`xhigh`). Marks the model as reasoning. Empty omits the param. |
| `effort_options,omitempty` | []string | `nil` | Accepted `reasoning_effort` values; drives the per-session effort selector. Empty ⇒ no effort control. |
| `thinking,omitempty` | *bool | `nil` | Explicit thinking toggle (Z.AI GLM-4.5+). `nil` = unset; non-nil forces on/off; marks the model as reasoning. |
| `free` | bool | `false` | Whether the model is free to use. |

> **Reasoning detection:** `IsReasoningModel()` returns true when
> `reasoning_effort != ""` **or** `thinking != nil`. The default context window
> (`defaultContextWindow`) is 32768 tokens when `context_window` is unset.

### Adding a model from the models.dev catalog

You do not have to hand-edit `config.json` or know a provider's endpoint, limits
and reasoning options to add a backend. In the TUI, open **Config → Add Model from
Catalog…** (also in the command palette as *Add model from catalog*). The flow:

1. **Pick a provider** from the live [models.dev](https://models.dev) catalog
   (searchable; each row shows the env var the provider expects, so you know which
   credential to have ready).
2. **Pick a model** (searchable; each row shows context window, output cap, and
   reasoning/free badges).
3. **Review** a pre-filled form. Every field models.dev can supply
   (`display_name`, `endpoint`, `model`, `max_tokens`, `context_window`,
   `reasoning_effort`, `effort_options`, `free`, derived `api_type`) is auto-filled
   and **editable**. You are prompted only for what models.dev cannot know — the
   **API key** (or, for Vertex, the GCP **project**/**location**, which use
   Application Default Credentials, no key). The `api_type` is read-only ("from
   catalog").
4. **Save** creates a NEW, uniquely-named entry, persists it, and makes it
   immediately selectable; you can optionally set it as the default for new
   sessions.

The catalog is fetched from `https://models.dev/api.json` and cached to
`~/.gogent/modelsdev-cache.json` with a **24-hour TTL** and `ETag`/
`If-Modified-Since` revalidation. A **Refresh** button forces an update. The flow
degrades gracefully: if models.dev is unreachable it serves the cached catalog,
and if there is no cache it offers the manual model editor instead — offline use is
never blocked. The existing manual editor (**Config → Models…**) and hand-editing
`config.json` continue to work unchanged.

---

## `SubAgentConfig`

| JSON tag | Type | Default | Description |
|---|---|---|---|
| `execution_model` | SubAgentExecutionModel | `"both"` | `both` (blocking + async), `one_shot` (blocking only), `interactive` (async only, experimental). |
| `allow_recursive` | bool | `false` | Permit spawned sub-agents to spawn sub-agents. |
| `max_subagents` | int | `4` | Per-agent spawn fan-out cap. `<=0` ⇒ default. |
| `max_depth` | int | `3` | Recursion depth cap. `<=0` ⇒ default. |
| `max_concurrent` | int | `8` | Global cap on concurrently running sub-agents. `<=0` ⇒ default. |
| `token_budget,omitempty` | int | `0` (unbounded) | Cumulative token cap per sub-agent before a graceful `BUDGET_EXCEEDED` stop. |

---

## `TimeoutConfig`

All fields are integers in **seconds**; default `300` each, and `<=0` ⇒ default.

| JSON tag | Description |
|---|---|
| `model_seconds` | Single model HTTP request timeout. |
| `tool_seconds` | Single tool/shell execution timeout. |
| `subagent_seconds` | Sub-agent run timeout. |

---

## `WindowConfig`

| JSON tag | Type | Default | Description |
|---|---|---|---|
| `resizable` | bool | `true` | Drag corners to resize. |
| `minimizable` | bool | `true` | Collapse to title bar. |
| `maximizable` | bool | `true` | Expand to fill the desktop minus the sidebar. |
| `min_width` | int | `50` | Minimum window width. |
| `min_height` | int | `12` | Minimum window height. |

---

## `BudgetConfig`

| JSON tag | Type | Default | Description |
|---|---|---|---|
| `token_budget,omitempty` | int | `0` (disabled) | Per-session cumulative token budget (prompt + completion) for the status-bar alert. Zero disables. |
| `warn_fraction,omitempty` | float64 | `0.8` | Fraction of budget at which the gauge turns amber. `<=0` or `>1` ⇒ default. |

```json
"budget": {
  "token_budget": 200000,
  "warn_fraction": 0.8
}
```

---

## `RateLimitConfig`

| JSON tag | Type | Default | Description |
|---|---|---|---|
| `requests_per_minute,omitempty` | int | `0` (disabled) | Process-wide sustained ceiling on model requests/min. `<=0` disables. |
| `burst,omitempty` | int | `0` ⇒ `requests_per_minute` | Back-to-back requests before the per-minute rate applies. `<=0` ⇒ RPM. |

```json
"rate_limit": {
  "requests_per_minute": 40,
  "burst": 5
}
```

---

## `NotifyConfig`

| JSON tag | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `true` | Master switch. |
| `bell` | bool | `true` | Terminal bell (`\a`). |
| `desktop` | bool | `true` | OSC 9 / OSC 777. |
| `native` | bool | `false` | `notify-send` / `terminal-notifier`. |
| `on_complete` | bool | `true` | Task finished. |
| `on_error` | bool | `true` | Task errored. |
| `on_approval` | bool | `true` | Permission prompt needs an answer. |
| `on_clarify` | bool | `true` | Sub-agent `CLARIFY` question. |
| `suppress_when_focused` | bool | `false` | Skip notifications for the focused session (approval prompts are never suppressed). |

---

## `LSPServerConfig`

Each entry in `lsp_servers[]` declares one Language Server Protocol server gogent can launch and route source files to (see [tools-and-permissions.md](tools-and-permissions.md) for the `lsp_*` tools they back). All per-language knowledge lives here as data: routing is by file extension, the workspace root is found by walking up for `root_markers`, and the wire languageId is resolved per file. A single generic client serves every server. Launching a server is gated through `ActionLSP`, so adding an entry advertises a server but does not silently run it.

When `lsp_servers` is omitted entirely, gogent uses `DefaultLSPServers`: a single `gopls` entry so Go works with zero config when `gopls` is on `PATH`. To disable LSP, set `"lsp_servers": []` (an empty list) or mark the entry `"disabled": true`.

| JSON tag | Type | Default | Description |
|---|---|---|---|
| `name` | string | — | Server name; also the permission-gate key. |
| `language,omitempty` | string | `""` | Default LSP languageId (e.g. `"go"`, `"rust"`, `"python"`). |
| `languages,omitempty` | map[string]string | `nil` | Per-extension languageId override (e.g. `".tsx"` → `"typescriptreact"`) for one process serving several languageIds. |
| `extensions,omitempty` | []string | `nil` | Routing keys, leading dot included (e.g. `[".go"]`). |
| `command` | string | — | stdio server executable. |
| `args,omitempty` | []string | `nil` | Server arguments. |
| `env,omitempty` | map[string]string | `nil` | Extra environment for the subprocess. |
| `root_markers,omitempty` | []string | `nil` | Files that mark a project root, searched by walking up from the file (e.g. `["go.work","go.mod"]`). Empty ⇒ the gogent workspace root. |
| `initialization_options,omitempty` | map[string]any | `nil` | Feeds the `initialize` request's `initializationOptions`. |
| `settings,omitempty` | map[string]any | `nil` | Answers `workspace/configuration` pulls and seeds `workspace/didChangeConfiguration`. |
| `allowed_commands,omitempty` | []string | `nil` | Scopes `lsp_execute_command` (`workspace/executeCommand`); an empty list means no command may run. |
| `disabled,omitempty` | bool | `false` | Skip this server without removing its config. |

```json
"lsp_servers": [
  {
    "name": "gopls",
    "language": "go",
    "extensions": [".go"],
    "command": "gopls",
    "args": ["serve"],
    "root_markers": ["go.work", "go.mod"],
    "allowed_commands": ["gopls.tidy"]
  }
]
```

---

## `MCPServerConfig`

Each entry in `mcp_servers[]` is an `MCPServerConfig`:

| JSON tag | Type | Default | Description |
|---|---|---|---|
| `name` | string | — | Server name. |
| `transport,omitempty` | string | `""` ⇒ `"stdio"` | `stdio`, `http`, or `streamable-http`. |
| `command,omitempty` | string | — | stdio executable. |
| `args,omitempty` | []string | — | stdio args. |
| `env,omitempty` | map[string]string | — | stdio environment. |
| `url,omitempty` | string | — | http URL. |
| `headers,omitempty` | map[string]string | — | http headers. |
| `disabled,omitempty` | bool | `false` | Skip the server without removing its config. |

```json
"mcp_servers": [
  {
    "name": "filesystem",
    "transport": "stdio",
    "command": "npx",
    "args": ["-y", "@modelcontextprotocol/server-filesystem", "/home/user"]
  },
  {
    "name": "fetch",
    "transport": "http",
    "url": "http://localhost:8080/mcp"
  }
]
```

---

## `ThemeConfig`

| JSON tag | Type | Default | Description |
|---|---|---|---|
| `name,omitempty` | string | `""` ⇒ `"default"` | Built-in palette: `default`, `high-contrast` (aliases `colorblind`/`high_contrast`), or `dark` (aliases `midnight`/`black`). |
| `no_color,omitempty` | bool | `false` | Disable all color. |
| `no_shadow,omitempty` | bool | `false` | Disable drop shadows (flat UI). Applied live. |
| `overrides,omitempty` | map[string]string | — | Per-role colors. Values may be `#RRGGBB`, an ANSI index `0`–`255`, or `default`/`none`. |

**`overrides` keys:** `user`, `agent`, `note`, `tool`, `result`, `info`,
`error`, `desktop_fg`, `desktop_bg`, `panel_fg`, `panel_bg`, `window_fg`,
`window_bg`, `title`, `divider`, `accent`, `code_bg`, `list_bg`, `list_fg`.

```json
"theme": {
  "name": "dark",
  "no_shadow": true,
  "overrides": {
    "accent": "#7aa2f7",
    "agent": "#9ece6a",
    "error": "#f7768e"
  }
}
```

---

## `KeybindingsConfig`

Customises the TUI keyboard shortcuts. Edit it from the running app via **Config →
Keybindings…** (or the command palette's *Customize keybindings*); the editor writes
this section back to `config.json`. Hand-editing is also supported.

| JSON tag | Type | Default | Description |
|---|---|---|---|
| `overrides,omitempty` | map[string]string | — | Maps an action ID to a chord spec. Only actions rebound away from their built-in default are stored. |

**`overrides` semantics.** Each key is a stable action ID (below); each value is a
**chord spec**. Modifiers are joined to the key token with `+` in `Ctrl+Alt+Shift`
order — e.g. `Ctrl+T`, `Ctrl+Shift+R`, `Alt+F4`. A lone character is a literal key
(`a`, `/`, `?`, `:`, `+`); a named key uses its token (`Esc`, `Enter`, `Tab`,
`Backspace`, `Up`/`Down`/`Left`/`Right`, `Home`/`End`, `PageUp`/`PageDown`,
`Insert`/`Delete`, `F1`–`F12`). The special value `"none"` **unbinds** the action
(no key fires it, not even its default). An override is **ignored** on load when its
action ID is unknown, its spec is unparseable, the terminal can't deliver the chord,
it breaks the scope rule, or it would collide (same scope) with another action — so a
stale or hand-edited config can't break startup. Letter matching is case-insensitive.

**Scope rule.** A *Global* action's chord must be chorded (Ctrl/Alt or a function/named
key): a plain printable key bound globally would be stolen from every text input.
*Focus* and *Fallthrough* actions may use plain keys (they fire only when focus is not
in a text input). The editor enforces this; a hand-edited violation is dropped on load.

**Action IDs** (ID — default — scope):

| Action ID | Default | Scope | What it does |
|---|---|---|---|
| `session.new` | `Ctrl+N` | Global | New session |
| `session.next` | `Ctrl+]` | Global | Next session |
| `session.close` | `Ctrl+W` | Global | Close active session |
| `app.quit` | `Ctrl+Q` | Global | Quit |
| `config.subagents` | `Ctrl+,` | Global | Open Sub-agent settings |
| `window.tileVertical` | `Ctrl+Shift+V` | Global | Tile windows vertically |
| `window.tileHorizontal` | `Ctrl+Shift+H` | Global | Tile windows horizontally |
| `window.tileGrid` | `Ctrl+Shift+G` | Global | Tile windows in a grid |
| `window.maximizeAll` | `Ctrl+Shift+M` | Global | Maximize all windows |
| `window.cascade` | `Ctrl+Shift+D` | Global | Cascade windows |
| `transcript.find` | `/` | Focus | Find in transcript |
| `transcript.showAll` | `Esc` | Focus | Clear filter / search |
| `transcript.toggle.messages` | `a` | Focus | Toggle assistant messages |
| `transcript.toggle.tools` | `t` | Focus | Toggle tool calls |
| `transcript.toggle.thinking` | `r` | Focus | Toggle thinking |
| `transcript.toggle.errors` | `e` | Focus | Toggle errors |
| `transcript.foldAll` | `f` | Focus | Fold all |
| `transcript.unfoldAll` | `u` | Focus | Unfold all |
| `transcript.copyAnswer` | `y` | Focus | Copy last answer |
| `app.commandPalette` | `:` | Fallthrough | Open the command palette |
| `app.help` | `?` | Fallthrough | Open the keybinding cheatsheet |

*Global* chords fire anywhere (before the focused widget) but are suppressed while a
modal dialog is up. *Focus* chords fire only while a session transcript holds focus.
*Fallthrough* chords fire only when the key reaches the desktop unconsumed (i.e. focus
is not in a text input). Slash commands (`/fork`, `/stop`, …) are typed text, not
keybindings, and are not customisable here.

```json
"keybindings": {
  "overrides": {
    "session.new": "Ctrl+T",
    "transcript.toggle.thinking": "i",
    "transcript.find": "none"
  }
}
```

---

## `ExperimentalConfig`

| JSON tag | Type | Default | Description |
|---|---|---|---|
| `supervisor,omitempty` | bool | `false` | Enable the harness-level idle watchdog that re-prompts toward a persisted `/goal`. |
| `stream_thinking,omitempty` | bool | `false` | Stream chain-of-thought tokens live into the transcript, folding per turn. Toggleable via `/thinking`. |

---

## `SupervisorConfig`

Only active when `experimental.supervisor` is on.

| JSON tag | Type | Default | Description |
|---|---|---|---|
| `max_nudges,omitempty` | int | `3` | Max consecutive supervisor nudges before giving up. `<=0` ⇒ default. A real user message resets the budget. |

---

## `max_steps`

`max_steps` is a pointer (`*int`) resolved via `MaxStepsOrDefault`:

| Value | Meaning |
|---|---|
| `nil` (unset) | `100` (`DefaultMaxSteps`) |
| `0` | **Unlimited** — bounded only by the final answer, token budget, or cancellation |
| `N > 0` | Cap at N round-trips |

It governs the root task loop **and** every sub-agent / interactive loop in the
session. When the cap fires with the just-finished turn still holding unexecuted
tool calls, the session ends on a visible `STEP_LIMIT_REACHED` notice ("Type a
message to continue") on a resumable transcript rather than stopping silently
(issue #449); raise `max_steps` or set it to `0` for tasks that legitimately need
more rounds.

```json
"max_steps": 40
```

---

## Fast model and `model_roles`

Point `fast_model` at a `models[]` entry by name. `model_roles` overrides
individual roles. Recognized roles: `compression`, `web_fetch_summarize`,
`title`, `json_repair`. Each value is the `"fast_model"` sentinel or a specific
`models[]` name. A role absent from the map defaults to the fast model when one
is configured; map a role to the primary model's name to pin it back to the
primary.

```json
{
  "default_model": "local-lan",
  "fast_model": "haiku",
  "model_roles": {
    "compression": "fast_model",
    "web_fetch_summarize": "fast_model",
    "title": "fast_model",
    "json_repair": "local-lan"
  },
  "models": [
    { "name": "local-lan", "display_name": "Local LAN", "model": "gpt-4o" },
    { "name": "haiku", "display_name": "Haiku", "model": "claude-3-haiku" }
  ]
}
```
