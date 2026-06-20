package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"gogent/internal/agent"
	"gogent/internal/command"
	"gogent/internal/config"
	"gogent/internal/diag"
	"gogent/internal/fileops"
	"gogent/internal/gogent"
	"gogent/internal/model"
	"gogent/internal/stats"
	"gogent/internal/tool"
	tuipkg "gogent/ui/tui"
)

var (
	verbose    = flag.Bool("verbose", false, "Enable verbose output")
	httpServer = flag.Bool("http", false, "Enable HTTP server mode (always on by default)")
	httpHost   = flag.String("http-host", "127.0.0.1", "HTTP server host")
	httpPort   = flag.Int("http-port", 8080, "HTTP server port")
	disableTUI = flag.Bool("no-tui", false, "Disable TUI (for API testing)")
	noColor    = flag.Bool("no-color", false, "Disable coloured output (also honours the NO_COLOR env var)")
)

var (
	toolLogs   []string
	toolLogsMu sync.Mutex
)

func main() {
	flag.Parse()

	// Get home directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Failed to get home directory: %v", err)
	}

	// Create paths
	skillsDir := filepath.Join(homeDir, ".gogent", "skills")
	configDir := filepath.Join(homeDir, ".gogent")

	// Create Gogent instance (loads skills + AGENTS.md and owns the registry)
	g := gogent.NewGogent(homeDir)
	fmt.Printf("\nWorking directory (file & shell ops): %s\n", g.GetWorkspaceRoot())

	// In TUI mode, redirect diagnostics to a log file so warnings/errors never
	// get written to stdout and corrupt the alternate screen (issue #17).
	// Headless mode keeps the stderr default. A failed open falls back to stderr.
	if !*disableTUI {
		if lg, err := diag.NewFile(filepath.Join(homeDir, ".gogent", "gogent.log")); err == nil {
			g.SetLogger(lg)
		} else {
			log.Printf("open diagnostic log: %v", err)
		}
	}

	// The security audit trail (permission decisions, tool calls) is always
	// file-backed — it is a durable post-incident artifact, not screen output, so
	// it goes to a file in both TUI and headless mode (issue #51).
	if au, err := diag.NewAuditFile(filepath.Join(homeDir, ".gogent", "audit.log")); err == nil {
		g.SetAudit(au)
	} else {
		log.Printf("open audit log: %v", err)
	}

	skillRegistry := g.GetSkillRegistry()
	skills := skillRegistry.ListSkills()
	fmt.Printf("\nLoaded %d skills:\n", len(skills))
	for _, s := range skills {
		fmt.Printf("  - %s: %s\n", s.Name, s.Description)
	}
	if *verbose {
		fmt.Printf("\nActive skills: %d\n", len(skillRegistry.ListActiveSkills()))
	}

	// Create model session for the default (HTTP) session, pointed at the
	// configured default endpoint (honors GOGENT_MODEL_URL).
	m := model.NewModelConnection()
	if def := g.GetConfig().GetModelConfig(g.GetConfig().DefaultModel); def != nil && def.Endpoint != "" {
		m.SetURL(def.Endpoint)
	}
	sess := model.NewModelSession("main", m)

	// Create root agent
	rootAgent := agent.NewAgent("root", sess)
	rootAgent.SetState(agent.StateIdle)

	// Create user session
	_ = g.CreateUserSession("default", rootAgent)

	// Create command registry
	cmdRegistry := command.NewCommandRegistry()
	cmdRegistry.RegisterBuiltInCommands()

	// Register file operation tools
	g.RegisterFileTools(cmdRegistry)

	if *verbose && *disableTUI {
		fmt.Printf("\nConfiguration:\n")
		fmt.Printf("  Home directory: %s\n", homeDir)
		fmt.Printf("  Skills directory: %s\n", skillsDir)
		fmt.Printf("  Config directory: %s\n", configDir)
		fmt.Printf("  Model URL: %s\n", m.URL)
		fmt.Printf("  Built-in commands: %d\n", len(cmdRegistry.ListCommands()))
		fmt.Printf("  Active skills: %d\n", len(skillRegistry.ListActiveSkills()))
	}

	// Start HTTP server (always)
	fmt.Printf("\nStarting HTTP server on http://%s:%d\n", *httpHost, *httpPort)
	go startHTTPServer(*httpHost, *httpPort, g)

	// Create and start the multi-session TUI if enabled
	var wb *tuipkg.Workbench
	if !*disableTUI {
		// Resolve and install the colour theme before the workbench (and its
		// desktop chrome) are built, honouring config, NO_COLOR and --no-color, and
		// degrading to the terminal's colour fidelity (issue #66).
		tuipkg.ApplyTheme(tuipkg.ResolveTheme(g.GetConfig().Theme, os.Getenv, *noColor))
		wb = tuipkg.NewWorkbench(g.GetConfig().ModelConfigs)
		fmt.Println("TUI enabled. Press Ctrl+C to exit.")

		// Route interactive permission prompts to the workbench modal.
		g.GetPermissionService().SetPrompter(wb)
		// Route diff-review approvals (issue #64) to the workbench modal too.
		g.SetReviewer(wb)

		wb.SetHandlers(tuipkg.Handlers{
			// OnCreate builds the backend session and bridges its live events to
			// the matching session window.
			OnCreate: func(sessionID, title string) {
				g.SetSessionTitle(sessionID, title)
				session := g.NewSession(sessionID)
				session.SetObserver(func(ev agent.SessionEvent) {
					wb.EmitSessionEvent(sessionID, ev)
				})
			},
			// OnSend runs the task loop in the background; progress (thoughts,
			// tool calls, final answer) flows back through the observer above.
			OnSend: func(sessionID, message, modelName string) {
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
				_, err := g.SendMessageToSessionWithModel(context.Background(), sessionID, "root", message, modelName)
				if err != nil {
					wb.EmitSessionEvent(sessionID, agent.SessionEvent{
						Type: agent.SessionEventError,
						Err:  err,
					})
				}
			},
			OnClose: func(sessionID string) {
				g.RemoveSession(sessionID)
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
			GetTheme: func() config.ThemeConfig {
				return g.Theme()
			},
			SetTheme: func(t config.ThemeConfig) {
				// Persist, then re-resolve and install the palette so the live
				// UI recolours without a restart (issue #103).
				g.SetTheme(t)
				tuipkg.ApplyTheme(tuipkg.ResolveTheme(t, os.Getenv, *noColor))
				wb.RefreshTheme()
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
						ID:        m.ID,
						Title:     m.Title,
						CreatedAt: m.CreatedAt,
						Turns:     m.Turns,
						Messages:  m.Messages,
						TokensIn:  m.TokensIn,
						TokensOut: m.TokensOut,
						Model:     m.Model,
						File:      m.File,
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
		})

		// Push the persisted notification config into the workbench's live
		// notifier so the very first notification respects the user's settings.
		wb.SetNotifyConfig(g.Notifications())
		// Push the persisted token-budget config so the status gauge's budget
		// alert (if any) is active from the first turn.
		wb.SetBudgetConfig(g.Budget())

		// Run the TUI in a goroutine.
		go func() {
			if err := wb.Run(); err != nil {
				log.Printf("TUI error: %v", err)
			}
		}()
	}

	// Connect any configured MCP servers and register their tools (issue #36).
	// Done after the permission prompter is installed above so the launch gate can
	// prompt interactively rather than defaulting to deny.
	g.StartMCPServers()

	// Keep running with graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Defer stats output for final shutdown. Work happens in the dynamically
	// created TUI sessions (and the "default" HTTP session), so we aggregate the
	// grand total across all of them rather than reading a single session.
	defer func() {
		stats := g.AggregateStats()
		fmt.Printf("\n=== Session Stats (all sessions) ===\n")
		fmt.Printf("Total Tokens In: %d\n", stats["tokens_in"])
		fmt.Printf("Total Tokens Out: %d\n", stats["tokens_out"])
		fmt.Printf("Tool Calls: %d\n", stats["tool_calls"])
		if stats["fast_tokens_in"] > 0 || stats["fast_tokens_out"] > 0 {
			fmt.Printf("Fast Model Tokens In: %d\n", stats["fast_tokens_in"])
			fmt.Printf("Fast Model Tokens Out: %d\n", stats["fast_tokens_out"])
		}
	}()

	sig := <-sigChan
	fmt.Printf("\nReceived signal %v, shutting down...\n", sig)

	// Release any MCP servers (terminates stdio subprocesses).
	g.CloseMCPServers()

	// Cancel TUI if running
	if wb != nil {
		wb.QuitFunc()()
	}

	// Give time for shutdown to complete
	time.Sleep(100 * time.Millisecond)
}

// toChatMessages converts backend transcript messages into the UI-facing chat
// message view used to render restored sessions and sub-agent monologues.
func toChatMessages(msgs []model.Message) []tuipkg.ChatMessage {
	out := make([]tuipkg.ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		cm := tuipkg.ChatMessage{Role: string(m.Role), Content: m.Content, Tool: m.Name}
		if len(m.ToolCalls) > 0 {
			tc := m.ToolCalls[0]
			cm.Tool = tc.Function.Name
			cm.Args = tc.Function.Arguments
		}
		out = append(out, cm)
	}
	return out
}

// loadedToRestored maps a backend LoadedSession into the UI's RestoredSession
// view, seeding an empty title with the session id. Shared by startup restore
// and the on-demand Sessions browser open/continue paths (issue #58).
func loadedToRestored(ls gogent.LoadedSession) tuipkg.RestoredSession {
	title := ls.Title
	if title == "" {
		title = ls.ID
	}
	return tuipkg.RestoredSession{
		ID:       ls.ID,
		Title:    title,
		Messages: toChatMessages(ls.Transcripts["root"]),
	}
}

// HTTP server tunables. The read/header timeouts and body cap bound slow-client
// (slowloris) and oversized-body attacks. WriteTimeout is intentionally left
// unset: the /message handler runs an expensive, multi-turn model loop whose
// response can legitimately take minutes, and a fixed write deadline would
// truncate valid answers. Slowloris is mitigated on the read side, which is
// where it applies.
const (
	httpReadHeaderTimeout = 10 * time.Second
	httpReadTimeout       = 30 * time.Second
	httpIdleTimeout       = 120 * time.Second
	httpMaxRequestBody    = 1 << 20 // 1 MiB
)

// httpBackend is the minimal slice of *gogent.Gogent the HTTP handlers need.
// Defining it as an interface keeps the handlers decoupled from the full Gogent
// and lets them be unit-tested with a fake.
type httpBackend interface {
	// SendMessage runs the agent task loop for the given client session and
	// returns the final assistant response. Each session id maps to its own
	// isolated UserSession, so concurrent clients neither serialize nor see each
	// other's transcript (issue #25). ctx cancels the loop when the client
	// disconnects (issue #24).
	SendMessage(ctx context.Context, sessionID, message, modelName string) (*model.CompletionResponse, error)
	// Stats returns aggregate counters for the given client session, or nil.
	Stats(sessionID string) map[string]interface{}
}

// HTTP per-client session bounds. Idle client sessions are evicted after the
// TTL and the live count is capped, so a flood of one-shot clients can neither
// keep stale transcripts in memory nor grow the session map without bound
// (issue #25).
const (
	httpSessionTTL = 30 * time.Minute
	httpSessionMax = 256
)

// gogentBackend adapts a *gogent.Gogent to the httpBackend interface, routing
// each client session id to its own backend UserSession and bounding their
// number/age via the registry.
type gogentBackend struct {
	g        *gogent.Gogent
	sessions *httpSessionRegistry
}

func newGogentBackend(g *gogent.Gogent) gogentBackend {
	return gogentBackend{
		g: g,
		sessions: &httpSessionRegistry{
			seen:     make(map[string]time.Time),
			maxIdle:  httpSessionTTL,
			maxItems: httpSessionMax,
			now:      time.Now,
			create:   func(id string) { g.NewEphemeralSession(id) },
			evict:    func(id string) { g.RemoveSession(id) },
		},
	}
}

func (b gogentBackend) SendMessage(ctx context.Context, sessionID, message, modelName string) (*model.CompletionResponse, error) {
	b.sessions.touch(sessionID)
	return b.g.SendMessageToSessionWithModel(ctx, sessionID, "root", message, modelName)
}

func (b gogentBackend) Stats(sessionID string) map[string]interface{} {
	if s := b.g.GetUserSession(sessionID); s != nil {
		return s.GetStats()
	}
	return nil
}

// httpSessionRegistry tracks the per-client sessions the headless HTTP server
// creates and bounds them by idle TTL and an LRU cap (issue #25). create lazily
// builds the backend session on first use; evict reclaims one that has expired
// or been pushed out of the cap. It is safe for concurrent use.
type httpSessionRegistry struct {
	mu       sync.Mutex
	seen     map[string]time.Time // session id -> last access
	create   func(id string)
	evict    func(id string)
	maxIdle  time.Duration
	maxItems int
	now      func() time.Time
}

// touch records access to a session id, lazily creating it on first sight and
// reclaiming idle/over-cap sessions. The just-touched id is never evicted.
func (r *httpSessionRegistry) touch(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()

	// Reclaim sessions idle past the TTL.
	for sid, last := range r.seen {
		if sid != id && now.Sub(last) > r.maxIdle {
			r.evict(sid)
			delete(r.seen, sid)
		}
	}

	if _, exists := r.seen[id]; !exists {
		r.create(id)
	}
	r.seen[id] = now

	// Enforce the LRU cap, evicting the least-recently-used session (other than
	// the one just touched) until we are back under the limit.
	for len(r.seen) > r.maxItems {
		oldest, oldestAt := "", now
		for sid, last := range r.seen {
			if sid == id {
				continue
			}
			if oldest == "" || last.Before(oldestAt) {
				oldest, oldestAt = sid, last
			}
		}
		if oldest == "" {
			break
		}
		r.evict(oldest)
		delete(r.seen, oldest)
	}
}

// writeJSON encodes v as JSON with the given status. Using the encoder (instead
// of hand-formatting) guarantees valid JSON even when model output contains
// quotes, newlines or backslashes.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("http: encode response: %v", err)
	}
}

