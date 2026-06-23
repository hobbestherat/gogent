package tool

import (
	"fmt"
	"strings"
	"time"

	"gogent/internal/config"
	"gogent/internal/watcher"
)

// WatcherController is the backend surface the agent-facing watcher tools drive
// (gogent.Gogent implements it). It is intentionally small: the tools marshal
// arguments and format results, while every kind decision (attached vs
// free-running), permission gate, persistence and cross-session scoping lives in
// the backend (issue #329 Phase 3). Reads go through ListWatchers, which the
// backend already scopes to the calling session.
type WatcherController interface {
	// CreateWatcher creates a watcher, deciding its kind from cfg.ReportToSession
	// (non-nil = attached/session-scoped, nil = free-running). sessionID is the
	// calling session.
	CreateWatcher(cfg config.WatcherConfig, sessionID string) (watcher.WatcherInfo, error)
	// UpdateWatcher applies a sparse patch (identified by patch.ID or patch.Name).
	UpdateWatcher(patch config.WatcherConfig, sessionID string) (watcher.WatcherInfo, error)
	// DeleteWatcher unregisters a watcher by id or name.
	DeleteWatcher(idOrName string) error
	// SetWatcherEnabled drives a watcher to a specific enabled state by id or name.
	SetWatcherEnabled(idOrName string, enabled bool) error
	// ListWatchers returns the watchers visible to sessionID: every free-running
	// watcher plus that session's own attached watchers (never another session's).
	ListWatchers(sessionID string) []watcher.WatcherInfo
}

// RegisterWatcherTools registers the agent-facing watcher tools as closures over
// ctrl: create_watcher, list_watchers, update_watcher, enable_watcher,
// disable_watcher and delete_watcher (issue #329 Phase 3). The mutating tools are
// ReadOnly=false (they change shared state, so they run serially) and are
// permission-gated inside the controller (ActionWatcher, scoped by name);
// list_watchers is ReadOnly=true and ungated. create_watcher defaults
// report_to_session to the calling session, so a conversational "watch X and
// report back here" creates an attached watcher private to the session.
//
// The caller registers these only when Experimental.Watchers is enabled.
func (tr *ToolRegistry) RegisterWatcherTools(ctrl WatcherController) {
	tr.Register(newCreateWatcherTool(ctrl))
	tr.Register(newListWatchersTool(ctrl))
	tr.Register(newUpdateWatcherTool(ctrl))
	tr.Register(newEnableWatcherTool(ctrl, true))
	tr.Register(newEnableWatcherTool(ctrl, false))
	tr.Register(newDeleteWatcherTool(ctrl))
}

// scheduleSchema is the shared JSON schema fragment for a watcher schedule:
// exactly one of every / daily_at, with an optional timezone for daily_at.
func scheduleSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":        "object",
		"description": "Recurring cadence. Set exactly one of every / daily_at.",
		"properties": map[string]interface{}{
			"every":    map[string]interface{}{"type": "string", "description": "Interval duration like \"5m\", \"1h\", \"30s\"."},
			"daily_at": map[string]interface{}{"type": "string", "description": "24h wall-clock time \"HH:MM\" to fire once per day."},
			"timezone": map[string]interface{}{"type": "string", "description": "IANA timezone for daily_at, e.g. \"Europe/Zurich\". Empty = UTC."},
		},
	}
}

