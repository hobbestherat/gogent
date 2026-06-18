package gogent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gogent/internal/agent"
	"gogent/internal/command"
	"gogent/internal/config"
	"gogent/internal/fileops"
	"gogent/internal/model"
	"gogent/internal/permission"
	"gogent/internal/skill"
	"gogent/internal/tool"
	"gogent/internal/vcs"
)

// Gogent is the main entry point for the agent system
type Gogent struct {
	userSessions     map[string]*agent.UserSession
	hooks            map[string]func(event HookEvent)
	mu               sync.RWMutex
	fileSystem       *fileops.FileSystem
	locationMutation *fileops.LocationMutation
	permissions      *permission.Service
	fileMutation     *fileops.FileMutation
	toolRegistry     *tool.ToolRegistry
	config           *config.Config
	workspaceRoot    string
	homeDir          string
	store            *SessionStore
	skills           *skill.SkillRegistry
	// agentsContext is the project AGENTS.md instruction text discovered at
	// startup, injected into every session's system prompt.
	agentsContext string
	// repoMap is the ranked symbol skeleton of the workspace, built at startup
	// and injected into every session's system prompt alongside agentsContext.
	repoMap string
	// gitRepo records whether the workspace is inside a git working tree,
	// detected once at startup. When true, a live `git status` summary is
	// injected into every session's system prompt.
	gitRepo bool
	// sessionTitles records a human-friendly title per session for persistence.
	sessionTitles map[string]string
}

// HookEvent represents an event that triggers hooks
type HookEvent struct {
	Type        HookEventType
	SessionID   string
	AgentID     string
	Token       string
	Response    string
	Usage       *model.TokenUsage
	Error       *model.ModelError
	State       agent.AgentState
	Compression *model.CompressionInfo
}

type HookEventType string

const (
	HookTokenReceived    HookEventType = "token_received"
	HookResponseComplete HookEventType = "response_complete"
	HookError            HookEventType = "error"
	HookStateChange      HookEventType = "state_change"
	HookCompression      HookEventType = "compression"
	HookToolCall         HookEventType = "tool_call"
)

// NewGogent creates a new Gogent instance.
//
// File operations (read/write/edit/list) are rooted at the directory gogent was
// launched from (the process working directory) so they stay in sync with the
// shell tool, which inherits that same cwd. If the working directory cannot be
// determined we fall back to ~/.gogent/workspace.
func NewGogent(homeDir string) *Gogent {
	workspaceRoot, err := os.Getwd()
	if err != nil || workspaceRoot == "" {
		workspaceRoot = filepath.Join(homeDir, ".gogent", "workspace")
	}
	return NewGogentWithWorkspace(homeDir, workspaceRoot)
}

// NewGogentWithWorkspace is like NewGogent but roots all file operations at an
// explicit workspace directory instead of the process working directory. This
// keeps production code using the launch directory (via NewGogent) while letting
// tests and embedders point gogent at an isolated sandbox.
func NewGogentWithWorkspace(homeDir, workspaceRoot string) *Gogent {
	if workspaceRoot == "" {
		workspaceRoot = filepath.Join(homeDir, ".gogent", "workspace")
	}

	// Load config
	cfg, err := config.LoadConfig(homeDir)
	if err != nil {
		fmt.Printf("Warning: Failed to load config: %v, using defaults\n", err)
		cfg = config.GetDefaultConfig()
	}

	g := &Gogent{
		userSessions:  make(map[string]*agent.UserSession),
		hooks:         make(map[string]func(event HookEvent)),
		toolRegistry:  tool.NewToolRegistry(),
		config:        cfg,
		workspaceRoot: workspaceRoot,
		homeDir:       homeDir,
		sessionTitles: make(map[string]string),
	}

	// Session transcript persistence (best-effort; a nil store disables it).
	if store, err := NewSessionStore(filepath.Join(homeDir, ".gogent", "sessions")); err == nil {
		g.store = store
	} else {
		fmt.Printf("Warning: session persistence disabled: %v\n", err)
	}

	// Initialize file operations services
	g.fileSystem = fileops.NewFileSystem(workspaceRoot)
	g.locationMutation = fileops.NewLocationMutation(workspaceRoot)
	g.permissions = permission.New(filepath.Join(homeDir, ".gogent"))
	// Default posture: file reads/writes inside the workspace are allowed
	// without prompting. Paths outside the workspace, shell, and sub-agents fall
	// through to "ask" (resolved interactively, or denied when headless).
	g.permissions.AddRule(permission.Rule{Action: string(permission.ActionRead), Resource: "*", Effect: string(permission.EffectAllow)})
	g.permissions.AddRule(permission.Rule{Action: string(permission.ActionWrite), Resource: "*", Effect: string(permission.EffectAllow)})
	g.fileMutation = fileops.NewFileMutation(g.fileSystem, g.locationMutation)

	// Load skills (user + built-in) and discover project AGENTS.md instructions
	// before building the tool registry so the skill tool and system-context
	// provider can see them.
	g.skills = skill.NewSkillRegistry()
	_ = g.skills.LoadSkills(filepath.Join(homeDir, ".gogent", "skills"))
	_ = g.skills.LoadSkills(filepath.Join(workspaceRoot, "skills"))
	g.agentsContext = renderAgentsContext(discoverAgentsDocs(workspaceRoot, filepath.Join(homeDir, ".gogent")))
	g.repoMap = buildRepoMap(workspaceRoot)
	g.gitRepo = vcs.IsRepo(workspaceRoot)

	// Initialize tool registry with file tools
	g.initializeToolRegistry()

	return g
}

// GetConfig returns the current configuration
func (g *Gogent) GetConfig() *config.Config {
	return g.config
}

// GetWorkspaceRoot returns the directory file/shell operations run in.
func (g *Gogent) GetWorkspaceRoot() string {
	return g.workspaceRoot
}

