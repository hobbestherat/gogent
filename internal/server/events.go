package server

import (
	"fmt"
	"net/http"

	"github.com/hobbestherat/webapi"
	"gogent/internal/agent"
)

// eventsSvc groups the read-only SSE subscription handlers.
type eventsSvc struct{ s *Server }

// SessionEvents handles GET /sessions/:id/events — a live SessionEvent stream
// for one session. The handler subscribes to the hub and fans events out as SSE
// until the client disconnects. It returns 404 for an unknown session.
func (svc eventsSvc) SessionEvents(r *http.Request, id string) (interface{}, error) {
	if svc.s.g.GetUserSession(id) == nil {
		return nil, webapi.NewHTTPError(http.StatusNotFound, "session not found")
	}
	sub, unsub := svc.s.hub.subscribeSession(id)
	return &webapi.EventStreamResponse{
		Producer: func(stream webapi.EventStream) error {
			defer unsub()
			for {
				select {
				case <-stream.Context().Done():
					return nil
				case te := <-sub:
					if err := stream.Send(sessionSSE(te.ev, te.sessionID)); err != nil {
						return fmt.Errorf("send session event: %w", err)
					}
				}
			}
		},
	}, nil
}

// GlobalEvents handles GET /events — every session's events, tagged with the
// originating session id so a multi-session client can route them.
func (svc eventsSvc) GlobalEvents(r *http.Request) (interface{}, error) {
	sub, unsub := svc.s.hub.subscribeGlobal()
	return &webapi.EventStreamResponse{
		Producer: func(stream webapi.EventStream) error {
			defer unsub()
			for {
				select {
				case <-stream.Context().Done():
					return nil
				case te := <-sub:
					if err := stream.Send(globalSSE(te)); err != nil {
						return fmt.Errorf("send global event: %w", err)
					}
				}
			}
		},
	}, nil
}

// sessionSSE builds an SSE event for a single-session subscriber.
func sessionSSE(ev agent.SessionEvent, sessionID string) webapi.SSEvent {
	return webapi.SSEvent{Name: string(ev.Type), Data: marshalJSON(eventToView(ev, sessionID))}
}

// notificationEventName is the SSE event: name carried by a backend notification
// on the global stream (issue #358 §9). It is distinct from every agent
// SessionEvent type so a client can tell a notification from a session event by
// the event name alone (a notification frame's data is a NotificationEvent, not a
// globalEventView).
const notificationEventName = "notification"

// globalSSE builds an SSE event for the global subscriber. A notification frame
// (te.notif set) is emitted under notificationEventName with the NotificationEvent
// as its payload; every other frame is a session event tagged with its
// originating session id.
func globalSSE(te taggedEvent) webapi.SSEvent {
	if te.notif != nil {
		return webapi.SSEvent{Name: notificationEventName, Data: marshalJSON(*te.notif)}
	}
	return webapi.SSEvent{
		Name: string(te.ev.Type),
		Data: marshalJSON(newGlobalEventView(te.ev, te.sessionID)),
	}
}

// globalEventView wraps an event with its session id for the global stream.
type globalEventView struct {
	SessionID string    `json:"session_id"`
	Event     eventView `json:"event"`
}

func newGlobalEventView(ev agent.SessionEvent, sessionID string) globalEventView {
	return globalEventView{SessionID: sessionID, Event: eventToView(ev, sessionID)}
}
