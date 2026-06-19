# gogent

A small Go coding agent with streaming model support, an agent tree with
sub-agents, and a Turbo-Vision-style multi-session TUI.

---

## Permissions & safety

gogent gates every side-effecting tool through a single permission service
(`internal/permission`):

- **Files inside the workspace** (the launch directory) — read/write/edit are
  allowed without prompting.
- **Codebase search** — the `grep`, `glob`, and `list` tools are read-only and
  confined to the workspace, so they run **without any prompt** (unlike shelling
  out to `grep`/`rg`, which prompts each time). `grep` searches file contents for
  a regex and returns `file:line` references you can pass straight to `read`;
  `glob` finds files by name and `list` lists a directory. Prefer them over the
  shell for search.
- **Anything outside the workspace** — file ops and any shell command that
  touches an external path prompt for approval, grouped **per external root
  folder** (e.g. one prompt covers all of `/etc`).
- **Shell** — asked once per session; you can choose *Allow once*, *Always*
  (persisted), or *Deny*. Shell commands run with their working directory pinned
  to the workspace root.
- **Network** — the `web_fetch` tool (GET a URL → readability-extracted Markdown,
  size-capped and briefly cached) prompts **per domain**, so an *Always* grant is
  scoped to a single host. Prefer it over `curl` in the shell for reading docs and
  API references: it returns clean Markdown instead of raw HTML.
- **Git** — the native `git` tool (`status`/`diff`/`log`/`commit`/`create_branch`/
  `restore`) runs git directly (no shell quoting, output returned structured) so the
  agent can inspect its diff and checkpoint work. Read-only operations run freely;
  mutating ones (commit/create_branch/restore) are gated like shell. When the
  workspace is a repo, a live `git status` is also injected into the prompt so the
  model always sees its working-tree state. Prefer it over `git` through the shell.
