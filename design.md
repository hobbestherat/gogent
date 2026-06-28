# Design — Persistent "Logs" window surfacing tool/daemon logs (issue #562)

Branch: `pair1/persistent-logs-window-surface-tool-daem`
Scope: **gogent only**. No turbotui change, no new deps, no `go.mod` bump (see Gate 4).
Serializes **after #560** (which established the shared-file-sink / stdlib-log-redirect discipline) and after #546.

---

## 1. What we are building (the ask, restated)

A persistent, **non-modal** "Logs" window in the TUI that:

1. Surfaces gogent's structured diagnostics (the `internal/diag` / `log/slog` stream) **in-app**, well-formatted (timestamp · level · message), wrapped, and **level-colored** (info/warn/error).
2. **Stays open** alongside session windows — tileable, cycleable, focusable, movable, minimizable — *not* a blocking modal. Reopening **raises** the existing window (no duplicate).
3. **Live-follows the tail**: auto-scrolls when parked at the bottom, does **not** yank focus/scroll when the user has scrolled up.
4. In **remote/attach mode**, **interlaces** local (client) logs with **daemon (remote)** logs in one view, each line tagged `[local]` / `[daemon]`, ordered chronologically.

The diagnostics today are write-only to sinks the user can't see in-app: headless→stderr (`internal/diag/logger.go:46`), TUI→`~/.gogent/gogent.log` (`cmd/main.go:113`), daemon→`gogent.log` + detached `daemon.log`. There is **no in-memory buffer and no UI surface**, and in remote mode the daemon's logs never leave the remote host. This feature adds an in-memory **tee** + an observable broadcast + a UI window + an SSE bridge for the daemon side.

---

## 2. Architecture overview

```
                 ┌──────────────────────── internal/diag ────────────────────────┐
   log call ───► │  *diag.Logger (slog)                                           │
                 │     └─ fanoutHandler ──┬─► TextHandler → file/stderr (UNCHANGED)│
                 │                        └─► ringHandler → *diag.Ring (NEW)       │
                 │                                  • bounded []Record             │
                 │                                  • Subscribe() (<-chan, cancel) │
                 └───────────────────────────────────┬───────────────────────────┘
                                                      │
        embedded/TUI ────────────────────────────────┤ (Workbench holds the local ring)
                                                      │
        daemon (cmd/daemon.go) ──► same ring ─► internal/server  GET /api/logs/stream (SSE)
                                                      │                    ▲
                                                      │                    │ AuthRequired
   remote client (ui/tui) ────────────────────────────┘   StreamLogs() ───┘ + reconnect/backoff
                                                      │
                          ┌───────────────────────────┴───────────────────────────┐
                          │  ui/tui/logs_window.go (NEW)  — singleton non-modal     │
                          │   • tv.Window + tv.NewWindowLayer (Modal=false)         │
                          │   • body = tv.TextView (Wrap, follow)                   │
                          │   • merges local ring records + [daemon] SSE records    │
                          │   • chronological interlace, level color, redacted      │
                          └─────────────────────────────────────────────────────────┘
```

Single source of truth for "what a log line is": a small structured `Record` defined in `internal/diag`, produced through the existing `diag.Logger` slog path so **Secret redaction (`internal/diag/secret.go`) holds on every captured/streamed line**.

---

## 3. gogent changes — exact files & functions

### 3.1 `internal/diag/ring.go` (NEW) — bounded, observable log ring

Mirror the hub broadcast pattern (`internal/server/hub.go:24,189,218`) but local and log-typed.

```go
// Record is one captured diagnostic line: enough to color by level and
// interlace by time, with the fully-formatted (already-redacted) text.
type Record struct {
    Time  time.Time
    Level slog.Level   // Info/Warn/Error → window color
    Text  string       // formatted "msg key=val ..." (redacted), no trailing \n
}

type Ring struct {
    mu   sync.Mutex
    buf  []Record                       // bounded history (rolling), cap = size
    size int
    subs map[chan Record]struct{}       // live subscribers
}

func NewRing(size int) *Ring
func (r *Ring) append(rec Record)                       // history + non-blocking fanout
func (r *Ring) Snapshot() []Record                      // history copy (prime on open)
func (r *Ring) Subscribe() (<-chan Record, func())      // buffered chan + unsubscribe
```

