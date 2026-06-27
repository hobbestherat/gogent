# Design — BOUND-RESTORE-ROUNDTRIPS (gogent issue #517)

> Restore does N+1 sequential HTTP round-trips with no pagination/bounding on the
> unbounded session list.

## Problem (confirmed against the code)

First-connect Restore (`ui/tui/remote_handlers.go` `Handlers().Restore`, ~line 653)
does:

1. one `GET /sessions` — `c.ListSessions()`, and
2. a **separate, sequential `GET /sessions/:id/transcript` per live session**
   (`c.GetTranscript(s.ID,"root")`, line 669), and
3. via `AdoptSession` (`tui.go:1600`, called in the synchronous `Run()` restore
   loop at `tui.go:2656`), a **`POST /sessions` per window** — `OnCreate` →
   `c.CreateSession` (`remote_handlers.go:594`).

So first connect is **1 + 2·(live sessions)** sequential round-trips. `GET /sessions`
(`internal/server/sessions.go` `sessionsSvc.List`) returns the **union of every
on-disk session** (`g.ListSessions()` → `store.ListAllSessions()`, which by design
includes **archived/closed** sessions, each tagged `SessionMeta.Archived`) **plus
every live ephemeral** (`g.SessionIDs()`), sorted by ID, with **no cap / filter**.
Transcript replay is the heavy half (daemon-side reconstruction + a large body each);
over `ssh://` every request is a fresh SSH channel (`cmd/attach.go`
`WithDialContext`), so each round-trip pays channel-setup latency → the multi-second
blocking window in the issue.

### What is on the blocking path (evidence, with the critic's corrections folded in)

- The restore loop runs synchronously in the UI startup goroutine
  (`tui.go:2650-2658`). The same path runs on **reconnect** via
  `refreshAfterReconnect` (`disconnect_modal.go:147`).
- For a **live** session the daemon's `createSession` (`api.go:298-300`) early-returns
  the existing session — a cheap *server-side* no-op, **but it is still one SSH
  round-trip over `ssh://`**. So the per-window `POST` is part of the O(live)
  round-trip cost, not "a cheap no-op" as the prior draft claimed. This design now
  bounds the `POST`s too (see Restore, below).
- Remote event routing does **not** depend on `OnCreate`: `RemoteClient.consume`
  routes global-SSE frames by `ge.SessionID` to the matching window
  (`remote_handlers.go:234`), and the daemon attached each session's observer when
  *it* restored the session at its own startup. → A window can exist and receive live
  events **without** an up-front `OnCreate`, which is what makes deferring `OnCreate`
  safe.

## Goal

Bound **every** up-front round-trip on first connect (and reconnect) to a small
constant — the transcript GETs **and** the per-window POSTs — exclude archived
sessions from the restored set, bound the wire response itself, and load the deferred
sessions' transcripts lazily and **without blocking the UI**. No turbotui change, no
deps, no `go.mod` bump.

---

## Approach (combine A + C; B deferred) — round-trip budget

| | before | after (first connect) |
|---|---|---|
| `GET /sessions` | 1 (full, incl. archived) | 1 (`live=true&limit=200`, archived excluded, ≤200 rows) |
| `GET transcript` | 1 per live session | ≤ `restoreEagerTranscripts` (20) |
| `POST /sessions` (OnCreate) | 1 per window | ≤ 20 (eager windows only) |

Deferred windows cost **zero** up-front round-trips; their `OnCreate` + transcript
fetch happen once, lazily, on first focus.

### Server contract — `GET /sessions` gains optional query params (A + C)

New query struct in `internal/server/wire.go`:

```go
// listSessionsQuery binds the optional ?live=&limit=&offset= params on
// GET /sessions (issue #517). All absent-by-default; an absent param preserves the
// pre-#517 full, ID-sorted listing (back-compat for any other caller).
type listSessionsQuery struct {
    Live   string `json:"live"`   // "true"/"1" => live sessions only (excludes archived)
    Limit  int    `json:"limit"`  // <=0 => no cap
    Offset int    `json:"offset"` // <0 => 0; >len => empty slice
}
```