- **MCP servers** — each [Model Context Protocol](https://modelcontextprotocol.info/)
  server declared under `mcp_servers` is dialed at startup, its tools discovered via
  `tools/list`, and each registered under an `mcp__<server>__<tool>` name so the
  agent can call it like any built-in. Launching a server is gated **per server**
  through `ActionMCP`, so an *Always* grant is scoped to one server and a config
  synced from elsewhere cannot silently spawn processes; a denied, disabled or
  unreachable server is skipped with a warning instead of blocking startup.

- **Review edits** — an optional tier (Settings → *Review edits before applying*,
  off by default) that defers every `write`/`edit` until you approve it. The TUI
  shows a colour-coded unified diff of the change and offers *Accept* / *Accept
  all this session* / *Reject*; a rejected edit is not written. *Accept all*
  is remembered for that session only. With the gate off, edits apply
  immediately as before.
- **Checkpoints / undo / rewind** — every `write`/`edit` first snapshots the
  file's pre-turn state (an in-memory shadow copy), so a botched multi-file edit
  can be rolled back without resorting to your own VCS. Type `/undo` to revert
  the last turn, or `/rewind [n]` to revert the last `n` turns (`/rewind` with no
  count reverts everything). Each file is snapshotted once per turn — the first
  mutation wins — so an undo always restores the workspace to the turn's start.
  Snapshots live in memory for the running process and are scoped per session;
  restarting (which still recovers the transcript from disk) clears them. The
  snapshots cover `write`/`edit`; shell-driven file changes are not captured.

Interactive decisions appear as a modal in the TUI. Choosing *Always* persists
the grant to `~/.gogent/permissions.json`. In headless/non-interactive runs
(`--disable-tui`, HTTP server) there is no one to ask, so any "ask" decision is
**denied** by default — keeping automated runs safe. The edit-review gate is
likewise interactive-only: without a UI to render the diff, writes proceed (the
operation is already permission-authorized; review is only a confirmation step).

> The shell guardrail is best-effort, not a sandbox: a shell is Turing-complete
> and a determined command can still reach outside the workspace (variables,
> `eval`, subshells). Treat it as a seatbelt against accidental damage, not as
> containment. OS-level sandboxing remains a future enhancement.

---

## Build & run

```sh
go build ./...
./gogent
```

Configuration lives in `~/.gogent/config.json`. The default LAN endpoint honors
the `GOGENT_MODEL_URL` environment variable. A sample config file with all
available options is included at `config.sample.json` in the repository.

### MCP servers

Extend the agent's tool set with [MCP](https://modelcontextprotocol.info/) servers
by adding an `mcp_servers` array to `config.json`. Two transports are supported —
a launched subprocess (`stdio`, the default) and `http`/`streamable-http`:

```json
{
  "mcp_servers": [
    { "name": "fs", "command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "/data"] },
    { "name": "github", "transport": "http", "url": "https://example.com/mcp", "headers": { "Authorization": "Bearer YOUR_TOKEN" } }
  ]
}
```

Each server's tools appear as `mcp__<name>__<tool>`. Add `"disabled": true` to keep
an entry without connecting it. Launching is gated through the permission service
(see **MCP servers** above), so in headless runs add an `mcp` allow rule to
`~/.gogent/permissions.json` or the servers will be denied.

### Window scaling

Session windows are **scalable** by default. You can:

- **Drag the bottom-right corner** to resize any session window
- **Drag the title bar** to move windows around
- **Minimize/restore** windows using the ▾/▴ button in the title bar

These features are controlled by the `window` section of `config.json`:

```json
{
  "window": {
    "resizable": true,
    "minimizable": true,
    "min_width": 50,
    "min_height": 12
  }
}
```

### Session management

When you run many sessions at once, the **Session** menu (and the right-hand
sidebar) let you keep them organized:

- **Rename** — *Rename Active…* sets a custom title shown in the window, sidebar
  and menu (the session id is unchanged).
- **Pin / favorite** — *Pin Active* marks a session with a ★ and floats it to the
  top of the sidebar; *Unpin Active* clears the mark.
- **Reorder** — *Move Active Up / Down* reorders sessions in the sidebar.
- **Close Others / Close All** — bulk-close every window except the active one,
  or all of them (a fresh window is opened so you always have somewhere to type).

The full desktop layout — sidebar order, titles, pin state, and each window's
position/size — is saved to `~/.gogent/workbench_layout.json` and restored on
the next launch, so your workbench comes back exactly as you left it. (Window
moves and resizes are captured when you quit; renames, pins, reorders and
open/close are saved immediately.)

Beneath the session tree, the sidebar carries an **Overall** panel: a live,
cluster-wide readout (open sessions, sub-agents, tokens in/out, requests,
errors and prompt-cache hit %) that updates as work streams in. It refreshes on
the session event stream, coalesced to ~250 ms so a fast stream can't thrash
the redraw (and once per second while any session is busy).

### Navigating a transcript

Long sessions are searchable and filterable. Focus a transcript (click it or
Tab to it) and use less/vim-style keys — or the **View** menu, which lists the
same actions:

- **Find** — `/` (or *View → Find…*, **Ctrl+F**) opens a prompt; the transcript
  is filtered to entries containing your text (case-insensitive), with a match
  count. An empty query clears the search.
- **Filter by type** — toggle event types in or out: `a` messages, `t` tool
  calls/results, `r` thinking, `e` errors.
- **Fold / unfold all** — `f` folds every entry, `u` unfolds them.
- **Copy** — `y` (or *View → Copy Last Answer*) yanks the most recent assistant
  answer to the system clipboard; *View → Copy Last Code Block* yanks just the
  fenced code from that answer.
- **Clear** — `Esc` removes any active search and filters.

The live view keeps only the most recent ~1000 entries so memory and render
cost stay bounded over a long session; older entries age out of the window (the
durable transcript still lives in the session JSONL).

Copy targets the system clipboard over an OSC 52 escape sequence, which works in
every capable terminal — including over SSH. When a native clipboard utility is
on your `$PATH` (`pbcopy` on macOS, `wl-copy`/`xclip` on Linux) the text is also
piped to it as a local fallback.

### Exporting a session

*Session → Export Markdown…* / *Export JSON…* writes the active session's full
transcript to `~/.gogent/gogent-session-<title>-<timestamp>.{md,json}`. Markdown
is a readable transcript (user/assistant turns, tool calls and results folded
into fenced blocks); JSON is the structured message list. The export reuses the
same data the restored-session view reads, so it reflects the whole conversation
rather than the capped live window.

### Notifications (step away from the terminal)

gogent can ping you when a long task finishes or a session needs your attention,
so you don't have to watch the screen:

- **Task complete** and **error** — fired when the agent's turn ends.
- **Approval needed** — a permission prompt is waiting for an answer. The
  requesting session is also badged with a ⏳ marker in the sidebar (and a global
  ⏳N count in its header) until you answer, even when that session isn't focused,
  so a background prompt is never silently missed. The prompt dialog names the
  session that is asking, and clicking the badged sidebar node jumps straight to
  it.
- **Clarification** — an interactive sub-agent asked a `CLARIFY` question.

Each event can be delivered through three independent channels:

- **Bell** — the terminal bell (`\a`; most terminals map it to a sound, a flash,
  or a window-urgency hint).
- **Desktop** — an OSC 9 (iTerm2/Ghostty) and OSC 777 (rxvt-unicode and others)
  escape sequence. Capable terminals surface it as a desktop notification;
  others ignore it, so it is always safe to leave on.
- **Native** — shells out to `notify-send` (Linux/BSDs) or `terminal-notifier`
  (macOS) when one is on your `$PATH`.

Toggle each channel, each event, and "skip the session I'm already watching"
under **Config → Notifications…**. The setting lives in the `notify` block of
`~/.gogent/config.json` and defaults to **on** (bell + desktop, every event). A
config that predates the setting (no `notify` key) resolves to those defaults,
so existing installs get notifications automatically.

```json
{
  "notify": {
    "enabled": true,
    "bell": true,
    "desktop": true,
    "native": false,
    "on_complete": true,
    "on_error": true,
    "on_approval": true,
    "on_clarify": true,
    "suppress_when_focused": false
  }
}
```

### Statistics

A dedicated **Config → Statistics…** view surfaces the counters gogent already
collects but previously only showed in part (or not at all). It has five tabs:

- **Overview** — grand totals across every session (sessions, turns, tokens
  in/out, tool calls, compactions) plus the primary and fast model backends'
  request/success/error counts, token totals, average latency and error
  breakdown (timeouts, context overflows, refusals, generic).
- **Sessions** — one row per session: turns, tokens, tool calls, context %,
  primary-model requests/errors and compactions.
- **Tools** — per registered tool: call count, success/failure split and average
  execution time.
- **Skills** — per skill: success/failure/total.
- **Models** — per-model token attribution.

**Export CSV** / **Export JSON** write a timestamped snapshot to
`~/.gogent/gogent-stats-<timestamp>.{csv,json}`. The CSV is long-format
(`section,name,metric,value`) so it loads cleanly into a spreadsheet.

Prompt-cache accounting is built in: the backend reads each provider's
cached-prompt-token count (OpenAI/Z.AI `prompt_tokens_details.cached_tokens`,
or a top-level `prompt_cache_hit_tokens`) and the Overview reports cached tokens
and a cache-hit % per backend, so reuse of the stable prefix (tools + system
prompt + history) is visible. OpenAI-compatible backends cache that prefix
automatically — no request markers needed — and gogent already sends it
stable→volatile (tools, then the frozen system prompt, then the running
transcript with the live message last).

The view is a point-in-time, in-memory snapshot. Durable/queryable history
arrives with the structured-logging/audit stream; per-model cost and TTFT are
likewise follow-ups (they depend on a per-model pricing configuration and
streaming instrumentation).

Each model entry has an `api_type` selecting the provider conventions:

- `openai` (default) — any OpenAI-compatible server. `endpoint` may be a full
  `.../chat/completions` URL or just a base URL (e.g. `https://host/v1`); the
  remaining paths are appended automatically.
- `zai` — the [Z.AI platform](https://docs.z.ai). Leave `endpoint` empty to use
  the built-in base URL (`https://api.z.ai/api/paas/v4`) and just set `api_key`.
- `anthropic` (alias `claude`) — the [Anthropic Messages API](https://platform.claude.com/docs/en/docs/build-with-claude/tool-use).
  Leave `endpoint` empty to use the built-in base URL
  (`https://api.anthropic.com`) and just set `api_key`. gogent talks the native
  `POST /v1/messages` protocol (`x-api-key` auth, top-level system prompt,
  content-block messages, `input_schema` tools), translating to and from its
  internal OpenAI-shaped types, so tools and prompt-cache accounting work
  unchanged. Extended thinking is not yet exposed for this provider.

- `openrouter` — the [OpenRouter](https://openrouter.ai) gateway. Leave
  `endpoint` empty to use the built-in base URL (`https://openrouter.ai/api/v1`)
  and just set `api_key`. It is OpenAI-compatible (bearer auth, same wire
  format) but additionally sends the recommended `HTTP-Referer` / `X-Title`
  attribution headers used for app ranking and free-tier prioritization.

Other OpenAI-compatible gateways (Google's Gemini OpenAI-compat layer, Azure
OpenAI) work under `openai` by pointing `endpoint` at their base URL and setting
`api_key`. Authentication is a per-provider policy: the key is sent as an
`Authorization: Bearer` token by default, or as `x-api-key` (Anthropic), an
Azure `api-key` header, or a URL query parameter, so providers that share the
OpenAI wire format can still authenticate differently.

In the model editor, the **Scan** button next to the model id queries the
backend's listing endpoint and replaces the model-id field with a dropdown of
the advertised models.

### Fast model for auxiliary tasks

A secondary, smaller/cheaper/faster model can handle lightweight auxiliary work
(context compression today; web-fetch summarization, title generation, and JSON
repair as they land) so the primary model is reserved for the actual reasoning.
Point `fast_model` at a `models[]` entry by name; an optional `model_roles` map
overrides individual roles. When `fast_model` is unset, every task runs on the
primary model (no behavior change). Fast-model token usage is tracked separately
in the session stats (`fast_tokens_in` / `fast_tokens_out`).

```json
{
  "default_model": "opus",
  "fast_model": "haiku-fast",
  "model_roles": {
    "compression": "fast_model",
    "title": "fast_model"
  },
  "models": [
    { "name": "opus", "model": "claude-opus-4-8", "max_tokens": 32000, "context_window": 200000 },
    { "name": "haiku-fast", "model": "claude-haiku-4-5", "max_tokens": 4096, "context_window": 200000 }
  ]
}
```

Each `model_roles` value is either the `"fast_model"` sentinel or a specific
`models[]` name. A role absent from the map defaults to the fast model when one
is configured; map a role to the primary model's name to pin it back to the
primary. Recognized roles: `compression`, `web_fetch_summarize`, `title`,
`json_repair`.

Per-model fields: `max_tokens` is the per-request **output** cap (the API's
`max_tokens`), while `context_window` is the model's **input** context window in
tokens. Context compaction is calibrated against `context_window` (it fires at
~80% and settles near ~50%, never against the output cap) — set `context_window`
to your model's real window so long sessions compact at the right point. When
omitted it falls back to a conservative built-in default.

Reasoning models add two optional per-model fields, surfaced in the model editor
and threaded into the request only where the provider understands them:

- `reasoning_effort` — forwarded as the API's `reasoning_effort`
  (`minimal`/`low`/`medium`/`high`, plus `none`/`max`/`xhigh` on Z.AI GLM-5.2).
- `thinking` — a boolean toggling chain-of-thought on providers with an explicit
  switch (Z.AI GLM-4.5+, sent as `thinking: {type: enabled|disabled}`). Omit it
  to leave the provider default.

Setting either marks the model as a reasoning model: on OpenAI o-series / GPT-5
the output cap is sent as `max_completion_tokens` (they reject `max_tokens`) and
the rejected `temperature` is dropped, so the most capable tiers no longer 400
on the first request. Providers report `reasoning_tokens` (a subset of the
output tokens) under `completion_tokens_details`, which gogent now parses.

The bottom status line of each session window carries a live usage readout
(issue #63): the current state, an elapsed timer and output throughput while a
turn is generating, cumulative tokens and turns, and a context-window gauge
(`ctx ▰▰▰▱▱▱ 38%`). The gauge turns amber as context approaches the compaction
threshold and red at it (the same ~80% point), so you get a visual warning just
before a compaction pass. A per-session token budget can optionally raise a
budget alert; configure it under the `budget` key:

```json
"budget": {
  "token_budget": 200000,
  "warn_fraction": 0.8
}
```

`token_budget` is the per-session cumulative (prompt + completion) token count at
which the status line turns amber (at the `warn_fraction`) and red (at the full
budget) and records a one-line transcript note; a zero/omitted budget disables
alerting (the default). Cost budgets are not yet supported — that needs
per-model pricing (tracked as a follow-up).

## Skills & project instructions

gogent assembles a system-context block for every task:

- **AGENTS.md** — project instructions are discovered automatically by walking from
  the workspace root up to the filesystem root (outermost first, nearest last), plus a
  global `~/.gogent/AGENTS.md`. They're concatenated (size-capped) and injected into
  the agent's context.
- **Repo map** — at startup gogent walks the workspace, extracts top-level Go
  declarations, and ranks them by how often each is referenced (Aider-style). The
  resulting size-capped symbol skeleton is injected into the agent's context so it can
  navigate larger projects without reading every file first. (Go today; other languages
  are a follow-up.)
- **Skills** — drop a folder containing a `SKILL.md` (YAML frontmatter with `name:` and
  `description:`) under `~/.gogent/skills` or `./skills`. Active skills are advertised
  to the model as a name+description index; the model calls the `skill` tool to load a
  skill's full instructions on demand (progressive disclosure). Browse and toggle
  skills, view usage stats and read the full `SKILL.md`, and inspect or enable/disable
  the registered tools, in the TUI under **Config → Resources…**. The skills
  directories are a trust boundary: symlinks are not followed and only files inside the
  directory are read, so symlink a shared skill with a real file rather than a link.

## Status

Early / experimental (0.x). The model-connection layer (`internal/model`) is
intended to be split out into its own library later.
