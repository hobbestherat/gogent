# Design — Persistent "Logs" window surfacing tool/daemon logs (issue #562)

Branch: `pair1/persistent-logs-window-surface-tool-daem`
Scope: **gogent only**. No turbotui change, no new deps, no `go.mod` bump (see Gate 4).
Serializes **after #560** (shared-file-sink / stdlib-log redirect discipline) and after #546.

> **Revision note (post-critique).** This version resolves seven defects found in review:
> (1) TextView has no trim primitive → line-cap is now *compaction-while-following*, not per-line trim;
> (2) the `internal/server → internal/diag` import boundary is respected via a server-local interface;
> (3) chronological interlace by **arrival order** (not absolute cross-host timestamps), which dissolves the out-of-order rebuild entirely;
> (4) the Logs window is now a **readOnly `SessionWindow`** (the analysis-window path), which inherits `Focus`/`cycle`/`activeIDLocked`/tiling/layout-exclusion correctly instead of requiring surgery on them;
> (5) `StreamLogs` reconnect is its own minimal loop (no blocking modal) with an explicit per-window goroutine lifecycle;
> (6) `colorWarn` does not exist — level→color mapping uses the real palette;
> (7) client auth is bearer-token + transport (no "password" path).

---

## 1. What we are building (the ask, restated)

A persistent, **non-modal** "Logs" window in the TUI that:

1. Surfaces gogent's structured diagnostics (`internal/diag` over `log/slog`) **in-app**: timestamp · level · message, wrapped, **level-colored**.
2. **Stays open** alongside session windows — tileable, cycleable, focusable, movable, minimizable — *not* a blocking modal. Reopening **raises** the existing window (no duplicate).
3. **Live-follows the tail**: auto-scrolls when parked at the bottom, does **not** yank when the user has scrolled up.
4. In **remote/attach mode**, **interlaces** local (client) logs with **daemon (remote)** logs in one view, each line tagged `[local]` / `[daemon]`.

Today diagnostics are write-only to invisible sinks: headless→stderr (`internal/diag/logger.go:46`), TUI→`~/.gogent/gogent.log` (`cmd/main.go:113`), daemon→`gogent.log` + detached `daemon.log`; in remote mode the daemon's logs never leave the host. This feature adds an in-memory **tee** + observable broadcast + a UI window + an SSE bridge for the daemon side.

---

## 2. Architecture overview

```
                 ┌──────────────────────── internal/diag (stays a stdlib-only leaf) ──────────┐
   log call ───► │  *diag.Logger (slog)                                                       │
                 │     └─ fanoutHandler ──┬─► TextHandler → file/stderr (UNCHANGED bytes)      │
                 │                        └─► ringHandler → *diag.Ring (NEW)                    │
                 │                                  • bounded []Record (Time,Level,Text)        │
                 │                                  • Snapshot() / Subscribe() (chan + cancel)  │
                 └───────────────────────────────────┬────────────────────────────────────────┘
                                                      │
   embedded/TUI (cmd/main.go) ────────────────────────┤ Workbench holds the local *diag.Ring
   remote client (cmd/attach.go) ─────────────────────┤ client ring = the [local] stream
                                                      │
   daemon (cmd/daemon.go) ─► ring ─► adapter ─► internal/server.LogStreamer (interface)
                                                      │            │
                                                      │   GET /api/logs/stream (SSE, AuthRequired)
   remote client (ui/tui) ◄───────────────────────────┘   StreamLogs() → [daemon] stream
                                                      │   (minimal reconnect: backoffFor, no modal)
                                                      │
            ┌──────────────────────────────────────────┴───────────────────────────────────┐
            │  Logs window = a readOnly SessionWindow (id "logs"), built in logs_window.go    │
            │   • lives in w.sessions / w.order → tiling, cycle, Focus, activeIDLocked,        │
            │     layout-exclusion all inherited (same as analysis windows, #58)              │
            │   • body = sw.history (tv.TextView): Wrap + follow; search/fold/yank for free    │
            │   • merges [local] ring + [daemon] SSE by ARRIVAL order; level-colored; redacted │
            │   • line-cap by compaction-while-following (no per-line trim needed)             │
            └────────────────────────────────────────────────────────────────────────────────┘
```

Single source of truth for "what a log line is": a structured `Record` produced through the existing `diag.Logger` slog path, so **`Secret` redaction (`internal/diag/secret.go`) holds on every captured/streamed line**.

