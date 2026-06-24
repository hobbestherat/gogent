# Using the TUI

gogent's default mode is a Turbo-Vision-style multi-session terminal UI built on
[turbotui](https://github.com/charmbracelet) (a Turbo-Vision-style widget toolkit). It
gives you one window per session, a top menu bar, a right-hand pinned sidebar, and a
fuzzy command palette. Launch it with no arguments:

```sh
./gogent
```

Everything you can do from the menus you can also do from the keyboard or the command
palette, so this guide is organised three ways: by **menu**, by **keyboard context**, and
by **feature area**. Cross-references to related guides:

- [Configuration](configuration.md) — `config.json`, model definitions, sub-agent settings
- [Tools & Permissions](tools-and-permissions.md) — the tool registry, permission prompts, plan mode
- [Architecture](architecture.md) — how the TUI, agent runtime, and model connector fit together

---

## Menus

There are five top-level menus: **File**, **Session**, **View**, **Config**, and **Help**.
Open a menu with the mouse or by typing its accelerator; most menu items also have a
global shortcut (listed in the tables below).

### File

| Item     | Shortcut | Description            |
|----------|----------|------------------------|
| Exit     | Ctrl+Q   | Quit gogent (confirms) |

### Session

The Session menu is **dynamic**: it always ends with one entry per open session (pinned
sessions are prefixed with ★), so you can jump to any session from the keyboard.

| Item                | Shortcut | Description                                              |
|---------------------|----------|----------------------------------------------------------|
| New Session         | Ctrl+N   | Open a fresh session window                              |
| Next Session        | Ctrl+]   | Cycle focus to the next session                          |
| Close Session       | Ctrl+W   | Close the active session                                 |
| Close Others        | —        | Close every session except the active one                |
| Close All           | —        | Close every session                                      |
| Saved Sessions…     | —        | Open the saved-sessions browser (when wired)             |
| Rename Active…      | —        | Rename the live (active) session                         |
| Pin Active / Unpin Active | —    | Toggle pin state (label flips)                           |
| Move Active Up      | —        | Reorder the active session earlier in the sidebar        |
| Move Active Down    | —        | Reorder the active session later in the sidebar          |
| Export Markdown…    | —        | Export the full transcript to Markdown                   |
| Export JSON…        | —        | Export the full transcript to JSON                       |
| Approve Plan        | —        | Approve a pending plan (when wired)                      |
| _one row per session_ | —      | Raise that session's window (★ = pinned)                 |

### View