func (g *Gogent) initializeToolRegistry() {

	// Register file operation tools
	g.toolRegistry.Register(&tool.Tool{
		Name:        "read",
		Description: "Read a file from the workspace. Use this when the user asks you to read, view, or display a file.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}},
			"required":   []string{"path"},
		},
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			path, ok := args["path"].(string)
			if !ok {
				return nil, fmt.Errorf("path argument is required")
			}

			auth, err := fileops.CheckFileAccess(g.permissions, g.locationMutation, false, path)
			if err != nil {
				return nil, err
			}

			content, err := g.fileSystem.ReadFile(path, auth)
			if err != nil {
				return nil, fmt.Errorf("failed to read file: %v", err)
			}

			return map[string]interface{}{
				"success": true,
				"path":    path,
				"content": content,
			}, nil
		},
	})

	g.toolRegistry.Register(&tool.Tool{
		Name:        "write",
		Description: "Write content to a file. Use this when the user asks you to create or overwrite a file.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}, "content": map[string]interface{}{"type": "string"}},
			"required":   []string{"path", "content"},
		},
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			path, ok := args["path"].(string)
			if !ok {
				return nil, fmt.Errorf("path argument is required")
			}
			content, ok := args["content"].(string)
			if !ok {
				return nil, fmt.Errorf("content argument is required")
			}

			auth, err := fileops.CheckFileAccess(g.permissions, g.locationMutation, true, path)
			if err != nil {
				return nil, err
			}

			if err := g.fileMutation.WriteFile(path, content, auth); err != nil {
				return nil, fmt.Errorf("failed to write file: %v", err)
			}

			return map[string]interface{}{
				"success": true,
				"path":    path,
			}, nil
		},
	})

	g.toolRegistry.Register(&tool.Tool{
		Name:        "edit",
		Description: "Edit a file by replacing exact text. Use this for precise edits.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}, "find": map[string]interface{}{"type": "string"}, "replace": map[string]interface{}{"type": "string"}},
			"required":   []string{"path", "find", "replace"},
		},
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			path, ok := args["path"].(string)
			if !ok {
				return nil, fmt.Errorf("path argument is required")
			}
			find, ok := args["find"].(string)
			if !ok {
				return nil, fmt.Errorf("find argument is required")
			}
			replace, ok := args["replace"].(string)
			if !ok {
				return nil, fmt.Errorf("replace argument is required")
			}

			auth, err := fileops.CheckFileAccess(g.permissions, g.locationMutation, true, path)
			if err != nil {
				return nil, err
			}

			err = g.fileMutation.EditFile(path, find, replace, auth)
			if err != nil {
				return nil, fmt.Errorf("failed to edit file: %v", err)
			}

			return map[string]interface{}{
				"success": true,
				"path":    path,
			}, nil
		},
	})

	g.toolRegistry.RegisterCalcTool()
	if g.config != nil {
		g.toolRegistry.ShellTimeout = time.Duration(g.config.Timeouts.ToolSecondsOrDefault()) * time.Second
	}
	g.toolRegistry.WorkspaceRoot = g.workspaceRoot
	g.toolRegistry.NetworkTimeout = g.toolRegistry.ShellTimeout
	g.toolRegistry.Permission = g.permissions
	g.toolRegistry.RegisterShellTool()
	g.toolRegistry.RegisterWebFetchTool()
	g.toolRegistry.RegisterGitTool()

	g.toolRegistry.Register(&tool.Tool{
		Name:        "spawn_subagent",
		Description: "Delegate work to one or more sub-agents. In one-shot mode they must end with SUCCESS:/FAILURE:. In interactive mode they may return CLARIFY: questions.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":     map[string]interface{}{"type": "string", "description": "Sub-agent name (single-task mode)"},
				"task":     map[string]interface{}{"type": "string", "description": "Task description (single-task mode)"},
				"subtasks": map[string]interface{}{"type": "array", "description": "Optional parallel batch: [{name, task}, ...]"},
			},
		},
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			session := g.GetUserSession(ctx.SessionID)
			if session == nil {
				return nil, fmt.Errorf("session not found: %s", ctx.SessionID)
			}

			rawBatch, hasBatch := args["subtasks"]
			if hasBatch {
				items, ok := rawBatch.([]interface{})
				if !ok {
					return nil, fmt.Errorf("subtasks must be an array")
				}
				type out struct {
					Name   string `json:"name"`
					Task   string `json:"task"`
					Result string `json:"result,omitempty"`
					Error  string `json:"error,omitempty"`
				}
				results := make([]out, len(items))
				var wg sync.WaitGroup
				for i, raw := range items {
					i, raw := i, raw
					obj, ok := raw.(map[string]interface{})
					if !ok {
						results[i].Error = "invalid subtask item"
						continue
					}
					name, _ := obj["name"].(string)
					task, _ := obj["task"].(string)
					results[i].Name = name
					results[i].Task = task
					if strings.TrimSpace(task) == "" {
						results[i].Error = "missing subtask.task"
						continue
					}
					wg.Add(1)
					go func() {
						defer wg.Done()
						text, err := session.SpawnSubAgent(ctx.AgentID, name, task, g.SubAgentOneShot())
						if err != nil {
							results[i].Error = err.Error()
							return
						}
						results[i].Result = text
					}()
				}
				wg.Wait()
				return map[string]interface{}{"success": true, "mode": map[string]bool{"one_shot": g.SubAgentOneShot(), "interactive": !g.SubAgentOneShot()}, "results": results}, nil
			}

			name, _ := args["name"].(string)
			task, _ := args["task"].(string)
			if strings.TrimSpace(task) == "" {
				return nil, fmt.Errorf("task is required")
			}
			result, err := session.SpawnSubAgent(ctx.AgentID, name, task, g.SubAgentOneShot())
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{
				"success": true,
				"name":    name,
				"task":    task,
				"mode":    map[string]bool{"one_shot": g.SubAgentOneShot(), "interactive": !g.SubAgentOneShot()},
				"result":  result,
			}, nil
		},
	})

	// Interactive (experimental) sub-agent coordination tools. They are only
	// surfaced to a session whose execution model is "interactive" (the registry
	// is filtered per session in CreateUserSession), but are registered globally
	// here so that filtering can include/exclude them by name.
	g.toolRegistry.Register(&tool.Tool{
		Name:        "launch_agent",
		Description: "Launch an asynchronous interactive sub-agent. Returns an agent_id immediately; the agent keeps running concurrently.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string"},
				"task": map[string]interface{}{"type": "string"},
			},
			"required": []string{"task"},
		},
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			session := g.GetUserSession(ctx.SessionID)
			if session == nil {
				return nil, fmt.Errorf("session not found: %s", ctx.SessionID)
			}
			name, _ := args["name"].(string)
			task, _ := args["task"].(string)
			if strings.TrimSpace(task) == "" {
				return nil, fmt.Errorf("task is required")
			}
			id, err := session.LaunchInteractiveAgent(ctx.AgentID, name, task)
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{"success": true, "agent_id": id}, nil
		},
	})

	g.toolRegistry.Register(&tool.Tool{
		Name:        "agent_status",
		Description: "Query the status (running/waiting/completed/failed) and last result of an interactive sub-agent.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"agent_id": map[string]interface{}{"type": "string"}},
			"required":   []string{"agent_id"},
		},
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			session := g.GetUserSession(ctx.SessionID)
			if session == nil {
				return nil, fmt.Errorf("session not found: %s", ctx.SessionID)
			}
			id, _ := args["agent_id"].(string)
			status, result, err := session.InteractiveAgentStatus(id)
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{"success": true, "agent_id": id, "status": string(status), "result": result}, nil
		},
	})

	g.toolRegistry.Register(&tool.Tool{
		Name:        "agent_send",
		Description: "Send a message to an interactive sub-agent, e.g. to answer its CLARIFY question or give more direction.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"agent_id": map[string]interface{}{"type": "string"},
				"message":  map[string]interface{}{"type": "string"},
			},
			"required": []string{"agent_id", "message"},
		},
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			session := g.GetUserSession(ctx.SessionID)
			if session == nil {
				return nil, fmt.Errorf("session not found: %s", ctx.SessionID)
			}
			id, _ := args["agent_id"].(string)
			message, _ := args["message"].(string)
			if err := session.SendToInteractiveAgent(id, message); err != nil {
				return nil, err
			}
			return map[string]interface{}{"success": true, "agent_id": id}, nil
		},
	})

	g.toolRegistry.Register(&tool.Tool{
		Name:        "agent_terminate",
		Description: "Terminate a running interactive sub-agent by id.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"agent_id": map[string]interface{}{"type": "string"}},
			"required":   []string{"agent_id"},
		},
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			session := g.GetUserSession(ctx.SessionID)
			if session == nil {
				return nil, fmt.Errorf("session not found: %s", ctx.SessionID)
			}
			id, _ := args["agent_id"].(string)
			if err := session.TerminateInteractiveAgent(id); err != nil {
				return nil, err
			}
			return map[string]interface{}{"success": true, "agent_id": id}, nil
		},
	})

	g.toolRegistry.Register(&tool.Tool{
		Name:        "wait_agent_event",
		Description: "Block until an interactive sub-agent finishes or asks for clarification, then return that event. Optional timeout_ms.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"timeout_ms": map[string]interface{}{"type": "number"}},
		},
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			session := g.GetUserSession(ctx.SessionID)
			if session == nil {
				return nil, fmt.Errorf("session not found: %s", ctx.SessionID)
			}
			timeout := time.Duration(0)
			if v, ok := args["timeout_ms"].(float64); ok && v > 0 {
				timeout = time.Duration(v) * time.Millisecond
			}
			ev, ok := session.NextAgentEvent(timeout)
			if !ok {
				return map[string]interface{}{"success": true, "timed_out": true}, nil
			}
			return map[string]interface{}{
				"success":  true,
				"agent_id": ev.AgentID,
				"name":     ev.Name,
				"type":     string(ev.Type),
				"text":     ev.Text,
			}, nil
		},
	})

	g.toolRegistry.Register(&tool.Tool{
		Name:        "structured_output",
		Description: "Use this tool to return your final response. Include your response text and any tool calls.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"response": map[string]interface{}{"type": "string"}, "final": map[string]interface{}{"type": "boolean"}},
			"required":   []string{"response"},
		},
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			response, _ := args["response"].(string)
			final, _ := args["final"].(bool)

			return map[string]interface{}{
				"success":  true,
				"response": response,
				"final":    final,
			}, nil
		},
	})

	// Register the skill tool only when skills are loaded, so models without
	// skills aren't offered a useless tool.
	if g.skills != nil && len(g.skills.ListSkills()) > 0 {
		g.toolRegistry.Register(&tool.Tool{
			Name:        "skill",
			Description: "Load the full instructions for a named skill before performing a task it covers. Returns the skill's markdown content. Use the skill names listed under 'Available skills'.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}},
				"required":   []string{"name"},
			},
			Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
				name, ok := args["name"].(string)
				if !ok || name == "" {
					return nil, fmt.Errorf("name argument is required")
				}
				sk := g.skills.GetSkill(name)
				if sk == nil || !g.skills.IsSkillActive(name) {
					g.skills.RecordSkillUsage(name, false)
					return nil, fmt.Errorf("no active skill named %q", name)
				}
				g.skills.RecordSkillUsage(name, true)
				return map[string]interface{}{
					"success": true,
					"name":    sk.Name,
					"content": sk.Content,
				}, nil
			},
		})
	}
}