> **Why `Live string`, not `bool`:** the webapi query binder
> (`webapi@v0.1.0/webapi.go:822-828`) binds only `string` and `int/int64` struct
> fields — `bool` is silently ignored. `live` is carried as a string and parsed
> truthy for `"true"`/`"1"`. `limit`/`offset` bind as `int`.

`sessionsSvc.List(r, q listSessionsQuery)` (`internal/server/sessions.go:20`):

1. Build `views` exactly as today (saved + live union, `Live` derivation, dedup).
2. **Bounded mode** = any of `q.Live` truthy / `q.Limit>0` / `q.Offset>0`:
   - If `q.Live` truthy → drop `view`s with `Live==false`. Archived/closed sessions
     are persisted-but-not-live, so this is the **archived-excluded** set (criterion
     C). No new field on the wire `sessionView` is needed.
   - Sort **most-recent first**: `CreatedAt` (RFC3339, lexically sortable —
     `session_store.go:385`) descending, `ID` descending as tiebreak. (Empty
     `CreatedAt`, possible on some live-ephemeral views, sorts last; ID breaks ties.)
   - Apply `offset`/`limit` with **clamping, never error**:
     `offset=clamp(offset,0,len)`; `end=len` if `limit<=0` else
     `clamp(offset+limit,offset,len)`; return `views[offset:end]`.
3. **Legacy mode** (no params) → the full slice in the existing **ID-ascending**
   order, byte-for-byte as before.

> **Dual ordering is intentional and regression-safe.** Param-less callers (the only
> ones that exist outside this change) see the unchanged ID-asc full list. Bounded
> callers opt in and get recency order — the natural order for "give me the recent
> N". No existing `/sessions` test asserts order under params (none pass params
> today), so all stay green.

### Client — pass real bounds (`ui/tui/api_client.go`)

- Keep `ListSessions()` unchanged (full listing) for back-compat callers.
- Add `ListSessionsBounded(live bool, limit, offset int) ([]SessionDTO, error)`
  building `"/sessions?..."` via `url.Values` and reusing `c.do`. `GetTranscript`
  **unchanged**.

### Restore — bound windows, eager transcripts, and POSTs (A)

In `remote_handlers.go`:

```go
// restoreEagerTranscripts bounds how many of the most-recent live sessions have
// their transcript (and OnCreate POST) fetched up front on (re)connect (#517).
// 20 ≈ what a user keeps visible at once.
const restoreEagerTranscripts = 20

// restoreMaxWindows hard-caps how many live-session windows Restore opens, so a
// pathological daemon with thousands of live sessions cannot make first connect
// build thousands of windows or ship a thousands-row /sessions body. Older live
// sessions beyond the cap are reachable from the Saved Sessions browser; the cap
// being hit is SURFACED, never silent.
const restoreMaxWindows = 200
```

Flow:

1. `sessions, _ := c.ListSessionsBounded(true, restoreMaxWindows, 0)` — **one** cheap
   index-only call: the most-recent ≤200 **live** sessions, **archived excluded**,
   recency-ordered by the server. (This is the *real* use of the server `limit`/`live`
   params — the wire response is now bounded.)
2. Filter out `default` and `watcher:` ids (unchanged exclusions).
3. First `restoreEagerTranscripts` (already recency-ordered): fetch transcript now →
   `RestoredSession{… Messages: …, Deferred:false}`.
4. The rest: `RestoredSession{ID,Title,Model, Deferred:true}` — **no** transcript
   fetch, and (see AdoptSession) **no** `OnCreate`.
5. If `len(sessions) == restoreMaxWindows`, surface a one-line status/log:
   *"Restored the most-recent 200 live sessions; open older ones from Saved
   Sessions."* (no silent truncation).

Result: first connect = 1 list + ≤20 transcript + ≤20 POST round-trips, independent
of how many live/archived sessions the daemon holds.

### Lazy-load on focus, async, exactly once (`ui/tui/tui.go`)