// HTTP headers/fields a client uses to name its session. A request that names
// none falls back to the shared "default" session.
const (
	httpSessionHeader = "X-Gogent-Session"
	httpSessionCookie = "gogent_session"
	httpSessionForm   = "session"
)

// clientSessionID derives the session id for a request so concurrent clients get
// isolated transcripts (issue #25). It prefers the header, then a cookie, then a
// form/query field, and falls back to "default". ParseForm must have been called
// for the form field to be visible on a POST body.
func clientSessionID(r *http.Request) string {
	if v := r.Header.Get(httpSessionHeader); v != "" {
		return sanitizeSessionID(v)
	}
	if c, err := r.Cookie(httpSessionCookie); err == nil && c.Value != "" {
		return sanitizeSessionID(c.Value)
	}
	if v := r.FormValue(httpSessionForm); v != "" {
		return sanitizeSessionID(v)
	}
	return "default"
}

// sanitizeSessionID reduces a client-supplied id to a bounded, safe token
// (alphanumerics plus -_.), so a hostile id can neither blow up memory nor leak
// into a filesystem path. An id that sanitizes to empty falls back to "default".
func sanitizeSessionID(s string) string {
	const maxLen = 128
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}

// isLoopbackAddr reports whether a RemoteAddr ("host:port") is a loopback
// address, used to gate the /exit kill switch to local callers.
func isLoopbackAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// newHTTPHandler builds the headless API. shutdown is invoked by an authorized
// /exit request; exitToken (from $GOGENT_HTTP_TOKEN), when set, additionally
// authorizes non-local callers that present a matching X-Gogent-Token header.
func newHTTPHandler(backend httpBackend, exitToken string, shutdown func()) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "healthy"})
	})

	mux.HandleFunc("/message", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Bound the request body so a malicious client can't exhaust memory.
		r.Body = http.MaxBytesReader(w, r.Body, httpMaxRequestBody)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid form", http.StatusBadRequest)
			return
		}

		message := r.FormValue("message")
		if message == "" {
			http.Error(w, "Missing message", http.StatusBadRequest)
			return
		}

		// Get model name from header or form value
		modelName := r.Header.Get("X-Gogent-Model")
		if modelName == "" {
			modelName = r.FormValue("model")
		}

		// Route the request to the caller's own session so concurrent clients are
		// isolated rather than multiplexed onto one shared transcript (issue #25).
		sessionID := clientSessionID(r)

		// Run the (long) model loop off the request goroutine so we can abandon
		// it the moment the client disconnects, instead of writing to a dead
		// connection. The request context is threaded into the loop so a
		// disconnect also cancels the in-flight model work rather than leaking it
		// (issue #24).
		type result struct {
			resp *model.CompletionResponse
			err  error
		}
		done := make(chan result, 1)
		go func() {
			resp, err := backend.SendMessage(r.Context(), sessionID, message, modelName)
			done <- result{resp, err}
		}()

		select {
		case <-r.Context().Done():
			return
		case res := <-done:
			if res.err != nil {
				writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": res.err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": res.resp.Content})
		}
	})

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		// Copy tool logs (non-nil so it always encodes as a JSON array).
		toolLogsMu.Lock()
		logs := make([]string, len(toolLogs))
		copy(logs, toolLogs)
		toolLogsMu.Unlock()

		writeJSON(w, http.StatusOK, map[string]any{
			"tool_logs": logs,
			"stats":     backend.Stats(clientSessionID(r)),
		})
	})

	mux.HandleFunc("/exit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Gate the kill switch: local callers always pass; remote callers must
		// present the configured token. Without either, refuse.
		authorized := isLoopbackAddr(r.RemoteAddr)
		if !authorized && exitToken != "" {
			tok := r.Header.Get("X-Gogent-Token")
			authorized = subtle.ConstantTimeCompare([]byte(tok), []byte(exitToken)) == 1
		}
		if !authorized {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "Shutdown initiated"})
		go shutdown()
	})

	return mux
}

func startHTTPServer(host string, port int, g *gogent.Gogent) {
	handler := newHTTPHandler(newGogentBackend(g), os.Getenv("GOGENT_HTTP_TOKEN"), func() {
		syscall.Kill(syscall.Getpid(), syscall.SIGINT)
	})

	server := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", host, port),
		Handler:           handler,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		IdleTimeout:       httpIdleTimeout,
	}

	fmt.Printf("HTTP server listening on http://%s:%d\n", host, port)
	fmt.Println("Endpoints:")
	fmt.Println("  GET  /health - Health check")
	fmt.Println("  POST /message - Send message (form-data: message=...)")
	fmt.Println("                  Set X-Gogent-Session (or cookie/session field) for an isolated session")
	fmt.Println("  GET  /status - Get tool execution logs and stats (per X-Gogent-Session)")
	fmt.Println("  POST /exit - Exit server (local-only, or X-Gogent-Token)")

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("HTTP server error: %v", err)
	}
}