---

## 3. gogent changes — exact files & functions

### 3.1 `internal/diag/ring.go` (NEW) — bounded, observable log ring (keeps `diag` a leaf)

```go
type Record struct {
    Time  time.Time
    Level slog.Level   // Info/Warn/Error → window color
    Text  string       // formatted "msg key=val …" (redacted), no trailing \n
}

type Ring struct {
    mu   sync.Mutex
    buf  []Record
    size int
    subs map[chan Record]struct{}
}

func NewRing(size int) *Ring
func (r *Ring) append(rec Record)                   // history + non-blocking fanout (drop-on-full)
func (r *Ring) Snapshot() []Record                  // history copy (prime on open)
func (r *Ring) Subscribe() (<-chan Record, func())  // buffered chan (256) + unsubscribe
```

- `append`: under lock, push to `buf` (trim oldest past `size`), then non-blocking `select { case ch<-rec: default: }` to each subscriber — same drop-on-full discipline as `hub.deliver` (`hub.go:94-109`); never stalls the logger/agent loop.
- Ring `size` = **2000** records (the 50-line notification ring is far too small for logs; window display caps at ~1000 — see §3.4).
- **Imports: stdlib only** (`sync`, `time`, `log/slog`). No `internal/*`. This is load-bearing for Gate 3 / §3.7: `internal/diag` must stay a leaf so the new `server`-side consumer (which deliberately avoids importing `diag`, `api.go:19,37`) can sit behind an interface.

### 3.2 `internal/diag/logger.go` — tee the logger into the ring (additive)

`Logger` wraps one `*slog.Logger` over a `TextHandler`. Add a private **fan-out `slog.Handler`** so the existing sink is untouched and a second handler captures structured records.

- `fanoutHandler` implements `slog.Handler`; `Enabled`/`Handle`/`WithAttrs`/`WithGroup` delegate to **both** the inner `TextHandler` (file/stderr, exactly as today) and a `ringHandler`.
- `ringHandler.Handle(ctx, rec)`: render `rec` to text via a pooled `bytes.Buffer` + a throwaway `slog.NewTextHandler` (this resolves `LogValuer`, so `Secret` is redacted identically to the file sink), strip the leading `time=… level=…` that we already carry as fields, and `ring.append(Record{rec.Time, rec.Level, text})`.
- **Additive constructors** (existing `New`/`Stderr`/`NewFile` stay byte-identical so headless/embedded are untouched):
  ```go
  func NewWithRing(w io.Writer, ring *Ring) *Logger
  func NewFileWithRing(path string, ring *Ring) (*Logger, error)
  ```
