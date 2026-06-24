package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"gogent/internal/agent"
	"gogent/internal/command"
	"gogent/internal/config"
	"gogent/internal/fileops"
	"gogent/internal/gogent"
	"gogent/internal/stats"
	"gogent/internal/tool"
	tuipkg "gogent/ui/tui"
)

// embeddedHandlersFor builds the in-process Handlers that drive the embedded core
// g for the TUI wb. It is the single source of the embedded Handlers, shared by
// startup and the daemon->embedded handoff (issue #358) so a process that started
// attached can rebuild byte-for-byte identical embedded behaviour when the user
// stops the daemon. noColor mirrors --no-color for the live theme re-apply.
func embeddedHandlersFor(g *gogent.Gogent, wb *tuipkg.Workbench, noColor bool) tuipkg.Handlers {
	return tuipkg.Handlers{
		// OnCreate builds the backend session and bridges its live events to
		// the matching session window.
		OnCreate: func(sessionID, title string) {
			g.SetSessionTitle(sessionID, title)
			session := g.NewSession(sessionID)
			session.SetObserver(func(ev agent.SessionEvent) {
				wb.EmitSessionEvent(sessionID, ev)
			})
			// Announce the session's effective yolo state now that its observer
			// is installed, so config/CLI-activated yolo lights the status line
			// from the backend (issue #356) rather than a UI-local mirror.
			g.EmitYoloState(sessionID)
		},
		// OnSend runs the task loop in the background; progress (thoughts,
		// tool calls, final answer) flows back through the observer above.
		OnSend: func(sessionID, message, modelName, effort string) {
			// This is a fire-and-forget background goroutine: contain any
			// panic so a single session's crash surfaces as an error in its
			// window instead of taking down the whole TUI process (issue #8).
			defer func() {
				if r := recover(); r != nil {
					wb.EmitSessionEvent(sessionID, agent.SessionEvent{
						Type: agent.SessionEventError,
						Err:  fmt.Errorf("session panicked: %v", r),
					})
				}
			}()
			// The TUI loop runs in the background; cancellation comes from the
			// session's own controls (Stop / window close), which cancel the
			// agent loop directly, so a plain background context is correct here.
			_, err := g.SendMessageToSessionWithModelAndEffort(context.Background(), sessionID, "root", message, modelName, effort)
			if err != nil {
				wb.EmitSessionEvent(sessionID, agent.SessionEvent{
					Type: agent.SessionEventError,
					Err:  err,
				})
			}
		},
		// OnFork forks parentSessionID into a new peer session seeded with a
		// deep copy of its full conversation history (issue #349), then bridges
		// the new session's live events to its window exactly as OnCreate does.
		OnFork: func(parentSessionID, newSessionID, title string) {
			g.SetSessionTitle(newSessionID, title)
			session, err := g.ForkSession(parentSessionID, newSessionID)
			if err != nil {
				wb.EmitSessionEvent(newSessionID, agent.SessionEvent{
					Type: agent.SessionEventError,
					Err:  err,
				})
				return
			}
			session.SetObserver(func(ev agent.SessionEvent) {
				wb.EmitSessionEvent(newSessionID, ev)
			})
			g.EmitYoloState(newSessionID)
		},
		OnClose: func(sessionID string) {
			g.RemoveSession(sessionID)
		},
		// OnRename persists a retitled session to its index so the Sessions
		// browser finds it by the new name (issue #272).
		OnRename: func(sessionID, title string) {
			g.RenameSession(sessionID, title)
		},
		GetSettings: func() config.SubAgentConfig {
			return g.SubAgentSettings()
		},
		SetSettings: func(cfg config.SubAgentConfig) {
			g.SetSubAgentSettings(cfg)
		},
		GetTimeouts: func() config.TimeoutConfig {
			return g.Timeouts()
		},
		SetTimeouts: func(t config.TimeoutConfig) {
			g.SetTimeouts(t)
		},
		GetNotifyConfig: func() config.NotifyConfig {
			return g.Notifications()
		},
		SetNotifyConfig: func(n config.NotifyConfig) {
			// Persist via the backend and push the live config into the
			// workbench's notifier so the next notification respects it.
			g.SetNotifications(n)
			wb.SetNotifyConfig(n)
		},
		GetBudget: func() config.BudgetConfig {
			return g.Budget()
		},
		SetBudget: func(b config.BudgetConfig) {
			// Persist via the backend and push the live config into the
			// workbench so the next status refresh reflects it.
			g.SetBudget(b)
			wb.SetBudgetConfig(b)
		},
		GetReviewEdits: func() bool {
			return g.ReviewEdits()
		},
		SetReviewEdits: func(enabled bool) {
			g.SetReviewEdits(enabled)
		},
		// Welcome/onboarding dialog preference (issues #339/#341/#342): gates the
		// startup trigger and persists the "Don't show again" opt-out.
		GetShowWelcome: func() bool {
			return g.GetShowWelcome()
		},
		SetShowWelcome: func(show bool) {
			_ = g.SetShowWelcome(show)
		},
		GetTheme: func() config.ThemeConfig {
			return g.Theme()
		},
		SetTheme: func(t config.ThemeConfig) {
			// Persist, then re-resolve and install the palette so the live
			// UI recolours without a restart (issue #103).
			g.SetTheme(t)
			tuipkg.ApplyTheme(tuipkg.ResolveTheme(t, os.Getenv, noColor))
			wb.RefreshTheme()
		},
		// Keybinding overrides (issue #269): GetKeybindings seeds the live
		// registry at startup; SetKeybindings only persists (the customizer
		// applies each rebind to the registry itself, live).
		GetKeybindings: func() config.KeybindingsConfig {
			return g.Keybindings()
		},
		SetKeybindings: func(k config.KeybindingsConfig) {
			g.SetKeybindings(k)
		},
		GetModels: func() []config.ModelConfig {
			return g.Models()
		},
		UpdateModel: func(m config.ModelConfig) error {
			return g.UpdateModel(m)
		},
		ScanModels: func(m config.ModelConfig) ([]string, error) {
			return g.ScanModels(m)
		},
		GetDefaultModel: func() string {
			return g.DefaultModelName()
		},
		SetDefaultModel: func(name string) error {
			return g.SetDefaultModel(name)
		},
		GetTranscript: func(sessionID, agentID string) []tuipkg.ChatMessage {
			return toChatMessages(g.AgentTranscript(sessionID, agentID))
		},
		GetSkills: func() []tuipkg.SkillInfo {
			reg := g.GetSkillRegistry()
			if reg == nil {
				return nil
			}
			var out []tuipkg.SkillInfo
			for _, s := range reg.ListSkills() {
				info := tuipkg.SkillInfo{
					Name:        s.Name,
					Description: s.Description,
					Active:      reg.IsSkillActive(s.Name),
					Content:     s.Content,
					Path:        s.Path,
				}
				if st := reg.GetSkillStats(s.Name); st != nil {
					info.Success = st.Success
					info.Failure = st.Failure
					info.TotalCalls = st.TotalCalls
				}
				out = append(out, info)
			}
			return out
		},
		SetSkillActive: func(name string, active bool) {
			reg := g.GetSkillRegistry()
			if reg == nil {
				return
			}
			if active {
				reg.ActivateSkill(name)
			} else {
				reg.DeactivateSkill(name)
			}
		},
		GetTools: func() []tuipkg.ToolInfo {
			reg := g.GetToolRegistry()
			if reg == nil {
				return nil
			}
			tools := reg.List()
			out := make([]tuipkg.ToolInfo, 0, len(tools))
			for _, t := range tools {
				info := tuipkg.ToolInfo{
					Name:        t.Name,
					Description: t.Description,
					InputSchema: tool.SchemaJSON(t.InputSchema),
					Enabled:     reg.IsEnabled(t.Name),
					Invocations: reg.Invocations(t.Name),
				}
				if last := reg.LastUsed(t.Name); !last.IsZero() {
					info.LastUsed = last.Format("2006-01-02 15:04")
				}
				out = append(out, info)
			}
			return out
		},
		SetToolEnabled: func(name string, enabled bool) {
			if reg := g.GetToolRegistry(); reg != nil {
				reg.SetEnabled(name, enabled)
			}
		},
		GetStatistics: func() stats.Report { return g.Statistics() },
		Restore: func() []tuipkg.RestoredSession {
			loaded := g.RestoreSessions()
			out := make([]tuipkg.RestoredSession, 0, len(loaded))
			for _, ls := range loaded {
				out = append(out, loadedToRestored(ls))
			}
			return out
		},
		// ListSavedSessions feeds the Sessions browser index-only metadata
		// (issue #58): no transcript is replayed to populate the list.
		ListSavedSessions: func() []tuipkg.SessionMeta {
			metas := g.ListSessions()
			out := make([]tuipkg.SessionMeta, 0, len(metas))
			for _, m := range metas {
				out = append(out, tuipkg.SessionMeta{
					ID:         m.ID,
					Title:      m.Title,
					CreatedAt:  m.CreatedAt,
					Turns:      m.Turns,
					Messages:   m.Messages,
					TokensIn:   m.TokensIn,
					TokensOut:  m.TokensOut,
					Model:      m.Model,
					ModelLabel: m.ModelLabel,
					ModelID:    m.ModelID,
					File:       m.File,
					Archived:   m.Archived,
				})
			}
			return out
		},
		// OpenSavedSession loads one persisted session for the browser: adopted
		// live when continueSession is true (so sends append), read-only
		// otherwise (issue #58).
		OpenSavedSession: func(file string, continueSession bool) (tuipkg.RestoredSession, bool) {
			var ls gogent.LoadedSession
			if continueSession {
				var ok bool
				ls, ok = g.ContinueSession(file)
				if !ok {
					return tuipkg.RestoredSession{}, false
				}
			} else {
				var err error
				ls, err = g.LoadSavedSession(file)
				if err != nil {
					return tuipkg.RestoredSession{}, false
				}
			}
			return loadedToRestored(ls), true
		},
		LoadLayout: func() gogent.Layout { return g.LoadLayout() },
		SaveLayout: func(layout gogent.Layout) {
			if err := g.SaveLayout(layout); err != nil {
				log.Printf("save layout: %v", err)
			}
		},
		OnUndo:   func(sessionID string) (string, error) { return g.UndoLastTurn(sessionID) },
		OnRewind: func(sessionID string, turns int) (string, error) { return g.Rewind(sessionID, turns) },
		// Plan mode (issue #43): toggle the session flag and execute an approved
		// plan with the full tool set, mirroring OnSend's goroutine + event
		// emission so the result flows back into the session window.
		OnSetPlanMode: func(sessionID string, on bool) { g.SetPlanMode(sessionID, on) },
		OnSetYoloMode: func(sessionID string, on bool) { g.SetYoloMode(sessionID, on) },
		OnApprovePlan: func(sessionID string) {
			defer func() {
				if r := recover(); r != nil {
					wb.EmitSessionEvent(sessionID, agent.SessionEvent{
						Type: agent.SessionEventError,
						Err:  fmt.Errorf("plan execution panicked: %v", r),
					})
				}
			}()
			_, err := g.ExecuteApprovedPlan(context.Background(), sessionID, "root")
			if err != nil {
				wb.EmitSessionEvent(sessionID, agent.SessionEvent{
					Type: agent.SessionEventError,
					Err:  err,
				})
			}
		},
		// Input queue & interruptibility (issue #170). OnStop cancels the
		// in-flight root turn (the window discards its queue on a manual stop);
		// OnInject splices the current input into the running turn now — the
		// agent-side path behind the per-message Interject button (issue #201).
		OnStop: func(sessionID string) {
			if s := g.GetUserSession(sessionID); s != nil {
				_ = s.StopAgent("root")
			}
		},
		OnInject: func(sessionID, message string) {
			if s := g.GetUserSession(sessionID); s != nil {
				s.InjectUserNote(message)
			}
		},
		// Harness-level supervisor (issue #172): the idle watchdog re-checks a
		// session's /goal on each busy→idle edge and nudges it to continue while
		// the goal is unmet, bounded by max_nudges. Off by default; the completion
		// check runs the cheap todo short-circuit and, when needed, one lightweight
		// model judge on the backend.
		SupervisorEnabled: func() bool {
			return g.GetConfig().Experimental.Supervisor
		},
		SupervisorMaxNudges: func() int {
			return g.GetConfig().Supervisor.MaxNudgesOrDefault()
		},
		OnSupervisorCheck: func(sessionID, goal string) (bool, error) {
			s := g.GetUserSession(sessionID)
			if s == nil {
				return false, nil
			}
			return s.GoalSatisfied(goal)
		},
		// Live thinking-token streaming toggle (issue #217): query with set==nil,
		// apply otherwise, returning the resulting per-session state.
		StreamThinking: func(sessionID string, set *bool) bool {
			s := g.GetUserSession(sessionID)
			if s == nil {
				return false
			}
			if set != nil {
				s.SetStreamThinking(*set)
			}
			return s.StreamThinking()
		},
		// @-file mention bridge (issue #46): the completer lists workspace files
		// and expansion reads the chosen ones, both confined to the workspace via
		// the existing FileSystem service.
		ListWorkspaceFiles: func() []string {
			files, _ := g.GetFileSystem().WorkspaceFiles(0)
			return files
		},
		ReadWorkspaceFile: func(path string) (string, bool) {
			content, err := g.GetFileSystem().ReadFile(path, fileops.Authorization{})
			if err != nil {
				return "", false
			}
			return content, true
		},
		// Watchers (issue #329 Phase 4). The TUI dialog, sidebar and /watcher
		// command read through ListWatchers and drive the controls below, which
		// map onto the Phase-3 gogent wrappers. Enable/Disable are the two
		// directions of SetWatcherEnabled (idempotent); the rest are 1:1.
		ListWatchers: func(sessionID string) []tuipkg.WatcherInfo {
			infos := g.ListWatchers(sessionID)
			out := make([]tuipkg.WatcherInfo, 0, len(infos))
			for _, info := range infos {
				out = append(out, toWatcherInfo(info))
			}
			return out
		},
		CreateWatcher: func(cfg tuipkg.WatcherConfig, sessionID string) (tuipkg.WatcherInfo, error) {
			wc := config.WatcherConfig{
				Name:            cfg.Name,
				Task:            cfg.Task,
				Model:           cfg.Model,
				Enabled:         true,
				Schedule:        config.ScheduleConfig{Every: cfg.Every, DailyAt: cfg.DailyAt, Timezone: cfg.Timezone},
				ReportToSession: cfg.ReportToSession,
			}
			info, err := g.CreateWatcher(wc, sessionID)
			if err != nil {
				return tuipkg.WatcherInfo{}, fmt.Errorf("create watcher: %w", err)
			}
			return toWatcherInfo(info), nil
		},
		EnableWatcher:  func(idOrName string) error { return g.SetWatcherEnabled(idOrName, true) },
		DisableWatcher: func(idOrName string) error { return g.SetWatcherEnabled(idOrName, false) },
		RunWatcher:     func(idOrName string) error { return g.RunWatcherNow(idOrName) },
		StopWatcher:    func(idOrName string) error { return g.StopWatcher(idOrName) },
		DeleteWatcher:  func(idOrName string) error { return g.DeleteWatcher(idOrName) },

		// --- Custom slash commands (issue #403) ---
		// Close over the gogent command service; map between the persisted
		// config.CommandDef and the decoupled ui/tui DTO at the seam.
		ListCommands: func() []tuipkg.CommandInfo {
			defs := g.ListCommands()
			out := make([]tuipkg.CommandInfo, 0, len(defs))
			for _, d := range defs {
				out = append(out, tuipkg.CommandInfo{Name: d.Name, Description: d.Description, Version: d.Version})
			}
			return out
		},
		ReservedCommandNames: func() map[string]bool { return command.ReservedNames() },
		OnSendCommand: func(sessionID, message, modelName, agentName string, subtask bool, effort string) {
			// Background goroutine: contain a panic so one command's crash surfaces as
			// an error in its window rather than taking down the TUI (mirrors OnSend).
			defer func() {
				if r := recover(); r != nil {
					wb.EmitSessionEvent(sessionID, agent.SessionEvent{
						Type: agent.SessionEventError,
						Err:  fmt.Errorf("custom command panicked: %v", r),
					})
				}
			}()
			// agent/subtask route the prompt through a one-shot sub-agent; its result
			// is surfaced as the turn's final answer. Otherwise it is an ordinary turn
			// with the model override applied.
			if subtask || agentName != "" {
				result, err := g.RunCommandSubtask(context.Background(), sessionID, agentName, message)
				if err != nil {
					wb.EmitSessionEvent(sessionID, agent.SessionEvent{Type: agent.SessionEventError, Err: err})
					return
				}
				wb.EmitSessionEvent(sessionID, agent.SessionEvent{Type: agent.SessionEventFinal, Text: result})
				return
			}
			if _, err := g.SendMessageToSessionWithModelAndEffort(context.Background(), sessionID, "root", message, modelName, effort); err != nil {
				wb.EmitSessionEvent(sessionID, agent.SessionEvent{Type: agent.SessionEventError, Err: err})
			}
		},
		GetCustomCommand: func(name string) (tuipkg.CommandDef, error) {
			def, err := g.GetCommand(name)
			if err != nil {
				return tuipkg.CommandDef{}, fmt.Errorf("get command: %w", err)
			}
			return toUICommand(def), nil
		},
		CreateCommand: func(def tuipkg.CommandDef) error {
			if _, err := g.CreateCommand(fromUICommand(def)); err != nil {
				return fmt.Errorf("create command: %w", err)
			}
			return nil
		},
		UpdateCommand: func(def tuipkg.CommandDef) error {
			if _, err := g.UpdateCommand(fromUICommand(def)); err != nil {
				return fmt.Errorf("update command: %w", err)
			}
			return nil
		},
		DeleteCommand: func(name string) error { return g.DeleteCommand(name) },
		GetCommandHistory: func(name string) ([]tuipkg.CommandVersion, error) {
			vers, err := g.GetCommandHistory(name)
			if err != nil {
				return nil, fmt.Errorf("command history: %w", err)
			}
			out := make([]tuipkg.CommandVersion, 0, len(vers))
			for _, v := range vers {
				out = append(out, toUICommandVersion(v))
			}
			return out, nil
		},
		RestoreCommandVer: func(name string, v int) error {
			if _, err := g.RestoreCommandVersion(name, v); err != nil {
				return fmt.Errorf("restore command version: %w", err)
			}
			return nil
		},
	}
}