| Item                       | Shortcut      | Description                                  |
|----------------------------|---------------|----------------------------------------------|
| Find…                      | Ctrl+F        | Open the find-in-transcript filter           |
| Show All                   | —             | Clear search + all type filters              |
| Toggle Messages            | —             | Show/hide user/assistant message records     |
| Toggle Tool Calls          | —             | Show/hide tool-call records                  |
| Toggle Thinking            | —             | Show/hide reasoning/thinking records         |
| Toggle Errors              | —             | Show/hide error records                      |
| Fold All                   | —             | Collapse every record body                   |
| Unfold All                 | —             | Expand every record body                     |
| Copy Last Answer           | —             | Copy the last assistant answer (OSC 52)      |
| Copy Last Code Block       | —             | Copy the last fenced ``` code block          |
| Tile Vertically            | Ctrl+Shift+V  | Stack windows in rows                        |
| Tile Horizontally          | Ctrl+Shift+H  | Stack windows in columns                     |
| Tile Grid                  | Ctrl+Shift+G  | Arrange windows in a grid                    |
| Maximize All               | Ctrl+Shift+M  | Maximize every open window                   |
| Cascade Windows            | Ctrl+Shift+D  | Overlap windows in a cascade                 |
| Pin Sidebar / Unpin Sidebar | —            | Toggle the pinned sidebar (label flips)      |
| Widen Sidebar              | —             | Increase sidebar width by 2 columns          |
| Narrow Sidebar             | —             | Decrease sidebar width by 2 columns          |

### Config

When the host application has not wired up settings getters/setters, the entire Config
menu is replaced with the placeholder **(settings unavailable)**.

| Item                          | Shortcut | Description                                              |
|-------------------------------|----------|----------------------------------------------------------|
| Sub-agents…                   | Ctrl+,   | Open sub-agent configuration                             |
| Models…                       | —        | Open the model editor                                    |
| Resources…                    | —        | Open resource limits                                     |
| Statistics…                   | —        | Open the statistics view (when wired)                    |
| Mode: \<one-shot\|interactive\|both\> | —  | Cycle sub-agent dispatch mode                            |
| Recursive: \<on\|off\>        | —        | Toggle recursive sub-agent spawning                      |
| Notifications…                | —        | Open notification settings (when wired)                  |
| Notifications: \<on\|off\>     | —        | Quick-toggle notifications                                |
| Theme…                        | —        | Open the theme editor (when wired)                       |
| Keybindings…                  | —        | Open the keybinding customizer (when wired)              |

The **Keybindings…** item opens the editor that rebinds shortcuts; it appears only when
the host wires the keybinding getter/setter. (The Help menu's **Keybindings (?)…** opens
the read-only cheatsheet instead.) See [Keybinding customizer](#keybinding-customizer).

### Help

| Item                  | Shortcut | Description                          |
|-----------------------|----------|--------------------------------------|
| Command Palette…      | :        | Open the command palette             |
| Keybindings (?)…      | ?        | Open the keybinding reference        |
| About                 | —        | Show version/about information       |

Menu shortcut hints reflect the **live** binding: if you rebind an action in the
customizer, its menu hint updates to the new chord.

---

## Command palette

Open it with **Ctrl+K** or **:** (the colon only opens it when you are not in a text
input). The palette is the single hub that drives every action in the TUI.

- **Title:** `Command Palette`
- **Hint line:** `Type to filter · ↑↓ move · Enter run · Esc close`
- **Filter:** fuzzy subsequence match (`fuzzyScore`) over command names.
- **Navigation:** ↑/↓ move, **Enter** runs the highlighted command, **Esc** closes.

Commands are grouped into five categories:

| Category   | Commands                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
|------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Session    | new, next, previous, close, close-others, close-all, rename, pin, move up, move down, switch model, stop turn (`/stop`), clear queued message (`/clearqueue`), set-show goal (`/goal`), toggle markdown (`/markdown`), export markdown, export json, saved sessions browser                                                                                                                                                                                                                                                                                                                  |
| Window     | tile vertically (Ctrl+Shift+V), tile horizontally (Ctrl+Shift+H), tile grid (Ctrl+Shift+G), cascade (Ctrl+Shift+D), maximize all (Ctrl+Shift+M)                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| Transcript | find (Ctrl+F / `/`), show all (Esc), toggle messages (`a`), tool calls (`t`), thinking (`r`), errors (`e`), fold all (`f`), unfold all (`u`), copy last answer (`y`), copy last code block                                                                                                                                                                                                                                                                                                                                                                                                  |
| Config     | sub-agent settings (Ctrl+,), models, resources, statistics, notifications, theme editor                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| App        | pin/unpin sidebar, command palette (Ctrl+K / `:`), keybinding help (`?`), quit (Ctrl+Q)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |

---

## Keybindings by context

Key behaviour depends on **what has focus**. There are four contexts: the global desktop,
the transcript view, the sidebar, and the message input box.

### Global / desktop

All chords below are **defaults**. The catalog chords are rebindable from the
[keybinding customizer](#keybinding-customizer); the exceptions are Ctrl+C (the native
quit-when-unconsumed tail) and the fixed Ctrl+K / Ctrl+F convenience accelerators.

| Action                  | Shortcut        | Notes                                            |
|-------------------------|-----------------|--------------------------------------------------|
| Quit                    | Ctrl+Q, Ctrl+C  | Confirms via `confirmQuit`                       |
| Command palette         | Ctrl+K, `:`     | Ctrl+K anywhere (fixed); `:` outside a text input (rebindable) |
| Find in transcript      | Ctrl+F, `/`     | Ctrl+F anywhere (fixed); `/` while a transcript is focused (rebindable) |
| Keybinding help         | `?`             | Only when not in a text input                    |
| New session             | Ctrl+N          | —                                                |
| Next session            | Ctrl+]          | —                                                |
| Close session           | Ctrl+W          | —                                                |
| Sub-agent settings      | Ctrl+,          | —                                                |
| Tile vertically         | Ctrl+Shift+V    | —                                                |
| Tile horizontally       | Ctrl+Shift+H    | —                                                |
| Tile grid               | Ctrl+Shift+G    | —                                                |
| Maximize all            | Ctrl+Shift+M    | —                                                |
| Cascade windows         | Ctrl+Shift+D    | —                                                |

### Transcript-focused

These single keys only fire while the transcript history / `TextView` has focus; Ctrl and
Alt modifiers are ignored in this context, so they never collide with global shortcuts.
Anything not listed here falls through to the `TextView`'s own scroll handling.

| Action                  | Key  | Notes                                                       |
|-------------------------|------|-------------------------------------------------------------|
| Find                    | `/`  | Opens the find filter                                       |
| Toggle messages         | `a`  | —                                                           |
| Toggle tool calls       | `t`  | —                                                           |
| Toggle thinking         | `r`  | —                                                           |
| Toggle errors           | `e`  | —                                                           |
| Fold all                | `f`  | —                                                           |
| Unfold all              | `u`  | —                                                           |
| Copy last answer        | `y`  | OSC 52 clipboard                                            |
| Clear filter / search   | Esc  | If a search/filter is active; otherwise falls through      |
| Scroll                  | ↑ ↓ PgUp PgDn Home End | Native `TextView` scrolling                    |

### Sidebar-focused

| Action                                | Input                          |
|---------------------------------------|--------------------------------|
| Move selection                        | ↑ / ↓                          |
| Open session node                     | Enter (or click the row)       |
| Open sub-agent monologue popup        | Enter (or click the sub-agent) |
| Raise a session window                | Click the session row          |
| Resize sidebar width                  | Drag the left-edge divider     |

### Input / message box

| Action                  | Key           | Notes                                                                                |
|-------------------------|---------------|--------------------------------------------------------------------------------------|
| Submit / send           | Enter         | Sends immediately, or queues if the session is busy                                  |
| Newline                 | Shift+Enter   | —                                                                                    |
| Prompt history — older  | ↑             | Only when caret is on the first visual line                                          |
| Prompt history — newer  | ↓             | Only when caret is on the last visual line; past newest restores the in-progress draft (no wrap) |

#### Slash commands

Type these at the start of the input box:

| Command                  | Description                                            |
|--------------------------|--------------------------------------------------------|
| `/undo`                  | Undo the last turn                                     |
| `/rewind [turns]`        | Rewind N turns (default 1)                             |
| `/plan`                  | Enter plan mode                                        |
| `/act`                   | Leave plan mode and act                                |
| `/stop`                  | Stop the running turn                                  |
| `/clearqueue`            | Clear the queued message                               |
| `/goal [text\|clear]`    | Set or clear the session goal                          |
| `/markdown [on\|off]`    | Toggle or set Markdown rendering                       |
| `/thinking [on\|off]`    | Toggle or set reasoning/thinking streaming             |

---

## Sessions & Agents sidebar

The right-hand pinned panel is titled **Sessions & Agents**. It has three regions: the
session/sub-agent tree at the top, a TODO region in the middle, and an Overall metrics
band at the bottom.

### Tree

One root node per open session, with nested sub-agent nodes.

**Session row format:** `<statusIcon> Title`
- `statusIcon` is `○` when idle, `●` when active.
- Pinned sessions are prefixed with `★`.
- A trailing `⏳` badge marks a pending **permission prompt**.
- A trailing `❓` badge marks a sub-agent blocked on **CLARIFY**.

**Sub-agent row format:** `<statusIcon> Name <mark>`

| statusIcon | Meaning   |
|------------|-----------|
| ▶          | Running   |
| ‖          | Waiting   |
| ✓          | Completed |
| ✗          | Failed    |
| •          | Other     |

The `mark` suffix identifies the sub-agent kind: ` (i)` for interactive, ` (1)` for
one-shot/tool sub-agents, and empty otherwise.

### TODO region

Header **TODOs**, showing the focused session's checklist (capped at 8 rows). Each row is
`<icon> content (note)` with one of these icons:

| Icon | State      |
|------|------------|
| ☐    | Pending    |
| ◐    | In-progress|
| ☑    | Completed  |

### Overall band

Below a top separator sit the model-selector dropdown and the **Overall** metrics block.
The dropdown lists `["all models"]` plus every model's display name; selecting a specific
model scopes the sessions / sub-agents / tokens / requests / errors / cache-hit rows to
that model only.

The Overall panel shows nine metric rows:

| Row        | Notes                                              |
|------------|----------------------------------------------------|
| sessions   | Open session count                                 |
| sub-agents | Live sub-agent count                               |
| tokens in  | Cumulative input tokens                            |
| tokens out | Cumulative output tokens                           |
| requests   | Model requests                                     |
| errors     | Rendered red when > 0                             |
| cache hit %| Prompt-cache hit rate                             |
| model      | Active/default model name                          |
| api        | Endpoint host / api_type / `openai` / `-`          |

### Divider & layout precedence

The sidebar's left edge is a 1-column drag handle (`│`). Drag it to resize the sidebar;
width is clamped to `[24, screen-40]` with a default of 32 columns. On short terminals
the layout degrades gracefully: the **tree wins**, the TODO region is dropped first, then
the Overall band.

---

## Window management

Each session lives in its own resizable, movable window inside the work area (left of the
pinned sidebar, below the menu bar).

| Operation        | How                                                                 |
|------------------|---------------------------------------------------------------------|
| Resize           | Drag a corner or edge (MinWidth 50, MinHeight 12; constrained to the pinned window area) |
| Move             | Drag the title bar                                                  |
| Minimize/restore | Click the title-bar `[▾]` / `[▴]` button                            |
| Maximize/restore | Click the title-bar `[□]` / `[▣]` button — fills the work area      |
| Pin/unpin sidebar| View → Pin/Unpin Sidebar. Pinning clamps windows left of the sidebar; unpinning allows overlap |
| Tile             | View → Tile Vertically / Horizontally / Grid, or Cascade, or Maximize All — arrange windows in sidebar order |
| Sidebar width    | Drag the divider, or View → Widen/Narrow Sidebar (±2 columns)       |

New windows **cascade** onto the desktop (offset = `len(order) % 6`).

### Layout persistence

The full workbench layout is saved to `~/.gogent/workbench_layout.json` and restored on
launch. Persisted state includes: window order, titles, pin state, minimized flag,
effort, goal, bounds, sidebar width, and the Overall model selection.

---

## Transcript navigation

The transcript is the scrollable history inside each session window.

| Action                | How                                                       |
|-----------------------|-----------------------------------------------------------|
| Find                  | `/` (or Ctrl+F / View → Find) opens the **Find in Transcript** input; filters case-insensitively over header + child lines. Status note: `— find "q": N · hidden: … · Esc to clear —` |
| Filter by type        | `a` messages, `t` tool calls, `r` thinking, `e` errors    |
| Show all / clear      | Esc (or View → Show All) clears search + all type filters |
| Fold all              | `f`                                                       |
| Unfold all            | `u`                                                       |
| Copy last answer      | `y` (OSC 52 clipboard)                                    |
| Copy last code block  | View → Copy Last Code Block (extracts fenced ``` blocks)  |
| Scroll                | ↑ ↓ PgUp PgDn Home End                                    |

