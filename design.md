# Design — BOUND-RESTORE-ROUNDTRIPS (gogent issue #517)

> Restore does N+1 sequential HTTP round-trips with no pagination/bounding on the
> unbounded session list.

## Problem (confirmed against the code)

First-connect Restore (`ui/tui/remote_handlers.go` `Handlers().Restore`, ~line 653)
does:

1. one `GET /sessions` — `c.ListSessions()`, and
2. a **separate, sequential `GET /sessions/:id/transcript` per live session**
   (`c.GetTranscript(s.ID,"root")` inside the loop, line 669).

`GET /sessions` (`internal/server/sessions.go` `sessionsSvc.List`) returns the
**union of every on-disk session** (`g.ListSessions()` → `store.ListAllSessions()`,
which by design includes **archived/closed** sessions, each tagged
`SessionMeta.Archived`) **plus every live ephemeral** (`g.SessionIDs()`), sorted by
ID, with **no cap / pagination / filter**. Transcript replay is the expensive part
(daemon-side reconstruction + a large body per session); over `ssh://` each request
is a fresh SSH channel (`cmd/attach.go` `WithDialContext`), so the per-transcript
cost multiplies into a multi-second blocking window while the UI is unresponsive.

### What is and isn't on the blocking path (evidence)

- The restore loop runs synchronously in the UI startup goroutine
  (`ui/tui/tui.go` ~2650: `restored := w.handlers.Restore()` then
  `for ... { w.AdoptSession(rs) }`). The same path runs on **reconnect**
  (`ui/tui/disconnect_modal.go` ~148–153).
- `AdoptSession` also calls `OnCreate` per window → remote `c.CreateSession`
  (`POST /sessions`). For a **live** session the daemon's `createSession`
  (`internal/server/api.go:297`) **early-returns the existing session** — a cheap
  server-side no-op (one round-trip, no replay). The expensive, issue-named cost is
  the **per-session transcript GET**, so that is what this change bounds. (The
  redundant per-window `POST` is noted under *Out of scope*.)
- Remote event routing does **not** depend on `OnCreate`: `RemoteClient.consume`
  routes global-SSE frames by `ge.SessionID` to the matching window
  (`remote_handlers.go:231`), and the daemon already attached each session's
  observer when *it* restored the session at its own startup.

## Goal

Bound the up-front transcript round-trips on first connect (and reconnect) to a
small constant, exclude archived sessions from the default restore set, and load
the deferred sessions' transcripts lazily — without changing behaviour for any
param-less caller and without touching turbotui or adding deps.

---

## Chosen approach (combine A + C; B deferred)

### Server contract — `GET /sessions` gains optional query params (A + C)

New query struct in `internal/server/wire.go`:

```go
// listSessionsQuery binds the optional ?live=&limit=&offset= params on
// GET /sessions. All are absent-by-default; an absent param preserves the
// pre-#517 full, ID-sorted listing (back-compat for any other caller).
type listSessionsQuery struct {
    Live   string `json:"live"`   // "true"/"1" => live sessions only (excludes archived)
    Limit  int    `json:"limit"`  // <=0 => no cap
    Offset int    `json:"offset"` // <0 => 0; >len => empty slice
}
```

> **Why `Live string`, not `bool`:** the webapi query binder
> (`webapi@v0.1.0/webapi.go:807-830`) only binds `string` and `int/int64` struct
> fields — `bool` is silently ignored. So `live` is carried as a string and parsed
> as truthy for `"true"`/`"1"`. `limit`/`offset` bind as `int` (supported).

`sessionsSvc.List(r *http.Request, q listSessionsQuery)`
(`internal/server/sessions.go:20`) becomes:

1. Build `views` exactly as today (saved + live union, `Live` derivation, dedup),
   and keep the existing `sort by ID` so order is **stable and unchanged**.
2. If `q.Live` is truthy → drop every `view` with `Live == false`. Archived/closed
   sessions are persisted-but-not-live, so this filter is the **archived-excluded**
   default-restore set (criterion C). This is the only place "archived" needs to be
   reasoned about — no new field on the wire `sessionView`.
3. Apply `offset`/`limit` to the (filtered) slice with **clamping, never error**:
   `offset = clamp(offset, 0, len)`; `end = len` if `limit<=0` else
   `clamp(offset+limit, offset, len)`; return `views[offset:end]`.
4. **No params** (`live==""`, `limit==0`, `offset==0`) → returns the full,
   ID-sorted slice byte-for-byte as before.

Ordering note: pagination keeps the existing **ID-ascending** order (stable, no
behavioural surprise). "Most-recent N" selection is done **client-side in Restore**
(below) by sorting the live set on `CreatedAt` (RFC3339, lexically sortable —
`session_store.go:385`) descending; this avoids a server-side dual-ordering rule and
keeps the server change purely additive (filter + window).

### Client — pass the bound (`ui/tui/api_client.go`)

