# Design — Remote-TUI watcher-management client wiring (gogent #572)

## Problem (one paragraph)
Over an SSH attach the entire watcher-management surface is dark: no `◷` watcher
sidebar nodes, the **Watchers…** menu/dialog is hidden, and `/watcher` reports
"watchers are unavailable." The cause is purely client-side: `RemoteClient.Handlers()`
deliberately leaves every watcher handler `nil` (`ui/tui/remote_handlers.go:1284-1288`,
a "Phase-3 API-gap" note). The daemon HTTP API for watchers already exists and is
fully tested (`internal/server/api.go:243-253`, `internal/server/watchers.go`). The
only missing piece is the `APIClient` methods that call those endpoints plus the
handler wiring that the embedded build already has (`cmd/embedded_handlers.go:431-458`).
This is a **feature / wiring gap**, gogent-only, client-side only: no daemon endpoint,
no turbotui change, no new dep, no go.mod bump.

## What exists today (verified)

Daemon API (reference only — DO NOT change):
- `GET    /api/watchers[?session_id=<id>]` → `[]watcherView`  (free-running; +session's attached when `session_id` set)
- `POST   /api/watchers`                    → `watcherView`    (body `createWatcherRequest`; kind decided by `report_to_session`)
- `GET    /api/watchers/:id`                → `watcherView`
- `PUT/PATCH /api/watchers/:id`             → `watcherView`    (sparse `updateWatcherRequest`)
- `DELETE /api/watchers/:id`                → 200
- `PUT    /api/watchers/:id/enabled`        (body `{ "enabled": bool }`)
- `POST   /api/watchers/:id/toggle`
- `POST   /api/watchers/:id/run`
- `POST   /api/watchers/:id/stop`
- `:id` is resolved by the backend as **id OR name** (unknown → 404, ambiguous name → 409). All require `requireHuman` + the `Experimental.Watchers` flag (404 when off).

Wire DTO shape (`internal/server/wire.go:151-196,400`):
- `watcherView{ id, name, kind("free"|"attached"), target(session id|"free"), task, schedule(human string), enabled, status, next_fire(RFC3339), last_run(RFC3339), last_result, last_error }`
- `createWatcherRequest{ name, task, schedule(config.ScheduleConfig{every,daily_at,timezone}), model, enabled?*bool, report_to_session?*string, on_complete?*config.WatcherOutput }`

UI seam that lights up the moment the handlers are non-nil (shared embedded/remote code, **must not fork**):
- `Handlers` watcher fields (`ui/tui/tui.go:283-304`): `ListWatchers(sessionID) []WatcherInfo`, `CreateWatcher(cfg WatcherConfig, sessionID) (WatcherInfo,error)`, `EnableWatcher/DisableWatcher/RunWatcher/StopWatcher/DeleteWatcher(idOrName string) error`. **There is no `UpdateWatcher`/`ToggleWatcher` field** — embedded maps Enable/Disable onto `SetWatcherEnabled` and does not expose update/toggle. Mirror exactly this set.
- `ui/tui/tui.go:3243` `refreshWatcherNodes()` early-returns when `ListWatchers==nil`; otherwise it calls `ListWatchers("")` (keeps `Free`) once + `ListWatchers(sid)` per open session (keeps `!Free && TargetSession==sid`) on the 1s status tick.
- `ui/tui/watchers_dialog.go:43-46` `showWatchersDialog` bails when `ListWatchers==nil`; the dialog buttons call `EnableWatcher/DisableWatcher/RunWatcher/StopWatcher/DeleteWatcher` and re-render via `loadWatcherItems(ListWatchers, ActiveID)` after each action. Footer is **Open/Enable/Disable/Run/Stop/Delete** — there is no in-dialog Create form today (so the acceptance "create/edit" reduces to: the handler is wired and available; toggle == enable/disable). The **Watchers…** menu item is gated on the same handler at `tui.go:1024`.
- `ui/tui/session_window.go:2237` (`/watcher list`) and `:2288` (`/watcher enable|disable|run|stop`) report unavailable when the handler is nil.

`WatcherInfo` (UI type, `ui/tui/tui.go:531-560`) fields the mapper must fill: `ID,Name,Free,TargetSession,SessionID,Enabled,Status,Running,Task,Schedule,NextFire,LastRun,LastResult,LastError`. The embedded mapper (`cmd/main.go:327 toWatcherInfo`) sets `Free = kind==free`, `SessionID = TargetSession` (attached) or `"watcher:"+name` (free), `Running = status=="running"`, and formats `NextFire/LastRun` as `"2006-01-02 15:04"`.

Backend-only `watcher:<name>` sessions are excluded from the remote session list already (`remote_handlers.go:988,1038` via `watcherSessionPrefix`; `internal/server/wire.go`) and their transcript + completion notification stream over the existing global SSE consumer — **preserve, touch nothing here.**

## Implementation plan (gogent, client-side only)

### 1. `ui/tui/api_client.go` — watcher endpoint methods + DTO
Add a `WatcherDTO` mirroring `watcherView` (all 11 JSON fields), then thin methods reusing the existing `c.do()` / auth / quickTimeout plumbing (mirror `ListSessionsBounded`/`CreateSession`/`SetPlanMode`):
- `ListWatchers(sessionID string) ([]WatcherDTO, error)` → `GET /watchers`, appending `?session_id=` via `url.Values` only when non-empty.
- `CreateWatcher(req WatcherCreateDTO) (WatcherDTO, error)` → `POST /watchers`. `WatcherCreateDTO` mirrors `createWatcherRequest`; `Schedule` reuses `config.ScheduleConfig` (already imported in api_client.go).
- `SetWatcherEnabled(idOrName string, enabled bool) error` → `PUT /watchers/{esc}/enabled` body `{enabled}`.
- `RunWatcher(idOrName string) error` → `POST /watchers/{esc}/run`.
- `StopWatcher(idOrName string) error` → `POST /watchers/{esc}/stop`.
- `DeleteWatcher(idOrName string) error` → `DELETE /watchers/{esc}`.
- (Optional, for completeness/symmetry — only if cheap: `GetWatcher`, `UpdateWatcher`, `ToggleWatcher`. Not required by any UI seam; **omit** to avoid dead code unless a test wants them. Decision: omit — keep the surface to exactly what the handlers use, matching embedded.)

All `idOrName` segments go through `url.PathEscape` (names can contain spaces). Each method returns the `c.do()` error verbatim so the handler can surface it.

### 2. `ui/tui/remote_handlers.go` — wire the 7 handlers (replace the nil note at 1284-1288)
Replace the deferred-note block with implementations that mirror `cmd/embedded_handlers.go:431-458`, calling the new `APIClient` methods. Also update the file-top "deferred … watcher-management API" comment (`:873`) to drop watchers from the deferred list.

- `ListWatchers: func(sessionID string) []WatcherInfo` → calls the **non-blocking cache** (below), mapping `WatcherDTO → WatcherInfo` with a client-side `watcherDTOToInfo` that reproduces `cmd/main.go:327 toWatcherInfo` semantics from the wire shape. **Copy every `WatcherInfo` field**, not just the non-trivial ones: the pass-through fields `ID, Name, Enabled, Status, Task, Schedule, LastResult, LastError` are taken verbatim from the DTO; the derived fields are `Free = dto.Kind=="free"`; `TargetSession = "" if free else dto.Target`; `SessionID = watcherSessionPrefix+dto.Name if free else dto.Target`; `Running = dto.Status=="running"`; and `NextFire`/`LastRun` are reparsed from RFC3339 and reformatted to `"2006-01-02 15:04"` (keep raw on parse failure). Returns `nil` on error (handler degrades to "no watchers", same as embedded on empty).
- `CreateWatcher: func(cfg WatcherConfig, sessionID string) (WatcherInfo, error)` → build `WatcherCreateDTO{Name,Task,Model, Schedule:{Every,DailyAt,Timezone}, ReportToSession: cfg.ReportToSession, Enabled: ptr(true)}` and POST. Set `Enabled` **explicitly to true** to mirror embedded's `Enabled:true` (the daemon defaults omitted→true, but being explicit removes any drift). The daemon decides kind purely from `report_to_session` (its Create passes "" as the calling session and normalizes the sentinel), so the client forwards `cfg.ReportToSession` and does **not** need `sessionID`; document that the over-the-wire create derives kind from `report_to_session`, identical to the daemon's tool path. On success map the returned `WatcherDTO`→`WatcherInfo` and invalidate the cache.
- `EnableWatcher`/`DisableWatcher` → `c.SetWatcherEnabled(name, true/false)`.
- `RunWatcher`/`StopWatcher`/`DeleteWatcher` → the matching `APIClient` method.
- Every **mutation** handler, on success, invalidates the watcher cache (see §3) so the dialog's immediate post-action re-render shows fresh state.

### 3. Non-blocking watcher cache on `RemoteClient` (the one design subtlety)
`refreshWatcherNodes` calls `ListWatchers` **on the UI thread every 1s**, once per open session + once for `""`. `c.do()` is bounded by `quickTimeout = 30s`, so a *synchronous* per-tick GET over a stalled SSH tunnel would freeze the UI for up to 30s per call — exactly the hazard the codebase already solved for the status-line path with `cachedWorkspaceRoot` (`remote_handlers.go:836`, "must NOT block the UI thread"). Mirror that established pattern, generalized to a key (the session id):

- Add to `RemoteClient`: `watchMu sync.Mutex`, `watchCache map[string][]WatcherInfo`, `watchFetching map[string]bool`, and **`watchGen uint64`** (an epoch counter — the fix for the write-skew race below).
- `cachedWatchers(sessionID)` (backs the `ListWatchers` handler): under `watchMu`, return a copy of `watchCache[sessionID]`; if no fetch is in flight for that key, set the flag, snapshot `gen := watchGen`, and `go fetchWatchers(sessionID, gen)`. First call returns `nil` and the node appears on the next tick — acceptable, watcher nodes already only move on the 1s tick.
- `fetchWatchers(sessionID, gen)`: call `c.ListWatchers(sessionID)` **without holding the lock** (network off-lock, exactly like `fetchWorkspaceRoot`), map to `[]WatcherInfo`, then lock (`defer` unlock). **Always clear the in-flight flag via `defer`** so a panic/abort can never freeze a key's cache (matching `fetchWorkspaceRoot`'s discipline). On success store under the key **only if `gen == watchGen`** — a stale fetch (one started before a mutation bumped the epoch) is discarded rather than clobbering fresh post-mutation state. On error keep the last-good slice (avoids flicker on a transient blip); the next tick retries.
- **Dialog freshness after a mutation, race-free:** a mutation returns on the *button-click* path (already a blocking call), not the 1s tick. On success the mutation handler, under `watchMu`, **bumps `watchGen`** (invalidating every in-flight background fetch), then synchronously re-fetches every currently-cached key (bounded — `""` + open sessions) and replaces the cache. The epoch bump guarantees that a background `fetchA` which read *pre-mutation* state and lands *after* the synchronous refresh is dropped (its `gen` no longer matches), so it cannot resurrect stale data. The dialog's `loadWatcherItems(ListWatchers, ActiveID)` re-render then reads the fresh cache. (Synchronous network here is fine: the user just clicked and we already blocked on the mutation.)
- **Disconnect staleness (acknowledged):** while the daemon is unreachable, fetches error and the last-good slices are kept, so `◷` nodes — and a watcher's `Running` marker — can linger stale until reconnect, when the next successful fetch corrects them. This is the same trade-off `cachedWorkspaceRoot` accepts (no flicker over correctness on a transient blip) and is acceptable for a 1s-tick affordance.

