package server

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hobbestherat/webapi"
	"gogent/internal/config"
	"gogent/internal/gogent"
	"gogent/internal/tool"
	"gogent/internal/vcs"
)

// modelsSvc handles model config read/update/scan.
type modelsSvc struct{ s *Server }

// List handles GET /models — all configured models, with api_key redacted.
func (svc modelsSvc) List(r *http.Request) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	models := svc.s.g.Models()
	out := make([]modelView, 0, len(models))
	for i := range models {
		out = append(out, modelToView(&models[i]))
	}
	return out, nil
}

// Create handles POST /models — create a NEW model entry from the request body
// (the catalog-assisted add). Unlike Update it never preserves a prior key (a new
// entry has none); a blank api_key is stored blank. Returns 409 on name conflict.
func (svc modelsSvc) Create(r *http.Request, req updateModelRequest) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	cfg := req.ModelConfig
	if err := svc.s.g.AddModel(cfg); err != nil {
		return nil, webapi.NewHTTPError(http.StatusConflict, err.Error())
	}
	return modelToView(&cfg), nil
}

// Update handles PUT /models/:name. An empty api_key in the body preserves the
// existing key (so a GET→edit→PUT round-trip doesn't wipe it).
func (svc modelsSvc) Update(r *http.Request, name string, req updateModelRequest) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	updated := req.ModelConfig
	updated.Name = name
	// Preserve an existing api_key when the request omits one.
	if updated.APIKey == "" {
		for _, m := range svc.s.g.Models() {
			if m.Name == name {
				updated.APIKey = m.APIKey
				break
			}
		}
	}
	if err := svc.s.g.UpdateModel(updated); err != nil {
		return nil, webapi.NewHTTPError(http.StatusNotFound, err.Error())
	}
	return modelToView(&updated), nil
}

// Delete handles DELETE /models/:name — remove a configured model. The removal
// policy is enforced in core (gogent.RemoveModel) so embedded and remote behave
// identically; here we only translate the sentinel errors to HTTP status:
// unknown name -> 404, blocked (default-while-others / in-use) -> 409.
func (svc modelsSvc) Delete(r *http.Request, name string) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	if err := svc.s.g.RemoveModel(name); err != nil {
		switch {
		case errors.Is(err, gogent.ErrModelNotFound):
			return nil, webapi.NewHTTPError(http.StatusNotFound, err.Error())
		case errors.Is(err, gogent.ErrModelInUse), errors.Is(err, gogent.ErrModelIsDefault):
			return nil, webapi.NewHTTPError(http.StatusConflict, err.Error())
		default:
			return nil, webapi.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	}
	return map[string]any{"removed": name}, nil
}

// Scan handles POST /models/:name/scan — probe the backend's model list.
func (svc modelsSvc) Scan(r *http.Request, name string) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	var cfg config.ModelConfig
	for _, m := range svc.s.g.Models() {
		if m.Name == name {
			cfg = m
			break
		}
	}
	if cfg.Name == "" {
		return nil, webapi.NewHTTPError(http.StatusNotFound, "model not found")
	}
	ids, err := svc.s.g.ScanModels(cfg)
	if err != nil {
		return nil, webapi.NewHTTPError(http.StatusBadGateway, err.Error())
	}
	return map[string]any{"models": ids}, nil
}

// --- tools ------------------------------------------------------------------

type toolsSvc struct{ s *Server }

// List handles GET /tools.
func (svc toolsSvc) List(r *http.Request) (interface{}, error) {
	reg := svc.s.g.GetToolRegistry()
	if reg == nil {
		return []toolView{}, nil
	}
	tools := reg.List()
	out := make([]toolView, 0, len(tools))
	for _, t := range tools {
		out = append(out, toolView{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: tool.SchemaJSON(t.InputSchema),
			Enabled:     reg.IsEnabled(t.Name),
			Invocations: reg.Invocations(t.Name),
			ReadOnly:    t.ReadOnly,
		})
	}
	return out, nil
}

