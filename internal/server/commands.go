package server

import (
	"errors"
	"net/http"

	"github.com/hobbestherat/webapi"
	"gogent/internal/command"
	"gogent/internal/config"
	"gogent/internal/gogent"
)

// commandsSvc groups the custom-command management handlers (issue #403). It
// wraps the gogent command service (ListCommands/GetCommand/CreateCommand/
// UpdateCommand/DeleteCommand/GetCommandHistory/RestoreCommandVersion), mirroring
// watchersSvc: each method has the webapi handler signature and the real work —
// validation, collision detection, versioning and persistence — lives in the
// backend. Custom commands are global (no per-session scope) and have no feature
// flag, so the surface is always available to an authenticated human caller.
type commandsSvc struct{ s *Server }

// List handles GET /commands — every custom command, latest content + history.
func (svc commandsSvc) List(r *http.Request) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	defs := svc.s.g.ListCommands()
	out := make([]commandView, 0, len(defs))
	for _, d := range defs {
		out = append(out, commandToView(d))
	}
	return out, nil
}

// Get handles GET /commands/:name — one command by name (404 if unknown).
func (svc commandsSvc) Get(r *http.Request, name string) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	def, err := svc.s.g.GetCommand(name)
	if err != nil {
		return nil, commandHTTPError(err)
	}
	return commandToView(def), nil
}

// Create handles POST /commands — create a command (version 1). Collision with a
// built-in or an existing custom command is rejected (409/400).
func (svc commandsSvc) Create(r *http.Request, req commandBody) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	def, err := svc.s.g.CreateCommand(commandFromBody(req))
	if err != nil {
		return nil, commandHTTPError(err)
	}
	return commandViewWithWarnings(def), nil
}

// Update handles PUT /commands/:name — record a new version of an existing
// command. The path name is authoritative (the body's name is overridden) so a
// rename cannot be smuggled through the update path.
func (svc commandsSvc) Update(r *http.Request, req commandBody, name string) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	req.Name = name
	def, err := svc.s.g.UpdateCommand(commandFromBody(req))
	if err != nil {
		return nil, commandHTTPError(err)
	}
	return commandViewWithWarnings(def), nil
}

// Delete handles DELETE /commands/:name — remove a command and its history.
func (svc commandsSvc) Delete(r *http.Request, name string) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	if err := svc.s.g.DeleteCommand(name); err != nil {
		return nil, commandHTTPError(err)
	}
	return nil, nil
}

// History handles GET /commands/:name/history — the version list, oldest first.
func (svc commandsSvc) History(r *http.Request, name string) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	vers, err := svc.s.g.GetCommandHistory(name)
	if err != nil {
		return nil, commandHTTPError(err)
	}
	out := make([]commandVersionView, 0, len(vers))
	for _, v := range vers {
		out = append(out, commandVersionToView(v))
	}
	return out, nil
}

// Restore handles POST /commands/:name/restore — restore version N (itself
// recorded as a new version).
func (svc commandsSvc) Restore(r *http.Request, req restoreCommandBody, name string) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	def, err := svc.s.g.RestoreCommandVersion(name, req.Version)
	if err != nil {
		return nil, commandHTTPError(err)
	}
	return commandToView(def), nil
}

// --- helpers ----------------------------------------------------------------

// commandViewWithWarnings is commandToView plus the save-time template warnings,
// so a non-TUI API client creating/updating a command sees the same unknown-
// placeholder feedback the editor surfaces. The warnings never block the save —
// an unknown $name expands literally at runtime by design (issue #403).
func commandViewWithWarnings(d config.CommandDef) commandView {
	v := commandToView(d)
	v.Warnings = command.ValidateTemplate(d.Template, d.Parameters)
	return v
}

// commandToView maps a backend command into its wire view.
func commandToView(d config.CommandDef) commandView {
	v := commandView{
		Name:        d.Name,
		Description: d.Description,
		Parameters:  paramsToView(d.Parameters),
		Template:    d.Template,
		Model:       d.Model,
		Agent:       d.Agent,
		Subtask:     d.Subtask,
		Version:     d.Version,
	}
	for _, ver := range d.Versions {
		v.Versions = append(v.Versions, commandVersionToView(ver))
	}
	return v
}

func commandVersionToView(v config.CommandVersion) commandVersionView {
	return commandVersionView{
		Version:    v.Version,
		Template:   v.Template,
		Parameters: paramsToView(v.Parameters),
		Model:      v.Model,
		Agent:      v.Agent,
		Subtask:    v.Subtask,
		SavedAt:    v.SavedAt,
	}
}

func paramsToView(params []config.CommandParam) []commandParamView {
	if params == nil {
		return nil
	}
	out := make([]commandParamView, len(params))
	for i, p := range params {
		out[i] = commandParamView{Name: p.Name, Description: p.Description, Required: p.Required, Default: p.Default}
	}
	return out
}

// commandFromBody maps a create/update request body into the backend type. The
// version history is server-owned, so it is never taken from the request.
func commandFromBody(req commandBody) config.CommandDef {
	def := config.CommandDef{
		Name:        req.Name,
		Description: req.Description,
		Template:    req.Template,
		Model:       req.Model,
		Agent:       req.Agent,
		Subtask:     req.Subtask,
	}
	if req.Parameters != nil {
		def.Parameters = make([]config.CommandParam, len(req.Parameters))
		for i, p := range req.Parameters {
			def.Parameters[i] = config.CommandParam{Name: p.Name, Description: p.Description, Required: p.Required, Default: p.Default}
		}
	}
	return def
}

// commandHTTPError maps a gogent command error to an HTTP status: not-found →
// 404, duplicate name → 409, everything else (invalid name/template/params) →
// 400. The wrappers wrap the sentinels with %w, so errors.Is sees through them.
func commandHTTPError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gogent.ErrCommandNotFound):
		return webapi.NewHTTPError(http.StatusNotFound, err.Error())
	case errors.Is(err, gogent.ErrCommandExists):
		return webapi.NewHTTPError(http.StatusConflict, err.Error())
	default:
		return webapi.NewHTTPError(http.StatusBadRequest, err.Error())
	}
}
