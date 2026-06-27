# Design — Reconnect: skip unchanged reload + dedupe early reconnects (issue #520)

## Problem recap

`refreshAfterReconnect` (ui/tui/disconnect_modal.go) re-runs `Restore()` and
**unconditionally** `reload()`s every already-open window. `reload()`
(session_window.go) does `records=nil; restore(); render()` — a full clear +
rebuild. On a flaky first connect the SSE stream is opened before the UI is fully
up (#516); a single early health trip (`healthFailThreshold=2`,
`daemonHealthEvery=10s`) drives `consume()→reconnect()→OnConnectionRestored()→
refreshAfterReconnect()`, rebuilding the *same* transcript. Multiple early flaps
rebuild it repeatedly — the "restores the same session many times" symptom.

Two independent levers, both required by the issue:

- **A. Skip the reload when the transcript is unchanged.** The dominant,
  user-visible cost is the redundant clear+rebuild render. If the fetched
  transcript is byte-for-byte the same source we last loaded into the window,
  the reload is a no-op and must be skipped.
- **B. Dedupe rapid early reconnects** so the restore *work* runs effectively
  once during the initial-connect flap window, instead of once per flap.

Direction **C** (a "messages-since-cursor" append) is **out of scope**; the
no-replay/jump-to-present contract is documented at remote_handlers.go:335
(`reconnect()` returns a brand-new `StreamEvents`, never a backlog). A code
comment will reference it.

## Key facts that shape the design

