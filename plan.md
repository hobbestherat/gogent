# Issue #201 — Running-turn input controls (Stop / Queue / Interject)

## Goal
While a turn is RUNNING, replace the single idle **Send** button with three buttons
next to the prompt box: **Interject**, **Queue ⏎**, **■ Stop**. Enter still queues
(unchanged drain-on-idle). Remove the `experimental.inject_queued_input` flag so
interject becomes a per-message button action that is always available. Add the
typed commands to the command palette for discoverability.

## Design

### Input row, two states (ui/tui/session_window.go LayoutFn + new layoutInputRow)
- **Idle** (`!busy`): existing single Send button at the right; three running
  buttons hidden (zero bounds). Unchanged geometry (`input W=wd-10`, `Send` at
  `wd-9 W=8`).
- **Running** (`busy`): Send hidden; three buttons right-aligned on the input row
  in the order `[ Interject ] [ Queue ⏎ ] [ ■ Stop ]` (two send-actions grouped,
  destructive Stop far right). The input box shrinks to the space left of them.
- Width degradation: full labels when they leave at least `minInputWidth` cells for
  the input; otherwise compact glyph labels `»` / `⏎` / `■`. Buttons clip their own
  caption as a last resort.
- Button width helper: `buttonWidth(label) = StringWidth(label)+4` (the `[ … ]`
  frame, matching the existing 8-wide "Send").
- Layout is recomputed every draw (turbotv runs LayoutFn at draw start), so the
  busy↔idle swap follows the existing redraw with no extra plumbing.

### Button behaviour (wired in newSessionWindow after `submit` is defined)
- **Queue button** → `submit` (identical to Enter: queue current text, clear box).
- **Interject button** → `sw.interject()`: inject current input text into the
  running turn now via `OnInject` → `UserSession.InjectUserNote`, then clear the
  box. No-op (and visually greyed) when input is empty; no-op when not busy.
- **Stop button** → `sw.stopTurn()` (cancel + clear queue), same as `/stop`.
- Interject disabled-on-empty: `interjectEnabled()` predicate; a draw wrapper greys
  the label when empty (mirrors `guardEffortSelect`), and `interject()` guards the
  action.
- Stop button rendered in `colorError` to separate it from the send-actions.

### Remove the experimental flag
- `internal/config/config.go`: delete `ExperimentalConfig.InjectQueuedInput`
  (keep `ExperimentalConfig`/`Supervisor`). Old configs that still set
  `experimental.inject_queued_input` load fine (unknown JSON keys are ignored).
- `ui/tui/tui.go`: delete `Handlers.InjectQueuedInputEnabled`; update `OnInject`
  doc (now reached via the Interject button, always wired).
- `cmd/main.go`: delete the `InjectQueuedInputEnabled` wiring; keep `OnInject`.
- `ui/tui/session_window.go`: delete `injectEnabled()`; `enqueue()` always uses the
  drain-on-idle path.
- `internal/gogent/gogent.go`: the config field is gone, so enable mid-turn
  injection unconditionally — `SetInjectQueuedInput(true)`. Mid-turn injection is
  now always the Interject button's job, so the backend always drains a pending
  note for the root agent. (Minimal backend touch; the gate/setter and its unit
  tests stay intact.)

### Command palette (ui/tui/command_palette.go)
Add to the Session group, acting on the active session via `handleSlashCommand`:
`/stop`, `/clearqueue`, `/goal`, `/markdown` (redraw after running).

## Tests (GLM partner writes these)
Button visibility per idle/busy; Enter→queue; Queue/Interject/Stop actions;
Interject disabled on empty input; old config with `experimental.inject_queued_input`
still loads. Key targets: `sw.busy`, `sw.layoutInputRow`, the four `*Button` fields
and their bounds, `sw.interject()`, `sw.interjectEnabled()`, `sw.stopTurn()`,
`sw.enqueue()`.

### Pre-existing tests touched by the flag removal (necessary, not new tests)
- `ui/tui/input_queue_test.go`: delete `TestInjectModeDoesNotDrainOnIdle` (it
  exercises the removed inject-mode/`InjectQueuedInputEnabled` handler).
- No backend test changes: gogent now passes `true`, so `task_loop_test.go`'s
  `SetInjectQueuedInput` tests still compile and pass.