// CreateUserSession creates a new user session
func (g *Gogent) CreateUserSession(id string, rootAgent *agent.Agent) *agent.UserSession {
	g.mu.Lock()
	defer g.mu.Unlock()

	userSession := agent.NewUserSession(id, rootAgent)
	userSession.SetSubAgentConfig(g.config.SubAgents)
	userSession.SetSubAgentTimeout(time.Duration(g.config.Timeouts.SubAgentSecondsOrDefault()) * time.Second)
	userSession.SetSystemContextProvider(g.buildSystemContext)
	// Route context compression to the configured fast model when its role
	// resolves to a model other than the session's primary one; otherwise leave
	// it unset so compaction keeps using the primary model (no behavior change).
	if g.config != nil {
		if m := g.config.ModelForRole(config.RoleCompression); m != nil && m.Name != g.config.DefaultModel {
			userSession.SetCompressionCompleter(g.buildConnection(m))
		}
	}
	// Hand the root agent a tool registry filtered to the active execution model
	// so the model is only offered the coordination tools it is instructed to use.
	rootAgent.SetToolRegistry(g.toolRegistryForMode(g.config.SubAgents))

	// Keep session token stats updated as the model session streams usage.
	if rootAgent.ThoughtTrain != nil {
		rootAgent.ThoughtTrain.AddTokenCallback(func(promptTokens, completionTokens int) {
			userSession.AddTokenUsage(promptTokens, completionTokens)
		})
	}

	// Set tool callback to increment tool call count
	userSession.SetToolCallback(func(toolName string, args map[string]interface{}) error {
		userSession.IncrementToolCall()
		return nil
	})

	g.userSessions[id] = userSession
	return userSession
}

