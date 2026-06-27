# Design — Reconnect: skip unchanged reload + dedupe early reconnects (issue #520)

## Problem recap

`refreshAfterReconnect` (ui/tui/disconnect_modal.go) re-runs `Restore()` and
**unconditionally** `reload()`s every already-open window. `reload()`
(session_window.go:2745) does `records=nil; restore(); render()` — a full clear +
rebuild. On a flaky first connect the SSE stream is opened before the UI is fully
up (#516); an early health trip (`healthFailThreshold=2`) drives
`consume()→reconnect()→OnConnectionRestored()→refreshAfterReconnect()`, rebuilding
the *same* transcript. Multiple early flaps rebuild it repeatedly — the "restores
the same session many times" symptom.

Two levers, both named by the issue:

- **A. Skip the reload when the transcript is unchanged** — the substantive fix.
  This alone removes the user-visible flicker/rebuild on every flap after the
  first: flaps 2..N fetch `T`, compare to the stored fingerprint, and skip the
  rebuild.
- **B. Dedupe rapid early reconnects** so the redundant `Restore()` HTTP fetch +
  resync collapses to ~one within the initial-connect flap window. **B is a
  bounded round-trip/work optimization layered on A — not the flicker fix (A is).**

Direction **C** (a "messages-since-cursor" replay/append) is **out of scope**; the
no-replay/jump-to-present contract is documented at remote_handlers.go:332-334 and
referenced in a code comment.

> **Revision note (addressing design-critic FAIL on regressions).** The previous
> draft proposed an async *trailing* refresh via `time.AfterFunc`. That was wrong:
> it runs on a timer goroutine concurrent with `consume()` (which is actively
> `Post`ing live events, tui.go:~2504), so its `records=nil; restore(msgs@T0)`
> could **wipe a freshly-streamed live event** that the no-replay stream never
> redelivers — violating this design's own synchronous-ordering invariant (Key
> fact #1). **B is now a synchronous, leading-edge debounce only**: no timer, no
> goroutine, no trailing refresh. Genuine changes that land *after* the coalesce
> window are synced by the next reconnect; the resumed live stream carries
> post-reconnect events as usual.

## Key facts that shape the design

1. **The fetch must stay synchronous.** `notifyRestored()→OnConnectionRestored()→
   refreshAfterReconnect()` runs **synchronously on the single consume goroutine**
   (remote_handlers.go:387-392), and `reconnect()` returns the fresh stream `next`
   only *after* it returns (remote_handlers.go:361-368). So `consume()` does not
   read the new stream — and cannot `Post` any live event — until the refresh's
   reload `Post`s are already enqueued. This ordering is what makes the reload
   safe against wiping live content. Any async work (a timer/goroutine) breaks it.
   Many tests also rely on the synchronous fetch (restore_issue517_test.go:577;
   daemon_lifecycle_issue358_test.go:335).
2. `restore(msgs []ChatMessage)` (session_window.go:2785) is the **single funnel**
   turning a `ChatMessage` slice into records, reached by every build path: eager
   `AdoptSession` (tui.go:1654), lazy focus-load, and `reload()`. Recording a
   fingerprint there keeps it in lock-step with what the window actually shows.
3. msg→record fan-out is **not 1:1** and live-streamed records are formatted by a
   different path than restored ones, so comparing record *counts* or diffing
   records is fragile. The robust, cheap signal is a fingerprint of the **source
   `ChatMessage` slice** the window was last built from. `ChatMessage` has exactly
   5 fields (`Role,Content,Reasoning,Tool,Args`, transcript.go:14-25) and
   `restore()` consumes exactly those — so a fingerprint over all 5 is a complete
   superset of `restore()`'s inputs and a **false skip is structurally
   impossible** (worst case is a harmless false reload). The one residual
   false-skip vector — a hash collision across field boundaries — is closed by
   length-delimiting each field and pairing the hash with the message count.
4. `reload(nil)` is already a deliberate no-op (nil = failed fetch; keep content,
   session_window.go:2752). The skip path must preserve that exactly.

## Change set (all pure ui/tui; no cmd touch)

### A — skip unchanged reload

**ui/tui/transcript_model.go**
- Add to `transcriptModel`: `srcLen int`, `srcHash uint64` (fingerprint of the
  `ChatMessage` slice last built into the model; zero-value on a never-restored
  model).
- `func transcriptSourceSig(msgs []ChatMessage) (int, uint64)` — returns
  `len(msgs)` and an FNV-64a (`hash/fnv`, stdlib) over each message's
  `lower(Role)`, `Content`, `Reasoning`, `Tool`, `Args`, **each length-delimited**
  so no field-boundary aliasing. `lower(Role)` mirrors `restore()`'s
  `strings.ToLower(m.Role)` so role-case never forces a spurious reload.
- `func (m *transcriptModel) matchesSource(n int, h uint64) bool { return
  m.srcLen == n && m.srcHash == h }` — a plain equality. No "was it ever built"
  special-case is needed: the only windows reaching the reload path went through
  `restore()` at least once (deferred shells are filtered upstream — see below),
  and an empty restore sets `srcHash` to the FNV offset basis (≠ the zero value),
  so a genuinely-empty window still matches a genuinely-empty refetch.

**ui/tui/session_window.go**
- `restore(msgs)`: after building records, set
  `sw.transcript.srcLen, sw.transcript.srcHash = transcriptSourceSig(msgs)`. Single
  place the fingerprint is recorded.
- `func (sw *SessionWindow) reloadIfChanged(msgs []ChatMessage)` — the
  reconnect-only wrapper, **delegating to `reload` to avoid duplicating the
  `records=nil; restore()` body**:
  ```
  if msgs == nil { return }                        // failed fetch: keep content
  n, h := transcriptSourceSig(msgs)
  if sw.transcript.matchesSource(n, h) { return }  // unchanged: no wipe, no rebuild
  sw.reload(msgs)                                  // reload() → restore() resets the fingerprint
  ```
  `reload()` is untouched for its other callers.

**ui/tui/disconnect_modal.go**
- In `refreshAfterReconnect`, change the two open-window reloads — the eager-open
  branch (`sw.reload(rs.Messages)`) and the deferred-but-loaded branch
  (`sw.reload(msgs)`) — to `sw.reloadIfChanged(...)`. The unloaded-deferred-shell
  `continue` (disconnect_modal.go:165-167) and the new-session `AdoptSession`
  branch are untouched: an unloaded shell never reaches `reloadIfChanged`.
- Add a one-line comment marking Direction C (cursor replay/append) out of scope,
  referencing the no-replay note at remote_handlers.go:332-334.

Net effect of A: a reconnect whose `Restore()` returns the same transcript a window
already shows does **zero** record rebuilds and **zero** re-renders for that window;
a genuinely advanced/cleared transcript has a different fingerprint and reloads
exactly as today.

### B — dedupe rapid early reconnects (synchronous leading-edge debounce)

**ui/tui/tui.go** (`Workbench` struct) — guarded by the existing `w.mu`:
- `reconnectCoalesce time.Duration` — coalesce window; `0` disables it.
- `reconnectRefreshAt time.Time` — when the last *leading* refresh ran.

`NewWorkbench` (tui.go:699) sets `reconnectCoalesce` to a production default
(`reconnectCoalesceWindow`, proposed **750ms** — below the `500ms` backoff floor,
≈ one approval-poll interval). Defaulting here (rather than at attach wiring)
**avoids touching cmd** and the 3-site `SetReconnectControls` inconsistency
(cmd/attach.go:186, cmd/handoff.go:285, :388). It is safe for existing tests:
each calls `refreshAfterReconnect()` exactly once per `Workbench` (verified across
all 6 sites), so each takes the leading path regardless of the window.

**ui/tui/disconnect_modal.go** — `refreshAfterReconnect` gains a synchronous guard
at the top, running on the consume goroutine (no timer, no goroutine):
```
if w.reconnectCoalesce > 0 {
    w.mu.Lock()
    skip := !w.reconnectRefreshAt.IsZero() &&
            time.Since(w.reconnectRefreshAt) < w.reconnectCoalesce
    if !skip { w.reconnectRefreshAt = time.Now() }
    w.mu.Unlock()
    if skip { return }   // collapse this flap's whole Restore()+resync
}
// …existing body, now using reloadIfChanged…
```
Behaviour: the **first** flap in a burst runs a normal synchronous refresh; every
further flap inside the window collapses entirely — no `Restore()` HTTP, no
resync. A reconnect *after* the window (`reconnectRefreshAt` now stale) is a full
refresh again, so a legitimately-later change is never permanently dropped. This
is the "coalesce within a short window" mechanism the issue suggests, and it stays
**within the existing jump-to-present no-replay contract** (a change occurring
during a sub-second mid-storm outage is reconciled by the next refresh / live
stream — it is not *actively wiped*, which is what the rejected trailing-timer
did).

**Relationship to A (made explicit, per the critique).** A already eliminates the
user-visible churn: flaps 2..N skip the rebuild via the fingerprint. B's marginal
value is purely **eliminating the redundant `Restore()` round-trips** (one list +
≤20 transcript GETs each, #517) and the per-window `Post`/compare work on those
flaps. It carries no rebuild-correctness risk because, being synchronous and
leading-only, it never races `consume()`.

## How the four criteria are met

**(1) Goal match.** A is the fix the issue asks for: unchanged open windows become
a no-op on reconnect. B collapses rapid early reconnects to ~one restore, scoped
honestly as an HTTP/work optimization (the design no longer over-claims B fixes the
flicker — A does). Direction C is excluded with a comment. No feature creep; the §7
jump-to-present / `AdoptSession` shape is unchanged.

**(2) Usability.** Headline win — no flicker/rebuild on a flaky first connect
(from A). **No active-session regression:** because B is leading-only and
synchronous (no trailing timer), it cannot wipe a just-streamed live message; the
earlier draft's active-streaming hazard is gone. New daemon-side sessions still
open via `AdoptSession`; pending approvals still re-surface via `kickApprovals`.
No new UI surface; the disconnect-modal interaction is unchanged.

**(3) No regressions.** This was the prior FAIL; resolved:
- **Synchronous-ordering invariant (Key fact #1) is preserved everywhere** — the
  leading refresh and the (removed) trailing refresh no longer race `consume()`.
  No live-event wipe is possible.
- `reloadIfChanged(nil)` keeps "failed fetch keeps content"
  (`TestReconnectFailedFetchKeepsContentLoaded`, restore_issue517_test.go:589).
- Advanced transcripts still reload (`TestReconnectReSyncsLoadedDeferredWindow`,
  :548) and a loaded→empty sync still reloads
  (`TestReconnectSyncsLoadedWindowToGenuinelyEmptyTranscript`, :624) — both have a
  different fingerprint than the displayed source.
- Unloaded deferred shells keep their placeholder
  (`TestReconnectLeavesDeferredShellUnloaded`, :519) — that branch `continue`s
  before `reloadIfChanged`. (No special-case in `matchesSource` is needed for it.)
- New live-during-outage session still adopts
  (`TestReconnectAdoptsSessionLiveDuringOutage`, :661), unaffected by either lever.
- B defaults via `NewWorkbench`; every existing `refreshAfterReconnect` test calls
  it once per `Workbench`, so all keep leading-path semantics. No `time.AfterFunc`,
  so **no goroutine outlives any test**.
- `srcLen/srcHash` track the *source* slice, independent of `trim()` dropping old
  records, so the transcript cap (issue #22) is unaffected.
- Dev gate: gofmt/build/vet/golangci-lint(0 new)/`go test ./...` (no `-race` on the
  Pi5; pre-existing `TestUserSessionSendMessage` 404 the only acceptable failure).

**(4) Holistic / cross-repo.** Entirely gogent ui/tui (`transcript_model.go`,
`session_window.go`, `disconnect_modal.go`, two struct fields + a default in
`tui.go`). **No cmd change** (default lives in `NewWorkbench`, fixing the prior
3-site wiring gap). The fingerprint is computed over gogent's own `ChatMessage`
and stored on gogent's `transcriptModel`; rendering still flows through turbotui's
`addAll`/`TextView`. **No turbotui change, no new turbotui API, no new dep
(`hash/fnv` is stdlib), no go.mod bump** — verified against the read-only clone.
Seam respected (gogent owns transcript/session state; turbotui owns widgets).

## Tests (mirror restore_issue517 / disconnect_modal style; ui/tui keeps no
internal/daemon/server imports)

- **Unchanged ⇒ no reload.** Open a window from `T`; `Restore()` returns `T`
  unchanged; call `refreshAfterReconnect`; assert no rebuild (spy on
  `restore`/render, or assert the records slice identity is unchanged).
- **Advanced ⇒ re-sync.** Same, but `Restore()` returns `T+1`; assert the new tail
  is present (reuses the `TestReconnectReSyncsLoadedDeferredWindow` shape).
- **Empty refetch on an empty window ⇒ no reload**, and **loaded ⇒ empty still
  reloads** — pin the FNV-offset-basis vs zero-value distinction.
- **Coalesce.** Set `reconnectCoalesce` > 0; fire `refreshAfterReconnect` rapidly
  N times; assert `Restore()` ran exactly once and the window rebuilt at most once;
  then advance past the window (e.g. set `reconnectRefreshAt` back, or use a tiny
  window + a real wait) and assert the next call refreshes again — proving a later
  reconnect is **not** permanently coalesced. No goroutine-leak / trailing-fire
  synchronization is required (there is none).
- **nil fetch keeps content** and **unloaded deferred shell untouched** — confirm
  the existing guards still hold through `reloadIfChanged` / the upstream `continue`.

## Open questions

1. **Coalesce window value** — `750ms` proposed (below the backoff floor; ≈ one
   approval-poll interval). Acceptable, or pin a named const next to
   `restoreEagerTranscripts`?
2. **B's within-window drop is accepted as within the no-replay contract.** A
   change during a sub-second *coalesced* outage is reconciled by the next
   reconnect / live stream rather than immediately. Confirm this matches the
   maintainer's intent for "must not drop a legitimate later change" (read as: a
   *post-window* reconnect must still fully refresh — which it does), or do we
   want the window dialled down further / B gated to first-connect only?