- `append`: under lock, push to `buf` (trim oldest past `size`), then non-blocking `select { case ch<-rec: default: }` to each subscriber (drop-on-full — best-effort live, never stall the logger / agent loop). Same discipline as `hub.deliver`.
- `Subscribe`: buffered channel (e.g. 256), returns unsubscribe func; **does not** auto-prime — the window primes from `Snapshot()` then subscribes, under a short guard to avoid a gap/dup at the seam (see §3.6).
- Ring size: **2000** records (the issue notes the 50-line notification ring is far too small for logs; logs warrant larger; window display itself caps at ~1000 lines).

### 3.2 `internal/diag/logger.go` — tee the logger into the ring

The `Logger` wraps `*slog.Logger` over a single `TextHandler`. Add a **fan-out `slog.Handler`** so the existing sink is untouched and a second handler captures structured records into the ring.

- New `fanoutHandler` (private) implementing `slog.Handler`: `Enabled`/`Handle`/`WithAttrs`/`WithGroup` delegate to **both** an inner `TextHandler` (file/stderr, exactly as today) and a `ringHandler`.
- `ringHandler.Handle(ctx, rec)`: render the record to a line via a *throwaway* `slog.NewTextHandler` writing into a pooled `bytes.Buffer` (this resolves `LogValuer`, so `Secret` is redacted identically to the file sink), capture the formatted text, strip timestamp/level prefix duplication (or keep the raw slog text — we store `rec.Level` and `rec.Time` separately and the message+attrs as `Text`), then `ring.append(Record{rec.Time, rec.Level, text})`.
- New constructors (additive; existing `New`/`Stderr`/`NewFile` unchanged so headless/embedded behavior is byte-for-byte the same when no ring is wired):
  ```go
  func NewWithRing(w io.Writer, ring *Ring) *Logger        // tee: TextHandler(w) + ring
  func NewFileWithRing(path string, ring *Ring) (*Logger, error)
  ```
- Redaction: preserved because both branches go through slog attr resolution; `Secret.LogValue()`/`String()` already redact. A unit test asserts a `Secret` value never appears in a `Ring` record.