The final answer or error re-anchors to the bottom automatically. The live view is capped
at roughly **1000 records** (oldest trimmed first); the full transcript is always retained
in the session JSONL on disk.

---

## @-file mentions

Typing `@` at the start of a line or after whitespace opens a popup listing workspace
files (via `ListWorkspaceFiles`). `@` not at a word boundary — for example inside an
email address — does **not** open the popup.

**Popup behaviour:**
- Width 50 cells, up to 8 rows, max 50 candidates.
- Anchored above the input (below if there is no room).
- Fuzzy subsequence filter.

The input box keeps focus while the popup is open:

| Key       | Action                                                        |
|-----------|---------------------------------------------------------------|
| ↑ / ↓     | Move selection                                                |
| Enter, Tab| Accept — replaces `@<partial>` with `@<path>`                 |
| Esc       | Dismiss                                                       |
| any other | Falls through to the input, then the popup re-filters        |

**On send,** `expandMentions` inlines each `@`-mentioned file's content (capped at 64 KB,
truncated with `… [truncated]`) into the message. A transcript note `attached <paths>`
records what was attached. Unresolved mentions are left verbatim, and the transcript keeps
the message exactly as typed.

---

## Saved sessions browser

Open with **Session → Saved Sessions…** Title: **Saved Sessions**. It is a two-pane dialog
covering roughly 85% of the terminal: a list on the left, details on the right.