func newCreateWatcherTool(ctrl WatcherController) *Tool {
	return &Tool{
		Name: "create_watcher",
		Description: "Create a scheduled watcher: a recurring agent task that fires on its own " +
			"cadence (every N minutes, or daily at a fixed local time) and runs a full agent " +
			"loop. By default the watcher is ATTACHED to the current session — it reports its " +
			"work back into this conversation and is removed when the session closes. Pass " +
			"report_to_session: null to create a FREE-RUNNING watcher instead: a global, " +
			"persistent watcher with its own session that survives restart.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":     map[string]interface{}{"type": "string", "description": "Human-friendly display label, e.g. \"github-issue-poller\"."},
				"schedule": scheduleSchema(),
				"task":     map[string]interface{}{"type": "string", "description": "The prompt the watcher runs through the agent loop on each fire."},
				"report_to_session": map[string]interface{}{
					"type": "string",
					"description": "Owning session id. Omit to attach to the current session (default). " +
						"Pass null or \"independent\" for a free-running (global) watcher.",
				},
				"model":   map[string]interface{}{"type": "string", "description": "Model config name for the fire; empty = the default model."},
				"enabled": map[string]interface{}{"type": "boolean", "description": "Whether the schedule is armed immediately. Defaults to true."},
			},
			"required": []string{"name", "schedule", "task"},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			name, _ := args["name"].(string)
			if strings.TrimSpace(name) == "" {
				return nil, fmt.Errorf("name argument is required")
			}
			task, _ := args["task"].(string)
			if strings.TrimSpace(task) == "" {
				return nil, fmt.Errorf("task argument is required")
			}
			sched, err := scheduleFromArg(args["schedule"])
			if err != nil {
				return nil, err
			}

			cfg := config.WatcherConfig{
				Name:            name,
				Task:            task,
				Schedule:        sched,
				Enabled:         boolArgDefault(args["enabled"], true),
				Model:           strings.TrimSpace(stringArg(args["model"])),
				ReportToSession: reportToSession(args, ctx.SessionID),
			}
			info, err := ctrl.CreateWatcher(cfg, ctx.SessionID)
			if err != nil {
				return nil, fmt.Errorf("create_watcher: %w", err)
			}
			return watcherInfoMap(info), nil
		},
	}
}

func newListWatchersTool(ctrl WatcherController) *Tool {
	return &Tool{
		Name:     "list_watchers",
		ReadOnly: true,
		Description: "List the watchers visible to this session: every free-running (global) " +
			"watcher plus this session's own attached watchers (never another session's). Each " +
			"entry shows its id, name, target (the session id for attached, or \"free\"), enabled " +
			"flag, status, and next/last fire times.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			infos := ctrl.ListWatchers(ctx.SessionID)
			out := make([]map[string]interface{}, 0, len(infos))
			for _, info := range infos {
				out = append(out, watcherInfoMap(info))
			}
			return map[string]interface{}{"watchers": out, "count": len(out)}, nil
		},
	}
}

func newUpdateWatcherTool(ctrl WatcherController) *Tool {
	return &Tool{
		Name: "update_watcher",
		Description: "Update an existing watcher (identified by name or id): change its schedule, " +
			"task, model or display name. Only the fields you supply are changed. The watcher's " +
			"kind (attached vs free-running) and owning session are not changed.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"watcher":  map[string]interface{}{"type": "string", "description": "Name or id of the watcher to update."},
				"name":     map[string]interface{}{"type": "string", "description": "New display label (optional)."},
				"schedule": scheduleSchema(),
				"task":     map[string]interface{}{"type": "string", "description": "New task prompt (optional)."},
				"model":    map[string]interface{}{"type": "string", "description": "New model config name (optional)."},
			},
			"required": []string{"watcher"},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			target := strings.TrimSpace(stringArg(args["watcher"]))
			if target == "" {
				return nil, fmt.Errorf("watcher argument (name or id) is required")
			}
			patch := config.WatcherConfig{
				ID:    target,
				Name:  strings.TrimSpace(stringArg(args["name"])),
				Task:  strings.TrimSpace(stringArg(args["task"])),
				Model: strings.TrimSpace(stringArg(args["model"])),
			}
			if raw, ok := args["schedule"]; ok && raw != nil {
				sched, err := scheduleFromArg(raw)
				if err != nil {
					return nil, err
				}
				patch.Schedule = sched
			}
			info, err := ctrl.UpdateWatcher(patch, ctx.SessionID)
			if err != nil {
				return nil, fmt.Errorf("update_watcher: %w", err)
			}
			return watcherInfoMap(info), nil
		},
	}
}

