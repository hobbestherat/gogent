# Design — `!`-prefixed shell commands (issue #571)

> Closes #571. gogent-only. stdlib-first, no new deps, no go.mod bump. FEATURE.

## Problem

Typing `!ls` / `!git status` in a session input box is sent to the model as a
plain user message: it wastes a turn/tokens, gives no shell feedback, and
pollutes the conversation. The only locally-handled prefix today is `/`
(`handleSlashCommand`). The `GetWorkspaceRoot` doc comment
(`ui/tui/tui.go:205`) already advertises WorkspaceRoot as "where `!`-prefixed
and agent shell commands run" — the `!`-half was never implemented.

## Goal (the exact ask)

`!<cmd>` runs `<cmd>` **out-of-band** — on the host (embedded) or the daemon
(remote/SSH-attached) — at the WorkspaceRoot, shows stdout/stderr/exit inline in
that session's transcript, and **never** involves the model (no turn, no tokens,
not added to the model conversation). `/` and normal messages are unchanged; a
bare `!` is a safe no-op. `ui/tui` adds **no** direct `os/exec` — execution is
dispatched through a handler/endpoint, preserving the presentation-vs-execution
layering.

## Architecture (mirrors the existing `OnSend` / `OnUndo` seams)

```
SessionWindow.submit ──"!cmd"──▶ handleBangCommand ──▶ wb.handlers.OnShell(cmd) ──▶ ShellResult
        │ (strip "!")                                          │
        │                              EMBEDDED ───────────────┤── internal/shell.Execute(Dir=WorkspaceRoot)
        │                              REMOTE   ───────────────┘── APIClient.Shell → POST /api/shell
        ▼                                                            │
  addShellResult(cmd, res, err)  ◀── render inline (display-only) ──┘ systemSvc.Shell → internal/shell.Execute(Dir=daemon WorkspaceRoot)
```

`OnShell` is the single new seam. Embedded wires it to `internal/shell`; the
RemoteClient wires it to a new daemon endpoint. `ui/tui` only ever calls the
handler — it never execs and never imports `internal/shell`.

## Files & functions to touch (all gogent; **no turbotui change**)

### 1. Input routing — `ui/tui/session_window.go`
- **New** `func (sw *SessionWindow) handleBangCommand(text string) bool`
  (sibling of `handleSlashCommand`, ~L1955):
  - `!strings.HasPrefix(text, "!")` → return `false` (fall through unchanged).
  - `strings.TrimSpace(text[1:]) == ""` (bare `!`) → `sw.addNote("usage: !<shell command>")`, return `true` (no-op, no exec).
  - else strip the leading `!`, and dispatch on a **background goroutine**
    (shell can block up to `shell.DefaultTimeout` = 5 min; must not freeze the
    UI thread). The goroutine calls `wb.handlers.OnShell(cmd)` and marshals the
    result back onto the UI thread (same `wb.EmitSessionEvent`/post-to-UI
    pattern `OnSend` uses) before calling `addShellResult`.
  - If `wb.handlers.OnShell == nil` → `sw.addNote("shell commands are unavailable")`, return `true`.
- **`submit`** (~L468-535): insert the bang check **before** the `if sw.busy`
  block (right after `recordHistory`), so `!cmd` works whether idle or busy and
  never queues / touches turn state:
  ```go
  if sw.handleBangCommand(text) { input.Clear(); return }
  ```
  Slash handling and the `addUser`+`OnSend` fall-through stay exactly as-is.
  Bang commands enter Up/Down history like slash commands (they pass through
  `recordHistory`, which already includes `/`-commands).

### 2. Handler hook + result type — `ui/tui/tui.go`
- **New** field on `Handlers` (near the other `On*` hooks, ~L36+):
  ```go
  // OnShell runs a !-prefixed shell command out-of-band — on the host
  // (embedded) or the daemon (remote) at the WorkspaceRoot — and returns its
  // output WITHOUT involving the model (no turn, no tokens, not added to the
  // conversation). err is non-nil only for an execution/transport failure; a
  // non-zero command exit is a normal result carried in ShellResult.ExitCode.
  // May be nil (the read-only analysis window leaves it unwired), in which case
  // !cmd reports the feature as unavailable.
  OnShell func(command string) (ShellResult, error)
  ```
- **New** display-only struct (in `tui.go`, next to other DTO-ish UI types):
  ```go
  // ShellResult is the UI-side view of a !cmd execution, mirroring
  // internal/shell.ExecuteResult without importing it (keeping ui/tui exec-free).
  type ShellResult struct {
      Stdout   string
      Stderr   string
      ExitCode int
      Timeout  bool
  }
  ```
  Returning the structured result (not a flat string) lets the transcript show
  exit code / timeout / stderr distinctly. `internal/shell.ExecuteResult` is the
  source shape; embedded and remote impls each map into `ShellResult`.
- Update the `GetWorkspaceRoot` doc (`tui.go:205`) — the "where `!`-prefixed …
  commands run" clause is now accurate; no aspirational caveat needed.