**List rows:** `<padded title> <YYYY-MM-DD HH:MM> <N>t <N>m`, sorted newest-first. A search
box filters by title, id, or model.

**Detail pane** shows: Title, ID, Created, Turns, Messages, Tokens (in/out), Model.

| Button   | Action                                                                                          |
|----------|-------------------------------------------------------------------------------------------------|
| Open     | Opens a read-only **analysis** window — full search/filter/fold/yank toolkit, no input, no cost, side-by-side comparable |
| Continue | Re-opens the session **live**; subsequent sends append to the existing transcript               |
| Close    | Close the dialog                                                                                |

Hint line: `Tab move · Enter open (analysis) · Esc close`. The dialog stays open after an
Open or Continue, so you can launch several sessions from it.

---

## Export

**Session → Export Markdown…** and **Session → Export JSON…** write the **full backend
transcript** (not the capped live view) to disk.

**Markdown** — a readable transcript: `# Title`, an `_Exported by gogent on <RFC3339>._`
line, `## You` / `## Gogent` turns, tool calls and results in fenced blocks, and system
messages as blockquotes.

**JSON** — indented object:

```json
{
  "title": "...",
  "exported_at": "...",
  "messages": [
    {"role": "...", "content": "...", "tool": "...", "args": {...}}
  ]
}
```

