package gogent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"gogent/internal/agent"
)

// Async turn dispatch (issue #481).
//
// The daemon/HTTP path must not tie an agent turn's lifetime to the client
// connection: when a TUI disconnects, Go's HTTP server cancels the request
// context, and — if that context reaches the model call — the in-flight turn is
// aborted mid-stream. The Dispatch* methods below decouple the two. Each mints a
// turn id, then runs the existing synchronous turn entrypoint in a daemon-owned
// goroutine under context.Background() (carrying the turn id), so the turn runs to
// completion regardless of any client. Progress and the final answer reach clients
// over the session observer → SSE hub exactly as before; the only difference is the
// HTTP handler returns immediately with the turn id instead of blocking.
//
// The synchronous methods (SendMessageToSessionWithModelAndEffort,
// ExecuteApprovedPlan, RunCommandSubtask) are retained unchanged — the embedded
// path calls them directly under context.Background() on its own goroutine and is
// already correct. These wrappers bring the daemon path to the same model.
//
// onDone is invoked once the turn goroutine returns (success or error). The server
// uses it to release the busy gate (and restore plan mode) on turn completion
// rather than on handler return, so the session stays busy for the full turn even
// across a client disconnect. Stop is unaffected: StopAgent → agent.Cancel()
// cancels the loop's own child context independently of the parent, so a turn
// launched under context.Background() can still be stopped.

// mintTurnID returns a process-unique, URL-safe turn id (issue #481). It uses
// crypto/rand hex to honour the stdlib-only / no-new-deps constraint (no ULID
// dependency); on the practically-impossible failure of the system RNG it falls
// back to a nanosecond timestamp so a turn is never dispatched without an id.
func mintTurnID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "turn_" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return "turn_" + hex.EncodeToString(b[:])
}

// DispatchMessage runs a normal root turn asynchronously and returns its turn id
// immediately (issue #481). The session and agent are validated synchronously so a
// missing session/agent is reported to the caller (and onDone is not consumed)
// before any goroutine starts; the turn itself then runs under context.Background()
// in a daemon-owned goroutine. The final answer and any error reach clients as
// SessionEventFinal/SessionEventError via the session observer, so there is no HTTP
// response to write — this is fire-and-forget from the caller's perspective.
func (g *Gogent) DispatchMessage(sessionID, agentID, message, modelName, effort, thinking string, onDone func()) (turnID string, err error) {
	us := g.GetUserSession(sessionID)
	if us == nil {
		return "", &SessionNotFoundError{ID: sessionID}
	}
	if us.GetAgent(agentID) == nil {
		return "", &SessionNotFoundError{ID: agentID}
	}
	turnID = mintTurnID()
	ctx := agent.WithTurnID(context.Background(), turnID)
	go func() {
		if onDone != nil {
			defer onDone()
		}
		defer recoverTurn(us, turnID)
		// runLoop is the single source of the root turn's terminal event: it emits
		// SessionEventFinal on success and SessionEventError on every error path
		// (model failure, cancellation, panic-in-loop). So the returned error is NOT
		// re-emitted here — doing so would surface a duplicate error event for one
		// failure. The only errors that escape runLoop without an emit are the
		// pre-loop session/agent-vanished cases, which are validated synchronously
		// above; a session destroyed in the tiny window after that has no client
		// left to receive an event anyway. (recoverTurn still covers a panic in the
		// synchronous entrypoint, before runLoop's own recovery is armed.)
		_, _ = g.SendMessageToSessionFull(ctx, sessionID, agentID, message, modelName, effort, thinking)
	}()
	return turnID, nil
}

// DispatchApprovedPlan runs an approved plan as a turn asynchronously and returns
// its turn id immediately (issue #481), the async wrapper around
// ExecuteApprovedPlan. It validates synchronously that the session exists and has a
// plan awaiting approval, so "no plan" is reported to the caller (400) rather than
// lost in the goroutine; the plan turn then runs under context.Background().
func (g *Gogent) DispatchApprovedPlan(sessionID, agentID string, onDone func()) (turnID string, err error) {
	us := g.GetUserSession(sessionID)
	if us == nil {
		return "", &SessionNotFoundError{ID: sessionID}
	}
	if us.PendingPlan() == "" {
		return "", fmt.Errorf("no plan awaiting approval in session %s", sessionID)
	}
	turnID = mintTurnID()
	ctx := agent.WithTurnID(context.Background(), turnID)
	go func() {
		if onDone != nil {
			defer onDone()
		}
		defer recoverTurn(us, turnID)
		// runLoop owns the terminal event for the plan-execution turn (see
		// DispatchMessage): the returned error is not re-emitted here to avoid a
		// duplicate SessionEventError. recoverTurn still contains a pre-runLoop panic.
		_, _ = g.ExecuteApprovedPlan(ctx, sessionID, agentID)
	}()
	return turnID, nil
}

// DispatchCommandSubtask runs a custom command's agent/subtask override as a
// one-shot sub-agent turn asynchronously and returns its turn id immediately
// (issue #481), the async wrapper around RunCommandSubtask. Because a sub-agent's
// own final answer is not surfaced as the session's final, the goroutine publishes
// the result (or error) as the session's SessionEventFinal/SessionEventError via
// the observer — the role the server's former runCommandOverride hub.deliver shim
// played — so a connected or reconnecting client renders the answer and idles.
func (g *Gogent) DispatchCommandSubtask(sessionID, agentID, message string, onDone func()) (turnID string, err error) {
	us := g.GetUserSession(sessionID)
	if us == nil {
		return "", &SessionNotFoundError{ID: sessionID}
	}
	turnID = mintTurnID()
	ctx := agent.WithTurnID(context.Background(), turnID)
	go func() {
		if onDone != nil {
			defer onDone()
		}
		defer recoverTurn(us, turnID)
		result, runErr := g.RunCommandSubtask(ctx, sessionID, agentID, message)
		if runErr != nil {
			us.EmitError(turnID, runErr)
			return
		}
		us.EmitFinal(turnID, result)
	}()
	return turnID, nil
}

// recoverTurn contains a panic from a dispatch goroutine and surfaces it as the
// session's error event (issue #481, mirroring the embedded path's per-session
// panic containment for issue #8). runLoop has its own recovery, but it is armed
// only once the loop is running — a panic in the synchronous entrypoint before
// then (model-config selection, buildConnection, ThoughtTrain setup,
// checkpoints.BeginTurn) would otherwise escape the goroutine and crash the daemon
// process. Used as `defer recoverTurn(us, turnID)`.
func recoverTurn(us *agent.UserSession, turnID string) {
	if r := recover(); r != nil {
		us.EmitError(turnID, fmt.Errorf("turn panicked: %v", r))
	}
}