// buildConnection builds a model connection from a model config and applies the
// configured global model timeout so every connection honors the user setting.
func (g *Gogent) buildConnection(cfg *config.ModelConfig) *model.ModelConnection {
	conn := model.NewModelConnectionFromConfig(cfg)
	if g.config != nil {
		conn.SetTimeout(time.Duration(g.config.Timeouts.ModelSecondsOrDefault()) * time.Second)
	}
	return conn
}

// CompleterForRole resolves the model backend for an auxiliary task role (see
// the config.Role* constants) and returns a ready completer for it. Roles mapped
// to the fast model — or defaulted to it when a fast model is configured — get
// the small/fast backend; otherwise the primary model is used. It always returns
// a usable completer, so callers (web_fetch summarization, JSON repair, title
// generation, …) need no fallback of their own.
func (g *Gogent) CompleterForRole(role string) model.Completer {
	if g.config != nil {
		if m := g.config.ModelForRole(role); m != nil {
			return g.buildConnection(m)
		}
	}
	return g.defaultConnection()
}

// NewSession creates a fully-wired user session (root agent + model session)
// under the given id and registers it. It is the convenience entry point used by
// UIs that spawn sessions on demand. If a session with the id already exists it
// is returned unchanged.
func (g *Gogent) NewSession(id string) *agent.UserSession {
	g.mu.RLock()
	existing := g.userSessions[id]
	g.mu.RUnlock()
	if existing != nil {
		return existing
	}

	conn := g.defaultConnection()
	sess := model.NewModelSession("main", conn)
	rootAgent := agent.NewAgent("root", sess)
	rootAgent.SetState(agent.StateIdle)
	return g.CreateUserSession(id, rootAgent)
}

// RemoveSession deletes a user session by id.
func (g *Gogent) RemoveSession(id string) {
	g.mu.Lock()
	delete(g.userSessions, id)
	delete(g.sessionTitles, id)
	g.mu.Unlock()

	// Archive the on-disk transcript so it is not auto-restored next start.
	if g.store != nil && id != "default" {
		if err := g.store.Archive(id); err != nil {
			fmt.Printf("Warning: failed to archive session %s: %v\n", id, err)
		}
	}
}

// SetSessionTitle records a human-friendly title used when persisting a session.
func (g *Gogent) SetSessionTitle(id, title string) {
	g.mu.Lock()
	g.sessionTitles[id] = title
	g.mu.Unlock()
}

// persistSession writes the session's transcript to disk (best-effort). The
// "default" HTTP session is intentionally not persisted.
func (g *Gogent) persistSession(id string) {
	if g.store == nil || id == "default" {
		return
	}
	g.mu.RLock()
	us := g.userSessions[id]
	title := g.sessionTitles[id]
	g.mu.RUnlock()
	if us == nil {
		return
	}
	if title == "" {
		title = id
	}
	if err := g.store.Save(us, title); err != nil {
		fmt.Printf("Warning: failed to persist session %s: %v\n", id, err)
	}
}

// AgentTranscript returns the message transcript of a specific agent (root or a
// sub-agent) within a session — used by the UI to show a sub-agent's internal
// monologue. Returns nil if the session or agent is unknown.
func (g *Gogent) AgentTranscript(sessionID, agentID string) []model.Message {
	g.mu.RLock()
	us := g.userSessions[sessionID]
	g.mu.RUnlock()
	if us == nil {
		return nil
	}
	a := us.GetAgent(agentID)
	if a == nil || a.ThoughtTrain == nil {
		return nil
	}
	return a.ThoughtTrain.GetTranscript()
}

// RestoreSessions reads any non-archived session files from disk, rebuilds the
// corresponding in-memory UserSessions (seeding the root agent's transcript so
// the conversation can continue), and returns the loaded sessions so a UI can
// re-open windows for them. The "default" session is skipped.
func (g *Gogent) RestoreSessions() []LoadedSession {
	if g.store == nil {
		return nil
	}
	loaded, err := g.store.ListActive()
	if err != nil {
		fmt.Printf("Warning: failed to list sessions: %v\n", err)
		return nil
	}
	var restored []LoadedSession
	for _, ls := range loaded {
		if ls.ID == "" || ls.ID == "default" {
			continue
		}
		g.mu.RLock()
		_, exists := g.userSessions[ls.ID]
		g.mu.RUnlock()
		if exists {
			continue
		}

		conn := g.defaultConnection()
		sess := model.NewModelSession("main", conn)
		if msgs := ls.Transcripts["root"]; len(msgs) > 0 {
			sess.ReplaceTranscript(msgs)
		}
		rootAgent := agent.NewAgent("root", sess)
		rootAgent.SetState(agent.StateIdle)
		g.CreateUserSession(ls.ID, rootAgent)
		g.SetSessionTitle(ls.ID, ls.Title)
		g.store.Adopt(ls.ID, ls.File) // continue appending to the same file
		restored = append(restored, ls)
	}
	return restored
}