**Destination:** `~/.gogent/`, filename
`gogent-session-<slug>-<YYYYMMDD-HHMMSS>.<md|json>` (file mode `0600`, directory `0750`).

---

## Statistics view

Open with **Config → Statistics…** Title: **Statistics**. A section dropdown selects
between five tabs:

| Tab       | Contents                                                                                                                                                                                                                                                                                                                                                          |
|-----------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Overview  | Grand totals (Sessions, Turns, Tokens in/out, Tool calls, Compactions) + Primary-model backend block (Requests, Success, Errors, Tokens in/out, Cached in, Cache hit %, Avg latency, error breakdown timeouts/overflows/refusals/generic) + optional Fast-model block + Models (tokens) summary                                                                 |
| Sessions  | Per-session table: ID, Turns, Tok in/out, Tools, Ctx%, Reqs, Errs, Comp                                                                                                                                                                                                                                                                                          |
| Tools     | Per-tool table: Name, Calls, OK, Fail, Avg ms                                                                                                                                                                                                                                                                                                                     |
| Skills    | Per-skill table: Name, OK, Fail, Total                                                                                                                                                                                                                                                                                                                            |
| Models    | Per-model token attribution: Model, Tokens in, Tokens out                                                                                                                                                                                                                                                                                                         |

**Export** buttons write a point-in-time snapshot to
`~/.gogent/gogent-stats-<timestamp>.<csv|json>`. CSV is long-format
(`section,name,metric,value`). Closed sessions are retained in the snapshot.

---

## Theme editor

Open with **Config → Theme…** Title: **Theme**. Fixed 80×22 dialog.

**Preset dropdown:** Default, High-contrast (Okabe–Ito), Dark (black background).

**Per-role color editing:** each role has a spec text field accepting an ANSI 0–255 code,
a `#RRGGBB` hex string, or `default`, plus a live swatch (`▉▉ Aa`). Roles are grouped:

| Group                | Roles                                                                                       |
|----------------------|---------------------------------------------------------------------------------------------|
| Session output       | user, agent, note, tool, result, info, error                                                |
| UI chrome            | desktop_fg/bg, panel_fg/bg, window_fg/bg, title, divider, accent                            |
| Controls             | menu_bar_fg/bg, dropdown_*                                                                  |
| Buttons and inputs   | button_fg/bg, input_fg/bg, text_selection_fg/bg                                             |
| Code                 | code_bg                                                                                     |

**Toggles:** Disable colours (honours `NO_COLOR`), Disable shadows (`no_shadow`).

The role list lives in a scrolling viewport with a scrollbar. Buttons: **Reset**, **Save**
(persists and re-applies live), **Cancel**.

---

## Keybinding customizer

