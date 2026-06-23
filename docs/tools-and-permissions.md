# Tools & permissions

A complete reference for every tool the agent can call, the permission Action constants that gate them, the default authorization posture, how "always" grants persist, and the checkpoint/undo/rewind + edit-review safety nets. All side-effecting tools route through a single resource+action-aware gate: `internal/permission.Service`. It resolves an `(action, resource)` pair to one of three effects — **allow**, **deny**, **ask** — and, on *ask*, consults an interactive `Prompter` (the UI). With no prompter installed (headless runs), *ask* resolves to *deny*.

## 1. Permission Action constants (`internal/permission/permission.go`)

| Action constant | Token | Description |
| --- | --- | --- |
| `ActionRead` | `read` | Read a file inside the workspace |
| `ActionWrite` | `write` | Write/edit a file inside the workspace |
| `ActionShell` | `shell` | Run a shell command; session-wide gate, asked once; also gates git mutating ops |
| `ActionExternal` | `external` | Touch a path outside the workspace; keyed on containing directory so one grant covers a whole folder |
| `ActionNetwork` | `network` | Network access / `web_fetch`; keyed per host |
| `ActionSubagent` | `subagent` | Spawn a sub-agent |
| `ActionMCP` | `mcp` | Launch/connect an MCP server; keyed on server name |
| `ActionDiagnostics` | `diagnostics` | Run compiler/linter; dedicated so an "always" grant never blesses the shell |
| `ActionVerify` | `verify` | Run the test command; dedicated, same rationale |

Path-style for `read`/`write`/`external` (an ancestor root covers descendants via `pathUnder`); the rest are scalar.

## 2. Default posture

Set up in `Gogent.NewGogentWithWorkspace`: static allow rules for `read`/`write` on `"*"`.

| Action | Resource scope | Default effect | Notes |
| --- | --- | --- | --- |
| `read` | in-workspace | allow | no prompt |
| `write` | in-workspace | allow | still subject to the edit-review gate |
| `external` | out-of-workspace | ask | keyed on containing directory |
| `shell` | session-wide | ask | one prompt can grant "always" |
| `network` | per host | ask | |
| `subagent` | per spawn | ask | |
| `mcp` | per server name | ask | |
| `diagnostics` | per call | ask | |
| `verify` | per call | ask | |

So, in summary: **workspace allow / outside ask / shell ask / headless deny**.

### How "always" grants persist

`DecisionAlways` / `DecisionAlwaysDeny` → `persist(action, resource, decision)`: records in the in-memory saved map, marshals to `~/.gogent/permissions.json` (dir `0700`, file `0600`), reloaded on startup. For path-style actions an allowed ancestor root covers descendants. Every resolved decision is recorded on the append-only audit trail.

### The resolution cascade (`Service.effect`)

`effect(action, resource, detail)` resolves a request as a fixed priority cascade — **deny guardrails first**, so a saved "always allow" can never silently defeat a guardrail:

1. **`rules.json` DENY guardrails** — if any matches → **deny**. *Hard stop; nothing below overrides it (not persisted allows, not allow rules, not yolo).*
2. **Persisted decisions** (`permissions.json`) — `always`→allow, `always_deny`→deny.
3. **ALLOW rules** — the default workspace allow-alls and any `rules.json` allow.
4. Fall through → **ask** (→ prompter; or yolo auto-approve; or deny when headless).

### Hard-deny guardrails (`~/.gogent/rules.json`, issue #355)

A user-editable file of policy rules that are respected **irrespective of mode**, including yolo. Loaded once at startup (restart to pick up edits); a missing/corrupt file or an individual malformed rule is logged and skipped — it never bricks the gate.

```json
{
  "rules": [
    {"action": "external", "resource": "/etc/*", "effect": "deny"},
    {"action": "network",  "resource": "evil.com", "effect": "deny"},
    {"action": "shell",    "resource": "*", "effect": "deny", "detail_pattern": "rm\\s+-rf\\s+/"}
  ]
}
```

- `action` — an `Action` constant, or `*` for any.
- `resource` — `*`, a trailing-`*` prefix wildcard, or a literal; for path actions (`read`/`write`/`external`) a literal also matches any path nested under it.
- `effect` — `allow` or `deny`. A **deny** rule is a hard guardrail (cascade step 1).
- `detail_pattern` (optional) — a Go regex matched against the request's `detail`. Because `shell` gates with `resource: ""` and carries the command text in `detail`, this is what enables command-level guardrails such as blocking `rm -rf /`.

### Yolo mode (issue #356)