// SetEnabled handles PUT /tools/:name/enabled.
func (svc toolsSvc) SetEnabled(r *http.Request, req setEnabledRequest, name string) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	reg := svc.s.g.GetToolRegistry()
	if reg == nil || reg.Get(name) == nil {
		return nil, webapi.NewHTTPError(http.StatusNotFound, "tool not found")
	}
	reg.SetEnabled(name, req.Enabled)
	return map[string]any{"name": name, "enabled": req.Enabled}, nil
}

// --- skills -----------------------------------------------------------------

type skillsSvc struct{ s *Server }

// List handles GET /skills.
func (svc skillsSvc) List(r *http.Request) (interface{}, error) {
	reg := svc.s.g.GetSkillRegistry()
	if reg == nil {
		return []skillView{}, nil
	}
	skills := reg.ListSkills()
	out := make([]skillView, 0, len(skills))
	for _, sk := range skills {
		v := skillView{
			Name:        sk.Name,
			Description: sk.Description,
			Active:      reg.IsSkillActive(sk.Name),
		}
		if st := reg.GetSkillStats(sk.Name); st != nil {
			v.Success = st.Success
			v.Failure = st.Failure
			v.TotalCalls = st.TotalCalls
		}
		out = append(out, v)
	}
	return out, nil
}

// SetActive handles PUT /skills/:name/active.
func (svc skillsSvc) SetActive(r *http.Request, req setEnabledRequest, name string) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	reg := svc.s.g.GetSkillRegistry()
	if reg == nil || reg.GetSkill(name) == nil {
		return nil, webapi.NewHTTPError(http.StatusNotFound, "skill not found")
	}
	if req.Enabled {
		reg.ActivateSkill(name)
	} else {
		reg.DeactivateSkill(name)
	}
	return map[string]any{"name": name, "active": req.Enabled}, nil
}

// Get handles GET /skills/:name — the full SKILL.md content.
func (svc skillsSvc) Get(r *http.Request, name string) (interface{}, error) {
	sk := svc.s.g.GetSkillRegistry().GetSkill(name)
	if sk == nil {
		return nil, webapi.NewHTTPError(http.StatusNotFound, "skill not found")
	}
	return map[string]any{"name": sk.Name, "description": sk.Description, "content": sk.Content}, nil
}

// --- settings ---------------------------------------------------------------

type settingsSvc struct{ s *Server }

// Get handles GET /settings.
func (svc settingsSvc) Get(r *http.Request) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	return settingsView{
		SubAgents:    svc.s.g.SubAgentSettings(),
		Timeouts:     svc.s.g.Timeouts(),
		Budget:       svc.s.g.Budget(),
		DefaultModel: svc.s.g.DefaultModelName(),
		ReviewEdits:  svc.s.g.ReviewEdits(),
	}, nil
}

// Set handles PUT /settings — merge + persist. The sub_agents/timeouts/budget/
// default_model/review_edits blocks are updated here; notifications have their own
// endpoint.
//
// default_model is validated and applied FIRST (issue #507): an invalid model name is
// user-correctable input, so it must fail the whole PUT with a 400 BEFORE any other
// field is persisted — otherwise a full PUT carrying a changed budget plus a bad model
// name would leave a partial write. g.SetDefaultModel returns a bare error, which
// webapi would otherwise map to 500; it is wrapped as 400 to match the repo-wide
// pattern. An empty default_model is ignored so an older client that omits the field
// never clears the daemon's default.
func (svc settingsSvc) Set(r *http.Request, req settingsView) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	if req.DefaultModel != "" && req.DefaultModel != svc.s.g.DefaultModelName() {
		if err := svc.s.g.SetDefaultModel(req.DefaultModel); err != nil {
			return nil, webapi.NewHTTPError(http.StatusBadRequest, err.Error())
		}
	}
	svc.s.g.SetSubAgentSettings(req.SubAgents)
	svc.s.g.SetTimeouts(req.Timeouts)
	svc.s.g.SetBudget(req.Budget)
	if req.ReviewEdits != svc.s.g.ReviewEdits() {
		svc.s.g.SetReviewEdits(req.ReviewEdits)
	}
	return svc.Get(r)
}

