package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hobbestherat/webapi"
	"gogent/internal/config"
	"gogent/internal/watcher"
)

// watchersSvc groups the watcher management handlers (issue #329 Phase 5). It
// wraps the gogent watcher methods (ListWatchers/CreateWatcher/UpdateWatcher/
// SetWatcherEnabled/ToggleWatcher/RunWatcherNow/StopWatcher/DeleteWatcher),
// mirroring sessionsSvc: each method has the webapi handler signature and the
// real work — kind decision (attached vs free-running), permission gating
// (ActionWatcher), persistence and scoping — lives in the backend.
//
// The HTTP surface manages both kinds. List scopes through the gogent
// ListWatchers wrapper: GET /watchers lists free-running watchers; GET
// /watchers?session_id=<id> additionally includes that session's attached
// watchers. Create honours report_to_session (nil ⇒ free-running; a live
// session id ⇒ attached). Get resolves a watcher of either kind by id or name
// so a just-created attached watcher is immediately readable by its returned id.
type watchersSvc struct{ s *Server }

// ensureEnabled gates the watcher API behind the same Experimental.Watchers
// feature flag as the rest of the watcher engine: when it is off the engine was
// never started, so the surface reports 404 rather than leaking a half-wired
// feature. The gogent wrappers additionally fail closed when the engine is nil.
func (svc watchersSvc) ensureEnabled() error {
	cfg := svc.s.g.GetConfig()
	if cfg == nil || !cfg.Experimental.Watchers {
		return webapi.NewHTTPError(http.StatusNotFound, "watchers are not enabled")
	}
	return nil
}

// List handles GET /watchers[?session_id=<id>] — free-running watchers, plus
// the given session's attached watchers when session_id is supplied (the
// scoping the gogent ListWatchers wrapper enforces).
func (svc watchersSvc) List(r *http.Request, q watcherListQuery) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	if err := svc.ensureEnabled(); err != nil {
		return nil, err
	}
	infos := svc.s.g.ListWatchers(q.SessionID)
	out := make([]watcherView, 0, len(infos))
	for _, info := range infos {
		out = append(out, watcherToView(info))
	}
	return out, nil
}

// Get handles GET /watchers/:id — one watcher by id or name (either kind). It
// shares the backend resolver semantics of the mutate endpoints: an unknown
// id/name is 404 and an ambiguous name (duplicate display names are valid, since
// identity is the stable id) is 409.
func (svc watchersSvc) Get(r *http.Request, id string) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	if err := svc.ensureEnabled(); err != nil {
		return nil, err
	}
	info, err := svc.s.g.GetWatcher(id)
	if err != nil {
		return nil, watcherHTTPError(err)
	}
	return watcherToView(info), nil
}

// Create handles POST /watchers. report_to_session nil/omitted ⇒ free-running;
// a non-nil (live) session id ⇒ attached. The backend decides the kind.
func (svc watchersSvc) Create(r *http.Request, req createWatcherRequest) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	if err := svc.ensureEnabled(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Name) == "" {
		return nil, webapi.NewHTTPError(http.StatusBadRequest, "name is required")
	}
	if strings.TrimSpace(req.Task) == "" {
		return nil, webapi.NewHTTPError(http.StatusBadRequest, "task is required")
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	cfg := config.WatcherConfig{
		Name:            strings.TrimSpace(req.Name),
		Task:            req.Task,
		Schedule:        req.Schedule,
		Model:           strings.TrimSpace(req.Model),
		Enabled:         enabled,
		ReportToSession: normalizeReportToSession(req.ReportToSession),
		Output:          req.Output,
	}
	info, err := svc.s.g.CreateWatcher(cfg, "")
	if err != nil {
		return nil, watcherHTTPError(err)
	}
	return watcherToView(info), nil
}

// Update handles PUT/PATCH /watchers/:id — a sparse patch (only supplied fields
// change). The watcher's kind and owning session are never changed.
func (svc watchersSvc) Update(r *http.Request, req updateWatcherRequest, id string) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	if err := svc.ensureEnabled(); err != nil {
		return nil, err
	}
	patch := config.WatcherConfig{
		ID:       id,
		Name:     strings.TrimSpace(req.Name),
		Task:     strings.TrimSpace(req.Task),
		Model:    strings.TrimSpace(req.Model),
		Schedule: req.Schedule,
	}
	info, err := svc.s.g.UpdateWatcher(patch, "")
	if err != nil {
		return nil, watcherHTTPError(err)
	}
	return watcherToView(info), nil
}

