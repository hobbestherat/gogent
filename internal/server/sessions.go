package server

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/hobbestherat/webapi"
	"gogent/internal/agent"
	"gogent/internal/model"
)

// sessionsSvc groups the session lifecycle handlers. Each is a method with the
// webapi handler signature (first param *http.Request, optional body struct,
// positional path params, returns (interface{}, error)).
type sessionsSvc struct{ s *Server }

// List handles GET /sessions: every saved + live session (index metadata only).
func (svc sessionsSvc) List(r *http.Request) (interface{}, error) {
	// Peer scope may not enumerate other sessions.
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	var views []sessionView

	// Saved sessions (from the store index, no transcript replay). A saved
	// session that is also currently in memory (restored on the daemon's startup)
	// is marked Live so an attached TUI reopens its window.
	for _, m := range svc.s.g.ListSessions() {
		views = append(views, sessionView{
			ID:           m.ID,
			Title:        m.Title,
			CreatedAt:    m.CreatedAt,
			State:        "idle",
			PrimaryModel: m.Model,
			Persisted:    true,
			Live:         svc.s.g.GetUserSession(m.ID) != nil,
		})
	}
	seen := make(map[string]bool, len(views))
	for i := range views {
		seen[views[i].ID] = true
	}

	// Live sessions not already listed as saved.
	for _, id := range svc.s.g.SessionIDs() {
		if seen[id] {
			continue
		}
		if us := svc.s.g.GetUserSession(id); us != nil {
			v := svc.toView(id, us, "", true)
			v.Live = true
			views = append(views, v)
		}
	}
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	return views, nil
}

// Create handles POST /sessions.
func (svc sessionsSvc) Create(r *http.Request, req createSessionRequest) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	// Honour a caller-supplied id (an attached TUI keeps its window id in sync
	// with the daemon session id); otherwise generate one as before.
	id := req.ID
	if id == "" {
		id = randomID("sess")
	}
	us := svc.s.createSession(id, req.Persisted)
	if req.Title != "" {
		svc.s.g.SetSessionTitle(id, req.Title)
	}
	if req.Model != "" {
		us.SetPrimaryModel(req.Model)
	}
	v := svc.toView(id, us, req.Title, req.Persisted)
	v.Live = true
	return v, nil
}

// Get handles GET /sessions/:id.
func (svc sessionsSvc) Get(r *http.Request, id string) (interface{}, error) {
	us := svc.s.g.GetUserSession(id)
	if us != nil {
		v := svc.toView(id, us, svc.titleFor(id), !svc.isEphemeral(id))
		v.Live = true
		return v, nil
	}
	// A saved-but-not-live session: reconstruct a minimal view from the index.
	for _, m := range svc.s.g.ListSessions() {
		if m.ID == id {
			return sessionView{
				ID: m.ID, Title: m.Title, CreatedAt: m.CreatedAt,
				State: "idle", PrimaryModel: m.Model, Persisted: true,
			}, nil
		}
	}
	return nil, webapi.NewHTTPError(http.StatusNotFound, "session not found")
}

// Delete handles DELETE /sessions/:id (close + archive if persisted).
func (svc sessionsSvc) Delete(r *http.Request, id string) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	if svc.s.g.GetUserSession(id) == nil && !svc.isSaved(id) {
		return nil, webapi.NewHTTPError(http.StatusNotFound, "session not found")
	}
	svc.s.g.RemoveSession(id)
	return nil, nil
}

// Transcript handles GET /sessions/:id/transcript?agent=root.
func (svc sessionsSvc) Transcript(r *http.Request, id string, q transcriptQuery) (interface{}, error) {
	agentID := q.Agent
	if agentID == "" {
		agentID = "root"
	}
	msgs := svc.s.g.AgentTranscript(id, agentID)
	if msgs == nil {
		return nil, webapi.NewHTTPError(http.StatusNotFound, "session or agent not found")
	}
	return messagesToViews(msgs), nil
}

// Stats handles GET /sessions/:id/stats.
func (svc sessionsSvc) Stats(r *http.Request, id string) (interface{}, error) {
	us := svc.s.g.GetUserSession(id)
	if us == nil {
		return nil, webapi.NewHTTPError(http.StatusNotFound, "session not found")
	}
	return snapshotToView(us.Snapshot()), nil
}

// Stop handles POST /sessions/:id/stop.
func (svc sessionsSvc) Stop(r *http.Request, id string) (interface{}, error) {
	us := svc.s.g.GetUserSession(id)
	if us == nil {
		return nil, webapi.NewHTTPError(http.StatusNotFound, "session not found")
	}
	if err := us.StopAgent("root"); err != nil {
		return nil, fmt.Errorf("stop agent: %w", err)
	}
	return nil, nil
}

// Inject handles POST /sessions/:id/inject.
func (svc sessionsSvc) Inject(r *http.Request, req injectRequest, id string) (interface{}, error) {
	if req.Message == "" {
		return nil, webapi.NewHTTPError(http.StatusBadRequest, "message is required")
	}
	us := svc.s.g.GetUserSession(id)
	if us == nil {
		return nil, webapi.NewHTTPError(http.StatusNotFound, "session not found")
	}
	us.InjectUserNote(req.Message)
	return nil, nil
}

