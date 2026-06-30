package server

import (
	"fmt"
	"net/http"

	"github.com/hobbestherat/webapi"
)

// messagesSvc groups the message handlers (blocking send + SSE stream).
type messagesSvc struct{ s *Server }

// Send handles POST /sessions/:id/messages — the non-blocking path that
// dispatches the full ReAct loop on a daemon-owned goroutine and returns the new
// turn's id immediately (issue #481). The turn runs to completion regardless of
// whether the client stays connected; its progress and final answer arrive over
// the SSE hub. A second concurrent turn on the same session is rejected with 409.
// (Returned with 200 rather than a literal 202 — the framework cannot set a custom
// status for a JSON body; see design §2.)
func (svc messagesSvc) Send(r *http.Request, req sendMessageRequest, id string) (interface{}, error) {
	if req.Message == "" {
		return nil, webapi.NewHTTPError(http.StatusBadRequest, "message is required")
	}
	us := svc.s.g.GetUserSession(id)
	if us == nil {
		return nil, webapi.NewHTTPError(http.StatusNotFound, "session not found")
	}

	release, ok := svc.s.markBusy(id)
	if !ok {
		return nil, webapi.NewHTTPError(http.StatusConflict, "session is busy")
	}

	// The busy gate (and any plan-mode restore) must be tied to the TURN's
	// completion, not this handler's return: the turn now outlives the request, so
	// releasing on handler return would drop the gate while the turn is still in
	// flight (letting a second send wrongly start) and would restore plan mode
	// before the turn even runs. onDone fires when the dispatched goroutine ends.
	planMode := req.Mode == "plan"
	if planMode {
		svc.s.g.SetPlanMode(id, true)
	}
	onDone := func() {
		if planMode {
			svc.s.g.SetPlanMode(id, false)
		}
		release()
	}

	var turnID string
	var err error
	if req.Subtask || req.Agent != "" {
		// A custom command's agent/subtask override routes the prompt through a
		// daemon-side sub-agent instead of the normal root turn (issue #403). The
		// sub-agent's result is surfaced as the session's SessionEventFinal by the
		// dispatch goroutine (no server-side shim needed).
		turnID, err = svc.s.g.DispatchCommandSubtask(id, req.Agent, req.Message, onDone)
	} else {
		turnID, err = svc.s.g.DispatchMessage(id, "root", req.Message, req.Model, req.Effort, req.Thinking, onDone)
	}
	if err != nil {
		onDone() // dispatch failed before the goroutine started; release the gate
		return nil, webapi.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return acceptedView{TurnID: turnID}, nil
}

// Stream handles POST /sessions/:id/messages/stream — returns immediately with
// SSE headers, then forwards every SessionEvent the turn produces until the turn's
// dispatch goroutine completes. It is a thin async-dispatch wrapper kept for
// backwards compatibility (issue #481): the turn is dispatched on a daemon-owned
// goroutine (context.Background()), so a client disconnect stops the streaming but
// does NOT cancel the turn and does NOT release the busy gate — the turn runs to
// completion and a reconnecting client recovers the result via the hub/transcript.
// 409 if already busy.
func (svc messagesSvc) Stream(r *http.Request, req sendMessageRequest, id string) (interface{}, error) {
	if req.Message == "" {
		return nil, webapi.NewHTTPError(http.StatusBadRequest, "message is required")
	}
	us := svc.s.g.GetUserSession(id)
	if us == nil {
		return nil, webapi.NewHTTPError(http.StatusNotFound, "session not found")
	}

	release, ok := svc.s.markBusy(id)
	if !ok {
		return nil, webapi.NewHTTPError(http.StatusConflict, "session is busy")
	}

	// Subscribe before kicking off the turn so no early events are missed.
	sub, unsub := svc.s.hub.subscribeSession(id)
	_ = us // session existence already checked above

	// The busy gate (and any plan-mode restore) is tied to the TURN's completion
	// via onDone, NOT to the producer's lifetime: the turn outlives the SSE
	// connection now, so releasing on producer return would drop the gate while the
	// turn is still running (a reconnecting client could wrongly start a second
	// turn). onDone fires when the dispatched goroutine ends. It also closes
	// turnDone, the producer's authoritative "no more events" signal.
	planMode := req.Mode == "plan"
	if planMode {
		svc.s.g.SetPlanMode(id, true)
	}
	turnDone := make(chan struct{})
	onDone := func() {
		if planMode {
			svc.s.g.SetPlanMode(id, false)
		}
		release()
		close(turnDone)
	}

	var err error
	if req.Subtask || req.Agent != "" {
		_, err = svc.s.g.DispatchCommandSubtask(id, req.Agent, req.Message, onDone)
	} else {
		_, err = svc.s.g.DispatchMessage(id, "root", req.Message, req.Model, req.Effort, req.Thinking, onDone)
	}
	if err != nil {
		onDone() // releases the gate (and closes turnDone, harmless — no producer)
		unsub()
		return nil, webapi.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	return &webapi.EventStreamResponse{
		Producer: func(stream webapi.EventStream) error {
			// Unsubscribe when the producer ends (turn done or client gone). The busy
			// gate is released by onDone, independently, when the turn itself
			// completes — so it is intentionally NOT released here.
			defer unsub()

			for {
				select {
				case <-stream.Context().Done():
					return nil // client gone — the turn keeps running on the daemon
				case <-turnDone:
					// The dispatch goroutine has finished, so every event it will ever
					// emit — including a plan-mode SessionEventPlan emitted AFTER the
					// final, and trailing usage events — is already buffered on the
					// subscription (terminal events are delivered with a blocking send
					// before onDone runs). Flush them, then close the stream. Stopping
					// on turn completion rather than on a guessed terminal event also
					// avoids missing the unstamped Plan event the old terminal-match
					// logic skipped.
					svc.drainRemaining(sub, stream)
					return nil
				case te := <-sub:
					if err := stream.Send(sessionSSE(te.ev, id)); err != nil {
						return fmt.Errorf("send message event: %w", err)
					}
				}
			}
		},
	}, nil
}

// drainRemaining flushes any buffered events after the turn finished, so the
// client sees the terminal final/error/usage events before the stream closes.
func (svc messagesSvc) drainRemaining(sub <-chan taggedEvent, stream webapi.EventStream) {
	for {
		select {
		case te := <-sub:
			_ = stream.Send(sessionSSE(te.ev, te.sessionID))
		default:
			return
		}
	}
}