### 3. Transcript rendering — `ui/tui/session_window.go`
- **New** `func (sw *SessionWindow) addShellResult(command string, res ShellResult, err error)`:
  - Echo the invocation as a distinct, non-user record — header `"!"` (or
    `"$ "+command`), `colorInfo`/`roleInfo`, **not** `kindUser` — reusing the
    `addNote`/`echoCommand` notice styling so it is visually separate from
    `You:`/`Gogent:`.
  - Render stdout then (if any) stderr as styled child lines
    (`styledChildLines(..., roleInfo)`), like other system blocks.
  - Annotate `[exit N]` when `res.ExitCode != 0`, `[timed out]` when
    `res.Timeout`, and on `err != nil` emit `sw.addNote("! "+command+" failed: "+err)`.
  - Empty output + exit 0 → a short `(no output)` note so the user gets feedback.
  - **Critical invariant:** these are `sw.transcript` display records only. They
    are never passed to `addUser`/`OnSend`, so they never enter the model's
    message history — satisfying "no turn, no tokens, not in context".

### 4. Embedded wiring — `cmd/embedded_handlers.go`
- Import `gogent/internal/shell`. Add to the returned `tuipkg.Handlers`:
  ```go
  OnShell: func(command string) (tuipkg.ShellResult, error) {
      res, err := shell.Execute(command, shell.ShellConfig{Dir: g.GetWorkspaceRoot()})
      if err != nil { return tuipkg.ShellResult{}, err }
      return tuipkg.ShellResult{Stdout: res.Stdout, Stderr: res.Stderr,
          ExitCode: res.ExitCode, Timeout: res.Timeout}, nil
  }
  ```
  Same `Dir=WorkspaceRoot` contract the agent shell tool uses
  (`internal/tool/tool.go:743,746`, `verify.go:66`). `shell.Execute` already
  returns a `nil` error for non-zero exits/timeouts (carried in the result), so
  `err` here is reserved for genuine launch failures.

### 5. Daemon endpoint — `internal/server`
- **`api.go`** (system block, ~L117): add
  ```go
  {Path: "/shell", Method: http.MethodPost, Handler: sys.Shell, AuthLevel: req},
  ```
  `AuthRequired`, reusing existing auth — same level as `/workspace`, `/stats`.
- **`resources.go`** — new handler beside `Workspace`:
  ```go
  func (svc systemSvc) Shell(r *http.Request, req shellRequest) (interface{}, error) {
      if err := requireHuman(r, svc.s.provider); err != nil { return nil, err } // human-only, like Stats/DaemonStatus
      if strings.TrimSpace(req.Command) == "" {
          return nil, webapi.NewHTTPError(http.StatusBadRequest, "command is required")
      }
      res, _ := shell.Execute(req.Command, shell.ShellConfig{Dir: svc.s.g.GetWorkspaceRoot()})
      return shellView{Stdout: res.Stdout, Stderr: res.Stderr,
          ExitCode: res.ExitCode, Timeout: res.Timeout, Error: res.Error}, nil
  }
  ```
  - `requireHuman` gate: `!` is an explicit human affordance; gating it to the
    human scope (as `Stats`/`DaemonStatus` do) keeps an *agent* token from
    driving the out-of-band shell. This is safety gate #5.
  - Body binds to the index-1 handler param with **no** path params (matches the
    webapi binding rules in memory: JSON body → first param after `r`,
    string fields).