// Undo handles POST /sessions/:id/undo.
func (svc sessionsSvc) Undo(r *http.Request, id string) (interface{}, error) {
	summary, err := svc.s.g.UndoLastTurn(id)
	if err != nil {
		return nil, webapi.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return map[string]string{"result": summary}, nil
}

// Rewind handles POST /sessions/:id/rewind.
func (svc sessionsSvc) Rewind(r *http.Request, req rewindRequest, id string) (interface{}, error) {
	turns := req.Turns
	if turns <= 0 {
		turns = 1
	}
	summary, err := svc.s.g.Rewind(id, turns)
	if err != nil {
		return nil, webapi.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return map[string]string{"result": summary}, nil
}

// --- plan mode --------------------------------------------------------------

type planModeView struct {
	Enabled bool `json:"enabled"`
}

// PlanMode handles GET /sessions/:id/plan-mode.
func (svc sessionsSvc) PlanMode(r *http.Request, id string) (interface{}, error) {
	if svc.s.g.GetUserSession(id) == nil {
		return nil, webapi.NewHTTPError(http.StatusNotFound, "session not found")
	}
	return planModeView{Enabled: svc.s.g.PlanMode(id)}, nil
}

// SetPlanMode handles PUT /sessions/:id/plan-mode.
func (svc sessionsSvc) SetPlanMode(r *http.Request, req planModeRequest, id string) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	if svc.s.g.GetUserSession(id) == nil {
		return nil, webapi.NewHTTPError(http.StatusNotFound, "session not found")
	}
	svc.s.g.SetPlanMode(id, req.Enabled)
	return planModeView{Enabled: svc.s.g.PlanMode(id)}, nil
}

// Plan handles GET /sessions/:id/plan.
func (svc sessionsSvc) Plan(r *http.Request, id string) (interface{}, error) {
	if svc.s.g.GetUserSession(id) == nil {
		return nil, webapi.NewHTTPError(http.StatusNotFound, "session not found")
	}
	if !svc.s.g.HasPendingPlan(id) {
		return nil, webapi.NewHTTPError(http.StatusNotFound, "no plan awaiting approval")
	}
	return planView{Plan: svc.pendingPlan(id)}, nil
}

// ApprovePlan handles POST /sessions/:id/plan/approve — non-blocking like Send
// (issue #481). It dispatches the approved plan as a daemon-owned turn and returns
// the turn id immediately; the turn runs to completion regardless of the client
// connection, with progress and the final answer flowing over the SSE hub. It
// acquires the busy gate for the turn's full duration (held until completion, not
// handler return), so a concurrent send gets 409 while the plan executes.
func (svc sessionsSvc) ApprovePlan(r *http.Request, id string) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	if svc.s.g.GetUserSession(id) == nil {
		return nil, webapi.NewHTTPError(http.StatusNotFound, "session not found")
	}

	release, ok := svc.s.markBusy(id)
	if !ok {
		return nil, webapi.NewHTTPError(http.StatusConflict, "session is busy")
	}

	turnID, err := svc.s.g.DispatchApprovedPlan(id, "root", release)
	if err != nil {
		release()
		// "no plan awaiting approval" is the caller's fault (400); a missing
		// session is 404 (already checked) — keep the existing 400 mapping.
		return nil, webapi.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return acceptedView{TurnID: turnID}, nil
}

// RejectPlan handles POST /sessions/:id/plan/reject.
func (svc sessionsSvc) RejectPlan(r *http.Request, id string) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	if svc.s.g.GetUserSession(id) == nil {
		return nil, webapi.NewHTTPError(http.StatusNotFound, "session not found")
	}
	svc.s.g.RejectPlan(id)
	return nil, nil
}

// --- helpers ----------------------------------------------------------------

func (svc sessionsSvc) toView(id string, us *agent.UserSession, title string, persisted bool) sessionView {
	v := sessionToView(svc.s.g, id, us, title)
	v.Persisted = persisted
	return v
}

func (svc sessionsSvc) titleFor(id string) string {
	for _, m := range svc.s.g.ListSessions() {
		if m.ID == id {
			return m.Title
		}
	}
	return ""
}

func (svc sessionsSvc) pendingPlan(id string) string {
	if us := svc.s.g.GetUserSession(id); us != nil {
		return us.PendingPlan()
	}
	return ""
}

func (svc sessionsSvc) isSaved(id string) bool {
	for _, m := range svc.s.g.ListSessions() {
		if m.ID == id {
			return true
		}
	}
	return false
}

// isEphemeral is true when the session is a live, non-restored (HTTP-style)
// session. We approximate: a live session whose id is not among the saved
// indices is treated as ephemeral. Restored persisted sessions are saved.
func (svc sessionsSvc) isEphemeral(id string) bool {
	return !svc.isSaved(id)
}

var _ = time.Now
var _ model.Role