// newEnableWatcherTool builds either enable_watcher (enable=true) or
// disable_watcher (enable=false): both take a watcher name/id and drive it to the
// target enabled state.
func newEnableWatcherTool(ctrl WatcherController, enable bool) *Tool {
	name := "disable_watcher"
	verb := "Disable"
	effect := "stops its schedule (no future fires; a running fire finishes naturally)"
	if enable {
		name = "enable_watcher"
		verb = "Enable"
		effect = "re-arms its schedule"
	}
	return &Tool{
		Name:        name,
		Description: fmt.Sprintf("%s a watcher by name or id: %s.", verb, effect),
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"watcher": map[string]interface{}{"type": "string", "description": "Name or id of the watcher."},
			},
			"required": []string{"watcher"},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			target := strings.TrimSpace(stringArg(args["watcher"]))
			if target == "" {
				return nil, fmt.Errorf("watcher argument (name or id) is required")
			}
			if err := ctrl.SetWatcherEnabled(target, enable); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			return map[string]interface{}{"success": true, "watcher": target, "enabled": enable}, nil
		},
	}
}

func newDeleteWatcherTool(ctrl WatcherController) *Tool {
	return &Tool{
		Name: "delete_watcher",
		Description: "Delete a watcher by name or id: unregister it and remove it permanently. A " +
			"free-running watcher is also dropped from disk and its dedicated session removed; an " +
			"attached watcher is removed from this session.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"watcher": map[string]interface{}{"type": "string", "description": "Name or id of the watcher to delete."},
			},
			"required": []string{"watcher"},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			target := strings.TrimSpace(stringArg(args["watcher"]))
			if target == "" {
				return nil, fmt.Errorf("watcher argument (name or id) is required")
			}
			if err := ctrl.DeleteWatcher(target); err != nil {
				return nil, fmt.Errorf("delete_watcher: %w", err)
			}
			return map[string]interface{}{"success": true, "watcher": target}, nil
		},
	}
}

// reportToSession resolves the create_watcher report_to_session argument into a
// WatcherConfig.ReportToSession pointer (issue #329 Phase 3):
//   - the key absent          -> attached to the calling session (the default).
//   - null / "" / "independent" -> free-running (nil pointer).
//   - any other string        -> attached to that explicit session id.
func reportToSession(args map[string]interface{}, callingSession string) *string {
	raw, present := args["report_to_session"]
	if !present {
		if callingSession == "" {
			return nil
		}
		s := callingSession
		return &s
	}
	if raw == nil {
		return nil // explicit null -> free-running
	}
	s, ok := raw.(string)
	if !ok {
		return nil
	}
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "independent") {
		return nil
	}
	return &s
}

// scheduleFromArg converts the tool's schedule object argument into a
// config.ScheduleConfig (validated downstream by config.ScheduleConfig.Schedule).
func scheduleFromArg(raw interface{}) (config.ScheduleConfig, error) {
	m, ok := raw.(map[string]interface{})
	if !ok {
		return config.ScheduleConfig{}, fmt.Errorf("schedule argument must be an object with every or daily_at")
	}
	return config.ScheduleConfig{
		Every:    strings.TrimSpace(stringArg(m["every"])),
		DailyAt:  strings.TrimSpace(stringArg(m["daily_at"])),
		Timezone: strings.TrimSpace(stringArg(m["timezone"])),
	}, nil
}

// watcherInfoMap renders a watcher.WatcherInfo as the JSON-friendly map the tools
// return. Zero timestamps are omitted; target is the owning session id for
// attached watchers or "free" for free-running ones.
func watcherInfoMap(info watcher.WatcherInfo) map[string]interface{} {
	target := "free"
	if info.Kind == watcher.KindAttached {
		target = info.TargetSession
	}
	out := map[string]interface{}{
		"id":      info.ID,
		"name":    info.Name,
		"target":  target,
		"enabled": info.Enabled,
		"status":  info.Status.String(),
	}
	if !info.NextFire.IsZero() {
		out["next_fire"] = info.NextFire.Format(time.RFC3339)
	}
	if !info.LastRun.IsZero() {
		out["last_run"] = info.LastRun.Format(time.RFC3339)
	}
	if info.LastResult != "" {
		out["last_result"] = info.LastResult
	}
	if info.LastError != "" {
		out["last_error"] = info.LastError
	}
	return out
}

// stringArg returns v as a string, or "" when v is nil or not a string.
func stringArg(v interface{}) string {
	s, _ := v.(string)
	return s
}

// boolArgDefault returns v as a bool, falling back to def when v is absent or not
// a bool.
func boolArgDefault(v interface{}, def bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}