This keeps every per-tick read O(1) and non-blocking, makes the dialog and `/watcher` reads fresh **without the write-skew race** a naive map-generalization of `cachedWorkspaceRoot` would have (single-key + no mutation refresh meant the original never hit it), and reuses the same off-lock-network idiom reviewers already accept. The cache map grows monotonically with distinct queried keys (`""` + each session ever open) — bounded by the session count and tiny, so no pruning is needed.

### 4. Live refresh & reconnect (no new code)
`refreshWatcherNodes` already runs on the 1s tick and now sees a non-nil `ListWatchers`, so `◷` nodes build/update live (create, delete, busy marker via `Running`, schedule/next-fire changes) automatically. On reconnect the same tick keeps calling the handler → the cache repopulates from the daemon → nodes re-sync with no special handling. SSE-driven `watcher:<name>` transcript + completion notification are untouched.

### 5. Tests (`ui/tui/*_test.go`)
- **APIClient**: one `httptest.Server` (or the existing mock harness) per verb asserting method+path+body and decoding the response: `GET /watchers` (and `?session_id=`), `POST`, `PUT …/enabled`, `POST …/run`, `POST …/stop`, `DELETE`. Assert `url.PathEscape` on a name with a space.
- **Handlers wiring — flip the pin**: `TestRemoteHandlersDoNotExposeDeferredWatcherAPI` (`remote_client_phase2_test.go:583`) currently asserts all watcher handlers are **nil**. Replace it with a test asserting they are now **non-nil** (rename to e.g. `TestRemoteHandlersWireWatcherAPI`) and that each routes to the right verb against a mock daemon.
- **Mapping**: `watcherDTOToInfo` table test — free vs attached `SessionID`/`TargetSession`, `Running` from status, RFC3339→`"2006-01-02 15:04"`, and that the pass-through fields (`ID,Name,Enabled,Status,Task,Schedule,LastResult,LastError`) all survive the round-trip.
- **Cache freshness / epoch**: after a mutation handler succeeds against a mock daemon, the next `ListWatchers(activeID)` returns the post-mutation state (asserts the synchronous epoch-guarded refresh), and a late background fetch carrying pre-mutation data does not resurrect it.
- **refreshWatcherNodes**: with the wired (mock-backed) `ListWatchers`, the existing sidebar-node assertions (`sidebar_watchers_test.go`) build free roots + attached children. The cache's first-call-returns-nil behaviour means a node-building test must let one fetch land (or call the handler twice / pre-warm) — document this in the test.

