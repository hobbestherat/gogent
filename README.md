# gogent

A small Go coding agent with streaming model support, an agent tree with
sub-agents, and a Turbo-Vision-style multi-session TUI.

---

## Permissions & safety

gogent gates every side-effecting tool through a single permission service
(`internal/permission`):

- **Files inside the workspace** (the launch directory) — read/write/edit are
  allowed without prompting.
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

Interactive decisions appear as a modal in the TUI. Choosing *Always* persists
the grant to `~/.gogent/permissions.json`. In headless/non-interactive runs
(`--disable-tui`, HTTP server) there is no one to ask, so any "ask" decision is
**denied** by default — keeping automated runs safe.

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

Each model entry has an `api_type` selecting the provider conventions:

- `openai` (default) — any OpenAI-compatible server. `endpoint` may be a full
  `.../chat/completions` URL or just a base URL (e.g. `https://host/v1`); the
  remaining paths are appended automatically.
- `zai` — the [Z.AI platform](https://docs.z.ai). Leave `endpoint` empty to use
  the built-in base URL (`https://api.z.ai/api/paas/v4`) and just set `api_key`.

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
  the registered tools, in the TUI under **Config → Resources…**.

## Status

Early / experimental (0.x). The model-connection layer (`internal/model`) is
intended to be split out into its own library later.
