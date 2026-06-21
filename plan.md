# Issue #187 — "Some tool state gets stuck in running"

## Root cause

A tool entry is stuck "running" whenever a `SessionEventToolCall` is emitted with
no matching `SessionEventToolResult`, **or** when the TUI cannot pair an emitted
result back to the call that started it. Two concrete gaps:

1. **TUI pairing (primary).** `SessionWindow` tracks in-flight tools with a single
   `pendingTool *transcriptRecord`. A concurrent batch (`runToolCallsConcurrent`,
   used for read-only and sub-agent-spawn batches) emits N `ToolCall` events
   back-to-back; each `beginToolCall` overwrites `pendingTool`, so only the last
   call is tracked. The first `finishToolCall` sets `pendingTool = nil`; the rest
   create *fresh* entries. The N-1 earlier entries stay "(running...)" forever.
   Results are matched by nothing — not even tool name.

2. **Backend serial-path panic.** `runToolCallsSerial` has no per-call panic
   recovery. A tool that panics emits its `ToolCall`, then the panic unwinds to
   the outer `runLoop` recover, which emits an Error and returns — the started
   call never gets a `ToolResult`. (The concurrent path already recovers per
   task; serial did not.)

## Design

Pair calls and results by a **stable id** carried on the event, and guarantee
every started call emits exactly one terminal result.

### Backend (`internal/agent/user_session.go`)
- Add `CallID string` to `SessionEvent`, documented as the stable id pairing a
  `ToolCall` with its `ToolResult`.
- `toolEventID(call, step, idx)`: use `call.CallID` when the model supplied one
  (native tool-calling); otherwise synthesize `tool#step.idx`, unique within a
  turn so even the fallback JSON path (which has no CallID) and repeated tool
  names still pair 1:1.
- `runAndEmitResult(...)`: single helper that executes one call with panic
  recovery and emits the paired `ToolResult` (carrying the id). On panic it
  still emits a terminal error result and returns the tool-result message.
- `emitToolCall(...)`: emits the `ToolCall` with the id.
- Rewire both `runToolCallsSerial` (now panic-safe per call) and
  `runToolCallsConcurrent` (ToolCall emitted up-front so all show running
  immediately; result via the shared helper) through these, so every started
  tool reaches a terminal state on success, error, panic, or cancellation.

### TUI (`ui/tui/session_window.go`)
- Replace `pendingTool *transcriptRecord` with
  `pendingTools map[string]*transcriptRecord` keyed by the event id.
- `beginToolCall(id, name, args)` stores the record under id; `finishToolCall(id,
  name, result)` looks it up, flips it to "(done)", and deletes it. Unknown/empty
  id falls back to a fresh entry (prior behaviour), so legacy events still render.
- `failPendingTools(state)`: sweep any still-running entries to a terminal
  "(state)" header. Called on the busy→idle edge in `setBusy(false)` (covers
  final, error, cancel, stop, budget) so nothing is left "running" even if a
  result event never arrives.
- Thread `ev.CallID` through `apply` into begin/finish.

## Tests (GLM writes these)
- agent: a panicking tool (serial **and** concurrent) emits a terminal
  ToolResult carrying the call id; every ToolCall in a concurrent batch has a
  matching ToolResult with the same id; a cancelled loop strands no started call.
- tui (`newTestWorkbench`): concurrent begin(a)/begin(b)/finish(a)/finish(b)
  leaves no entry "running"; `failPendingTools` sweeps an orphaned running entry
  on busy→idle.