- **`wire.go`** — new request/view types:
  ```go
  type shellRequest struct { Command string `json:"command"` }
  type shellView struct {
      Stdout   string `json:"stdout"`
      Stderr   string `json:"stderr"`
      ExitCode int    `json:"exit_code,omitempty"`
      Timeout  bool   `json:"timeout,omitempty"`
      Error    string `json:"error,omitempty"`
  }
  ```
  (`shellView` mirrors `shell.ExecuteResult`'s JSON tags so the client unmarshals symmetrically.)

### 6. Remote client — `ui/tui/api_client.go`
- **New** `ShellResultDTO` (mirrors `shellView`) + method:
  ```go
  func (c *APIClient) Shell(command string) (ShellResultDTO, error) {
      var out ShellResultDTO
      if err := c.do(http.MethodPost, "/shell", shellRequestDTO{Command: command}, &out); err != nil {
          return ShellResultDTO{}, err
      }
      return out, nil
  }
  ```
  (Background context is fine — `do` is the existing client helper.)

### 7. Remote wiring — `ui/tui/remote_handlers.go`
- In `RemoteClient.Handlers()` (~L876), add:
  ```go
  OnShell: func(command string) (ShellResult, error) {
      dto, err := c.Shell(command)
      if err != nil { return ShellResult{}, err }
      return ShellResult{Stdout: dto.Stdout, Stderr: dto.Stderr,
          ExitCode: dto.ExitCode, Timeout: dto.Timeout}, nil
  }
  ```
  Daemon-side `shellView.Error` (genuine launch failure) maps to a non-zero/timeout
  signal; a transport error surfaces as the returned `err`.

## User-facing behaviour

- `!ls` → runs `ls` at the workspace root, output appears inline as a system
  block headed `! ls`, with `[exit N]` only when non-zero. No `You:` bubble, no
  `Gogent:` reply, no spinner/turn.
- Works identically attached over SSH (executes on the daemon at the daemon's
  workspace root — same path the status line shows, issue #570).
- Bare `!` → one-line usage note, nothing executed.
- `!cmd` is recallable via Up/Down history, like `/cmd`.
- Long-running `!cmd` honours `shell.DefaultTimeout` (5 min) and reports
  `[timed out]`; the UI stays responsive (dispatch is on a background goroutine).

---

## Criterion 1 — GOAL MATCH
Exactly the issue's ask: a **feature** that parses leading `!`, runs the command
host/daemon-side at WorkspaceRoot, and shows output inline with **zero** model
involvement. No scope creep — no shell history pane, no env/cwd switching, no
piping into the model. Reuses `internal/shell` and the existing
handler/endpoint/auth patterns rather than inventing new machinery.

## Criterion 2 — USABILITY
Matches the chat-with-shell mental model (`/` = client command, `!` = shell, plain
= model). The user drives the input directly; output is **surfaced** inline (not
silent) and visually distinct from model turns; bare `!` gives a help note rather
than a confusing no-op; errors/exit/timeout are shown explicitly; no token cost.
Dispatch is async so the UI never blocks on a slow command.

## Criterion 3 — NO REGRESSIONS
- `handleSlashCommand` and the `addUser`+`OnSend` fall-through are untouched; the
  bang check is an additive early-return that only fires on a leading `!`.
- Shell records are display-only `sw.transcript` entries — they never reach
  `OnSend`, so the model conversation/context and token accounting are unchanged
  (transcript/session invariants preserved).
- `ui/tui` stays exec-free: **no** `os/exec` and **no** `internal/shell` import
  there (execution dispatched via `OnShell`); only `cmd/` and `internal/server`
  import `internal/shell`, both already core-side.
- New daemon route is additive and `AuthRequired`; existing routes unchanged.
- gofmt/build/vet/golangci-lint clean; `go test ./...` green save the
  pre-existing `TestUserSessionSendMessage` 404.

## Criterion 4 — HOLISTIC (gogent ↔ turbotui seam)
gogent-only. turbotui is the presentation toolkit and stays a dumb renderer: the
new logic lives entirely in gogent's `ui/tui` (which *uses* turbotui) and
`internal/server`. **No turbotui change** — `ShellResult` is defined in gogent's
`ui/tui`, and rendering reuses existing turbotv `TextView` records via the
established `transcriptRecord`/`styledChildLines` helpers. The repo seam
(turbotui = UI primitives, gogent = behaviour) is respected; no new deps, no
go.mod bump. Cross-repo downstream effects: none — turbotui has no awareness of
shell execution and gains none.

## Tests (gogent)
1. **Routing** (`session_window` test): `handleBangCommand("!ls")` dispatches to a
   stub `OnShell` with `"ls"` and returns true; `"/x"` and `"plain"` return false;
   bare `"!"` adds a note and does **not** call `OnShell`; assert `OnSend` is
   never invoked on the `!` path.
2. **Embedded** `OnShell`: wired to `internal/shell`, `!echo hi` → stdout `hi`,
   exit 0 (linux/Pi5-safe command).
3. **Daemon endpoint** (`server_test`): `POST /api/shell {command:"echo hi"}`
   returns stdout + exit 0; empty command → 400; an **agent**-scoped token →
   403 (requireHuman gate); `APIClient.Shell` round-trips against the mock/test
   server.
4. **Remote wiring**: `RemoteClient.Handlers().OnShell` maps `ShellResultDTO` →
   `ShellResult` against a stub server.
5. **Transcript**: `addShellResult` adds a system record carrying the output and
   the model is **not** called (assert no `OnSend`, transcript record kind is not
   `kindUser`).

## Open questions
1. **Result shape**: I chose a structured `ShellResult`/`shellView` (exit/timeout
   distinct) over the issue's sketch of `OnShell(cmd) (output string, error)`.
   Richer UX and testability; flat-string is the fallback if reviewers prefer the
   minimal signature.
2. **`requireHuman` on `/api/shell`**: gating to the human scope blocks an agent
   token from the out-of-band shell (safety). If the daemon ever runs without the
   human/agent scope split for a legitimate caller, drop to plain `AuthRequired`.
   Default: keep the gate.
3. **Streaming vs bounded**: I bound output via `shell.DefaultMaxOutput` (1 MB)
   and return it whole, matching the agent shell tool. No incremental streaming
   (would need an SSE channel) — acceptable for `!`-style one-shots; flag if a
   long-running `!cmd` live-tail is desired.
4. **Persistence/restore**: shell records render into the live transcript; they
   are display-only and never re-sent to the model. If session restore replays
   transcript records, shell blocks reappear as inert text (harmless). Confirm we
   don't want them excluded from the persisted index.