Open with **Config → Keybindings…** (or the command palette's *Customize keybindings*).
Title: **Customize Keybindings**. It lists every rebindable action grouped by category,
showing each action's current chord and a `(default)`/`(custom)`/`(unbound)` tag.

- **Enter** on a row enters *capture mode*: the next chord you press becomes the new
  binding. **Esc** cancels capture; **Backspace** clears the binding (unbinds it).
- A change takes effect **immediately** — in every open window — and is persisted to
  `config.json` under `keybindings.overrides`. The matching menu shortcut hint updates too.
- If the captured chord is already used by another action in the same scope, a prompt
  offers to **reassign** it (a lossless swap that gives the other action your action's old
  chord). Rebinding a path *to* this customizer (`?`/`:`) asks for confirmation first.
- A *Global* shortcut must be a chorded key (Ctrl/Alt or a function key); a plain letter
  is rejected because text inputs would capture it.
- **Reset** restores the selected action's default; **Reset All** restores every default.

Every action ID, default, and scope is listed in
[configuration.md → KeybindingsConfig](configuration.md#keybindingsconfig). Slash commands
(`/fork`, `/stop`, …) are typed text, not keybindings, and are not shown here.

---

## Notifications settings

Open with **Config → Notifications…** Title: **Notifications**. Two groups of checkboxes;
changes are written back only on **OK**.

**Delivery (channels):**

| Channel  | Meaning                                    |
|----------|--------------------------------------------|
| Enabled  | Master switch                              |
| Bell     | Terminal bell (`\a`)                       |
| Desktop  | OSC 9 / OSC 777 escape sequences           |
| Native   | `notify-send` / `terminal-notifier`        |

**Notify on (events):**

| Event            | Notes                                                       |
|------------------|-------------------------------------------------------------|
| Task complete    | —                                                           |
| Errors           | —                                                           |
| Approval prompts | Never suppressed even when "Skip when focused" is on        |
| Clarification    | Sub-agent CLARIFY state                                     |
| Skip when focused| Suppress non-critical events while the session has focus    |

Buttons: **OK**, **Cancel**.

---

## Model editor

Open with **Config → Models…** Title: **Model Settings**.

At the top is a **Model dropdown** listing each model as `DisplayName (Name)` with a ✓
marking the default. Editing fields:

| Field          | Control   | Notes                                                                                       |
|----------------|-----------|---------------------------------------------------------------------------------------------|
| Display name   | text      | —                                                                                           |
| API type       | dropdown  | —                                                                                           |
| Endpoint       | text      | —                                                                                           |
| Model id       | text/dropdown | **Scan** button probes the backend listing and replaces the field with a dropdown of advertised models |
| API key        | text      | —                                                                                           |
| Temperature    | text      | —                                                                                           |
| Max tokens     | text      | —                                                                                           |
| Reasoning      | select    | Effort level                                                                                |
| Thinking       | select    | default / on / off                                                                          |

Buttons: **Save**, **Cancel**, **Set Default** (marks the current model as default for new
sessions).

> Note: `effort_options`, `context_window`, `top_p`, and `free` are config-only fields
> (set in `config.json`); they are not exposed in this editor. See
> [configuration.md](configuration.md).

---

## Per-window status line

Each session window shows a status line summarising the running turn:

```
‹state› · [budget!] · ‹elapsed› · ‹N t/s› · <in>/<out> tok · <n> turns · ctx ▰▰▱▱▱▱ <pct>%
```

| Segment   | Meaning                                                                                                                  |
|-----------|--------------------------------------------------------------------------------------------------------------------------|
| state     | `idle` / `working...` / `thinking... (step N)`. Prefixed `PLAN · ` in plan mode. Appended ` · queued: <preview>` when a message is queued. |
| budget!   | Shown when cumulative token usage ≥ `TokenBudget`; a one-time `[Budget] token budget exceeded` note is added to the transcript. |
| elapsed   | Live timer since turn start (`Ns` or `MmSSs`), only while busy.                                                         |
| throughput| Output tokens/sec (`N t/s`; `<1 t/s` under 1), only while busy.                                                         |
| tokens    | Cumulative `<in>/<out> tok` (k/M suffixes).                                                                              |
| turns     | `<n> turns`.                                                                                                             |
| ctx gauge | 6-cell bar of context-window usage. Amber at ≥60%, red at ≥80% (the compaction threshold).                               |

**Color rules:** red when budget-exceeded or context ≥80%; amber when budget-approaching
or context ≥60%; dim grey when idle; green when working. On narrow windows the right-most
segments are dropped first; the state segment is always visible.

### Running-turn input controls

While a turn is running, the **Send** button is replaced by a right-aligned vertical
column of controls:

| Control      | Behaviour                                                              |
|--------------|------------------------------------------------------------------------|
| Queue ⏎      | Drain-on-idle queue (mirrors Enter)                                    |
| Interject    | Splice the current input into the running turn (greyed when input empty)|
| ■ Stop       | Cancel the running turn and clear the queue                            |

Labels degrade to glyphs on narrow windows.
