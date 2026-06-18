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

Each model entry has an `api_type` selecting the provider conventions:

- `openai` (default) — any OpenAI-compatible server. `endpoint` may be a full
  `.../chat/completions` URL or just a base URL (e.g. `https://host/v1`); the
  remaining paths are appended automatically.
- `zai` — the [Z.AI platform](https://docs.z.ai). Leave `endpoint` empty to use
  the built-in base URL (`https://api.z.ai/api/paas/v4`) and just set `api_key`.

In the model editor, the **Scan** button next to the model id queries the
backend's listing endpoint and replaces the model-id field with a dropdown of
the advertised models.

## Skills & project instructions

gogent assembles a system-context block for every task:

- **AGENTS.md** — project instructions are discovered automatically by walking from
  the workspace root up to the filesystem root (outermost first, nearest last), plus a
  global `~/.gogent/AGENTS.md`. They're concatenated (size-capped) and injected into
  the agent's context.
- **Skills** — drop a folder containing a `SKILL.md` (YAML frontmatter with `name:` and
  `description:`) under `~/.gogent/skills` or `./skills`. Active skills are advertised
  to the model as a name+description index; the model calls the `skill` tool to load a
  skill's full instructions on demand (progressive disclosure). Toggle skills and view
  usage stats in the TUI under **Config → Skills…**.

## Status

Early / experimental (0.x). The model-connection layer (`internal/model`) is
intended to be split out into its own library later.