// SetEnabled handles PUT /watchers/:id/enabled — drive to an explicit state.
func (svc watchersSvc) SetEnabled(r *http.Request, req setEnabledRequest, id string) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	if err := svc.ensureEnabled(); err != nil {
		return nil, err
	}
	if err := svc.s.g.SetWatcherEnabled(id, req.Enabled); err != nil {
		return nil, watcherHTTPError(err)
	}
	return map[string]any{"watcher": id, "enabled": req.Enabled}, nil
}

// Toggle handles POST /watchers/:id/toggle — flip the enabled state.
func (svc watchersSvc) Toggle(r *http.Request, id string) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	if err := svc.ensureEnabled(); err != nil {
		return nil, err
	}
	if err := svc.s.g.ToggleWatcher(id); err != nil {
		return nil, watcherHTTPError(err)
	}
	return nil, nil
}

// Run handles POST /watchers/:id/run — fire now, ignoring schedule + enabled.
func (svc watchersSvc) Run(r *http.Request, id string) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	if err := svc.ensureEnabled(); err != nil {
		return nil, err
	}
	if err := svc.s.g.RunWatcherNow(id); err != nil {
		return nil, watcherHTTPError(err)
	}
	return nil, nil
}

// Stop handles POST /watchers/:id/stop — cancel the in-flight fire (the
// schedule keeps running).
func (svc watchersSvc) Stop(r *http.Request, id string) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	if err := svc.ensureEnabled(); err != nil {
		return nil, err
	}
	if err := svc.s.g.StopWatcher(id); err != nil {
		return nil, watcherHTTPError(err)
	}
	return nil, nil
}

// Delete handles DELETE /watchers/:id — unregister + remove permanently.
func (svc watchersSvc) Delete(r *http.Request, id string) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	if err := svc.ensureEnabled(); err != nil {
		return nil, err
	}
	if err := svc.s.g.DeleteWatcher(id); err != nil {
		return nil, watcherHTTPError(err)
	}
	return nil, nil
}

// --- helpers ----------------------------------------------------------------

// normalizeReportToSession resolves the create request's report_to_session into
// the WatcherConfig.ReportToSession the gogent wrapper expects, matching the
// watcher tool's semantics (internal/tool reportToSession): nil, an empty/blank
// string, or the case-insensitive sentinel "independent" all mean free-running
// (nil); any other non-empty string is an attached target session id. Keeping
// this identical to the tool path means the same create concept behaves the same
// whether it arrives over HTTP or from the agent tools.
func normalizeReportToSession(p *string) *string {
	if p == nil {
		return nil
	}
	s := strings.TrimSpace(*p)
	if s == "" || strings.EqualFold(s, "independent") {
		return nil
	}
	return &s
}

// watcherToView maps a backend watcher snapshot to its wire view, matching the
// agent tools' watcherInfoMap shape (target = owning session id for attached
// watchers, "free" for free-running; zero timestamps omitted).
func watcherToView(info watcher.WatcherInfo) watcherView {
	kind := "free"
	target := "free"
	if info.Kind == watcher.KindAttached {
		kind = "attached"
		target = info.TargetSession
	}
	v := watcherView{
		ID:         info.ID,
		Name:       info.Name,
		Kind:       kind,
		Target:     target,
		Task:       info.Task,
		Schedule:   info.Schedule,
		Enabled:    info.Enabled,
		Status:     info.Status.String(),
		LastResult: info.LastResult,
		LastError:  info.LastError,
	}
	if !info.NextFire.IsZero() {
		v.NextFire = info.NextFire.Format(time.RFC3339)
	}
	if !info.LastRun.IsZero() {
		v.LastRun = info.LastRun.Format(time.RFC3339)
	}
	return v
}

// watcherHTTPError maps a gogent watcher wrapper error to an HTTP status:
// not-found → 404, ambiguous/duplicate name → 409, everything else (invalid
// schedule, denied permission, inactive target session, engine not running) →
// 400. The wrappers wrap the sentinels with %w, so errors.Is sees through them.
func watcherHTTPError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, watcher.ErrNotFound):
		return webapi.NewHTTPError(http.StatusNotFound, err.Error())
	case errors.Is(err, watcher.ErrAmbiguous), errors.Is(err, watcher.ErrDuplicate):
		return webapi.NewHTTPError(http.StatusConflict, err.Error())
	default:
		return webapi.NewHTTPError(http.StatusBadRequest, err.Error())
	}
}
