package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hobbestherat/webapi"
	"gogent/internal/agent"
)

// messagesSvc groups the message handlers (blocking send + SSE stream).
type messagesSvc struct{ s *Server }

// Send handles POST /sessions/:id/messages — the blocking path that runs the
// full ReAct loop and returns only the final answer. A second concurrent turn on
// the same session is rejected with 409.
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
	defer release()

	// Plan mode: toggle on for this request when requested, restoring after.
	if req.Mode == "plan" {
		svc.s.g.SetPlanMode(id, true)
		defer svc.s.g.SetPlanMode(id, false)
	}

	// A custom command's agent/subtask override routes the prompt through a
	// daemon-side sub-agent instead of the normal root turn (issue #403).
	if req.Subtask || req.Agent != "" {
		result, err := svc.runCommandOverride(r.Context(), id, req.Agent, req.Message)
		if err != nil {
			return nil, webapi.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return messageView{Role: "assistant", Content: result}, nil
	}

	resp, err := svc.s.g.SendMessageToSessionWithModelAndEffort(r.Context(), id, "root", req.Message, req.Model, req.Effort)
	if err != nil {
		return nil, webapi.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return messageView{Role: "assistant", Content: resp.Content}, nil
}

// runCommandOverride runs an agent/subtask custom-command invocation on the
// daemon: it spawns the one-shot sub-agent (its run streams to the session's SSE
// subscribers via the installed observer) and then surfaces the result as the
// session's final answer event, so a streamed or globally-subscribed remote
// window renders the answer and idles — matching the embedded path's
// SessionEventFinal. It returns the result for the blocking response body.
func (svc messagesSvc) runCommandOverride(ctx context.Context, id, agentName, message string) (string, error) {
	result, err := svc.s.g.RunCommandSubtask(ctx, id, agentName, message)
	if err != nil {
		return "", fmt.Errorf("run command subtask: %w", err)
	}
	svc.s.hub.deliver(id, agent.SessionEvent{Type: agent.SessionEventFinal, Text: result})
	return result, nil
}

// Stream handles POST /sessions/:id/messages/stream — returns immediately with
// SSE headers, then emits every SessionEvent the loop produces until final/error.
// Client disconnect cancels the loop (SSE context). 409 if already busy.
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

	planMode := req.Mode == "plan"
	if planMode {
		svc.s.g.SetPlanMode(id, true)
	}

	modelName := req.Model
	effort := req.Effort
	message := req.Message
	agentName := req.Agent
	subtask := req.Subtask

	return &webapi.EventStreamResponse{
		Producer: func(stream webapi.EventStream) error {
			// The busy claim, plan-mode restore and unsubscribe MUST be tied to the
			// producer's lifetime, not the handler method's: webapi invokes Producer from
			// processResults AFTER the handler returns (handlerValue.Call has already run
			// the handler's defers by then). A handler-level `defer release()` would drop
			// the concurrency claim before the streamed model loop even starts, so a
			// second turn would wrongly get 200 mid-stream instead of 409 (issue #353,
			// busy gate). Releasing here holds it for the whole streamed turn — and
			// unsubscribing here, rather than at handler return, keeps the subscription
			// live so the turn's events actually reach the stream. A returned
			// EventStreamResponse always has its Producer invoked by writeEventStream (the
			// only skip is a non-flushable writer, which 500s SSE entirely), so the claim
			// cannot leak in any functioning stream.
			defer release()
			defer unsub()
			if planMode {
				defer svc.s.g.SetPlanMode(id, false)
			}

			// Run the model loop off the SSE goroutine so the stream context
			// (cancelled on client disconnect) aborts in-flight model work.
			done := make(chan struct{})
			go func() {
				defer close(done)
				if subtask || agentName != "" {
					// agent/subtask: spawn the sub-agent and emit its final answer (its
					// run + the final event both reach this stream's subscriber).
					_, _ = svc.runCommandOverride(stream.Context(), id, agentName, message)
					return
				}
				_, _ = svc.s.g.SendMessageToSessionWithModelAndEffort(
					stream.Context(), id, "root", message, modelName, effort)
			}()

			for {
				select {
				case <-stream.Context().Done():
					return nil // client gone
				case <-done:
					// Drain any events emitted just before completion, then stop.
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