A single switch that (a) removes the step cap (every session runs unlimited steps) and (b) auto-approves every permission prompt — **except** the hard-deny guardrails above, which always hold. Enable it via `"yolo": true` in `config.json`, the `--yolo` CLI flag (overrides config), or the TUI `/yolo` command / command-palette entry (per session). In `CheckWithContext` a yolo-active request whose cascade result is **ask** becomes **allow**; `deny` is resolved by `effect()` *before* this conversion, so guardrails are never bypassed. Cancellation, the optional token budget, and the audit trail remain in force under yolo — set a token budget as the financial brake.

## 3. Every tool

`ReadOnly: true` tools run concurrently with other read-only calls in one turn (parallel fast-path); side-effecting tools run serially. MCP/dynamic tools default to `false`.

### File tools (registered in `internal/gogent`; primitives in `internal/fileops`)

| Tool | Read-only | Permission gate | Description |
| --- | --- | --- | --- |
| `read` | ✅ | `read` in-ws / `external` out-of-ws | Read a file from the workspace |
| `write` | ❌ | `write` / `external` | Create or overwrite a file |
| `edit` | ❌ | `write` / `external` | Replace exact text, single match or `replace_all`; conditional write |
| `multi_edit` | ❌ | `write` / `external` | Apply several ordered find→replace edits to one file; all-or-nothing |
| `apply_patch` | ❌ | `write` / `external` per file | Apply a `*** Begin Patch` unified-diff envelope to add/update/delete files; all-or-nothing per file |
| `grep` | ✅ | none (workspace-confined) | Search file contents for a Go regex; returns `file:line` refs |
| `glob` | ✅ | none | List workspace files matching a shell-style glob |
| `list` | ✅ | none | List files/subdirs immediately inside a directory |

File access is gated by `fileops.CheckFileAccess`: in-workspace uses `read`/`write` keyed on the workspace-relative resource; escaping paths use `external` keyed on the containing directory. `grep`/`glob`/`list` are read-only **and** workspace-confined — they never prompt.

### Shell & system tools (`internal/tool`)

| Tool | Read-only | Permission gate | Description |
| --- | --- | --- | --- |
| `shell` | ❌ | `shell` (session-wide) + `external` per external root dir | Execute a shell command, cwd=workspace root; 5-min timeout, 1 MB cap |
| `git` | ❌ mutating / ✅-ish read | `shell` only for mutating ops (`commit`/`create_branch`/`restore`) | Run native git via explicit arg vectors; `status`/`diff`/`log` run free |
| `web_fetch` | ✅ | `network` (host) | Fetch an http(s) URL, return readability-extracted Markdown; size-capped, short-TTL cached |
| `calc` | ✅ | none | Evaluate a math expression via the hardened `mathexpr` evaluator (`github.com/expr-lang/expr` + a curated math env): operators `+ - * /`, power (`**`/`^`), unary minus, parentheses (`%` is integer-only modulo — `mod(x,y)` for floats; a comparison is valid only as a ternary condition); functions (sqrt, trig, log, abs/round, factorial/gcd/lcm, …) and constants (`pi`, `e`, physics `c`/`G`/`g`/…). Integer results print cleanly, fractionals at full precision |
| `diagnostics` | ❌ | `diagnostics` | Run configured compiler/linter (default `go vet ./...`); returns structured `file:line:col` findings |
| `verify` | ❌ | `verify` | Run configured test command (default `go test ./...`); returns structured pass/fail + parsed failures |

**Shell gating:** the session-wide `ActionShell` is asked once, then `shell.ExternalRoots` best-effort scans for paths escaping the workspace and gates each distinct external root through `ActionExternal`.

**Git gating:** `commit`/`create_branch`/`restore` gate through `ActionShell`; `status`/`diff`/`log` run free.

`diagnostics`/`verify` commands are fixed by config, pinned to the workspace root, but execute build/test code so they are gated through dedicated actions. See [configuration.md](configuration.md) for the `ReviewEdits` toggle and the `diagnostics`/`verify` command settings.

### Sub-agent & coordination tools (registered in `internal/gogent`)

| Tool | Read-only | Permission gate | Description |
| --- | --- | --- | --- |
| `spawn_subagent` | ❌ | `subagent` | Delegate work to sub-agents; a `subtasks` array runs them concurrently one-shot, returns `SUCCESS`/`FAILURE` |
| `launch_agent` | ❌ | `subagent` | Launch an async interactive sub-agent; returns `agent_id` immediately |
| `agent_status` | ❌ | none | Query status/result of an interactive sub-agent |
| `agent_send` | ❌ | none | Answer an interactive sub-agent's `CLARIFY` question |
| `agent_terminate` | ❌ | none | Terminate a running interactive sub-agent |
| `wait_agent_event` | ❌ | none | Block until an interactive sub-agent finishes/clarifies or `timeout_ms` (default 30 s) |

`spawn_subagent` and the `launch_agent` family are mutually filtered per session by the execution model. Fan-out is globally bounded by `SubAgentLimiter` and rate-limited by `RateLimiter`.

### Output / planning / skill tools