- `RestoredSession` gains `Deferred bool` (`tui.go:411`).
- `Workbench` gains `deferredTranscripts map[string]bool` (distinct from the existing
  `deferred*` permission-dialog names), **initialized in the Workbench constructor**
  (no nil-map write), guarded by `w.mu`.
- `AdoptSession` (`tui.go:1600`): when `rs.Deferred`, open the window shell
  (`openWindow` + model preselect) but **skip `sw.restore`** and **skip `OnCreate`**,
  seed a faint placeholder record ("transcript loads on focus"), and record
  `w.deferredTranscripts[rs.ID]=true`. Non-deferred path unchanged (still calls
  `OnCreate` + `restore`). Deferring `OnCreate` is safe because remote event routing
  is by session id over the global SSE stream, independent of `OnCreate` (evidence
  above).
- New `Workbench.ensureTranscript(id)` (async): under `w.mu`, if
  `w.deferredTranscripts[id]`, **delete the flag first** (so a concurrent/repeat
  focus cannot double-fetch — exactly-once), then in a **goroutine** call
  `OnCreate(id,title)` then `GetTranscript(id,"root")`, and `w.desktop.Post(func(){
  sw.reload(msgs) })` to apply on the UI thread. No-op when not deferred.
  - **Async by default because of `ssh://`:** a synchronous fetch would block the
    whole UI for one SSH-channel round-trip on every first-focus — re-introducing the
    exact symptom the issue targets. `reload` (clear+restore) is used, not append, so
    any live deltas that streamed into the shell before load are reconciled to the
    server's authoritative transcript. (A delta arriving in the narrow window between
    fetch and `reload` is superseded by the next delta on the live stream — noted as
    a benign edge.)
- Trigger from `Workbench.Focus(id)` (`tui.go:1728`) — the single user-driven focus
  chokepoint: `cycle` (`tui.go:1764`), the session menu (`tui.go:995`), the sidebar,
  close, and watcher-open all route through `Focus`. Construction-time focus uses
  `w.desktop.SetFocus` directly (not `w.Focus`), so adopting the shells during restore
  does **not** trigger any fetch.
- After the restore loop **and `applyLayout`** (`tui.go:2659` — `applyLayout` touches
  no focus, so read `activeID` after it), call `ensureTranscript(w.ActiveID())` so the
  window the user lands on is populated even if it fell outside the eager N.

The Saved Sessions browser path already lazy-fetches on open (`OpenSavedSession` →
`GetTranscript`), so the alternate "lazy-load when the browser opens" trigger and the
beyond-cap sessions are covered with no change.

### Reconnect — deferred-aware re-sync (fixes the blanking regression)

`refreshAfterReconnect` (`disconnect_modal.go:147`) currently does
`sw.reload(rs.Messages)` for every already-open window. `reload`
(`session_window.go:2710`) clears records then restores, so a deferred
`rs.Messages==nil` would **blank an open window** — a real regression in the #358 §7
jump-to-present flow. (The prior draft's "reconnect inherits bounding automatically"
claim was wrong.) Fix — the reconnect branch becomes deferred-aware
(`refreshAfterReconnect` is now an explicitly-touched function; it runs on the
background goroutine, so its fetches do not block the UI):

```text
for rs in Restore():
  if open[rs.ID] && !sw.readOnly:
     if rs.Deferred:
         if w.deferredTranscripts[rs.ID]:        # still an unloaded shell
             continue                            # leave it; do NOT blank; loads on focus
         msgs := GetTranscript(rs.ID,"root")     # was loaded before the drop → re-sync
         desktop.Post(reload(msgs))
     else:
         desktop.Post(reload(rs.Messages))       # eager: unchanged
  else:
     desktop.Post(AdoptSession(rs))              # new-during-outage: adopt (deferred stays a shell)
```

This re-syncs windows the user actually has loaded, never blanks an unloaded shell,
and keeps adopting sessions that went live during the outage. An open window that was
loaded but now returns deferred (it fell out of the recent-N on reconnect) is
re-synced with one fresh `GetTranscript` — bounded by the number of windows the user
has open, on the background goroutine.

### B (concurrency) — deferred

