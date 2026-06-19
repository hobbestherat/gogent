package main

import (
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
	"sync"
	"syscall"
	"time"

	"gogent/internal/agent"
	"gogent/internal/command"
	"gogent/internal/config"
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
				_, err := g.SendMessageToSessionWithModel(sessionID, "root", message, modelName)
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
					title := ls.Title
					if title == "" {
						title = ls.ID
					}
					out = append(out, tuipkg.RestoredSession{
						ID:       ls.ID,
						Title:    title,
						Messages: toChatMessages(ls.Transcripts["root"]),
					})
				}
				return out
			},
			LoadLayout: func() gogent.Layout { return g.LoadLayout() },
			SaveLayout: func(layout gogent.Layout) {
				if err := g.SaveLayout(layout); err != nil {
					log.Printf("save layout: %v", err)
				}
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
	// SendMessage runs the agent task loop for the default HTTP session and
	// returns the final assistant response.
	SendMessage(message, modelName string) (*model.CompletionResponse, error)
	// Stats returns aggregate counters for the default HTTP session, or nil.
	Stats() map[string]interface{}
}

// gogentBackend adapts a *gogent.Gogent to the httpBackend interface.
type gogentBackend struct{ g *gogent.Gogent }

func (b gogentBackend) SendMessage(message, modelName string) (*model.CompletionResponse, error) {
	return b.g.SendMessageToSessionWithModel("default", "root", message, modelName)
}

func (b gogentBackend) Stats() map[string]interface{} {
	if s := b.g.GetUserSession("default"); s != nil {
		return s.GetStats()
	}
	return nil
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

		// Run the (long) model loop off the request goroutine so we can abandon
		// it the moment the client disconnects, instead of writing to a dead
		// connection. The loop itself keeps running in the background — true
		// propagation of cancellation into the model layer is a follow-up.
		type result struct {
			resp *model.CompletionResponse
			err  error
		}
		done := make(chan result, 1)
		go func() {
			resp, err := backend.SendMessage(message, modelName)
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
			"stats":     backend.Stats(),
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
	handler := newHTTPHandler(gogentBackend{g}, os.Getenv("GOGENT_HTTP_TOKEN"), func() {
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
	fmt.Println("  GET  /status - Get tool execution logs and stats")
	fmt.Println("  POST /exit - Exit server (local-only, or X-Gogent-Token)")

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("HTTP server error: %v", err)
	}
}
