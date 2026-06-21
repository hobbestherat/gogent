package server

import (
	"fmt"
	"net/http"

	"github.com/hobbestherat/webapi"
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

	resp, err := svc.s.g.SendMessageToSessionWithModelAndEffort(r.Context(), id, "root", req.Message, req.Model, req.Effort)
	if err != nil {
		return nil, webapi.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return messageView{Role: "assistant", Content: resp.Content}, nil
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
	defer unsub()
	_ = us // session existence already checked above

	if req.Mode == "plan" {
		svc.s.g.SetPlanMode(id, true)
		defer func() {
			svc.s.g.SetPlanMode(id, false)
			release()
		}()
	} else {
		defer release()
	}

	modelName := req.Model
	effort := req.Effort
	message := req.Message

	return &webapi.EventStreamResponse{
		Producer: func(stream webapi.EventStream) error {
			// Run the model loop off the SSE goroutine so the stream context
			// (cancelled on client disconnect) aborts in-flight model work.
			done := make(chan struct{})
			go func() {
				defer close(done)
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
