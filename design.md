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
    UI thread). The goroutine calls `wb.handlers.OnShell(cmd)` and **must** hand
    the result back onto the UI thread via the existing public
    `Workbench.Post` primitive (`tui.go:936`, `w.desktop.Post`) — the same
    mechanism `EmitSessionEvent` uses (`tui.go:2632`) — before touching the
    transcript:
    ```go
    go func() {
        res, err := wb.handlers.OnShell(cmd)
        sw.wb.Post(func() { sw.addShellResult(cmd, res, err) })
    }()
    ```
    Calling `addShellResult` (which mutates `sw.transcript`) directly from the
    goroutine would race the UI thread — and because `!cmd` is allowed **while
    busy**, several can be in flight at once, so funneling every transcript
    mutation through `Post` is mandatory, not optional. This is the single
    highest-risk spot; it is spelled out here rather than left to the implementer.
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
- **`api.go`** (system block, ~L264-268, beside `/workspace`/`/stats`): add
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
- **New** `ShellResultDTO` (mirrors `shellView`) + method.
  **Must NOT use `c.do()`**: `do` caps every call at `quickTimeout = 30s`
  (`api_client.go:121`, applied at `:250`), which would kill any `!cmd` running
  longer than 30 s and contradict the daemon's 5-minute `shell.DefaultTimeout`.
  Instead mirror `SendMessage` (`api_client.go:623-642`), which deliberately
  bypasses `do` by taking a caller `ctx` and calling `newRequest`/`http.Do`
  directly so a long turn is not capped:
  ```go
  func (c *APIClient) Shell(ctx context.Context, command string) (ShellResultDTO, error) {
      req, err := c.newRequest(ctx, http.MethodPost, "/shell", shellRequestDTO{Command: command})
      if err != nil { return ShellResultDTO{}, err }
      resp, err := c.http.Do(req)
      if err != nil { return ShellResultDTO{}, fmt.Errorf("shell: %w", err) }
      defer resp.Body.Close()
      if resp.StatusCode < 200 || resp.StatusCode >= 300 { /* read body → error, as SendMessage does */ }
      var out ShellResultDTO
      // decode …
      return out, nil
  }
  ```
  The caller (RemoteClient) passes `context.Background()`, so the request's only
  bound is the daemon-side `shell.Execute` timeout — making remote `!cmd` honour
  the same 5-minute `[timed out]` semantics as embedded. This keeps embedded and
  remote behaviour identical (the usability defect the critic flagged).

### 7. Remote wiring — `ui/tui/remote_handlers.go`
- In `RemoteClient.Handlers()` (~L876), add:
  ```go
  OnShell: func(command string) (ShellResult, error) {
      // Background context: the request's only bound is the daemon's 5-min
      // shell timeout, NOT the 30s quickTimeout — matching embedded behaviour.
      dto, err := c.Shell(context.Background(), command)
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
  `[timed out]` **in both embedded and remote modes** — the remote client uses a
  background-context request (not the 30 s `quickTimeout`), so the only bound is
  the daemon-side shell timeout (see §6). The UI stays responsive (dispatch is on
  a background goroutine, result marshalled back via `Workbench.Post`).
- **Interject path note:** the `!` interception is on the `submit`/Enter path
  only. The Interject button (`session_window.go:542`, `OnPress = sw.interject`)
  is a separate path that injects a clarification into a live turn; a `!cmd`
  typed and sent via Interject goes to the model verbatim. This is an accepted
  edge (Interject is explicitly "slip text into the model's turn"); `!` is a
  no-turn affordance and intentionally does not interject. Documented so it is a
  conscious choice, not an oversight. (Optional follow-up: short-circuit `!` in
  `interject` too — out of scope here.)

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
Dispatch is async so the UI never blocks on a slow command. **Embedded and remote
behave identically**, including the 5-minute timeout: §6 routes the remote `Shell`
call through a background-context request (mirroring `SendMessage`) instead of the
30 s `quickTimeout`-capped `do()`, eliminating the embedded-vs-remote divergence.
The one deliberate asymmetry — Interject does not honour `!` — is documented above
as an accepted edge.

## Criterion 3 — NO REGRESSIONS
- `handleSlashCommand` and the `addUser`+`OnSend` fall-through are untouched; the
  bang check is an additive early-return that only fires on a leading `!`.
- Shell records are display-only `sw.transcript` entries — they never reach
  `OnSend`, so the model conversation/context and token accounting are unchanged
  (transcript/session invariants preserved).
- `ui/tui` stays exec-free: **no** `os/exec` and **no** `internal/shell` import
  there (execution dispatched via `OnShell`); only `cmd/` and `internal/server`
  import `internal/shell`, both already core-side.
- **UI-thread safety** (the highest-risk spot, resolved in §1): all transcript
  mutation from the `OnShell` goroutine funnels through `Workbench.Post`
  (`tui.go:936`) — the same primitive `EmitSessionEvent` uses — so concurrent
  `!cmd`s (allowed while busy) never race `sw.transcript`. Spelled out in §1, not
  deferred to implementation.
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
   `ShellResult` against a stub server. **Plus a timeout-bound test**: assert
   `APIClient.Shell` does **not** inherit `quickTimeout` — e.g. a stub handler
   that sleeps >30 s (or, deterministically, that the call path uses
   `newRequest` with the caller ctx, not `do`) so a future refactor back onto
   `do()` is caught.
5. **Transcript**: `addShellResult` adds a system record carrying the output and
   the model is **not** called (assert no `OnSend`, transcript record kind is not
   `kindUser`). UI-thread marshalling via `Workbench.Post` is exercised by the
   routing test (the result lands in the transcript after the posted closure runs).

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
4. **Persistence/restore** — *resolved, not open*: `restore()`
   (`session_window.go:2952`) rebuilds the transcript **solely** from persisted
   agent messages (user/assistant/tool/system). Shell records are display-only
   and are therefore **dropped** on reopen/restore — which is exactly correct for
   "not part of the conversation." No persistence work is needed; this is a
   feature of the chosen seam, not a gap.