| Tool | Read-only | Permission gate | Description |
| --- | --- | --- | --- |
| `structured_output` | ❌ | none | Return the final response text + optional tool call + `final` flag |
| `todo` | ❌ | none | Read/replace the session's task checklist; shown live in sidebar, kept in system prompt |
| `skill` | ❌ | none | Load the full markdown instructions for a named skill; registered only when skills are loaded |

`todo` is intentionally **not** read-only so concurrent calls stay serial, but it is retained in plan mode.

### MCP tools (`internal/gogent/mcp.go`, `internal/mcp`)

MCP servers are connected at startup via `StartMCPServers`; launching is gated through `ActionMCP` keyed on server name. A denied/disabled/unreachable server is skipped with a warning. Each remote tool is registered as `mcp__<server>__<tool>`.

| Tool prefix | Read-only | Permission gate | Description |
| --- | --- | --- | --- |
| `mcp__<server>__<tool>` | ❌ (default) | `mcp` (at launch only) | A remote MCP tool dispatched over the shared client |

MCP/dynamic tools default `ReadOnly: false`. Per-call execution is **not** re-gated; the launch gate is the authorization point.

## 4. Checkpoints / undo / rewind (`internal/fileops/checkpoint.go`)

The `Checkpointer` snapshots touched files before each mutating `write`/`edit`, grouped by turn, so a botched multi-file edit can be rolled back without the user's VCS. Snapshots live in memory for the running process; restarting loses them (the transcript is still recovered from JSONL).

| Operation | Behavior |
| --- | --- |
| `BeginTurn` | Starts a fresh active checkpoint at the start of a user-driven turn |
| `Snapshot` | Records a file's pre-turn state; the **first** snapshot of a path within a turn wins. Best-effort: read failures swallowed |
| `CommitTurn` | Finalizes the active checkpoint onto history; a turn that mutated nothing is dropped; called even on error/cancellation |
| `AbortTurn` | Discards the in-flight checkpoint |
| `UndoLastTurn` | Reverts the most recently committed turn (existing files restored content+mode; files the turn created are removed; present-but-unreadable skipped); returns `ErrNoCheckpoint` when nothing to undo |
| `Rewind` | Reverts the last N turns (all when `N <= 0`), merging oldest-wins |
| `CheckpointCount` | Number of committed undoable turns |

History is bounded to `maxCheckpoints = 100` turns (FIFO). `Snapshot` is called by `snapshotBefore` in `write`/`edit`/`multi_edit`/`apply_patch` after the review gate and before the mutation. Per-file restore errors are joined rather than aborting the whole rollback.

## 5. Edit-review gate (`internal/gogent/review.go`)

An optional interactive diff-approval gate layered on top of the permission gate. Enabled when `config.ReviewEdits == true` **and** an `EditReviewer` is installed. Toggleable at runtime via `SetReviewEdits` (persists; disabling clears per-session approve-all grants).

`reviewActive` is `true` when review is enabled, a reviewer is installed, and the session has not chosen "approve all". Applied in `write`, `edit`, `multi_edit`, `apply_patch` (per file, in phase one before any write): preview the change, render a unified diff, call `reviewer.ReviewEdit`.

| Decision | Effect |
| --- | --- |
| `EditReject` | Nothing written; returns an error |
| `EditApprove` | Apply this one edit |
| `EditApproveAll` | Apply this and every later edit in the session without prompting |

A no-op change passes without prompting. For `apply_patch`, gating in phase one means a rejection partway through never leaves a partially-written set on disk. With no reviewer installed (headless), the gate is inert and writes proceed unchanged. The review gate runs **before** the checkpoint snapshot, so a rejected write is not recorded in undo history. See [usage-tui.md](usage-tui.md) for the edit-review modal in the TUI.

## 6. How a write/edit flows

For `write`/`edit`/`multi_edit`/`apply_patch`:

1. **Permission gate** — `CheckFileAccess` → `read`/`write` (in-workspace, default-allowed) or `external` (ask/deny); returns an `Authorization`.
2. **Edit-review gate** (if active) — preview, render diff, prompt; reject ⇒ abort before any snapshot or write. See [api.md](api.md) for the approval flow.
3. **Checkpoint snapshot** — `snapshotBefore` records the file's pre-turn state (first mutation in the turn wins).
4. **Mutation** — `FileMutation` write/edit/patch (or `Remove` for a patch delete), honoring the `Authorization`'s external allowance; `edit`/`multi_edit` use conditional writes (`WriteIfUnchanged`) to detect concurrent modification.

Every tool invocation also passes through `ToolRegistry.ExecuteToolCall`: validates args against the schema, records invocation/outcome/duration stats, contains panics, fires the `ToolCallback` (increments the session tool-call count + audit log), and refuses disabled tools. See [architecture.md](architecture.md) for where these layers sit in the overall design.