≤20 sequential eager transcript fetches is acceptable; a stdlib bounded-semaphore
fan-out is **not** added (no dep, low churn-vs-risk payoff under the N-bound).
Recorded as a possible follow-up.

---

## Files / functions touched (gogent only)

| File | Change |
|---|---|
| `internal/server/wire.go` | add `listSessionsQuery` |
| `internal/server/sessions.go` | `List` takes `listSessionsQuery`; bounded mode = live-filter + recency sort + clamped offset/limit; legacy mode unchanged |
| `ui/tui/api_client.go` | add `ListSessionsBounded`; `ListSessions`/`GetTranscript` unchanged |
| `ui/tui/remote_handlers.go` | `Restore` uses `ListSessionsBounded(true, restoreMaxWindows, 0)`, eager-N transcripts, `Deferred` for the rest, surfaced cap message |
| `ui/tui/tui.go` | `RestoredSession.Deferred`; `Workbench.deferredTranscripts` (init in ctor); async `ensureTranscript`; deferred branch in `AdoptSession` (skip `OnCreate`+`restore`, seed placeholder); hook in `Focus`; post-`applyLayout` active-window load |
| `ui/tui/disconnect_modal.go` | `refreshAfterReconnect` deferred-aware branch (no blanking; re-sync loaded; adopt new) |

**turbotui: no change.** It supplies the desktop/layer/focus primitives we already
call (`AddLayer`/`RemoveLayer`/`SetFocus`/`Post`, `sw.restore`/`reload`). The seam —
gogent drives turbotui via `w.desktop.*` and the daemon via the `/api` HTTP/SSE
contract — is untouched. No `go.mod` bump, no deps, and `ui/tui` gains no
`internal/server`/`daemon` import (Restore/lazy-load/reconnect use only `Handlers`
funcs + DTOs).

---

## User-facing behavior

- First connect / reconnect is fast: the most-recent live windows appear with their
  transcripts immediately; older live windows appear as labelled shells (faint
  "transcript loads on focus" placeholder) — no archived/closed clutter.
- Focusing a deferred window loads its transcript asynchronously: the placeholder is
  replaced when the fetch returns; the UI never freezes waiting on it (important over
  `ssh://`). The fetch happens once; refocus never re-fetches.
- If the daemon holds more than `restoreMaxWindows` live sessions, the surplus is
  reachable from Saved Sessions and the user is told so — nothing is silently
  dropped.
- Across a reconnect, loaded windows are re-synced to the daemon's current
  transcript; unloaded shells stay shells (and still load on focus). No window is
  blanked.

---

## Criteria

**(1) Goal match.** `GET /sessions` gains pagination/limit + archived-excluded
(`live`) support, **and Restore uses real server bounds** (`live=true&limit=200`), so
the wire response, the window count, the transcript GETs, and the per-window POSTs are
**all** bounded on first connect — the issue's "N+1 round-trips on an unbounded list"
is closed end-to-end, not just the transcript half. It is a *fix*, not a refactor or
feature; concurrency (B) is intentionally omitted.

**(2) Usability.** The user drives the deferred load by the natural gesture (focusing
a window); no new control. Deferred windows are labelled, not blank shells. The load
is **async**, so even over `ssh://` focus never freezes the UI. The recent-live
default and surfaced over-cap message keep the restored set meaningful and honest
(nothing silent).

**(3) No regressions.** Server defaults are byte-for-byte unchanged with no params
(existing `/sessions` tests green); bounded mode can only narrow an opted-in response.
`ListSessions`/`GetTranscript` clients unchanged. The non-deferred restore path is the
old path verbatim (still `OnCreate`+`restore`). **The reconnect blanking regression is
explicitly fixed** in `refreshAfterReconnect` and covered by a new test. Exactly-once
is guaranteed by clearing the flag before the fetch. Invariants preserved: ordering
(legacy ID-asc), "default"/"watcher:" exclusion, model preselect, layout apply,
read-only windows skipped on reconnect.