// defaultConnection builds a model connection for the configured default model.
func (g *Gogent) defaultConnection() *model.ModelConnection {
	if g.config != nil {
		var def *config.ModelConfig
		for _, m := range g.config.ModelConfigs {
			if m.Name == g.config.DefaultModel {
				def = m
				break
			}
		}
		if def == nil && len(g.config.ModelConfigs) > 0 {
			def = g.config.ModelConfigs[0]
		}
		if def != nil {
			return g.buildConnection(def)
		}
	}
	return model.NewModelConnection()
}

// GetFileSystem returns the file system service
func (g *Gogent) GetFileSystem() *fileops.FileSystem {
	return g.fileSystem
}

// GetLocationMutation returns the location mutation service
func (g *Gogent) GetLocationMutation() *fileops.LocationMutation {
	return g.locationMutation
}

// GetPermissionService returns the permission service
func (g *Gogent) GetPermissionService() *permission.Service {
	return g.permissions
}

// GetSkillRegistry returns the shared skill registry.
func (g *Gogent) GetSkillRegistry() *skill.SkillRegistry {
	return g.skills
}

// GetFileMutation returns the file mutation service
func (g *Gogent) GetFileMutation() *fileops.FileMutation {
	return g.fileMutation
}

// GetToolRegistry returns the tool registry
func (g *Gogent) GetToolRegistry() *tool.ToolRegistry {
	return g.toolRegistry
}

// GetUserSession gets a user session by ID
func (g *Gogent) GetUserSession(id string) *agent.UserSession {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.userSessions[id]
}

// GetSystemPrompt gets the system prompt for the agent
func (g *Gogent) GetSystemPrompt(sessionID, agentID string) string {
	return `You are Gogent, a helpful AI assistant with access to powerful tools.

## Working Directory

All file and shell operations run in the current working directory:
` + g.workspaceRoot + `
Use paths relative to this directory (e.g. "main.go", "src/app.go"). The shell
tool also runs here, so its output stays in sync with read/write/edit.

## Available Tools

### read
Read a file from the workspace. Use this when the user asks you to read, view, or display a file's contents.
- Input: {"path": "string"} - Path to the file
- Example: {"tool": "read", "args": {"path": "hello.txt"}}

### write
Write content to a file. Use this when the user asks you to create or overwrite a file.
- Input: {"path": "string", "content": "string"}
- Example: {"tool": "write", "args": {"path": "hello.txt", "content": "Hello World!"}}

### edit
Edit a file by replacing exact text. Use this for precise edits. The find string must match exactly.
- Input: {"path": "string", "find": "string", "replace": "string"}
- Example: {"tool": "edit", "args": {"path": "hello.txt", "find": "World", "replace": "Universe"}}

### calc
 	Calculate mathematical expressions. Use this when the user asks you to calculate, compute, or solve a math problem.
 	- Input: {"expression": "string"} - A mathematical expression like "5+5" or "10*20/5"
 	- Example: {"tool": "calc", "args": {"expression": "5+5"}}
 	- Returns: {"success": true, "result": {"expression": "...", "result": "evaluated: ..."}}
 	- Supports: +, -, *, /, parentheses for grouping

### shell
 	Execute shell commands. Use this when you need to run shell commands like curl, wget, ls, grep, etc.
 	- Input: {"command": "string"} - A shell command string
 	- Example: {"tool": "shell", "args": {"command": "curl -s https://unsorted.ch/account/api/info"}}
 	- Returns: {"command": "...", "stdout": "...", "stderr": "...", "exit_code": 0, "timeout": false, "error": null}
 	- Timeout: 5 minutes, max output: 1MB

## How to Use Tools

When the user asks you to do something, determine which tool(s) to use and output a JSON object:

{"tool": "tool_name", "args": {"key": "value"}}

## Tool Results

	After you call a tool, include the tool result in your next response so the user can see it.

## Final Responses

	When you have completed the task or need to provide a final answer, use:

	{"response": "Your final response here", "final": true}
`
}

// ListSessions returns all session IDs
func (g *Gogent) ListSessions() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()

	sessions := make([]string, 0, len(g.userSessions))
	for id := range g.userSessions {
		sessions = append(sessions, id)
	}
	return sessions
}

// ListBackendModels asks a configured backend which models it serves, using the
// connector's optional ModelLister capability (OpenAI/OpenRouter "GET /v1/models").
// modelName selects which configured backend to query; empty means the default.
// Backends that do not implement the endpoint return an error, which the caller
// can treat as "use the configured model".
func (g *Gogent) ListBackendModels(modelName string) ([]model.ModelInfo, error) {
	g.mu.RLock()
	cfg := g.config
	g.mu.RUnlock()
	if cfg == nil {
		return nil, fmt.Errorf("no configuration available")
	}

	target := modelName
	if target == "" {
		target = cfg.DefaultModel
	}

	var selected *config.ModelConfig
	for _, m := range cfg.ModelConfigs {
		if m.Name == target {
			selected = m
			break
		}
	}
	if selected == nil && len(cfg.ModelConfigs) > 0 {
		selected = cfg.ModelConfigs[0]
	}
	if selected == nil {
		return nil, fmt.Errorf("no model backend configured")
	}

	conn := g.buildConnection(selected)
	lister, ok := model.Connector(conn).(model.ModelLister)
	if !ok {
		return nil, fmt.Errorf("backend %q does not support model listing", selected.Name)
	}
	return lister.ListModels()
}