### 6. Known scope limitation — dialog **Open** on a *free-running* watcher over SSH
The dialog's **Open** button (`openWatcherSession`, `tui.go:3280-3307`) has two success paths, and **both are dead for free-running watchers over SSH** — this is a pre-existing gap in the saved-session restore machinery (which #572 says to *preserve*), not in the watcher API, so it is **explicitly scoped out** of this slice:
- *Path A* (`w.sessions[id]!=nil` → Focus): a `watcher:<name>` session never gets an open window remotely — the SSE consumer (`remote_handlers.go:322 consume`) only forwards events and `deliverSessionEvent` (`tui.go:2669`) only *reads* `w.sessions`; the sole writer `openWindowKind` is never reached from the watcher path.
- *Path B* (find id in `ListSavedSessions()` → `OpenSavedSession`): the remote `ListSavedSessions` filters `watcher:<name>` out (`remote_handlers.go:1038`), so the loop finds no match.
- **Net:** Open on a free-running watcher always shows *"Session … is not open yet — it appears once the watcher has fired"* — misleading even after it has fired. Embedded works because `embedded_handlers.go:292` lists watcher sessions unfiltered.
- **Attached** watchers' Open **does** work remotely: their `SessionID` is the live target session (not `watcher:`-prefixed, not filtered, and openable via the normal restore path).