1. `refreshAfterReconnect` runs **synchronously on the single `consume`/reconnect
   goroutine**; its HTTP fetch (`Restore()` + transcripts) runs there, and every
   UI mutation is `Post`ed onto the event loop. Many existing tests rely on this
   synchronous fetch (e.g. restore_issue517_test.go:577 "re-fetch is synchronous
   on the reconnect goroutine"; daemon_lifecycle_issue358_test.go:335). **The
   fetch must stay synchronous** — making it async would reorder the reload
   `Post`s relative to live-stream event `Post`s and could transiently drop a
   freshly-streamed event. So coalescing cannot rely on "in-flight overlap":
   the flaps are *sequential*, so B is a **recency throttle with a guaranteed
   trailing refresh**, not an in-flight single-flight.
2. `restore(msgs []ChatMessage)` (session_window.go:2785) is the **single funnel**
   that turns a `ChatMessage` slice into transcript records, reached by every
   build path: eager `AdoptSession` (tui.go:1654), lazy focus-load, and
   `reload()` (session_window.go:2745). Setting a fingerprint there covers all of
   them with one touch.
3. msg→record fan-out is **not 1:1** (one assistant `ChatMessage` can yield
   thought + answer + tool-call records), and live-streamed records are formatted
   by a different path than restored ones. So comparing raw record *counts* (or
   doing a record-level prefix diff) is both lossy and fragile. The robust,
   cheap signal is a **fingerprint of the source `ChatMessage` slice** that the
   window was last built from: `(len, FNV-64 hash of role|content|reasoning|tool|
   args)`. Equal fingerprint ⇒ identical source ⇒ rebuild would be identical ⇒
   safe to skip. Any difference ⇒ reload (never a false skip).
4. `reload(nil)` is already a deliberate no-op (nil = failed fetch; keep content,
   session_window.go:2752). The new skip path must preserve that exactly.

## Change set (all pure ui/tui)

### A — skip unchanged reload

**ui/tui/transcript_model.go**
- Add fields to `transcriptModel`: `srcLen int`, `srcHash uint64` — the
  fingerprint of the `ChatMessage` slice the model was last built from.
- Add `func transcriptSourceSig(msgs []ChatMessage) (int, uint64)` — `len` plus a
  `hash/fnv` (stdlib, no new dep) FNV-64a over each message's
  `Role|Content|Reasoning|Tool|Args`.
- Add `func (m *transcriptModel) matchesSource(n int, h uint64) bool` — `m.srcLen
  == n && m.srcHash == h && (n>0 || m has been built from a real slice)`. (Guard
  so a never-loaded/placeholder model — `srcLen==0` from `markDeferred`, which
  never sets it — does not spuriously "match" an empty fetch; the empty case is
  handled because deferred shells never reach the reload path, see below.)

**ui/tui/session_window.go**
- `restore(msgs)`: after building records, set `sw.transcript.srcLen,
  sw.transcript.srcHash = transcriptSourceSig(msgs)`. This is the single place the
  fingerprint is recorded, so it is always in sync with what the window actually
  shows after any build/reload.
- Add `func (sw *SessionWindow) reloadIfChanged(msgs []ChatMessage)`:
  ```
  if msgs == nil { return }                 // failed fetch: keep content (as reload)
  n, h := transcriptSourceSig(msgs)
  if sw.transcript.matchesSource(n, h) { return }  // unchanged: no wipe, no rebuild
  sw.transcript.records = nil
  sw.restore(msgs)                          // restore() refreshes the fingerprint
  ```
  `reload()` is left intact for its other callers; `reloadIfChanged` is the
  reconnect-only wrapper.

**ui/tui/disconnect_modal.go**
- In `refreshAfterReconnect`, replace both `sw.reload(rs.Messages)` /
  `sw.reload(msgs)` (the eager-open branch and the deferred-but-loaded branch)
  with `sw.reloadIfChanged(...)`. The unloaded-deferred-shell `continue` and the
  new-session `AdoptSession` branches are untouched.
- Add a one-line comment referencing remote_handlers.go:335's no-replay note,
  marking Direction C (cursor append) as deliberately out of scope.

Net effect of A: a reconnect whose `Restore()` returns the same transcript a
window already shows performs **zero** record rebuilds and **zero** re-renders for
that window. A genuinely advanced transcript has a different fingerprint and
reloads exactly as today (covered by the existing
`TestReconnectReSyncsLoadedDeferredWindow`).

### B — dedupe rapid early reconnects (recency throttle + trailing refresh)

**ui/tui/tui.go** (`Workbench` struct) — fields guarded by the existing `w.mu`:
- `reconnectCoalesce time.Duration` — coalesce window; **`0` = disabled
  (default)**.
- `reconnectRefreshAt time.Time` — when the last *leading* refresh ran.
- `reconnectRefreshPending bool`, `reconnectTrailingArmed bool`.

**ui/tui/disconnect_modal.go**
- Rename the current body to `doRefreshAfterReconnect()` (unchanged logic, now
  using `reloadIfChanged`).
- `refreshAfterReconnect()` becomes a thin throttle in front of it:
  ```
  if w.reconnectCoalesce <= 0 { w.doRefreshAfterReconnect(); return }  // disabled
  lock
  if within reconnectCoalesce of reconnectRefreshAt:
      reconnectRefreshPending = true
      if !reconnectTrailingArmed:
          reconnectTrailingArmed = true
          time.AfterFunc(reconnectCoalesce, w.runTrailingReconnectRefresh)
      unlock; return                    // collapse this flap's full restore
  reconnectRefreshAt = now; unlock
  w.doRefreshAfterReconnect()           // LEADING refresh — still synchronous
  ```
- `runTrailingReconnectRefresh()` (fires once, after the storm settles): clear
  `reconnectTrailingArmed`; if `reconnectRefreshPending`, clear it, stamp
  `reconnectRefreshAt = now`, and run `doRefreshAfterReconnect()`.

Behaviour: the **first** flap in a burst runs a normal synchronous refresh; every
further flap inside the window collapses (no fetch, no rebuild) but **arms a
single trailing refresh**. The trailing refresh re-fetches the *authoritative*
daemon transcript once after the storm, so a genuine change that landed during a
mid-storm flap is still synced — it is **never dropped** (the trailing run always
captures the latest state, and its per-window `reloadIfChanged` is idempotent).
A burst of N flaps thus does at most **2** restores (leading + trailing) instead
of N.

**Why default `0` / where it is enabled.** Defaulting to `0` means **no existing
test or runtime path changes behaviour** — every `refreshAfterReconnect` runs the
leading refresh synchronously, exactly as today. The production value (proposed
`750ms`, ≈ one approval-poll interval and well under the `500ms→1s→…` backoff) is
enabled at attach wiring next to the existing `SetReconnectControls` call via a
small `SetReconnectCoalesce(d)` setter (one line in cmd/attach.go; logic stays in
ui/tui). The coalesce unit test sets the field directly. *(If strict pure-ui/tui
is preferred over touching cmd, the default can instead be set in `NewWorkbench`;
this is called out in Open questions.)*

## How the four criteria are met

**(1) Goal match.** This is a fix, not a feature/refactor. A: unchanged open
windows become a no-op on reconnect (the exact ask). B: rapid early reconnects
collapse to ~one restore. No scope creep — C is explicitly excluded with a
comment. The §7 jump-to-present contract and `Restore()`/`AdoptSession` shape are
unchanged.

**(2) Usability.** The user sees **no transcript flicker / repeated rebuild** on a
flaky first connect — the dominant complaint. Nothing is hidden: a window whose
transcript truly advanced still re-syncs (visible new tail), a new daemon-side
session still opens via `AdoptSession`, and pending approvals still re-surface via
the existing `kickApprovals`. No new UI surface or input is introduced; the
disconnect modal's interaction is unchanged.

**(3) No regressions.**
- `reloadIfChanged(nil)` keeps the existing "failed fetch keeps content" invariant
  (`TestReconnectFailedFetchKeepsContentLoaded`).
- Advanced transcripts still reload (`TestReconnectReSyncsLoadedDeferredWindow`):
  different fingerprint ⇒ reload.
- Unloaded deferred shells are still skipped and keep their placeholder
  (`TestReconnectDeferredShellNotBlanked`) — that branch never reaches
  `reloadIfChanged`.
- B defaults to disabled, so `TestIssue358ReconnectRefreshesFullSessionState…`
  and every `refreshAfterReconnect()` test keep their synchronous semantics.
- The fetch stays synchronous and the reload `Post` ordering vs. live events is
  preserved (no async reorder), so a freshly-streamed live event cannot be wiped.
- `srcLen/srcHash` track the *source* slice, independent of `trim()` dropping old
  records, so the transcript cap (issue #22) is unaffected.
- gofmt/build/vet/golangci-lint/`go test ./...` per the dev gate; tests run
  without `-race` on the Pi5 (pre-existing `TestUserSessionSendMessage` 404 is the
  only acceptable failure).

**(4) Holistic / cross-repo seam.** Entirely within gogent ui/tui
(`transcript_model.go`, `session_window.go`, `disconnect_modal.go`, a struct field
+ comment in `tui.go`, one wiring line in `cmd/attach.go`). The fingerprint is
computed over gogent's own `ChatMessage` view and stored on gogent's
`transcriptModel`; rendering still flows through the existing turbotv `TextView`
via `addAll`. **No turbotui change, no new turbotui API, no new dep (`hash/fnv` is
stdlib), no go.mod bump.** The repo seam — gogent owns transcript/session state,
turbotui owns the widget toolkit — is respected.

## Tests (mirror disconnect_modal / session_window / issue517 style; ui/tui keeps
no internal/daemon/server imports)

- **Unchanged ⇒ no reload.** Open a window from transcript T; set `Restore()` to
  return T unchanged; call `refreshAfterReconnect`; assert the window's records
  were not rebuilt (spy: wrap render/`restore`, or assert record identity /
  fingerprint unchanged and no re-render).
- **Advanced ⇒ re-sync.** As above but `Restore()` returns T+1; assert the new
  tail is present (reuse the `TestReconnectReSyncsLoadedDeferredWindow` shape).
- **Coalesce.** Set `reconnectCoalesce` to a small value; fire
  `refreshAfterReconnect` rapidly N times; assert `Restore()` ran ~once (leading)
  plus at most one trailing, and the window rebuilt once. Cover the trailing-run
  path (a change arriving during the window is still synced once).
- **nil fetch keeps content** and **unloaded deferred shell untouched** — confirm
  the existing guards still hold through `reloadIfChanged`.

## Open questions

1. **Production coalesce window value** — `750ms` proposed (one approval-poll
   interval; below the reconnect backoff floor). Acceptable, or tie it to a named
   const near `restoreEagerTranscripts`?
2. **Enabling B without touching cmd** — preference between a one-line
   `SetReconnectCoalesce` at attach wiring (keeps logic in ui/tui, default `0`) vs.
   defaulting the window in `NewWorkbench` (zero files outside ui/tui, but must be
   re-verified that no existing test calls `refreshAfterReconnect` twice within
   the window on one `Workbench`). Leaning toward the setter for the strongest
   no-regression guarantee.
3. **Fingerprint inputs** — `Role|Content|Reasoning|Tool|Args` is proposed.
   `Args` is the pretty-printed string already on `ChatMessage`; including it
   catches tool-call arg changes. Confirm no other `ChatMessage` field can change
   without one of these changing.