- Keep `ListSessions()` unchanged (full listing) for any back-compat caller.
- Add:
  ```go
  // ListSessionsBounded lists sessions with the #517 bounding params. live=true
  // restricts to live sessions (archived excluded); limit<=0 means no cap.
  func (c *APIClient) ListSessionsBounded(live bool, limit, offset int) ([]SessionDTO, error)
  ```
  It builds `"/sessions?..."` with `url.Values` (`live=true`, `limit`, `offset`
  only when meaningful) and reuses `c.do`. `GetTranscript` is **unchanged**.

### Restore — cap eager transcripts to N, defer the rest (A)

In `remote_handlers.go` `Handlers().Restore`:

```go
// restoreEagerTranscripts bounds how many sessions have their transcript fetched
// up front on (re)connect (issue #517). The remaining live sessions open as
// deferred windows whose transcript is fetched exactly once, lazily, the first
// time the window is focused (Workbench.Focus) — or, for a session reached via
// the Saved Sessions browser, when it is opened (OpenSavedSession). 20 covers the
// windows a user realistically has visible at once while keeping first connect to
// one cheap GET /sessions + at most 20 transcript round-trips.
const restoreEagerTranscripts = 20
```

New Restore flow:

1. `sessions, _ := c.ListSessionsBounded(true, 0, 0)` — **one** cheap, index-only
   `GET /sessions?live=true`: all live sessions, **archived excluded**, no
   transcript replay.
2. Filter out `default` and `watcher:` ids (unchanged exclusions).
3. Sort the remaining by `CreatedAt` descending (most-recent first).
4. For the first `restoreEagerTranscripts`: fetch transcript now
   (`c.GetTranscript`) → `RestoredSession{... Messages: ...}`.
5. For the rest: emit `RestoredSession{ID, Title, Model, Deferred: true}` with **no**
   transcript fetch.
6. Return all of them (every live session still gets a window — see Usability).

Result: first connect = 1 list round-trip + **≤ N** transcript round-trips,
regardless of how many live/archived sessions exist on the daemon.

### Lazy-load on focus, exactly once (`ui/tui/tui.go`)

- `RestoredSession` gains `Deferred bool` (`tui.go:411`).
- `Workbench` gains `deferred map[string]bool` (guarded by the existing `w.mu`).
- `AdoptSession` (`tui.go:1600`): when `rs.Deferred`, open the window shell
  (`openWindow` + model preselect + `OnCreate`, exactly as today) but **skip**
  `sw.restore` and record `w.deferred[rs.ID] = true`. Non-deferred path unchanged.
- New `Workbench.ensureTranscript(id)`: if `w.deferred[id]` and
  `w.handlers.GetTranscript != nil`, **delete the flag first** (so a concurrent /
  repeat focus cannot double-fetch), call `GetTranscript(id,"root")`, and
  `sw.restore(msgs)`. No-op when not deferred → idempotent / **exactly once**.
- Trigger it from `Workbench.Focus(id)` (`tui.go:1728`) — the single user-driven
  focus chokepoint; `cycle` delegates to `Focus` (`tui.go:1764`), and
  menu "jump to session" calls `Focus` (`tui.go:995`). Construction-time focus uses
  `w.desktop.SetFocus` directly (not `w.Focus`), so adopting the windows during
  restore does **not** trigger any deferred fetch.
- After the restore loop, call `ensureTranscript(activeID)` once so the window the
  user actually lands on is populated immediately even if it fell outside the eager
  N (the eager set is most-recent-first, so this is usually already loaded).

The Saved Sessions browser path already lazy-fetches on open
(`OpenSavedSession` → `GetTranscript`), so deferred sessions reached that way are
covered with no change.

### B (concurrency) — deferred

The N-bound makes ≤20 sequential transcript fetches the worst up-front case, which
is acceptable; a stdlib bounded-semaphore fan-out is **not** added to avoid
churn/risk. Left sequential per the issue's "only if cheap" note. Recorded as a
possible follow-up.

---

## Files / functions touched

gogent only:

| File | Change |
|---|---|
| `internal/server/wire.go` | add `listSessionsQuery` struct |
| `internal/server/sessions.go` | `List` takes `listSessionsQuery`; add live-filter + clamped offset/limit; defaults unchanged |
| `ui/tui/api_client.go` | add `ListSessionsBounded`; `ListSessions`/`GetTranscript` unchanged |
| `ui/tui/remote_handlers.go` | `Restore` uses `ListSessionsBounded(true,…)`, recency sort, `restoreEagerTranscripts` cap, sets `Deferred` |
| `ui/tui/tui.go` | `RestoredSession.Deferred`; `Workbench.deferred` map; `ensureTranscript`; hook in `Focus`; deferred branch in `AdoptSession`; post-restore active-window load |

**turbotui: no change.** turbotui supplies the desktop/layer/focus primitives we
already call; the seam (gogent drives turbotui via `w.desktop.*`, the daemon via the
`/api` HTTP/SSE contract) is unchanged. No `go.mod` bump, no new deps, no
`internal/daemon|server` import added to `ui/tui` (Restore/lazy-load use only the
existing `Handlers` funcs and DTOs).

---

## User-facing behavior

- First connect / reconnect is fast: all live-session windows appear immediately
  (titles, model, layout), with the most-recent ~20 already showing their
  transcript. No archived/closed sessions clutter the restored set.
