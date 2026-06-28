# Design — Issue #570: working-directory path missing on the status line in remote/SSH-attached TUI mode

## Problem

In remote (SSH-attached) TUI mode the session's working-directory path is NOT shown
right-aligned on the status line above the input box (the idle / "thinking… (step N)"
line). Locally it shows correctly (shipped #551/#563). Remotely it is absent because the
attached client leaves the `GetWorkspaceRoot` presentation handler **nil**, so
`Workbench.WorkspaceRoot()` returns `""` and the status-line `DrawFn` omits the path.

Expectation: show the **DAEMON-side** working directory (where `!` shell commands and the
agent's shell tool calls actually run) on that status line in remote mode too, exactly as
locally.

## Root cause (confirmed by reading the code)

The rendering is already complete and needs **no change**:

- `ui/tui/tui.go:205-212` — `Handlers.GetWorkspaceRoot func() string`. Documented as "Read
  live on each status refresh via `Workbench.WorkspaceRoot`, so a runtime Handlers swap (a
  daemon attach/handoff) is reflected immediately."
- `ui/tui/tui.go:917-929` — `Workbench.WorkspaceRoot()` returns `""` when the handler is nil,
  else calls it live (no cache).
- `ui/tui/session_window.go` — the status-line `DrawFn` already paints the shortened,
  right-aligned path when `WorkspaceRoot()` is non-empty (#551/#563).

The gap is purely wiring on the gogent client side:

- Embedded mode wires it: `cmd/embedded_handlers.go:404-406`
  `GetWorkspaceRoot: func() string { return g.GetWorkspaceRoot() }` — the *local* core's root.
- Remote mode does NOT: `RemoteClient.Handlers()` (`ui/tui/remote_handlers.go:825`) never sets
  `GetWorkspaceRoot`, so it stays nil. `installPresentationHandlers`
  (`cmd/attach.go:334-368`) deliberately does not set it either (it is daemon-owned, like
  the default model).

### Already-present server half (no daemon change needed)

The daemon endpoint the issue asks us to "add" **already exists** on `origin/main`:

- `internal/server/api.go:266` — `{Path: "/workspace", Method: GET, Handler: sys.Workspace,
  AuthLevel: req}` (same `req` auth level as `/stats`, `/sessions`, etc.).
- `internal/server/resources.go:314-327` — `systemSvc.Workspace` returns
  `workspaceView{Root: g.GetWorkspaceRoot(), Git: …}` (root + optional git branch/dirty).
- `internal/server/wire.go:421-424` — `workspaceView{Root string; Git *gitInfo}`.
- `internal/server/server_test.go:437` — `TestWorkspaceEndpoint` already covers the 200 +
  non-empty root round-trip.

So the daemon already exposes its own `g.GetWorkspaceRoot()` — exactly the path where shell
and tool calls run. The remaining work is to consume it from the attached client. (If a
rebase onto current `origin/main` ever shows the endpoint missing, we add it mirroring the
above; the design below does not depend on which PR introduced it.)

## Implementation (gogent only)

### 1. `ui/tui/api_client.go` — client method + DTO

Mirror the established "one DTO per server view" convention:

```go
// WorkspaceDTO mirrors the server's workspaceView (GET /api/workspace): the daemon's
// own working directory (where ! shell commands and agent tool calls run) plus optional
// git info. The attached TUI uses Root for the status-line path affordance (issue #570).
type WorkspaceDTO struct {
    Root string        `json:"root"`
    Git  *GitInfoDTO   `json:"git,omitempty"`
}

type GitInfoDTO struct {
    Branch string `json:"branch,omitempty"`
    Dirty  bool   `json:"dirty"`
}

// Workspace returns the daemon's workspace root (and optional git info). It backs the
// attached status-line path (issue #570); the daemon root is immutable for the daemon's
// lifetime, so the caller caches it.
func (c *APIClient) Workspace() (WorkspaceDTO, error) {
    var out WorkspaceDTO
    if err := c.do(http.MethodGet, "/workspace", nil, &out); err != nil {
        return WorkspaceDTO{}, err
    }
    return out, nil
}
```

`Git`/`GitInfoDTO` is carried for forward-use (future status-line git decoration) but only
`Root` is consumed now; including it keeps the DTO a faithful mirror and avoids a later API
touch. (Acceptable alternative: return a bare `string`. I prefer the DTO for convention
parity — every other endpoint mirrors its view type.)

### 2. `ui/tui/remote_handlers.go` — wire `GetWorkspaceRoot`, cached + non-blocking

`GetWorkspaceRoot` is read **live on every status refresh** (per its doc), which can fire
on each "thinking" tick. A naive `func() string { r, _ := c.Workspace(); return r }` would
issue an HTTP round-trip on every refresh and, worse, block the UI render thread on the SSH
tunnel. The daemon root is immutable, so we fetch **once, lazily, in the background**, cache
it, and return `""` until it arrives (the status line simply omits the path for the first
tick or two, then shows it — graceful, no flespec, matches the nil-safe contract).

Add cache state to `RemoteClient`:

```go
// wsRoot caches the daemon's (immutable) workspace root for the status-line path
// (issue #570). GetWorkspaceRoot is read live on every status refresh, so the value is
// fetched once in the background and cached rather than round-tripped per refresh; until
// the first fetch lands the status line omits the path (nil-safe contract preserved).
wsMu       sync.Mutex
wsRoot     string
wsFetching bool
```

Helper:

```go
// cachedWorkspaceRoot returns the daemon's workspace root for the status line, fetching
// it once in the background and caching it (issue #570). It never blocks the UI thread:
// the first call kicks an async GET /api/workspace and returns "" (status line omits the
// path); once the fetch lands every later call returns the cached root. A failed fetch is
// not cached, so a later refresh retries — covering a transient blip at attach time.
func (rc *RemoteClient) cachedWorkspaceRoot() string {
    rc.wsMu.Lock()
    defer rc.wsMu.Unlock()
    if rc.wsRoot != "" {
        return rc.wsRoot
    }
    if !rc.wsFetching {
        rc.wsFetching = true
        go rc.fetchWorkspaceRoot()
    }
    return ""
}

func (rc *RemoteClient) fetchWorkspaceRoot() {
    ws, err := rc.client.Workspace()
    rc.wsMu.Lock()
    defer rc.wsMu.Unlock()
    rc.wsFetching = false // allow a later refresh to retry if this attempt failed
    if err == nil && ws.Root != "" {
        rc.wsRoot = ws.Root
    }
}
```

Wire it in `Handlers()` (add one field to the returned struct, next to the other
daemon-owned getters):

```go
// GetWorkspaceRoot reports the DAEMON's workspace root — where ! shell commands and
// agent tool calls actually run — so the attached status line shows the same path the
// local TUI does (issue #570). Daemon-owned (like the default model), so it is wired
// HERE rather than by installPresentationHandlers. Cached + non-blocking.
GetWorkspaceRoot: rc.cachedWorkspaceRoot,
```

Update the `Handlers()` doc block: remove "the @-file workspace bridge" from the
"intentionally left nil" list only insofar as `GetWorkspaceRoot` is concerned — actually
that line refers to `ListWorkspaceFiles`/`ReadWorkspaceFile` (still nil); leave it but the
narrower point is that `GetWorkspaceRoot` is now wired and no longer in the deferred set.

### 3. `cmd/attach.go` / `cmd/handoff.go` — verify only (no change)

Both build the attached handler set as `rc.Handlers()` then layer
`installPresentationHandlers` (`cmd/attach.go:204`, `cmd/handoff.go:286`).
`installPresentationHandlers` (`cmd/attach.go:334-368`) does **not** set `GetWorkspaceRoot`,
so the value from `rc.Handlers()` survives. No change required; confirmed by reading both.

The local `g` passed to `installPresentationHandlers` in attached mode is a *client-side*
core whose root is the client's cwd — which is exactly why we must NOT source the path from
it. Sourcing from `rc` (the daemon) is the whole point of the fix.

### 4. Tests

- **Flip the #551 pin**: `cmd/getworkspaceroot_wiring_issue551_test.go`
  `TestGetWorkspaceRootAbsentFromRemoteHandlersIssue551` currently asserts
  `handlers.GetWorkspaceRoot != nil` is a **failure** and documents "v1 scope: no protocol
  field". Flip it to assert the handler IS wired (`!= nil`) and rewrite the comment block to
  describe #570 (daemon root over GET /api/workspace). The test composes the same
  `rc.Handlers()` → `installPresentationHandlers` path the real attach does, so it directly
  pins the wiring. Optionally assert the handler returns the daemon root by pointing the test
  daemon's core at a known workspace (the helper `daemonWithModelsIssue507` builds a real
  in-process server; calling the handler should return the daemon `g`'s root once the async
  fetch lands — may need a short poll/eventually, so keep the core assertion to `!= nil` and
  cover the value in the api_client unit test below to avoid timing flakiness).

- **New `APIClient.Workspace()` unit test**: `ui/tui/api_client_workspace_test.go` using the
  same `httptest.NewServer` + `NewAPIClient(srv.URL, "tok")` pattern as
  `api_client_remove_model_test.go`. Assert the client issues `GET /api/workspace`, carries
  the bearer token, decodes `{"root":"/daemon/ws"}`, and returns `Root == "/daemon/ws"`.
  Add a second case asserting a non-2xx surfaces an error (root empty).

- **Optional handler-cache test** (`ui/tui/remote_handlers_workspace_test.go`): point a
  `RemoteClient` at an httptest server, call `cachedWorkspaceRoot()` until it returns the
  root (eventually), and assert only one `/api/workspace` request was made across repeated
  calls (cache works). Keep it tolerant of the async fetch (poll with a bounded deadline).

## Design criteria

### (1) Goal match
This is a **fix** for the exact gap the issue names: remote status line omits the path. It
wires the existing nil handler to the existing daemon endpoint. No new feature, no refactor,
no scope creep — the rendering (#551/#563) and the server endpoint are untouched. The path
shown is the daemon's `g.GetWorkspaceRoot()`, i.e. where shell/tool calls run (acceptance #2).

### (2) Usability
Parity with local: same right-aligned, shortened path on the same idle/thinking line, via
the same `DrawFn`. The path reflects where commands actually run (the daemon), not the
client's cwd — which is the correct, non-misleading thing to surface for an SSH-attached
user. The fetch is async so the UI never stalls on the SSH tunnel; the path appears within a
refresh tick of attach and stays put (immutable root). Nothing is silently wrong: before the
fetch lands the line simply has no path (the documented nil-safe behaviour), never a stale or
client-side path.

### (3) No regressions
- Embedded/local mode: untouched — `cmd/embedded_handlers.go` still sources the local root.
- `installPresentationHandlers` does not set `GetWorkspaceRoot`, so layering order is safe;
  handoff embedded↔attached swaps the handler and `WorkspaceRoot()` reads live, so the path
  follows the active core (consistent with `TestWorkbenchWorkspaceRootNilSafeAndLive`).
- The only behavioural change to an existing test is the intentional #551 pin flip.
- Concurrency: `cachedWorkspaceRoot` is called from the UI thread; the background fetch
  touches only mutex-guarded fields — clean under `-race`. The mutex is never held across the
  HTTP call (the fetch goroutine acquires it only after `Workspace()` returns), so the UI
  thread never blocks on the network.
- Forbidden imports: `ui/tui` gains no new import (`net/http`, `sync` already present); no new
  dep, no go.mod bump.
- Session/transcript invariants: unaffected — read-only metadata fetch.

### (4) Holistic design across both repos
**gogent-only.** turbotui is the generic TUI widget/runtime library; the status-line
`DrawFn` and `WorkspaceRoot` plumbing live in gogent's `ui/tui`, and the workspace concept is
a gogent domain notion (a daemon's working directory). turbotui has no concept of a "daemon
workspace root" and must not gain one — the seam is respected. Checked `$HOME/work/turbotui`
is reference-only; no change there. The change sits at the correct layer: the
client↔daemon protocol seam (`api_client.go` ↔ `internal/server`), reusing the existing
endpoint, auth level (`req`), DTO-mirror convention, and the daemon-owned-handler pattern
already established for the default model (#507). Downstream effect on the other repo: none.

## Regression risks (call-outs)
- **Async-appears latency**: the path is absent for the first status refresh(s) after attach.
  This is by design and matches the documented nil-safe contract; acceptable and strictly
  better than blocking the UI thread on the SSH tunnel. If a reviewer prefers
  eager-on-connect, an alternative is to prime `cachedWorkspaceRoot` once from
  `StartGated`/attach wiring — noted as an option, not chosen (keeps the fetch lazy and out of
  the connect fast-path, issue #516 spirit).
- **Auth**: `/workspace` is `req` (authenticated), same as the other read endpoints the
  attached client already calls successfully; no new auth surface.
- **Pin-flip value assertion timing**: asserting the *returned root value* in the cmd wiring
  test risks async-fetch flakiness; the design keeps the cmd test to `!= nil` and proves the
  value/round-trip in the deterministic api_client unit test.

## Open questions
1. **DTO vs bare string** for `APIClient.Workspace()` — chosen the `WorkspaceDTO` mirror for
   convention parity and future git-info use; reviewer may prefer a bare `(string, error)`.
   Low-stakes, trivially reversible.
2. **Surface git branch/dirty on the remote status line too?** The endpoint already returns
   it and the local line may show it; if local parity includes git decoration, we already
   have the data in `WorkspaceDTO.Git` and would only need the same `DrawFn` to consume it.
   Out of scope for #570 (which is specifically the *path*); flagged so it isn't a surprise
   gap. No code added for it now.
3. **Eager vs lazy prime** (see regression call-out) — confirm lazy/async is acceptable;
   that is the chosen default.
