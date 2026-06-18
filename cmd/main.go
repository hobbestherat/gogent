package main

import (
	"flag"
	"fmt"
	"log"
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
		})

		// Run the TUI in a goroutine.
		go func() {
			if err := wb.Run(); err != nil {
				log.Printf("TUI error: %v", err)
			}
		}()
	}

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
	}()

	sig := <-sigChan
	fmt.Printf("\nReceived signal %v, shutting down...\n", sig)

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

func startHTTPServer(host string, port int, g *gogent.Gogent) {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"healthy"}`)
	})

	mux.HandleFunc("/message", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

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

		// Send message through Gogent to agent
		resp, err := g.SendMessageToSessionWithModel("default", "root", message, modelName)
		if err != nil {
			fmt.Fprintf(w, `{"success":false,"error":"%s"}`, err.Error())
			return
		}

		fmt.Fprintf(w, `{"success":true,"message":"%s"}`, resp.Content)
	})

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Get tool logs
		toolLogsMu.Lock()
		logs := make([]string, len(toolLogs))
		copy(logs, toolLogs)
		toolLogsMu.Unlock()

		// Get stats
		session := g.GetUserSession("default")
		var stats map[string]interface{}
		if session != nil {
			stats = session.GetStats()
		}

		fmt.Fprintf(w, `{"tool_logs":%v,"stats":%v}`, logs, stats)
	})

	mux.HandleFunc("/exit", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"message":"Shutdown initiated"}`)
		go func() {
			syscall.Kill(syscall.Getpid(), syscall.SIGINT)
		}()
	})

	server := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", host, port),
		Handler: mux,
	}

	fmt.Printf("HTTP server listening on http://%s:%d\n", host, port)
	fmt.Println("Endpoints:")
	fmt.Println("  GET  /health - Health check")
	fmt.Println("  POST /message - Send message (form-data: message=...)")
	fmt.Println("  GET  /status - Get tool execution logs and stats")
	fmt.Println("  GET  /exit - Exit server")

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("HTTP server error: %v", err)
	}
}