**(4) Holistic / two-repo seam.** Cost is bounded where it originates: the unbounded
listing on the **server** (filter + recency-page) and the N+1 driver in the
**client/TUI** (cap windows, defer transcripts *and* POSTs). turbotui is untouched; no
`go.mod` change; gogent keeps driving turbotui's primitives and the daemon's `/api`
contract exactly as before. The param contract is additive and versionless (absent ⇒
old behaviour), so a mixed old/new client⇄daemon pairing degrades gracefully (an old
client sends no params → full list; a new client against an old daemon → params
ignored, falls back to the full list, still correct just unbounded).

### Critic concerns — resolutions

1. **[REGRESSION, was blocking] reconnect blanks deferred windows** → fixed:
   `refreshAfterReconnect` is now deferred-aware (skip unloaded shells, re-sync loaded
   windows, adopt new); new reconnect test. Prior false "inherits automatically"
   claim removed.
2. **[GOAL GAP] server `limit` unused / windows unbounded** → fixed: Restore passes
   `limit=restoreMaxWindows` (real bound on the wire response and window count) and
   recency order is now server-side; window count, list rows, POSTs, and transcripts
   are all bounded.
3. **[USABILITY] sync focus fetch blocks UI over ssh://** → fixed: `ensureTranscript`
   is async (goroutine fetch + `desktop.Post` apply); sync is no longer the default.
4. **[SCOPE HONESTY] OnCreate POSTs O(live)** → fixed, not just acknowledged:
   `OnCreate` is deferred for deferred windows, so up-front POSTs ≤ eager-N.
5. **[MINOR]** `deferredTranscripts` map initialized in the constructor and renamed to
   avoid the `deferred*` collision; `ensureTranscript(activeID)` reads `activeID`
   after `applyLayout`.

### Residual risks (acknowledged)

- The eager windows still do ≤20 `OnCreate` POSTs up front (bounded, not eliminated).
  Eliminating them entirely would touch the embedded-shared `AdoptSession` contract —
  out of scope.
- A live delta arriving in the sub-millisecond gap between a deferred window's
  `GetTranscript` and its `reload` could be momentarily superseded; the next live
  delta on the stream corrects it. Benign.
- `restoreMaxWindows=200` is a defensive cap essentially never hit in practice; if it
  is, the surplus is browser-reachable and surfaced.

---

## Tests (to be written in the build phase)

- **Server** (`internal/server`): `?limit=&offset=` returns the bounded/paginated
  slice; `?live=true` excludes archived (seed live + archived, assert only live);
  bounded mode is recency-ordered; zero/absent params == full ID-asc back-compat
  listing; out-of-range offset/limit clamp (no error). Existing `/sessions` tests stay
  green.
- **Client** (`ui/tui`): `ListSessionsBounded` emits the expected query string
  (httptest capturing `r.URL.RawQuery`); `GetTranscript` unchanged.
- **Restore / lazy-load** (`ui/tui`, handler-level, no server import): with a stub
  `Handlers` returning >N sessions of which only N carry messages and the rest
  `Deferred`, assert ≤N eager transcripts and that deferred windows did **not**
  `OnCreate`; `Focus` on a deferred id triggers `OnCreate`+`GetTranscript` exactly
  once and renders; a second `Focus` does not re-fetch.
- **Reconnect** (`ui/tui`): an open, already-loaded window outside the recent-N is
  re-synced (not blanked) across `refreshAfterReconnect`; an open unloaded shell stays
  a shell and is **not** blanked; a session gone-live-during-outage is adopted.

---

## Out of scope (noted, not done)

- Eliminating the eager windows' `OnCreate` POSTs (touches embedded-shared
  `AdoptSession`).
- Concurrency fan-out for eager transcripts (B).
- Wiring the Saved Sessions browser to page via `limit/offset` (server now supports
  it; `offset` is part of the complete, tested contract reserved for that caller).

## Open questions

1. **`restoreEagerTranscripts=20` / `restoreMaxWindows=200`** — sane defaults;
   confirm with maintainer or make configurable. Named consts either way.
2. **Recency key = `CreatedAt`** (creation time). If "most-recently-*active*" is
   wanted, the index has no last-activity field today — that would need a store/index
   addition (larger scope). Flag if required.
