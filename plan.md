# Fix plan — issue #227: assistant final answer dropped from the live TUI render

## Root cause (reproduced)

The four layers kloune proved correct (backend emit, live `addAssistant`, restore,
direct `apply`) are all genuinely correct. The defect is in the **live render
seam**, specifically the transcript **scroll/follow** behaviour for a freshly
appended record:

- `transcriptModel.renderOne` (the incremental add path used while NOT filtering)
  appends a record's entries and relies entirely on the backing TextView's
  `follow` flag to keep the bottom in view. `render()` (the full rebuild path)
  ends with `ScrollToBottom()`; `renderOne` does **not** — they are inconsistent.
- The TextView re-pins to the bottom on a content change (`touch`) **only while
  `follow` is true**. `follow` goes false the moment the user scrolls up (very
  common while watching a turn produce tool output / code).
- Once `follow` is false, every subsequently appended record — including the
  turn's **final answer** — is added to the model (so it is in `AllText`, the
  session file, and every unit test that asserts on content) but is **never
  scrolled into view**. To the user the agent "stopped replying"; the answer is
  present but off-screen.

Reproduced with a real `Workbench` + `Desktop.Redraw()` + `App.ReadCell`: with a
transcript taller than the viewport and `follow` off, a `SessionEventFinal`
lands in `AllText` but not on screen. Calling `ScrollToBottom()` **before** the
append (so the append's `touch` re-pins) makes it visible.

This is the same symptom class as #171 but distinct: #171 was an *empty* final
(nothing added); here the final is non-empty and added, but not revealed.

## Fix

1. **Reveal the answer (core fix).** `SessionWindow.addAssistant` re-anchors the
   transcript on the bottom *before* appending the answer record, so the turn's
   final answer is shown even if the user had scrolled up. Streaming events
   (thoughts, tool calls/results) deliberately do **not** re-anchor, so reading
   scrolled-up history mid-turn is undisturbed until the answer lands. Turn-ending
   errors (`addError` via `apply(SessionEventError)`) are revealed the same way.
   New `transcriptModel.scrollToBottom()` documents the "call before the add"
   contract.

2. **Make the delivery seam testable + observable.** Split the
   `EmitSessionEvent` post-callback body into `Workbench.deliverSessionEvent(id,
   ev) bool`, which runs the real lookup → apply → notify → overall-refresh on the
   UI thread and returns whether a window received it. `EmitSessionEvent` becomes
   `desktop.Post(func(){ w.deliverSessionEvent(id, ev) })`. This lets a test drive
   the exact live delivery logic without pumping the desktop post-queue (which has
   no headless drain). When no window is registered the event is counted via
   `noteUndeliveredEvent` / exposed by `UndeliveredEventCount()` instead of being
   silently skipped, turning suspect (1) (a momentarily-nil window dropping a
   final) into something a test/running session can observe.

## Tests (written by GLM, targeting these seams)

- Integration/repro (fails without fix): real `Workbench`, `openWindow`, fill the
  transcript past the viewport, disable follow (scroll up), `deliverSessionEvent`
  a `SessionEventFinal`, `desktop.Redraw()`, assert the answer text is on screen
  via `App.ReadCell`. Without the `addAssistant` reveal the answer is off-screen.
- `UndeliveredEventCount()` increments when an event is delivered for an unknown id.

## Notes on the turbotui post-queue

The desktop/app post-queue itself is robust (FIFO, re-entrant-safe, unbounded; no
drops) and the synchronous apply/drain path renders correctly, so no turbotui
change is required for this fix. The only turbotui gap is that the post-queue has
no public headless drain, which is why the fix exposes `deliverSessionEvent` as
the gogent-side seam rather than testing through `Post`.