- Handler tee (not `io.MultiWriter`): keeps `slog.Level`/`time.Time` first-class for color + future filtering, avoiding text re-parse (the issue's RECOMMENDED "structured records over raw file lines").

### 3.3 Wiring the ring at startup

The ring is a **client-side** capability wherever a Logs window can exist, and a **daemon-side** capability to feed SSE.

- `cmd/main.go:113` (embedded TUI): `ring := diag.NewRing(2000)`; build the logger with `diag.NewFileWithRing(gogent.log, ring)`; `g.SetLogger(lg)`; pass `ring` to the Workbench. **Headless path keeps `diag.Stderr()` — unchanged, no ring.**
- `cmd/attach.go:245` (remote client TUI): tee the client logger (`diag.New(f)` → `diag.NewWithRing(f, ring)`) and pass `ring` to the Workbench. This is the `[local]` stream in remote mode.
- `cmd/daemon.go:380` (daemon): build `diag.NewFileWithRing(gogent.log, ring)`; `g.SetLogger(lg)`; pass an **adapter** (ring → `server.LogStreamer`, §3.7) into `server.Options`. This is the `[daemon]` stream.

Workbench gains `logRing *diag.Ring` (nil-safe: nil ⇒ the window shows only `[daemon]` records, or nothing locally; never panics).

### 3.4 `ui/tui/logs_window.go` (NEW) — the persistent window, built on the analysis-window path

**Window-approach decision (was the largest review gap).** The Logs window is a **readOnly `SessionWindow`** with a synthetic singleton id `"logs"`, opened through the same `openWindowAny(id, title, readOnly=true)` machinery (`tui.go:1779`) that already backs analysis windows (#58). **Rationale, verified in code:** analysis windows are already non-conversational readOnly windows that live in `w.sessions`/`w.order`, and the entire UI already copes with "the active window is a readOnly window" — `activeIDLocked` (`tui.go:1970`) finds it in `w.sessions` (no desync), `Focus`/`cycle` already iterate it, tiling's `openWindows()` (`tiling.go:28`) already gathers it, `captureLayout` already **excludes** it (`tui.go:2397 if sw.readOnly continue`), and chrome already branches on `activeRO`/`!sw.readOnly` (`tui.go:1006`, `command_palette.go:375`, `disconnect_modal.go:207-239`). So the Logs window inherits **tiling, cycle, focus, raise, minimize, move, and layout-exclusion correctly and for free**, with no surgery on `Focus`/`cycle`/`activeIDLocked`. *(The discarded alternative — a bare `tv.Window` per the issue's literal wording — would require teaching all three of those session-keyed functions about a non-session window, including the `activeIDLocked` "no session matches → wrong sidebar/Overall highlight" desync. That is strictly more regression-prone; we reject it.)*

Minimal generalization needed on the SessionWindow shell (well-contained, in `session_window.go` + `openWindowAny`):
- A window-kind discriminator so the logs window (a) skips the `" (analysis)"` title suffix (`session_window.go:215`) and uses title `"Logs"`, (b) skips the `"[System] … ready"` transcript banner (`:243`), and (c) marks itself for log population instead of transcript restore. Concretely: add `kind windowKind` (`kindLive`/`kindAnalysis`/`kindLogs`) to `SessionWindow`, derive `readOnly = kind != kindLive` so **every existing `sw.readOnly` check keeps working unchanged**, and branch the suffix/banner on `kind`.

`func (w *Workbench) showLogsWindow()`:
1. **Raise-not-duplicate**: `openWindowAny("logs", …)` already returns the existing window on id collision (`tui.go:1788-1791`, the analysis duplicate-guard verified by `duplicate_window_guard_issue518_test.go`); after it returns, call `w.Focus("logs")` to raise + focus. So reopening raises, never duplicates — inherited, not re-implemented.
2. On first open: populate history from `w.logRing.Snapshot()` (each record → `appendLogLine(localSource, r)`), then start the live subscriptions (§3.5).
3. Body is the window's existing `sw.history` `tv.TextView` (`Wrap=true`, `follow=true` default) — we get the search/filter/fold/yank toolkit (`registerTranscriptBindings`) over log lines for free.

`func (w *Workbench) appendLogLine(src logSource, r diag.Record)` — **UI thread only**, append-only hot path:
- Build the display line: `"<HH:MM:SS.mmm> [local|daemon] <LVL> <text>"`. The `[local]/[daemon]` tag is **present only in remote mode** (omitted in embedded mode for a clean local view).
- Color by level using the **real palette** (`theme.go:23-26`): `colorInfo` (cyan) for Info, **`colorTool` (yellow)** for Warn — *there is no `colorWarn`; either reuse `colorTool` or introduce a `colorWarn` alias if a distinct role is wanted* — `colorError` (red) for Error. `sw.history.AddColored(line, color)`.
- **Ordering = ARRIVAL order, not absolute timestamp.** Local and daemon records are appended as they arrive and merged into the one view; each line shows its own host's clock in the text. We deliberately do **not** sort across the two streams: their timestamps come from two different hosts, so absolute-time merge is meaningless under clock skew, and two live tails interleaved by arrival *are* the chronological reality of "what happened on the wire." This keeps the hot path strictly append-only and removes any insert-above-the-viewport rebuild (the previous design's out-of-order case is gone).
- **Auto-follow discipline = free** (verified in `widget_textview.go`): `follow=true` default (`:168`); `touch()` pins to bottom only while following (`:401`); `scrollBy()` re-enables follow only at the absolute bottom and clears it on scroll-up (`:799-805`); `clampScroll()` re-anchors each draw (`:625`). So append auto-scrolls only when parked at the bottom and never yanks when scrolled up — no extra code.

**Line cap — compaction-while-following (resolves the "TextView has no trim" blocker).** Verified: `TextView` exposes only `SetText`/`Clear`/`AddLine`/`AddColored`/`AddStyled` — **no trim/remove/`SetMaxLines`**. So we cannot drop the oldest displayed line cheaply. Mechanism:
- Keep a bounded backing slice `logsLines []displayLine` (cap ~1000, trim oldest — cheap, it's a slice).
- The displayed `TextView` is allowed to exceed the cap *while the user is scrolled up reading history*. **Compaction (`history.Clear()` then re-add the last ~1000 from `logsLines`) runs only when `follow==true`** (user parked at the tail) and the displayed count crosses a high-water mark (e.g. 1500). When following, re-adding ends at the tail with `follow` intact, so the rebuild is invisible. A user reading history is **never** disrupted; compaction simply waits until they return to the tail. This is an amortized O(N) compaction (~once per 500 lines), not a per-line cost, so it does not contradict the append-only hot path, and it needs no scroll-position restore (we only compact when the position is "bottom").
- *Optional clean alternative, called out for the seam (Gate 4):* a one-method turbotui `TextView.TrimFront(n int)` would make the cap O(1) and drop the compaction entirely. We keep the gogent-side compaction as primary to honor "no turbotui change"; `TrimFront` is noted as an explicit, optional cross-repo addition if a maintainer prefers it.

**Close / lifecycle.** `window.OnClose` already routes to `CloseSession("logs")` (the readOnly shell wires `OnClose → CloseSession(id)`, `session_window.go:226`). We hook the `"logs"` id in/near `CloseSession` to also call the subscription cancel (below). Optional `q`/`Esc`-to-close while focused via a binding on the history view.

### 3.5 Subscriptions & the per-window goroutine lifecycle (new, explicit)

There is **no** existing per-window goroutine convention (the Workbench has only the global `shutdown`/`quit`, `tui.go:592`; the monolog owns no goroutine). The Logs window introduces one, spelled out:
- On open, store `w.logsCancel`. Start a goroutine that drains `w.logRing.Subscribe()` and, per record, `w.Post(func(){ w.appendLogLine(localSource, r) })` (`tui.go:932` marshals onto the UI thread; widgets are touched only there).
- In remote mode, start a second goroutine for the daemon SSE stream (§3.7), appending with `daemonSource`.
- `w.logsCancel()` (called from the `"logs"` `CloseSession` hook and from app shutdown) cancels a `context.Context`, unsubscribes the ring channel, and tears down the SSE stream — so we never hold an idle daemon stream when the window is closed.

### 3.6 Menu + keybinding — `ui/tui/tui.go` `settingsItems()` (~:1170, near Statistics :1209)

- Add `tv.NewMenuItem("&Logs…", func() { w.showLogsWindow() })` to the Settings menu. No handler gating (the window is always available; nil ring ⇒ remote-only/empty).
- Optional chord **Ctrl+Shift+L**: add an action id in `command_palette.go` (mirror "Sub-agents" at :246) so `rebuildBindings` (`keybindings.go:239`) registers it → `showLogsWindow` (which raises if already open).

### 3.7 Remote interlace — SSE endpoint + client consumer

**Server (`internal/server`) — respecting the `diag` boundary.** `internal/server` deliberately does not import `internal/diag` (`api.go:19,37`). We keep it that way: define a server-local interface and DTO; the daemon supplies the adapter.

```go
// internal/server (new logs.go) — no import of internal/diag.
type LogRecord struct { Time time.Time; Level string; Text string }
type LogStreamer interface {
    Snapshot() []LogRecord
    Subscribe() (<-chan LogRecord, func())
}
```
- `server.Options` gains `Logs LogStreamer` (nil in embedded mode ⇒ the endpoint streams nothing / is unused).
- `cmd/daemon.go` (which already imports both `diag` and `server`) provides a tiny adapter from `*diag.Ring` to `server.LogStreamer` (mapping `slog.Level` → `"INFO"/"WARN"/"ERROR"`). This is the *only* place the two packages meet — boundary preserved, `diag` stays a leaf.
- Endpoint: register `{Path: "/api/logs/stream", Method: GET, Handler: ev.LogStream, AuthLevel: req}` in the `api.go:157` table (`req = webapi.AuthRequired`, same gate as `/events`). Handler returns a `webapi.EventStreamResponse{Producer}` (the proven shape, `events.go:22-37`, `webapi/sse.go:47`): prime from `Snapshot()`, then drain `Subscribe()` and `stream.Send(logSSE(rec))` until `stream.Context().Done()`, `defer cancel()`. SSE event name `"log"` (distinct from session-event type names and `"notification"` at `events.go:70`, so the client discriminates frames). Optional one-shot `{Path:"/api/logs", …}` with `?tail=N` from `Snapshot()` for reconnect catch-up.
- Backpressure: the SSE producer is an ordinary drop-on-full subscriber; never stalls the daemon/agent.

**Client (`ui/tui`).**
- DTO near `api_client.go:312`:
  ```go
  type LogRecordDTO struct {
      Time  string `json:"time"`   // RFC3339Nano
      Level string `json:"level"`  // "INFO"|"WARN"|"ERROR"
      Text  string `json:"text"`   // already redacted server-side
  }
  ```
- `func (c *APIClient) StreamLogs(ctx) (<-chan LogRecordDTO, error)` — sibling of `StreamEvents` (`api_client.go:867`) reusing `parseSSE` (`:927`), filtering frames named `"log"`. **Auth is bearer-token + transport** (`api_client.go:195`: token for non-loopback TCP; unix-socket file perms; SSH `DialContext` for the tunnel) — *there is no client "password" path; the brief's "token/password/loopback" is imprecise.*
- **Reconnect — its own minimal loop, not the session scaffolding.** The session `reconnect`/`consume` path (`remote_handlers.go`) raises a blocking "connection lost" modal, restarts the tunnel, and offers "retry now" — appropriate for the interactive session stream, **wrong for a best-effort log tail**. `StreamLogs`'s loop reuses only the pure `backoffFor` helper (`remote_handlers.go:457`: 0.5→1→2→5→10s cap) and reconnects **silently** (no modal, no "retry now"); a gap in the log tail is acceptable and optionally re-primed via `?tail=N`. The loop is the per-window goroutine of §3.5, cancelled on window close.
- In `remote_handlers.go`, when attached and the Logs window is open, the SSE goroutine does `w.Post(func(){ w.appendLogLine(daemonSource, toRecord(dto)) })`.

---

## 4. The four design gates

### Gate 1 — Goal match → **OK**
Delivers exactly the issue: persistent **non-modal** Logs window, **in-memory ring tee** on `diag.Logger` (file/stderr sink untouched), **level-colored live-follow** body, **`/api/logs/stream` SSE** interlacing `[local]`+`[daemon]` in remote mode. Filtering UI deferred (issue-sanctioned); raw `daemon.log` stdout capture deferred with the #560 overlap noted; window excluded from layout persistence (issue recommendation). One honored deviation from the issue's *literal* wording: the window is built on the readOnly-`SessionWindow` shell rather than a bare `tv.Window` — same user-facing result (a non-modal `NewWindowLayer` window), lower regression risk (Gate 3).

### Gate 2 — Usability → **OK**
- **Auto-follow-only-at-bottom, no-yank-when-scrolled-up**: free from `TextView.follow` semantics (verified `widget_textview.go:168,401,799-805,625`).
- **Tileable / cycleable / focusable / movable / minimizable / raised-via-cycle**: inherited from the analysis-window path (lives in `w.sessions`/`w.order`); `activeIDLocked` finds it (no sidebar/Overall desync), unlike the rejected bare-window approach.
- **Reopen raises, never duplicates**: inherited from `openWindowAny`'s id-collision guard (verified test `duplicate_window_guard_issue518_test.go`).
- **History on open then live updates**; `[local]/[daemon]` tags only in remote mode; standard close button (+ optional `q`/`Esc`); search/fold/yank over logs for free.
- **Right thing surfaced, not silent**: previously-invisible diagnostics are in-app; stranded remote daemon logs are streamed and clearly tagged.

### Gate 3 — No regressions → **OK**
- **Embedded/headless unchanged**: `diag.New/Stderr/NewFile` untouched; tee is opt-in via `*WithRing`. Same `gogent.log` bytes.
- **Redaction preserved**: ring branch resolves attrs via `slog.NewTextHandler`, so `Secret` redacts identically (`secret.go:23`); asserted by a new unit test.
- **Non-blocking / bounded**: ring fan-out and SSE sends are drop-on-full over bounded buffers (hub discipline); logger/agent/daemon never stall.
- **Import boundary preserved**: `internal/server` still does not import `internal/diag` (interface + daemon-side adapter); `internal/diag` stays a stdlib-only leaf.
- **Session-keyed UI integration is reuse, not surgery**: the window is just another readOnly `SessionWindow`; the only new code in `Focus`/`cycle`/`activeIDLocked` is **none** — they already handle readOnly windows. The contained change is the `windowKind` discriminator on the SessionWindow shell, with `readOnly` *derived* so every existing `sw.readOnly` check is unchanged.
- **New per-window goroutine lifecycle** (§3.5) is explicitly bounded by `w.logsCancel` on close/shutdown — no leaked goroutine, no idle daemon stream.
- **Line-cap compaction** only runs while following, so it never disturbs a user's scroll/selection.
- gofmt/build/vet/golangci-lint clean; `go test ./...` green (pre-existing `TestUserSessionSendMessage` 404 and load-induced `TestStopGracefulAndForced` flake acceptable per brief; tests run **without `-race`** on the Pi5).

### Gate 4 — Holistic (both repos) → **OK**
- **turbotui: NO change required.** `tv.Window` (`ShowClose`/`Resizable`/`Minimizable`/`Maximizable`/`OnClose`) + `NewWindowLayer` (non-modal, `layer.go:99` — lower-layer menu shortcuts stay live) + `TextView` (`Wrap`/`follow`/`AddColored`/`ScrollToBottom`/search/scrollbar) cover everything via the existing SessionWindow shell. A native `LogView` widget would be redundant. The **only** place a turbotui primitive would be *cleaner* is line-cap (`TextView.TrimFront`); we keep that gogent-side (compaction-while-following) so "no turbotui change" holds honestly, and flag `TrimFront` as an explicit optional cross-repo addition rather than hand-waving it.
- **Right place in gogent**: capture in `internal/diag` (where redaction lives), broadcast reuses the hub pattern, SSE sits with the other event streams behind a boundary-respecting interface, UI reuses the analysis-window shell + one new `logs_window.go`. `ui/tui` stays free of forbidden imports (consumes `diag.Record`/`diag.Ring` + the client DTO only).
- No new deps; stdlib-first; rebase onto current `origin/main` (post-#560) at the gate.

---

## 5. Test plan (sketch)
- `internal/diag`: ring append/trim/snapshot; subscribe fan-out + drop-on-full under a stalled subscriber; **`Secret` redaction holds on the ring path**; fan-out logger writes byte-identical output to the file sink vs the non-teed logger; `diag` has no `internal/*` import (leaf assertion).
- `internal/server`: `LogStream` producer primes snapshot then streams live; bounded/non-blocking under a stalled subscriber; auth gate `AuthRequired`; server package does not import `internal/diag`.
- `ui/tui`: `showLogsWindow` raises-not-duplicates (reuses the analysis collision guard); `appendLogLine` arrival-order append + level→color mapping (`colorInfo`/`colorTool`/`colorError`); `[local]/[daemon]` tag only in remote mode; **compaction runs only while following** and is a no-op while scrolled up; the `windowKind` discriminator leaves all existing `readOnly` behavior intact; mock SSE interlace test merging two arrival-ordered streams; goroutine cancelled on close.

---

## 6. Open questions (implementer decides + documents)
1. **Reconnect catch-up**: on SSE reconnect, re-prime daemon tail via `GET /api/logs?tail=N`, or accept a small gap (best-effort tail)? Lean: accept the gap; add `?tail=N` if cheap.
2. **`daemon.log` raw stdout** (stdlib `log.Printf`, webapi warnings detached to `~/.gogent/daemon.log`): capture too, or structured `diag` path only? Overlaps #560's stdlib-log redirect. Lean: **structured `diag` path only** for this issue; raw-stdout capture is a follow-up once #560 lands.
3. **Ring size 2000 / display cap ~1000 / compaction high-water 1500** — confirm against memory budget; tune if needed.
4. **Warn color**: reuse `colorTool` (yellow) or introduce a dedicated `colorWarn` theme role? Lean: reuse `colorTool` initially; a distinct role is a one-line theme addition if desired.
5. **`windowKind` vs `readOnly bool`**: introduce the kind enum with `readOnly` derived (recommended, keeps all call sites), or thread a separate flag? Lean: derived enum.
6. **Source/level filter UI** (`[local]`-only / `[daemon]`-only / level filter): deferred per the issue; the structured `Record` already carries level+source to make it cheap later.
7. **Optional turbotui `TextView.TrimFront(n)`**: keep gogent-side compaction (default), or land the one-method primitive for an O(1) cap? Cross-repo, opt-in.