// AggregateStats sums token and tool-call counters across every active session.
// The TUI spawns work in dynamically-created sessions (session-1, session-2, …),
// so the only meaningful "grand total" is the sum across all of them.
func (g *Gogent) AggregateStats() map[string]int {
	// Snapshot the session pointers under the registry lock, then release it
	// before touching each session's own lock to avoid lock-ordering issues.
	g.mu.RLock()
	sessions := make([]*agent.UserSession, 0, len(g.userSessions))
	for _, s := range g.userSessions {
		sessions = append(sessions, s)
	}
	g.mu.RUnlock()

	total := map[string]int{"tokens_in": 0, "tokens_out": 0, "tool_calls": 0, "fast_tokens_in": 0, "fast_tokens_out": 0}
	for _, s := range sessions {
		stats := s.GetStats()
		if v, ok := stats["tokens_in"].(int); ok {
			total["tokens_in"] += v
		}
		if v, ok := stats["tokens_out"].(int); ok {
			total["tokens_out"] += v
		}
		if v, ok := stats["tool_calls"].(int); ok {
			total["tool_calls"] += v
		}
		if v, ok := stats["fast_tokens_in"].(int); ok {
			total["fast_tokens_in"] += v
		}
		if v, ok := stats["fast_tokens_out"].(int); ok {
			total["fast_tokens_out"] += v
		}
	}
	return total
}

// SendMessageToSession sends a message to a session and executes any tool calls
// This uses the ExecuteTaskLoop for proper multi-turn tool calling support
func (g *Gogent) SendMessageToSession(sessionID, agentID, message string) (*model.CompletionResponse, error) {
	return g.SendMessageToSessionWithModel(sessionID, agentID, message, "")
}

// SendMessageToSessionWithModel sends a message to a session with a specific model
// This uses the ExecuteTaskLoop for proper multi-turn tool calling support
func (g *Gogent) SendMessageToSessionWithModel(sessionID, agentID, message, modelName string) (*model.CompletionResponse, error) {
	g.mu.RLock()
	userSession, exists := g.userSessions[sessionID]
	cfg := g.config
	g.mu.RUnlock()

	if !exists {
		return nil, &SessionNotFoundError{ID: sessionID}
	}

	// Get the tool registry from the agent
	ag := userSession.GetAgent(agentID)
	if ag == nil {
		return nil, &SessionNotFoundError{ID: agentID}
	}

	// Find the model config for the selected model
	var selectedConfig *config.ModelConfig
	if modelName != "" {
		// Prefer an exact unique-name match so distinct endpoints sharing the
		// same underlying model id can still be selected individually.
		for _, m := range cfg.ModelConfigs {
			if m.Name == modelName {
				selectedConfig = m
				break
			}
		}
		if selectedConfig == nil {
			for _, m := range cfg.ModelConfigs {
				if m.Model == modelName {
					selectedConfig = m
					break
				}
			}
		}
	}

	// If no config found, use default
	if selectedConfig == nil {
		for _, m := range cfg.ModelConfigs {
			if m.Name == cfg.DefaultModel {
				selectedConfig = m
				break
			}
		}
	}

	// Point the agent's existing session at the selected model, preserving the
	// conversation transcript across user messages. Only build a fresh session
	// if the agent somehow has none.
	if selectedConfig != nil {
		newModel := g.buildConnection(selectedConfig)
		if ag.ThoughtTrain == nil {
			ag.ThoughtTrain = model.NewModelSession("session", newModel)
			ag.ThoughtTrain.AddTokenCallback(func(promptTokens, completionTokens int) {
				userSession.AddTokenUsage(promptTokens, completionTokens)
			})
		} else {
			ag.ThoughtTrain.Resume(newModel)
		}
		// Calibrate compaction against the model's input context window, not its
		// max_tokens output cap (they are independent — a sane output cap like 4096
		// must not be mistaken for the context window).
		ag.ThoughtTrain.SetMaxContextLength(selectedConfig.ContextWindowOrDefault())
	}

	// Execute the task loop with multi-turn tool calling support
	// The last response may be a tool call or a final response
	responses, err := userSession.ExecuteTaskLoopWithModel(agentID, message, selectedConfig)
	if err != nil {
		return nil, err
	}

	// Persist the updated transcript (best-effort) for crash recovery.
	g.persistSession(sessionID)

	// Return the last response (this will be the final response from the model)
	if len(responses) > 0 {
		return responses[len(responses)-1], nil
	}

	return nil, fmt.Errorf("no responses generated")
}

// CountMessages counts messages in a session
func (g *Gogent) CountMessages(sessionID string) int {
	g.mu.RLock()
	userSession, exists := g.userSessions[sessionID]
	g.mu.RUnlock()

	if !exists {
		return 0
	}

	return userSession.CountMessages()
}

// AddHook adds a hook
func (g *Gogent) AddHook(hookID string, hook func(event HookEvent)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hooks[hookID] = hook
}

// RemoveHook removes a hook
func (g *Gogent) RemoveHook(hookID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.hooks, hookID)
}

// NotifyHooks notifies all hooks
func (g *Gogent) NotifyHooks(event HookEvent) {
	g.mu.RLock()
	hooks := make([]func(event HookEvent), 0, len(g.hooks))
	for _, hook := range g.hooks {
		hooks = append(hooks, hook)
	}
	g.mu.RUnlock()

	for _, hook := range hooks {
		hook(event)
	}
}