Fixing free-running Open would require either un-filtering `watcher:` from remote `ListSavedSessions` + giving the daemon a way to serve a non-live watcher transcript (a saved-session API-enrichment slice), or wiring SSE to open a watcher window — both beyond "client-side wiring of the existing watcher API." Recorded as a follow-up. The `◷` node still renders, refreshes live, and all other actions (Enable/Disable/Run/Stop/Delete) work on a free-running watcher; only its **Open** is inert over SSH.

## The four design criteria

**(1) Goal match.** Exactly the issue's ask: client-side wiring so the attached TUI exposes watcher management — `◷` nodes, Watchers… dialog, `/watcher` — over the **already-existing** daemon API. No daemon endpoint added, no new UI invented (we mirror the embedded handler set 1:1), no scope creep. **Parity caveats, stated honestly:** (a) there is no `UpdateWatcher`/`ToggleWatcher` field on the `Handlers` seam, so task/schedule **editing is unavailable through the TUI in both embedded and remote** (the daemon's `PUT/PATCH` is unreachable via the seam); the acceptance's literal "edit" reduces to enable/disable, which is itself parity-accurate. (b) dialog **Open** on a free-running watcher is inert over SSH (§6) — a pre-existing saved-session gap, scoped out. Everything else reaches full embedded parity. The one non-trivial addition (the cache) is an internal performance/correctness mechanism, not new behaviour.

**(2) Usability.** The user drives the same controls they drive embedded, with two honestly-bounded exceptions. Working: the sidebar shows free-running watchers as top-level `◷` roots and attached ones as children of their target session, with live busy/schedule/enabled updates on the 1s tick; **Watchers…** opens and **Enable/Disable/Run/Stop/Delete** all act against the daemon with the dialog re-rendering fresh after each action (epoch-guarded cache invalidation, §3); `/watcher list|enable|disable|run|stop` work. **Open** works for *attached* watchers (live target session) but is inert for *free-running* watchers over SSH (§6) — the message is pre-existing and misleading, called out as a follow-up rather than claimed as working. There is **no create form in the dialog in either mode** (watchers are created via the `create_watcher` agent tool, per `watchers_dialog.go:340`), so `CreateWatcher` is wired for handler parity but not reachable from the remote UI — parity intact. Failures surface as the handler's returned error in the dialog/echoCommand line and an empty/disconnected daemon shows "no watchers" rather than a hang — nothing silent.