- Focusing a session whose transcript was deferred fetches and renders it on the
  spot (one round-trip), then it behaves identically to an eagerly-restored window.
  The fetch happens once; refocusing never re-fetches.
- Nothing disappears: every live session the daemon holds still gets a window, so
  the user's running work is all present — only the *transcript body* of the
  less-recent ones is loaded on demand.

---

## Criteria

**(1) Goal match.** Exactly the issue's ask: `GET /sessions` gets
pagination/limit + archived-excluded (`live`) support; Restore restores only the
most-recent N live sessions eagerly and lazy-loads the rest on focus. It's a *fix*
(bounding + filtering + deferral), no refactor, no feature creep — concurrency (B)
is intentionally omitted.

**(2) Usability.** The user drives the deferred load implicitly by focusing a
window — the natural gesture, no new control to learn. All sessions stay visible
(no silent truncation of the window set); only transcript bodies defer. The
archived-excluded default means the restored set is the user's *live* work, not a
historical dump. Lazy fetch is surfaced as the transcript simply appearing on
focus.

**(3) No regressions.** Server defaults are byte-for-byte unchanged when params are
absent (existing `/sessions` tests stay green); the live-filter and offset/limit
clamp can only *narrow* a response that opted in via params. `ListSessions`/
`GetTranscript` clients unchanged. The deferred branch reuses the existing
`openWindow`/`OnCreate`/`st.restore` machinery; non-deferred restore is the old
path verbatim. Session/transcript invariants (ordering, "default"/"watcher:"
exclusion, model preselect, layout apply) preserved. Exactly-once is guaranteed by
deleting the deferred flag before the fetch.

**(4) Holistic / two-repo seam.** Change lives where the cost lives: the unbounded
listing on the **server** (filter + page) and the N+1 driver in the **client/TUI**
(cap + defer). The gogent↔turbotui seam is untouched — no turbotui edit, no
`go.mod` change; gogent keeps driving turbotui's desktop primitives and the daemon's
`/api` contract exactly as before. Downstream: the param contract is additive and
versionless (absent ⇒ old behaviour), so a mixed old-client/new-daemon (or vice
versa) pairing degrades gracefully.

### Regression risks & mitigations

- **Dual ordering confusion** → avoided: pagination keeps server-side ID order;
  recency selection is client-side in Restore only.
- **Deferred window focused before transcript loads** → `ensureTranscript` on the
  post-restore active window + on every `Focus`; fetch is fast and on-demand.
- **Double-fetch on rapid refocus** → flag deleted before fetch (exactly-once).
- **`bool` query binding silently dropped** → `live` carried as string, parsed
  truthy.
- **Focus-time blocking on the UI thread** → one bounded `GetTranscript`
  round-trip; acceptable for on-demand load (could be made async later — noted).
- **Reconnect path** also routes through `Restore`/`AdoptSession`, so it inherits
  the same bounding automatically (no separate change).

---

## Tests (to be written in the build phase)

- **Server** (`internal/server`): `GET /sessions?limit=&offset=` returns the
  bounded/paginated slice; `?live=true` excludes archived (seed a live + an
  archived session, assert only live returned); zero/absent params == full
  back-compat listing; out-of-range offset/limit clamp (no error). Existing
  `/sessions` tests stay green.
- **Client** (`ui/tui`): `ListSessionsBounded` emits the expected query string
  (httptest capturing `r.URL.RawQuery`); `GetTranscript` unchanged.
- **Restore / lazy-load** (`ui/tui`, handler-level so no server import): with a
  stub `Handlers{Restore}` returning >N sessions of which only N carry messages and
  the rest `Deferred`, assert at most N eager transcripts; `Focus` on a deferred id
  invokes `GetTranscript` exactly once and renders; a second `Focus` does not
  re-fetch.

---

## Out of scope (noted, not done)

- Redundant per-window `POST /sessions` (`OnCreate`) during remote restore: a cheap
  server no-op for already-live sessions; eliminating it touches the embedded-shared
  `AdoptSession` contract and is a separate optimization.
- Concurrency fan-out for the eager transcripts (B) — left sequential under the
  N-bound.
- Pagination of the Saved Sessions browser via `limit/offset` — the server now
  supports it; wiring the browser to page is a future enhancement, not required.

## Open questions

1. **N = 20** — sane default for "windows visible at once"; confirm with maintainer
   if a different bound (or making it configurable) is wanted. Documented as a
   named const either way.
2. **Recency key** — using `CreatedAt` (creation time) for "most-recent". If
   "most-recently-*active*" is preferred, the index has no last-activity field today;
   `CreatedAt` is the available, stable key. Flag if last-activity ordering is
   required (would need a store/index addition — larger scope).
3. **Focus-time fetch sync vs async** — proposed synchronous (one round-trip, simple,
   matches "loads on focus"). If even a single ssh:// round-trip on focus is judged
   too janky, make `ensureTranscript` async (fetch in a goroutine, `Post` the
   restore back to the UI thread). Easy to switch; defaulting to sync for clarity.
