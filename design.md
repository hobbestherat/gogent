# Design — Persistent "Logs" window surfacing tool/daemon logs (issue #562)

Branch: `pair1/persistent-logs-window-surface-tool-daem`
Scope: **gogent only**. No turbotui change, no new deps, no `go.mod` bump (see Gate 4).
Serializes **after #560** (shared-file-sink / stdlib-log redirect discipline) and after #546.

> **Revision note (round 1, post-critique).** Resolved seven defects:
> (1) TextView has no trim primitive; (2) `internal/server → internal/diag` boundary via a server-local interface;
> (3) interlace by **arrival order** (not cross-host timestamps); (4) the Logs window is a **readOnly `SessionWindow`** (analysis-window path), inheriting tiling/cycle/layout-exclusion; (5) `StreamLogs` reconnect is its own minimal loop; (6) `colorWarn` does not exist; (7) client auth is bearer-token + transport.
>
> **Revision note (round 2, post-critique).** Resolved four residual defects, all by leaning *harder* on the `transcriptModel` that the readOnly shell already owns (`transcript_model.go`):
> (A, blocker) log lines are populated through **`sw.transcript.add(transcriptRecord)`**, NOT `sw.history.AddColored` direct — the direct path is wiped by `transcriptModel.render()`'s `Clear()` on the first search/fold; routing through records also makes "search/fold/yank for free" actually true;
> (B) `Focus` targets `sw.history` for the (input-less) readOnly Logs window — `SetFocus(sw.input)` would leave focus nil;
> (C) the line cap reuses the transcript model's **existing amortised `trim()`** (`m.limit` + `trim()`, `transcript_model.go:427-446`) instead of an unobservable "compact-while-following" gate (`TextView` has no `Following()` accessor);
> (D) the `[System] … ready` banner is gated *before* its unconditional `m.add` (`session_window.go:243`).

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
            │   • lives in w.sessions / w.order → tiling, cycle, raise, activeIDLocked,        │
            │     layout-exclusion all inherited (same as analysis windows, #58)              │
            │   • body = sw.transcript (transcriptModel over sw.history): each log line is a    │
            │     transcriptRecord(kindLog) → search/fold/filter/yank genuinely free           │
            │   • merges [local] ring + [daemon] SSE by ARRIVAL order; level-colored; redacted │
            │   • line-cap = the transcript model's existing amortised trim() (m.limit ≈ 1000) │
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

Three well-contained changes on the SessionWindow shell (`session_window.go` + `openWindowAny` + `Focus`) — verified necessary, not hand-waved:

1. **Window-kind discriminator.** Add `kind windowKind` (`kindLive`/`kindAnalysis`/`kindLogs`) to `SessionWindow`, derive `readOnly = kind != kindLive` so **every existing `sw.readOnly` check keeps working unchanged**. Branch on `kind` for: (a) the `" (analysis)"` title suffix (`session_window.go:215`) → use plain `"Logs"`; (b) the `"[System] … ready"` transcript banner — *this `m.add` is unconditional at `:243-248`, **before** the readOnly branch returns at `:269*, so the banner must be gated right there (skip the `m.add` for `kindLogs`), not in the post-branch body (Defect D).

1a. **New `kindLog` eventKind** (`transcript_model.go`): logs are their own record kind, distinct from `kindSystem`, so they read cleanly and a future level/source filter is trivial. Touch points are small and enumerated: the `eventKind` const block (`:20`), the kind→label/role switch (`:38-50`), and the filter-cycle upper bound (`for k := kindSystem; k <= kindCompaction`, `:608`) — extend the range to include `kindLog`. Per-line *level* color does **not** come from the kind (kind is fixed) — it rides the record's `color` field (below), so info/warn/error coloring is independent of the kind taxonomy.

2. **Focus target for an input-less window (Defect B).** `Focus(id)` ends with `w.desktop.SetFocus(sw.input)` (`tui.go:1874`), but `sw.input == nil` for readOnly windows (the readOnly branch returns at `:269`, before `sw.input` is assigned at `:327`). `SetFocus(nil)` is nil-safe but leaves **focus nil**, so keyboard scroll / `q`/`Esc` don't work until the user clicks the body. Fix: branch `Focus` to `SetFocus(sw.history)` when `sw.input == nil` (i.e. readOnly). This is a **one-line, nil-guarded** change — and it also fixes the latent "analysis window opens unfocused" case, so it's a net improvement, not a regression. (The round-1 "no surgery on `Focus`" claim was overstated; this single guarded branch is the only `Focus` change, and it touches nothing session-specific.)

`func (w *Workbench) showLogsWindow()`:
1. **Raise-not-duplicate**: `openWindowAny("logs", "Logs", true)` returns the existing window on id collision (`tui.go:1788-1791`, verified by `duplicate_window_guard_issue518_test.go`); after it returns, `w.Focus("logs")` raises + focuses (now via `sw.history`, per the Focus fix). Reopening raises, never duplicates — inherited.
2. **Configure the cap**: set the logs transcript's `limit` (the transcript model's existing capping field, `transcript_model.go`) to ~1000.
3. **Prime history**: from `w.logRing.Snapshot()` (capped to the limit), adding records via the **batch path `restore()` uses** (append records + a single `render()`, `transcript_model.go:~420`) so priming doesn't fire `trim()` repeatedly. Each primed record is a `kindLog` `transcriptRecord` (below).
4. Start the live subscriptions (§3.5).

**Populating a log line — through the transcript model, never the raw view (Defect A).** The readOnly shell registers the transcript toolkit (`registerTranscriptBindings`, `session_window.go:255`) over `sw.transcript` (`transcriptModel`), and `transcriptModel.render()` (`transcript_model.go:500-514`) does `m.view.Clear()` then re-renders **only from `m.records`**. So any line written via `sw.history.AddColored` *directly* would be wiped on the first search/filter/fold. Therefore `appendLogLine` routes through the model:

`func (w *Workbench) appendLogLine(src logSource, r diag.Record)` — **UI thread only**:
- Build the display text `"<HH:MM:SS.mmm> [local|daemon] <LVL> <text>"`; the `[local]/[daemon]` tag is **present only in remote mode** (omitted embedded for a clean local view).
- Construct `&transcriptRecord{kind: kindLog, header: text, color: levelColor}` and call **`sw.transcript.add(rec)`** (plain `add`, not `addAndReveal` — `add` renders incrementally via `renderOne`, which *respects the current scroll position*, `transcript_model.go:458,475`). Because the line is now a real record, search / filter / fold / yank operate on it for real — "for free" is now true, not aspirational.
- **Level color**: carried on the record's `color` field (`headerColor()` returns `r.color` when `role==roleNone`, `transcript_model.go`). Mapping uses the **real palette** (`theme.go:23-26`): `colorInfo` (cyan)=Info, **`colorTool` (yellow)=Warn** (*there is no `colorWarn`*; reuse `colorTool`, or add color *roles* `roleInfo/roleWarn/roleError` if theme-on-render recolor is wanted), `colorError` (red)=Error.
- **Ordering = ARRIVAL order, not absolute timestamp.** Local and daemon records are appended as they arrive; each line shows its own host's clock in the text. We deliberately do **not** sort across the two streams (two hosts' clocks make absolute-time merge meaningless under skew; two live tails interleaved by arrival *are* the on-the-wire chronology). This keeps population append-only — no insert-above-the-viewport rebuild.
- **Auto-follow discipline = free**: `add`→`renderOne` "respects the current scroll position (so streaming does not yank a user who scrolled up)" (`transcript_model.go:475`); the backing `TextView` re-pins to the bottom on append only while following (`widget_textview.go:401,799-805,625`). So it auto-scrolls only when parked at the bottom and never yanks when scrolled up — the same behavior live session windows already have.

**Line cap — reuse the transcript model's existing amortised `trim()` (resolves the round-1 blocker honestly).** The earlier "compact-while-following" gate was unimplementable: `TextView` exposes **no `Following()`/`AtBottom()` accessor**, only `ScrollToBottom`/`ScrollToTop` setters (`widget_textview.go:383,392`). But we no longer need it: `transcriptModel.add` already enforces a cap — when `len(m.records) > m.limit` it calls `trim()`, which drops the oldest ~10% and rebuilds, *amortised* ("roughly a tenth of the limit dropped at once so the full rebuild is amortised across many adds rather than firing on every add", `transcript_model.go:427-446`). Setting the logs transcript's `limit ≈ 1000` gives exactly the issue's "cap ~1000, trim oldest" with an O(N) rebuild only ~once per 100 lines. This is the **identical mechanism every live session window already uses**, so logs inherit its (rare) trim-time re-render-to-bottom rather than introducing any new behavior — and it needs **no turbotui change** and **no unobservable gate**. The optional turbotui `TextView.TrimFront(n)` (O(1) cap) remains a possible cross-repo nicety but is now unnecessary.

**Close / lifecycle.** `window.OnClose` routes to `CloseSession("logs")` (the readOnly shell wires `OnClose → CloseSession(id)`, `session_window.go:226`). Hook the `"logs"` id in/near `CloseSession` to also call the subscription cancel (§3.5). Optional `q`/`Esc`-to-close while focused via a binding on `sw.history` (now actually reachable, given the Focus fix).

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
- **Auto-follow-only-at-bottom, no-yank-when-scrolled-up**: free, because population goes through `transcript.add`→`renderOne`, which respects the current scroll position (`transcript_model.go:475`) over a `TextView` that re-pins only while following (`widget_textview.go:401,799-805,625`).
- **Tileable / cycleable / movable / minimizable / raised-via-cycle**: inherited from the analysis-window path (lives in `w.sessions`/`w.order`); `activeIDLocked` finds it (no sidebar/Overall desync), unlike the rejected bare-window approach.
- **Focusable on open/cycle**: the one-line `Focus` fix targets `sw.history` for the input-less Logs window, so keyboard scroll / `q`/`Esc` work immediately (without it, focus would land on a nil `sw.input` — Defect B). This also fixes analysis windows, which open unfocused today.
- **Search / fold / filter / yank over logs are genuinely free** — log lines are real `transcriptRecord`s, so they survive `transcriptModel.render()`'s `Clear()` (the direct-`AddColored` path would have wiped them — Defect A).
- **Reopen raises, never duplicates**: inherited from `openWindowAny`'s id-collision guard (verified test `duplicate_window_guard_issue518_test.go`).
- **History on open then live updates**; `[local]/[daemon]` tags only in remote mode; standard close button.
- **Right thing surfaced, not silent**: previously-invisible diagnostics are in-app; stranded remote daemon logs are streamed and clearly tagged.

### Gate 3 — No regressions → **OK**
- **Embedded/headless unchanged**: `diag.New/Stderr/NewFile` untouched; tee is opt-in via `*WithRing`. Same `gogent.log` bytes.
- **Redaction preserved**: ring branch resolves attrs via `slog.NewTextHandler`, so `Secret` redacts identically (`secret.go:23`); asserted by a new unit test.
- **Non-blocking / bounded**: ring fan-out and SSE sends are drop-on-full over bounded buffers (hub discipline); logger/agent/daemon never stall.
- **Import boundary preserved**: `internal/server` still does not import `internal/diag` (interface + daemon-side adapter); `internal/diag` stays a stdlib-only leaf.
- **Session-keyed UI integration is reuse, not surgery**: `cycle`/`activeIDLocked`/tiling/layout-exclusion need **no** change — they already handle readOnly windows. The only touched function is `Focus`, gaining a **single nil-guarded branch** (`SetFocus(sw.history)` when `sw.input == nil`) that improves analysis windows too. The rest is contained to the SessionWindow shell: the `windowKind` discriminator (with `readOnly` *derived*, so every `sw.readOnly` check is unchanged), the banner gate, and the new `kindLog` record kind (enumerated touch points in §3.4).
- **Population through `transcript.add`, not the raw view**: log lines are real records, so `render()` (search/fold/filter) never wipes them (Defect A) — the round-2 functional break is closed.
- **New per-window goroutine lifecycle** (§3.5) is explicitly bounded by `w.logsCancel` on close/shutdown — no leaked goroutine, no idle daemon stream.
- **Line cap reuses the transcript model's existing amortised `trim()`** (`m.limit ≈ 1000`), the identical mechanism every live session window already uses — so it introduces no new behavior and needs no unobservable scroll-state gate (the round-2 `Following()` gap is moot).
- gofmt/build/vet/golangci-lint clean; `go test ./...` green (pre-existing `TestUserSessionSendMessage` 404 and load-induced `TestStopGracefulAndForced` flake acceptable per brief; tests run **without `-race`** on the Pi5).

### Gate 4 — Holistic (both repos) → **OK**
- **turbotui: NO change required — and now with no caveat.** `tv.Window` (`ShowClose`/`Resizable`/`Minimizable`/`Maximizable`/`OnClose`) + `NewWindowLayer` (non-modal, `layer.go:99` — lower-layer menu shortcuts stay live) + `TextView` (`Wrap`/`follow`/`AddColored`/`ScrollToBottom`/search/scrollbar) cover everything via the existing SessionWindow shell + its `transcriptModel`. A native `LogView` widget would be redundant. The round-1/round-2 line-cap question (`TextView.TrimFront`/`Following()`) is **resolved entirely gogent-side** by reusing the transcript model's amortised `trim()`, so we no longer rely on — or even need — any new turbotui primitive. `TrimFront(n)` remains a purely optional O(1) nicety, not a dependency.
- **Right place in gogent**: capture in `internal/diag` (where redaction lives), broadcast reuses the hub pattern, SSE sits with the other event streams behind a boundary-respecting interface, UI reuses the analysis-window shell + one new `logs_window.go`. `ui/tui` stays free of forbidden imports (consumes `diag.Record`/`diag.Ring` + the client DTO only).
- No new deps; stdlib-first; rebase onto current `origin/main` (post-#560) at the gate.

---

## 5. Test plan (sketch)
- `internal/diag`: ring append/trim/snapshot; subscribe fan-out + drop-on-full under a stalled subscriber; **`Secret` redaction holds on the ring path**; fan-out logger writes byte-identical output to the file sink vs the non-teed logger; `diag` has no `internal/*` import (leaf assertion).
- `internal/server`: `LogStream` producer primes snapshot then streams live; bounded/non-blocking under a stalled subscriber; auth gate `AuthRequired`; server package does not import `internal/diag`.
- `ui/tui`: `showLogsWindow` raises-not-duplicates (reuses the analysis collision guard); `appendLogLine` builds a `kindLog` `transcriptRecord` and goes through `sw.transcript.add` (assert a subsequent `render()`/search keeps the log lines — the Defect-A regression test); arrival-order append + level→color mapping (`colorInfo`/`colorTool`/`colorError` via the record `color` field); `[local]/[daemon]` tag only in remote mode; cap is the transcript `trim()` at `limit` (oldest dropped, newest kept); `Focus("logs")` focuses `sw.history` not nil (Defect B); the `windowKind` discriminator leaves all existing `readOnly` behavior intact and gates the banner; mock SSE interlace test merging two arrival-ordered streams; goroutine cancelled on close.

---

## 6. Open questions (implementer decides + documents)
1. **Reconnect catch-up**: on SSE reconnect, re-prime daemon tail via `GET /api/logs?tail=N`, or accept a small gap (best-effort tail)? Lean: accept the gap; add `?tail=N` if cheap.
2. **`daemon.log` raw stdout** (stdlib `log.Printf`, webapi warnings detached to `~/.gogent/daemon.log`): capture too, or structured `diag` path only? Overlaps #560's stdlib-log redirect. Lean: **structured `diag` path only** for this issue; raw-stdout capture is a follow-up once #560 lands.
3. **Ring size 2000 / transcript `limit` ~1000** — confirm against memory budget; tune if needed. Note the ring (reopen history) and the transcript limit (in-session display) are independent caps; ring ≥ limit so a reopen can refill the view.
4. **Warn color**: reuse `colorTool` (yellow) via the record `color` field, or introduce dedicated `roleWarn`/`roleInfo`/`roleError` color *roles* (theme-recolor-on-render)? Lean: `colorTool` initially; roles are a small follow-up if live re-theming of old log lines matters.
5. **`windowKind` vs `readOnly bool`**: introduce the kind enum with `readOnly` derived (recommended, keeps all call sites), or thread a separate flag? Lean: derived enum.
6. **`kindLog` vs reusing `kindSystem`**: a dedicated kind (recommended) reads cleaner and enables a later level/source filter; `kindSystem` reuse is lighter but lumps logs with system messages under the kind filter. Lean: dedicated `kindLog`.
7. **Source/level filter UI** (`[local]`-only / `[daemon]`-only / level filter): deferred per the issue; the structured `Record` + `kindLog` already carry level+source to make it cheap later.
8. **Trim-time yank**: the transcript `trim()` ends with `render()`→`ScrollToBottom`, so a user scrolled up exactly when a trim fires is pulled to the tail — a rare, pre-existing characteristic shared with every live session window, not new to this feature. Acceptable; revisit only if logs prove far chattier than chat transcripts in practice.