// RegisterFileTools registers file operation tools with the command registry
func (g *Gogent) RegisterFileTools(cmdRegistry *command.CommandRegistry) {
	// Create tools
	readTool := fileops.NewReadTool(g.GetFileSystem(), g.GetLocationMutation(), g.GetPermissionService())
	writeTool := fileops.NewWriteTool(g.GetFileMutation(), g.GetLocationMutation(), g.GetPermissionService())
	editTool := fileops.NewEditTool(g.GetFileMutation(), g.GetLocationMutation(), g.GetPermissionService())

	// Register as commands
	cmdRegistry.Register(&command.InternalCommand{
		Name:        "read",
		Description: "Read a file",
		Handler: func(ctx context.Context, args []string) (*command.CommandResult, error) {
			if len(args) < 1 {
				return &command.CommandResult{Success: false, Stderr: "missing path", ExitCode: 1}, errors.New("missing path")
			}
			result, err := readTool.Execute(map[string]interface{}{"path": args[0]})
			if err != nil {
				return &command.CommandResult{Success: false, Stderr: err.Error(), ExitCode: 1}, err
			}
			return &command.CommandResult{Success: true, Stdout: result.(string), ExitCode: 0}, nil
		},
	})

	cmdRegistry.Register(&command.InternalCommand{
		Name:        "write",
		Description: "Write content to a file",
		Handler: func(ctx context.Context, args []string) (*command.CommandResult, error) {
			if len(args) < 2 {
				return &command.CommandResult{Success: false, Stderr: "missing path or content", ExitCode: 1}, errors.New("missing path or content")
			}
			result, err := writeTool.Execute(map[string]interface{}{
				"path":    args[0],
				"content": args[1],
			})
			if err != nil {
				return &command.CommandResult{Success: false, Stderr: err.Error(), ExitCode: 1}, err
			}
			return &command.CommandResult{Success: true, Stdout: fmt.Sprintf("%v", result), ExitCode: 0}, nil
		},
	})

	cmdRegistry.Register(&command.InternalCommand{
		Name:        "edit",
		Description: "Edit a file by replacing text",
		Handler: func(ctx context.Context, args []string) (*command.CommandResult, error) {
			if len(args) < 3 {
				return &command.CommandResult{Success: false, Stderr: "missing path, find, or replace", ExitCode: 1}, errors.New("missing arguments")
			}
			result, err := editTool.Execute(map[string]interface{}{
				"path":    args[0],
				"find":    args[1],
				"replace": args[2],
			})
			if err != nil {
				return &command.CommandResult{Success: false, Stderr: err.Error(), ExitCode: 1}, err
			}
			return &command.CommandResult{Success: true, Stdout: fmt.Sprintf("%v", result), ExitCode: 0}, nil
		},
	})
}

// SessionNotFoundError represents an error when a session is not found
type SessionNotFoundError struct {
	ID string
}

func (e *SessionNotFoundError) Error() string {
	return "session not found: " + e.ID
}

// ParseToolCall parses a tool call from a model response
func (g *Gogent) ParseToolCall(response string) (*tool.ToolCall, error) {
	// Try to parse as JSON first (structured output)
	var toolCall tool.ToolCall
	if err := json.Unmarshal([]byte(response), &toolCall); err == nil {
		return &toolCall, nil
	}

	// Fallback: try to extract JSON from response
	if extracted := extractJSON(response); extracted != "" {
		if err := json.Unmarshal([]byte(extracted), &toolCall); err == nil {
			return &toolCall, nil
		}
	}

	return nil, fmt.Errorf("no valid tool call found in response")
}

func extractJSON(text string) string {
	// Look for JSON in triple backticks
	start := -1
	end := -1

	for i := 0; i < len(text)-2; i++ {
		if text[i] == '`' && text[i+1] == '`' && text[i+2] == '`' {
			if start == -1 {
				start = i + 3
			} else {
				end = i
				break
			}
		}
	}

	if start != -1 && end != -1 && end > start {
		jsonStr := text[start:end]
		if idx := strings.Index(jsonStr, "{"); idx != -1 {
			return extractJSONFrom(jsonStr[idx:])
		}
	}

	// Try to find JSON without backticks
	if idx := strings.Index(text, "{"); idx != -1 {
		return extractJSONFrom(text[idx:])
	}

	return ""
}

func extractJSONFrom(text string) string {
	// Find balanced braces
	braceCount := 0
	start := -1

	for i, ch := range text {
		if ch == '{' {
			if braceCount == 0 {
				start = i
			}
			braceCount++
		} else if ch == '}' {
			braceCount--
			if braceCount == 0 && start != -1 {
				return text[start : i+1]
			}
		}
	}

	return ""
}

// ExecuteToolCall executes a tool call and returns the result
func (g *Gogent) ExecuteToolCall(toolCall *tool.ToolCall, sessionID, agentID, messageID string) (*tool.ToolCallResponse, error) {
	ctx := tool.ToolContext{
		SessionID:         sessionID,
		AgentID:           agentID,
		MessageID:         messageID,
		ToolCallID:        toolCall.CallID,
		PermissionService: g.permissions,
	}

	result, err := g.toolRegistry.ExecuteToolCall(toolCall, ctx)

	// Notify hooks of tool call
	if result != nil {
		resultStr := fmt.Sprintf("%v", result.Result)
		g.NotifyHooks(HookEvent{
			Type:        HookToolCall,
			SessionID:   sessionID,
			AgentID:     agentID,
			Token:       toolCall.Tool,
			Response:    resultStr,
			Usage:       nil,
			Error:       nil,
			State:       agent.StateIdle,
			Compression: nil,
		})
	}

	return result, err
}

// oneShotOnlyTools are coordination tools that only make sense in one-shot mode.
// interactiveOnlyTools only make sense in interactive mode. Each session is
// handed a registry with the inactive mode's tools stripped out.
var (
	interactiveOnlyTools = []string{"launch_agent", "agent_status", "agent_send", "agent_terminate", "wait_agent_event"}
	oneShotOnlyTools     = []string{"spawn_subagent"}
)