Why a handler tee, not an `io.MultiWriter`: a multi-writer would force us to **re-parse** `level=INFO`/timestamps out of the text format to color lines. The handler tee keeps `slog.Level` and `time.Time` first-class (the issue's RECOMMENDED "structured records over raw file lines"), enabling color + future filtering with no string parsing.

### 3.3 Wiring the ring at startup (where the tee is installed)

The ring is a **client-side capability** wherever a Logs window can exist, and a **daemon-side capability** to feed SSE.

- `cmd/main.go:113` (embedded TUI): create `ring := diag.NewRing(2000)`, build the logger with `diag.NewFileWithRing(gogent.log, ring)` instead of `NewFile`, `g.SetLogger(lg)`, and hand `ring` to the Workbench (new field on the TUI constructor / handlers struct). Headless path (`!*disableTUI` false) keeps `diag.Stderr()` — **unchanged**, no ring, no behavior change.
- `cmd/attach.go:245` (remote client TUI): same — tee the client logger (`diag.New(f)` → `diag.NewWithRing(f, ring)`) and pass `ring` to the Workbench. This is the `[local]` stream in remote mode.
- `cmd/daemon.go:380` (daemon): create a ring, build `diag.NewFileWithRing(gogent.log, ring)`, `g.SetLogger(lg)`, and pass the ring to the API server (new `server.Options.LogRing *diag.Ring`, consumed in `NewServer` at `internal/server/api.go:88`). This is the `[daemon]` stream surfaced over SSE.

The Workbench gains a `logRing *diag.Ring` field (nil-safe: if nil, the Logs window simply shows only remote/`[daemon]` records, or nothing locally — never panics).

### 3.4 `ui/tui/logs_window.go` (NEW) — the persistent non-modal window

Modeled on the monologue popup (`agent_monolog.go`: bare `*tv.Window` + `tv.NewWindowLayer`, tracked by a single Workbench field) rather than `SessionWindow` (no input/transcript/agent semantics needed).

State on `Workbench` (mirroring `monolog`/`monologWindow` at `tui.go:582-588`):
```go
logsLayer   *tv.Layer        // non-modal layer (nil when closed)
logsWindow  *tv.Window       // the frame (for re-clamp on sidebar width change)
logsView    *tv.TextView     // the scrolling body
logsCancel  func()           // ring/SSE unsubscribe(s); called on close
logsRecords []logLine        // merged, bounded (~1000) backing buffer for interlace
```

`func (w *Workbench) showLogsWindow()`:
1. **Raise-not-duplicate**: if `w.logsLayer != nil` → `w.desktop.RemoveLayer(layer); w.desktop.AddLayer(layer); w.desktop.SetFocus(w.logsView)` and return (same raise idiom as `Focus`, `tui.go:1872-1874`).
2. Build the frame: `win := tv.NewWindow("Logs", bounds, tui.LineSingle)`; enable `win.Minimizable = true`, `win.Maximizable = true`, `win.Resizable = true`, `ShowClose=true` (defaults), `applyWindowShadow(win)` (theme parity with other windows).
3. Body: `tv := tv.NewTextView("", bounds)` with `Wrap = true`, `follow = true` (default). `win.AddContent(tv)` + `win.Content.LayoutFn` to size the TextView to the content rect (same as `session_window.go:259-267`).
4. `win.OnClose = func(*tv.Window){ w.closeLogsWindow() }` → calls `logsCancel()`, removes layer, nils fields. Optional `q`/`Esc` when the view is focused (close key handler on the TextView).
5. **Prime history**: `for _, r := range w.logRing.Snapshot() { w.appendLogRecord(localSource, r) }`.
6. **Subscribe live (local)**: `ch, cancel := w.logRing.Subscribe()`; goroutine drains `ch` and marshals each onto the UI thread via `w.Post(func(){ w.appendLogRecord(localSource, r) })` (`tui.go:932` — thread-safe; never touch widgets off the UI thread).
7. **Remote**: if attached, also subscribe to the daemon SSE stream (see §3.7), appending with `daemonSource`.
8. `layer := tv.NewWindowLayer("logs", win)` (**Modal=false**, `turbotv/layer.go:99` — menu shortcuts of lower layers stay live, the window is non-blocking). `w.desktop.AddLayer(layer); w.desktop.SetFocus(tv)`.

`func (w *Workbench) appendLogRecord(src logSource, r diag.Record)` (UI thread only):
- Build the display line: `"<HH:MM:SS.mmm> [local|daemon] <LEVEL> <text>"` (the `[local]/[daemon]` tag is **omitted in embedded mode**, present in remote mode — keeps the local-only view clean).
- Color by level: `colorInfo` (cyan, `theme.go:25`), `colorWarn`, `colorError` (reuse the existing theme roles).
- **Interlace / auto-follow discipline** (the only subtle part):
  - Keep `logsRecords` bounded at ~1000 (trim oldest).
  - **Common path (in-order):** if the new record's `Time >= ` last appended time → `logsView.AddColored(line, color)` (append-only). TextView's own `follow` flag keeps it pinned to the bottom only when the user is parked there, and `scrollBy` re-enables follow only at the absolute bottom (`widget_textview.go:383`) — so we get auto-scroll-when-at-bottom and no-yank-when-scrolled-up **for free**, and selection is preserved.
  - **Out-of-order path (stream skew in remote mode):** if the record's time precedes the last shown line, insert into `logsRecords` at the correct sorted position and **rebuild** the TextView (`Clear()` + re-add) **preserving the prior follow/scroll state** (capture `follow`/`scrollY` before, restore after). This is rare (streams arrive ~ordered) so the expensive rebuild is the exception, not the per-line cost.

This satisfies: well-formatted + level-colored + wrapped + live-follow + chronological interlace, while keeping the hot path append-only.

### 3.5 Menu + keybinding — `ui/tui/tui.go` `settingsItems()` (~:1170, near Statistics :1209)

- Add `tv.NewMenuItem("&Logs…", func() { w.showLogsWindow() })` to the Settings menu, alongside Statistics. No handler-gating needed (the window is always available; with no ring it shows remote-only / empty).
- Optional chord **Ctrl+Shift+L**: add an action id to `command_palette.go` (mirror "Sub-agents" at :246) so `rebuildBindings` (`keybindings.go:239`) registers it → `showLogsWindow`. Reopen via chord also raises (same `showLogsWindow` entry point).

### 3.6 Tiling / cycle / layout — `ui/tui/tiling.go`, `ui/tui/tui.go`

The Logs window is a bare `*tv.Window` (not in `w.sessions`), like the monologue. To meet the acceptance ("tileable Ctrl+Shift+V/H/G, cycleable, focusable, movable, minimizable"):

- **Tiling** (`tiling.go:28 openWindows`, `:50 arrange`): append `w.logsWindow` to the returned `wins []*tv.Window` when open, so `tv.TileWindows` lays it out alongside session windows. Movable/minimizable/maximizable come from the `tv.Window` buttons (no extra work). Re-clamp on sidebar width change next to the monolog re-clamp (`tui.go:2207-2211`): `if w.logsWindow != nil { clampWindowToArea(w, w.logsWindow) }`.
- **Cycle** (`tui.go:1948 cycle` / `:1970 activeIDLocked` / `:1864 Focus`): `cycle`/`Focus` are keyed off session ids in `w.order`/`w.sessions`. Generalize minimally so the logs window participates: include it as a pseudo-entry in the cycle order and add a focus branch that raises `w.logsLayer` + `SetFocus(w.logsView)` when the cycle lands on it. Keep the SessionWindow-specific code (`ensureTranscript`, input focus) guarded so the logs branch never touches transcript/agent state. *(Regression-sensitive — see Gate 3.)*
- **Layout persistence** (`tui.go:2382 captureLayout` / `:2440 applyLayout`): **EXCLUDE** the Logs window from capture/restore initially (issue's recommendation; same treatment as `readOnly` analysis windows at `:2395`). It is a transient diagnostic surface; not restoring it on next launch is the expected behavior and avoids layout-schema churn.

### 3.7 Remote interlace — SSE endpoint + client consumer (CORE remote requirement)

**Server (`internal/server`)** — copy the `events.go` SSE shape exactly:

- `internal/server/api.go:88 NewServer` + `:157` endpoint table: register
  `{Path: "/api/logs/stream", Method: GET, Handler: ev.LogStream, AuthLevel: req}` (`req = webapi.AuthRequired`, same gate as `/events`). Optionally `{Path: "/api/logs", Method: GET, Handler: ev.LogsTail, AuthLevel: req}` (one-shot `?tail=N` catch-up from `ring.Snapshot()`).
- `internal/server/events.go` (or a new `logs.go`): `func (svc eventsSvc) LogStream(r *http.Request) (interface{}, error)` returning a `webapi.EventStreamResponse` whose `Producer` (a) primes from `ring.Snapshot()` then (b) drains `ring.Subscribe()` and `stream.Send(logSSE(rec))` until `stream.Context().Done()`, `defer cancel()`. SSE event name `"log"` (distinct from session-event type names and `"notification"`, so the client discriminates frames — same discrimination already used for `notification` at `events.go:70`).
- The server's ring is `Options.LogRing` (the daemon's teed ring from §3.3). Embedded mode leaves it nil → endpoint returns an empty/ended stream (or is simply never hit, since embedded TUI uses the in-process ring directly).
- Backpressure: ring fan-out is already non-blocking drop-on-full; the SSE producer is a normal subscriber. **Never stalls the daemon/agent.**

**Client (`ui/tui`)**:

- DTO near `api_client.go:312`:
  ```go
  type LogRecordDTO struct {
      Time  string `json:"time"`   // RFC3339Nano
      Level string `json:"level"`  // "INFO"|"WARN"|"ERROR"
      Text  string `json:"text"`   // already redacted server-side
  }
  ```
- `func (c *APIClient) StreamLogs(ctx) (<-chan LogRecordDTO, error)` — a sibling of `StreamEvents` (`api_client.go:867`) reusing `parseSSE` (`:927`), filtering frames named `"log"`. Same auth (token/password/loopback/SSH-tunnel `DialContext`) and **same reconnect/backoff** path as `StreamEvents` (0.5→1→2→5→10s cap, `remote_handlers.go:455`). On reconnect, re-prime via `?tail=N` or just resume live (gap acceptable for a diagnostic tail; note in Open Questions).
- `remote_handlers.go` (near the event consumer ~:294): when attached and the Logs window is open, drive `StreamLogs` and `w.Post(func(){ w.appendLogRecord(daemonSource, rec) })`. Convert `LogRecordDTO` → `diag.Record` (parse time, map level string → `slog.Level`). Lifecycle bound to the window (subscribe on open, cancel on close) so we don't hold an idle daemon stream when the window is closed.

---

## 4. The four design gates

### Gate 1 — Goal match (feature, no scope creep)
Delivers exactly the issue: a persistent **non-modal** Logs window, **in-memory ring tee** on `diag.Logger` (existing file/stderr sink **untouched**), **level-colored live-follow** TextView, and **`/api/logs/stream` SSE** interlacing `[local]`+`[daemon]` in remote mode. No filtering UI (explicitly deferred by the issue), no daemon.log raw-stdout capture (we capture the structured `diag` path only — the complete-but-heavier raw-stdout capture overlaps #560's stdlib-log redirect and is called out in Open Questions, not built). No layout persistence for the window (issue's recommendation). Nothing beyond the ask.

### Gate 2 — Usability
- **User drives it**: opens via `Settings ▸ Logs…` (and optional Ctrl+Shift+L). Keeps working in session windows while it stays open (non-modal layer — lower-layer menu shortcuts stay active, `layer.go:99`).
- **Right thing surfaced, not silent**: previously-invisible diagnostics are now visible in-app; remote daemon logs that used to be stranded on the host are streamed and clearly tagged `[daemon]` vs `[local]`.
- **Behaves as expected**: reopening **raises** (no duplicate); **history on open** then **live updates**; **auto-scroll only when parked at bottom**, **no yank** when scrolled up (TextView `follow` semantics); tileable/cycleable/movable/minimizable like any window; standard close button (+ optional `q`/`Esc` when focused).

### Gate 3 — No regressions
- **Embedded/headless unchanged**: `diag.New/Stderr/NewFile` are untouched; the tee is opt-in via new `*WithRing` constructors used only where a Logs window/SSE consumer exists. Headless still goes to stderr; TUI/daemon still write the same `gogent.log` line-for-line (the ring is an *additional* handler branch).
- **Redaction preserved**: captured/streamed lines go through the same slog attr resolution; `Secret` redacts on the ring path. Asserted by a new unit test.
- **Non-blocking / bounded**: ring fan-out and SSE sends are drop-on-full over bounded buffers (hub discipline). The logger/agent/daemon never stall on a slow/absent UI consumer.
- **Risk — cycle/Focus generalization** (`tui.go:1864/1948`): these are keyed to session ids today; adding a non-session window to the cycle order must not break session focus, `ensureTranscript`, or `activeIDLocked`. Mitigation: the logs branch is guarded and never enters SessionWindow-specific code; covered by a focused unit test on cycle order with the logs window open/closed. *If this proves invasive, fall back to implementing the window as a `readOnly` SessionWindow (like analysis windows) which already participates in tiling/cycle and is already layout-excluded — at the cost of more SessionWindow plumbing.*
- **Risk — out-of-order rebuild** could disturb scroll/selection: mitigated by capturing/restoring `follow`+`scrollY` around the rare rebuild; common path is append-only.
- **Tests stay green**: gofmt/build/vet/golangci-lint clean; `go test ./...` green (pre-existing `TestUserSessionSendMessage` 404 and load-induced `TestStopGracefulAndForced` flake noted as acceptable per the brief). Tests run **without `-race`** on the Pi5 per the dev gate.

### Gate 4 — Holistic, both repos / right seam
- **turbotui: NO change required.** `tv.Window` + `tv.NewWindowLayer` (non-modal) + `tv.TextView` (`Wrap`, `follow`, `AddColored`, `ScrollToBottom`) already provide everything: framed window with title/border/shadow/close/min/max, a scrolling color-capable wrapped text body, and the exact auto-follow-only-at-bottom semantics we need. A native `LogView` widget would be **redundant** — we explicitly avoid it (no go.mod bump, no cross-repo dependency). The seam is respected: gogent composes turbotui primitives; turbotui stays a generic toolkit with no log-domain knowledge.
- **Right place in gogent**: capture lives in `internal/diag` (where redaction already lives, so every sink is covered), the broadcast reuses the proven hub pattern, the SSE endpoint sits with the other event streams in `internal/server`, and the UI is one new `ui/tui/logs_window.go` plus minimal menu/tiling hooks. `ui/tui` stays free of forbidden imports (consumes `diag.Record`/`diag.Ring` types and the API client DTO only).
- **Downstream**: no new deps; stdlib-first (`log/slog`, `bytes`, `sync`, `time`). Coexists with #547 (internal/model). Builds **on top of** #560 (rebase onto current `origin/main` at the gate).

---

## 5. Test plan (sketch — implementation phase)
- `internal/diag`: ring append/trim/snapshot; subscribe fan-out + drop-on-full; **`Secret` redaction holds on the ring path**; fan-out logger writes identical bytes to the file sink as the non-teed logger.
- `internal/server`: `LogStream` producer primes snapshot then streams live; bounded/non-blocking under a stalled subscriber; auth gate = `AuthRequired`.
- `ui/tui`: `showLogsWindow` raises-not-duplicates; `appendLogRecord` append-only in-order + sorted rebuild out-of-order with follow/scroll preserved; level→color mapping; `[local]/[daemon]` tagging only in remote mode; cycle order includes/excludes the window correctly. Mock SSE interlace test merging two timestamped streams.

---

## 6. Open questions (implementer decides + documents)
1. **Reconnect catch-up**: on SSE reconnect, re-prime daemon tail via `GET /api/logs?tail=N`, or accept a small gap (diagnostic tail, not lossless)? Lean: accept the gap initially; add `?tail=N` if cheap.
2. **daemon.log raw stdout** (stdlib `log.Printf`, webapi warnings detached to `~/.gogent/daemon.log`): capture too (most complete) or structured `diag` path only? This overlaps #560's stdlib-log redirect. Lean: **structured `diag` path only** for this issue; raw-stdout capture is a follow-up once #560's redirect discipline is in place.
3. **Ring size**: 2000 records proposed (display cap ~1000). Confirm against memory budget.
4. **Cycle integration vs readOnly-SessionWindow fallback**: prefer the bare-window + minimal cycle/tiling hook; fall back to a `readOnly` SessionWindow if generalizing `cycle`/`Focus` proves too invasive (Gate 3 risk).
5. **Source filter UI** (`[local]`-only / `[daemon]`-only / level filter): deferred to a follow-up per the issue (the structured `Record` already carries level/source to make this cheap later).
6. **Embedded-mode tag**: omit `[local]/[daemon]` when not attached (cleaner) — assumed yes.