// NotificationsGet handles GET /settings/notifications.
func (svc settingsSvc) NotificationsGet(r *http.Request) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	return svc.s.g.Notifications(), nil
}

// NotificationsSet handles PUT /settings/notifications.
func (svc settingsSvc) NotificationsSet(r *http.Request, req config.NotifyConfig) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	svc.s.g.SetNotifications(req)
	return svc.s.g.Notifications(), nil
}

// ReviewEditsGet handles GET /settings/review-edits.
func (svc settingsSvc) ReviewEditsGet(r *http.Request) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	return reviewEditsView{Enabled: svc.s.g.ReviewEdits()}, nil
}

// ReviewEditsSet handles PUT /settings/review-edits.
func (svc settingsSvc) ReviewEditsSet(r *http.Request, req reviewEditsView) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	svc.s.g.SetReviewEdits(req.Enabled)
	return reviewEditsView{Enabled: svc.s.g.ReviewEdits()}, nil
}

// --- system -----------------------------------------------------------------

type systemSvc struct{ s *Server }

// Health handles GET /health.
func (svc systemSvc) Health(r *http.Request) (interface{}, error) {
	return healthView{Status: "healthy"}, nil
}

// Workspace handles GET /workspace.
func (svc systemSvc) Workspace(r *http.Request) (interface{}, error) {
	root := svc.s.g.GetWorkspaceRoot()
	v := workspaceView{Root: root}
	if vcs.IsRepo(root) {
		info := &gitInfo{}
		if summary := vcs.StatusSummary(root); summary != "" {
			info.Branch = branchLine(summary)
			info.Dirty = workingTreeIsDirty(summary)
		}
		v.Git = info
	}
	return v, nil
}

// Stats handles GET /stats — aggregate Statistics() across all sessions.
func (svc systemSvc) Stats(r *http.Request) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	return svc.s.g.Statistics(), nil
}

// daemonSessionPrefix is the id prefix of the backend-only session a free-running
// watcher fires into ("watcher:<name>"); such sessions are excluded from the
// user-facing live-session count the daemon status reports.
const daemonSessionPrefix = "watcher:"

// DaemonStatus handles GET /daemon/status — the one-call summary the TUI's
// "Daemon status" menu renders (issue #358 §6): pid, uptime and the live
// session/watcher/MCP figures. It composes process state with the live core so a
// remote client needs a single round-trip. Requires the human scope, like the
// other inspection endpoints.
func (svc systemSvc) DaemonStatus(r *http.Request) (interface{}, error) {
	if err := requireHuman(r, svc.s.provider); err != nil {
		return nil, err
	}
	g := svc.s.g
	live := 0
	for _, id := range g.SessionIDs() {
		if id == "default" || strings.HasPrefix(id, daemonSessionPrefix) {
			continue
		}
		live++
	}
	started := svc.s.startedAt
	return daemonStatusView{
		PID:           os.Getpid(),
		StartedAt:     started.Format(time.RFC3339),
		UptimeSeconds: int64(time.Since(started).Seconds()),
		LiveSessions:  live,
		Watchers:      len(g.ListWatchers("")),
		MCPServers:    g.MCPServerNames(),
	}, nil
}

// branchLine extracts the branch header from a `git status --short --branch`
// summary (its first line, "## branch..."), stripping the "## " marker.
func branchLine(summary string) string {
	line := firstLine(summary)
	if len(line) >= 3 && line[:3] == "## " {
		return line[3:]
	}
	return line
}

// workingTreeIsDirty reports whether the summary contains any tracked changes
// (any line that is not the branch header).
func workingTreeIsDirty(summary string) bool {
	for _, line := range splitLines(summary) {
		if line == "" || (len(line) >= 3 && line[:3] == "## ") {
			continue
		}
		return true
	}
	return false
}

// firstLine returns the text up to the first newline.
func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i, c := range s {
		if c == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