// toolRegistryForMode returns a copy of the global tool registry tailored to the
// given sub-agent execution model, exposing only that mode's coordination tools.
func (g *Gogent) toolRegistryForMode(cfg config.SubAgentConfig) *tool.ToolRegistry {
	if cfg.IsOneShot() {
		return g.toolRegistry.CloneWithout(interactiveOnlyTools...)
	}
	return g.toolRegistry.CloneWithout(oneShotOnlyTools...)
}

// SubAgentSettings returns the current sub-agent execution-model settings.
func (g *Gogent) SubAgentSettings() config.SubAgentConfig {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.config.SubAgents
}

// SetSubAgentSettings updates the sub-agent execution-model settings and
// propagates them to all existing sessions (refreshing their tool registries and
// the delegation instructions their agents receive on the next turn). The
// updated configuration is also persisted to disk so it survives restarts.
func (g *Gogent) SetSubAgentSettings(cfg config.SubAgentConfig) {
	g.mu.Lock()
	g.config.SubAgents = cfg
	sessions := make([]*agent.UserSession, 0, len(g.userSessions))
	for _, s := range g.userSessions {
		sessions = append(sessions, s)
	}
	g.mu.Unlock()

	registry := g.toolRegistryForMode(cfg)
	for _, s := range sessions {
		s.SetSubAgentConfig(cfg)
		if s.RootAgent != nil {
			s.RootAgent.SetToolRegistry(registry)
		}
	}

	if err := g.SaveConfig(); err != nil {
		fmt.Printf("Warning: Failed to persist config: %v\n", err)
	}
}

// SaveConfig writes the current configuration to ~/.gogent/config.json. It is a
// no-op (returning nil) when no home directory is known, so embedders/tests that
// construct Gogent without a real home are unaffected.
func (g *Gogent) SaveConfig() error {
	g.mu.RLock()
	home := g.homeDir
	cfg := g.config
	g.mu.RUnlock()
	if home == "" || cfg == nil {
		return nil
	}
	return config.SaveConfig(home, cfg)
}

// Timeouts returns the current timeout configuration.
func (g *Gogent) Timeouts() config.TimeoutConfig {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.config.Timeouts
}

// SetTimeouts updates the timeout configuration, applies it to the shell tool
// and all existing sessions' sub-agents, and persists it to disk.
func (g *Gogent) SetTimeouts(t config.TimeoutConfig) {
	g.mu.Lock()
	g.config.Timeouts = t
	g.toolRegistry.ShellTimeout = time.Duration(t.ToolSecondsOrDefault()) * time.Second
	g.toolRegistry.NetworkTimeout = g.toolRegistry.ShellTimeout
	g.toolRegistry.WorkspaceRoot = g.workspaceRoot
	g.toolRegistry.Permission = g.permissions
	sessions := make([]*agent.UserSession, 0, len(g.userSessions))
	for _, s := range g.userSessions {
		sessions = append(sessions, s)
	}
	g.mu.Unlock()

	// Re-register the shell and web_fetch tools so the new timeout is captured,
	// then refresh every session's root registry so running sessions pick it up.
	g.toolRegistry.RegisterShellTool()
	g.toolRegistry.RegisterWebFetchTool()
	g.toolRegistry.RegisterGitTool()
	subAgentTO := time.Duration(t.SubAgentSecondsOrDefault()) * time.Second
	for _, s := range sessions {
		s.SetSubAgentTimeout(subAgentTO)
	}

	if err := g.SaveConfig(); err != nil {
		fmt.Printf("Warning: Failed to persist config: %v\n", err)
	}
}

// Models returns deep copies of the configured models so callers (e.g. the UI
// model editor) can edit them without mutating the live config.
func (g *Gogent) Models() []config.ModelConfig {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]config.ModelConfig, 0, len(g.config.ModelConfigs))
	for _, m := range g.config.ModelConfigs {
		if m != nil {
			out = append(out, *m)
		}
	}
	return out
}

// UpdateModel replaces the configuration of the model with the given Name and
// persists the change. Sessions pick up the new endpoint/key on their next turn
// (connections are rebuilt per send). Returns an error if no model matches.
func (g *Gogent) UpdateModel(updated config.ModelConfig) error {
	g.mu.Lock()
	var found *config.ModelConfig
	for _, m := range g.config.ModelConfigs {
		if m != nil && m.Name == updated.Name {
			found = m
			break
		}
	}
	if found != nil {
		*found = updated
	}
	g.mu.Unlock()

	if found == nil {
		return fmt.Errorf("model %q not found", updated.Name)
	}
	if err := g.SaveConfig(); err != nil {
		fmt.Printf("Warning: Failed to persist config: %v\n", err)
	}
	return nil
}

// ScanModels queries the given backend's model-listing endpoint and returns the
// advertised model ids, so the model editor can offer a pick-list instead of a
// free-text model id. The draft config need not be saved first; only its
// api_type, endpoint and api_key are used to reach the backend.
func (g *Gogent) ScanModels(cfg config.ModelConfig) ([]string, error) {
	conn := model.NewModelConnectionFromConfig(&cfg)
	infos, err := conn.ListModels()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(infos))
	for _, info := range infos {
		if info.ID != "" {
			ids = append(ids, info.ID)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("backend returned no models")
	}
	return ids, nil
}

// SetSubAgentOneShot switches sub-agent mode.
// true  => one-shot agents (SUCCESS/FAILURE)
// false => interactive agents (may return CLARIFY)
func (g *Gogent) SetSubAgentOneShot(oneShot bool) {
	cfg := g.SubAgentSettings()
	if oneShot {
		cfg.ExecutionModel = config.SubAgentOneShotModel
	} else {
		cfg.ExecutionModel = config.SubAgentInteractiveModel
	}
	g.SetSubAgentSettings(cfg)
}

// SubAgentOneShot returns the current sub-agent mode.
func (g *Gogent) SubAgentOneShot() bool {
	return g.SubAgentSettings().IsOneShot()
}