**(3) No regressions.** Embedded path is untouched (`cmd/` unchanged). The shared UI code (`tui.go`/`watchers_dialog.go`/`session_window.go`) is *not* forked — only the handler functions become non-nil. `watcher:<name>` backend-only sessions stay excluded from the session list and their SSE transcript/notification stream is untouched. The per-tick UI thread is protected by the non-blocking cache (mirrors `cachedWorkspaceRoot`), so no new 30s-stall hazard. The cache generalizes `cachedWorkspaceRoot` from one key to a map **and** adds mutation-driven refresh, which reintroduces a write-skew window the single-key original lacked; this is closed by the **epoch counter** (a stale in-flight fetch is discarded on commit) and the in-flight flag is cleared via `defer` so no key can freeze (§3). `sidebar_watchers_test.go` injects a synchronous `ListWatchers`, so it is unaffected by the remote cache. The only behavioural test that must change is the one explicitly pinning the deferred-nil contract (intended). gofmt/vet/build/golangci-lint must stay clean and `ui/tui` keeps its forbidden-import discipline (we add only `net/http`, `net/url`, `time`, `gogent/internal/config` — all already imported in these files).

**(4) Holistic / repo seam.** The gogent↔turbotui seam is respected: turbotui is the rendering toolkit (read-only clone confirmed); this change lives entirely in gogent's `ui/tui` client and `internal/server` is read-only reference. No turbotui change, no new daemon endpoint, no new dep, no go.mod bump. The wire DTOs are mirrored from `internal/server/wire.go` (the same mirroring discipline the file already uses for `SessionDTO`/`CommandDTO`), keeping the client↔daemon contract typed and in lock-step. Downstream effect: none on turbotui; on the daemon, only additional read/mutate traffic the API already serves.

## Regression risks & mitigations
- **UI-thread stall on a slow tunnel** (per-tick `ListWatchers`): mitigated by the non-blocking cache (§3).
- **Cache write-skew after a mutation** (a stale background fetch clobbering fresh post-mutation state): mitigated by the epoch counter — the mutation bumps `watchGen` and any in-flight fetch with an older `gen` is dropped on commit (§3).
- **A key's cache freezing** if a fetch aborts without clearing its in-flight flag: mitigated by clearing the flag via `defer` (§3).
- **Dialog showing stale state after an action**: mitigated by synchronous, epoch-guarded cache refresh in mutation handlers (§3).
- **Stale `◷` nodes / `Running` marker while disconnected**: last-good kept on fetch error, corrected on reconnect's next successful tick (§3) — acknowledged, acceptable for a 1s-tick affordance.
- **Flipping the deferred-nil test**: intentional; the new test asserts the wired contract.
- **Watchers feature flag off on the daemon**: API returns 404 → `ListWatchers` returns `nil` → nodes/dialog/`/watcher` degrade exactly as the nil-handler case did embedded (no crash). Acceptable and matches embedded when the engine is off.
- **`NextFire`/`LastRun` format drift** between embedded and remote: mitigated by reproducing the `"2006-01-02 15:04"` reformat in the mapper.
- **Free-running watcher Open inert over SSH** (§6): pre-existing saved-session gap, explicitly scoped out, not a regression of this change (Open was entirely absent before, since the dialog was hidden); attached-watcher Open works.

## Open questions
1. **Cache vs. direct synchronous `ListWatchers`.** I recommend the non-blocking cache (mirrors `cachedWorkspaceRoot`, avoids the 30s-stall regression). The simpler alternative is a direct synchronous GET in the handler (like `ListSavedSessions`), accepting per-tick UI-thread network and dropping the epoch machinery. If the maintainer prefers minimal surface over stall-safety, the direct version is ~40 fewer lines — flag for review.
2. **Free-running watcher Open over SSH (§6).** Scoped out here as a saved-session follow-up (the `◷` node + all other actions still work; only Open is inert). Confirm this is acceptable for #572, or whether the issue expects the follow-up (un-filter `watcher:` from remote `ListSavedSessions` + a daemon non-live-transcript read) folded into this slice — which would breach "no new daemon endpoint."
3. **Create/edit UI.** The dialog has no Create form and there's no `/watcher create` in *either* mode (watchers are created via the `create_watcher` agent tool); editing task/schedule is unavailable in both modes (no `UpdateWatcher` seam). `CreateWatcher` is wired for handler parity + to flip the nil-pin test but is unreachable from the remote UI. Confirm that wiring the handler (not building a new create/edit form) satisfies acceptance #2 — a new form is scope beyond "client-side wiring of the existing API."
4. **`GetWatcher`/`UpdateWatcher`/`ToggleWatcher` APIClient methods.** The endpoints exist but no UI seam uses them; I plan to omit them to avoid dead code. Confirm acceptable (embedded also doesn't expose update/toggle through `Handlers`).
