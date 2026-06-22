package gogent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gogent/internal/agent"
	"gogent/internal/command"
	"gogent/internal/config"
	"gogent/internal/diag"
	"gogent/internal/fileops"
	"gogent/internal/mcp"
	"gogent/internal/model"
	"gogent/internal/permission"
	"gogent/internal/skill"
	"gogent/internal/stats"
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
	// checkpoints snapshots touched files before each mutating write/edit, grouped
	// by turn, so a botched edit can be rolled back with UndoLastTurn / Rewind
	// without the user resorting to their own VCS (issue #41). Nil when the file
	// system could not be built.
	checkpoints   *fileops.Checkpointer
	toolRegistry  *tool.ToolRegistry
	config        *config.Config
	workspaceRoot string
	homeDir       string
	store         *SessionStore
	skills        *skill.SkillRegistry
	// log routes diagnostics (warnings, errors) to a sink that never corrupts
	// the TUI's alternate screen: a file in TUI mode, stderr when headless
	// (issue #17). Defaults to stderr; the TUI entry point redirects it via
	// SetLogger.
	log *diag.Logger
	// audit is the append-only security trail (issue #51): permission decisions
	// and tool calls, kept apart from diagnostics so it survives as a
	// post-incident artifact. Defaults to a discard sink; the entry point
	// redirects it to a file via SetAudit.
	audit *diag.Audit
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
	// ephemeral marks sessions that must never be persisted to disk or
	// auto-restored — the per-client sessions the headless HTTP server creates,
	// which are bounded by LRU/TTL eviction rather than kept across restarts
	// (issue #25). The "default" session is implicitly ephemeral.
	ephemeral map[string]bool
	// reviewer, when set, gates write/edit operations behind an interactive
	// diff-review approval (issue #64). reviewApprovedAll records the sessions
	// that chose "approve all this session", so their later edits skip the gate.
	reviewer          EditReviewer
	reviewApprovedAll map[string]bool
	// mcpClients holds the connected MCP servers (issue #36) so their transports
	// (e.g. stdio subprocesses) can be released on shutdown.
	mcpClients []*mcp.Client
	// subAgentLimiter bounds the number of sub-agent loops running concurrently
	// across every session, so the multiplicative fan-out (MaxSubAgents^MaxDepth)
	// cannot spawn an unbounded goroutine herd against the backend (issue #23). It
	// is shared by all sessions, created once at startup from the configured
	// SubAgents.MaxConcurrent.
	subAgentLimiter *agent.SubAgentLimiter
	// rateLimiter paces model requests against the provider's request-rate ceiling
	// across every session, so a wide fan-out (or several cluster nodes) cannot
	// stampede the provider into 429s (issue #28). It is shared by all sessions,
	// created once at startup from the configured RateLimit. Nil/unbounded when
	// throttling is disabled.
	rateLimiter *agent.RateLimiter
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

	// Diagnostics default to stderr (headless); the TUI entry point redirects
	// them to a log file via SetLogger so they never hit the alternate screen.
	log := diag.Stderr()
	// The audit trail defaults to a discard sink; the entry point redirects it to
	// an append-only file via SetAudit (issue #51).
	audit := diag.NewAudit(nil)

	// Load config
	cfg, err := config.LoadConfig(homeDir)
	if err != nil {
		log.Warnf("Failed to load config: %v, using defaults", err)
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
		ephemeral:     make(map[string]bool),

		reviewApprovedAll: make(map[string]bool),
		log:               log,
		audit:             audit,
		subAgentLimiter:   agent.NewSubAgentLimiter(cfg.SubAgents.MaxConcurrentOrDefault()),
		rateLimiter:       agent.NewRateLimiter(cfg.RateLimit.RequestsPerMinute, cfg.RateLimit.Burst),
	}

	// Session transcript persistence (best-effort; a nil store disables it).
	if store, err := NewSessionStore(filepath.Join(homeDir, ".gogent", "sessions")); err == nil {
		g.store = store
	} else {
		log.Warnf("session persistence disabled: %v", err)
	}

	// Initialize file operations services
	g.fileSystem = fileops.NewFileSystem(workspaceRoot)
	g.locationMutation = fileops.NewLocationMutation(workspaceRoot)
	g.permissions = permission.New(filepath.Join(homeDir, ".gogent"))
	// Record every resolved permission decision on the append-only audit trail
	// (issue #51). The sink reads the current audit logger each call, so it picks
	// up the file sink installed later via SetAudit.
	g.permissions.SetAuditSink(func(rc permission.RequestContext, a permission.Action, resource string, allowed bool) {
		g.auditLog().Permission(rc.SessionID, rc.Agent, string(a), resource, allowed)
	})
	// Default posture: file reads/writes inside the workspace are allowed
	// without prompting. Paths outside the workspace, shell, and sub-agents fall
	// through to "ask" (resolved interactively, or denied when headless).
	g.permissions.AddRule(permission.Rule{Action: string(permission.ActionRead), Resource: "*", Effect: string(permission.EffectAllow)})
	g.permissions.AddRule(permission.Rule{Action: string(permission.ActionWrite), Resource: "*", Effect: string(permission.EffectAllow)})
	g.fileMutation = fileops.NewFileMutation(g.fileSystem, g.locationMutation)
	// Snapshot touched files before each mutating write/edit so a turn can be
	// undone (issue #41). Reads through the same file system so snapshots and
	// writes resolve paths identically.
	g.checkpoints = fileops.NewCheckpointer(g.fileSystem)

	// Load skills (user + built-in) and discover project AGENTS.md instructions
	// before building the tool registry so the skill tool and system-context
	// provider can see them. A skill that fails to read or parse is surfaced as
	// a warning instead of vanishing silently (issue #17).
	g.skills = skill.NewSkillRegistry()
	if err := g.skills.LoadSkills(filepath.Join(homeDir, ".gogent", "skills")); err != nil {
		log.Warnf("load user skills: %v", err)
	}
	if err := g.skills.LoadSkills(filepath.Join(workspaceRoot, "skills")); err != nil {
		log.Warnf("load workspace skills: %v", err)
	}
	g.agentsContext = renderAgentsContext(discoverAgentsDocs(workspaceRoot, filepath.Join(homeDir, ".gogent")))
	g.repoMap = buildRepoMap(workspaceRoot)
	g.gitRepo = vcs.IsRepo(workspaceRoot)

	// Initialize tool registry with file tools
	g.initializeToolRegistry()

	return g
}

// SetLogger redirects gogent's diagnostics to lg. The TUI entry point uses it to
// route warnings/errors to a log file (so they never corrupt the alternate
// screen); headless mode keeps the stderr default. It is meant to be called once
// at startup, before sessions generate. A nil argument is ignored.
func (g *Gogent) SetLogger(lg *diag.Logger) {
	if lg == nil {
		return
	}
	g.mu.Lock()
	g.log = lg
	g.mu.Unlock()
}

// SetAudit redirects gogent's security audit trail to a (typically file-backed)
// sink. Like SetLogger it is meant to be called once at startup. A nil argument
// is ignored.
func (g *Gogent) SetAudit(a *diag.Audit) {
	if a == nil {
		return
	}
	g.mu.Lock()
	g.audit = a
	g.mu.Unlock()
}

// logger returns the current diagnostic logger, snapshotted under the registry
// lock since it can be swapped via SetLogger while goroutines log. The returned
// *Logger is nil-safe, so callers need no nil check.
func (g *Gogent) logger() *diag.Logger {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.log
}

// auditLog returns the current audit sink, snapshotted under the registry lock.
// The returned *Audit is nil-safe.
func (g *Gogent) auditLog() *diag.Audit {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.audit
}

// warnf emits a warning through the configured logger, snapshotting it under the
// registry lock since warnings fire from multiple goroutines.
func (g *Gogent) warnf(format string, args ...any) {
	g.logger().Warnf(format, args...)
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
		ReadOnly:    true,
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

			auth, err := fileops.CheckFileAccess(g.permissions, g.locationMutation, false, path,
				permission.RequestContext{SessionID: ctx.SessionID, Agent: ctx.AgentID})
			if err != nil {
				return nil, fmt.Errorf("check file access: %w", err)
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

			auth, err := fileops.CheckFileAccess(g.permissions, g.locationMutation, true, path,
				permission.RequestContext{SessionID: ctx.SessionID, Agent: ctx.AgentID})
			if err != nil {
				return nil, fmt.Errorf("check file access: %w", err)
			}

			// Diff-review gate (issue #64): when enabled, surface the change as a
			// unified diff and defer the write until the user approves.
			if g.reviewActive(ctx.SessionID) {
				before, after, err := g.fileMutation.PreviewWrite(path, content, auth)
				if err != nil {
					return nil, fmt.Errorf("failed to preview write: %v", err)
				}
				if err := g.reviewEdit(ctx, "write", path, before, after); err != nil {
					return nil, err
				}
			}

			// Snapshot the file's pre-turn state so the turn can be undone
			// (issue #41). Done after the review gate so a rejected write is not
			// recorded, and before the mutation so the original content is captured.
			g.snapshotBefore(ctx.SessionID, path, auth)

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
		Description: "Edit a file by replacing exact text. Use this for precise edits. The find text must match exactly once; include surrounding context to make it unique, or set replace_all to true to replace every occurrence.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":        map[string]interface{}{"type": "string"},
				"find":        map[string]interface{}{"type": "string"},
				"replace":     map[string]interface{}{"type": "string"},
				"replace_all": map[string]interface{}{"type": "boolean", "description": "Replace every occurrence of find instead of requiring a single unique match. Defaults to false."},
			},
			"required": []string{"path", "find", "replace"},
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
			replaceAll, _ := args["replace_all"].(bool)

			auth, err := fileops.CheckFileAccess(g.permissions, g.locationMutation, true, path,
				permission.RequestContext{SessionID: ctx.SessionID, Agent: ctx.AgentID})
			if err != nil {
				return nil, fmt.Errorf("check file access: %w", err)
			}

			// Diff-review gate (issue #64): when enabled, surface the change as a
			// unified diff and defer the edit until the user approves.
			if g.reviewActive(ctx.SessionID) {
				before, after, err := g.fileMutation.PreviewEdit(path, find, replace, replaceAll, auth)
				if err != nil {
					return nil, fmt.Errorf("failed to preview edit: %v", err)
				}
				if err := g.reviewEdit(ctx, "edit", path, before, after); err != nil {
					return nil, err
				}
			}

			// Snapshot the file's pre-turn state so the turn can be undone
			// (issue #41), after the review gate and before the mutation.
			g.snapshotBefore(ctx.SessionID, path, auth)

			err = g.fileMutation.EditFile(path, find, replace, replaceAll, auth)
			if err != nil {
				return nil, fmt.Errorf("failed to edit file: %v", err)
			}

			return map[string]interface{}{
				"success": true,
				"path":    path,
			}, nil
		},
	})

	// multi_edit applies several find→replace edits to a single file in one call
	// (issue #45). The batch is all-or-nothing — if any edit is ambiguous or its
	// find text is absent, nothing is written — so the model can land several
	// related changes without a round-trip per edit or risk a half-applied file.
	g.toolRegistry.Register(&tool.Tool{
		Name:        "multi_edit",
		Description: "Apply several exact text replacements to one file in a single call. Edits run in order, each against the result of the previous one, and each find must match exactly once (set replace_all on an edit to replace every occurrence). The batch is all-or-nothing: if any edit fails, the file is left untouched.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{"type": "string"},
				"edits": map[string]interface{}{
					"type":        "array",
					"description": "Edits applied in order. Each is {find, replace, replace_all?}.",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"find":        map[string]interface{}{"type": "string"},
							"replace":     map[string]interface{}{"type": "string"},
							"replace_all": map[string]interface{}{"type": "boolean", "description": "Replace every occurrence of this edit's find instead of requiring a single unique match. Defaults to false."},
						},
						"required": []string{"find", "replace"},
					},
				},
			},
			"required": []string{"path", "edits"},
		},
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			path, ok := args["path"].(string)
			if !ok {
				return nil, fmt.Errorf("path argument is required")
			}
			edits, err := parseEditOps(args["edits"])
			if err != nil {
				return nil, err
			}

			auth, err := fileops.CheckFileAccess(g.permissions, g.locationMutation, true, path,
				permission.RequestContext{SessionID: ctx.SessionID, Agent: ctx.AgentID})
			if err != nil {
				return nil, fmt.Errorf("check file access: %w", err)
			}

			// Diff-review gate (issue #64): preview the whole batch as one diff and
			// defer the write until the user approves.
			if g.reviewActive(ctx.SessionID) {
				before, after, err := g.fileMutation.PreviewMultiEdit(path, edits, auth)
				if err != nil {
					return nil, fmt.Errorf("failed to preview multi_edit: %v", err)
				}
				if err := g.reviewEdit(ctx, "edit", path, before, after); err != nil {
					return nil, err
				}
			}

			// Snapshot the pre-turn state for undo (issue #41), after the review
			// gate and before the mutation.
			g.snapshotBefore(ctx.SessionID, path, auth)

			if err := g.fileMutation.MultiEditFile(path, edits, auth); err != nil {
				return nil, fmt.Errorf("failed to apply multi_edit: %v", err)
			}

			return map[string]interface{}{
				"success": true,
				"path":    path,
				"edits":   len(edits),
			}, nil
		},
	})

	// apply_patch applies a single "*** Begin Patch" envelope that can add, update
	// and delete several files at once (issue #45). The unified-diff hunk format is
	// less error-prone for multi-location edits than repeated find→replace calls.
	// All changes are previewed (and reviewed) before any file is written, so a
	// patch that fails to parse or whose context does not match leaves the
	// workspace untouched.
	g.toolRegistry.Register(&tool.Tool{
		Name:        "apply_patch",
		Description: "Apply a unified-diff patch in the \"*** Begin Patch\" / \"*** End Patch\" envelope to add, update and delete files in one call. Sections are \"*** Add File: <path>\" (followed by '+' content lines), \"*** Delete File: <path>\", and \"*** Update File: <path>\" (followed by '@@' hunks whose lines are prefixed ' ' for context, '-' to remove, '+' to add). Update hunks are located by their context, so include a few surrounding lines. The whole patch is all-or-nothing per file and leaves the workspace untouched if it does not apply.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"patch": map[string]interface{}{"type": "string", "description": "The full patch text, from \"*** Begin Patch\" to \"*** End Patch\"."},
			},
			"required": []string{"patch"},
		},
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			patch, ok := args["patch"].(string)
			if !ok {
				return nil, fmt.Errorf("patch argument is required")
			}
			return g.applyPatch(patch, ctx)
		},
	})

	// Codebase search tools (issue #37): grep/glob/list are read-only and
	// workspace-confined, so — unlike the same searches routed through the
	// shell — they run without a permission prompt and return structured
	// file:line results the model can pass straight back to read. They build on
	// the existing FileSystem primitives (Glob/List) plus the Grep primitive.
	g.toolRegistry.Register(&tool.Tool{
		Name:     "grep",
		ReadOnly: true,
		Description: "Search file contents across the workspace for a regular expression (Go regex syntax). " +
			"Read-only and workspace-confined, so it runs without a permission prompt — prefer it over " +
			"shelling out to grep/rg. It returns file:line references the read tool can open. " +
			"output_mode selects the shape: content (default, every match with its line), " +
			"files_with_matches (just the matching paths), or count (match total per file).",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"pattern":          map[string]interface{}{"type": "string", "description": "Go regular expression to search for."},
				"path":             map[string]interface{}{"type": "string", "description": "File or directory to search, relative to the workspace root (default: whole workspace)."},
				"output_mode":      map[string]interface{}{"type": "string", "enum": []string{"content", "files_with_matches", "count"}, "description": "Result shape (default content)."},
				"include":          map[string]interface{}{"type": "string", "description": "Only search files whose name matches this glob, e.g. \"*.go\"."},
				"case_insensitive": map[string]interface{}{"type": "boolean", "description": "Match regardless of letter case."},
				"max_results":      map[string]interface{}{"type": "integer", "description": "Cap on returned matches/files (default 200)."},
			},
			"required": []string{"pattern"},
		},
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			pattern, ok := args["pattern"].(string)
			if !ok || strings.TrimSpace(pattern) == "" {
				return nil, fmt.Errorf("pattern argument is required")
			}
			opts := fileops.GrepOptions{
				Path:            stringArg(args, "path"),
				OutputMode:      stringArg(args, "output_mode"),
				Include:         stringArg(args, "include"),
				CaseInsensitive: boolArg(args, "case_insensitive"),
			}
			if v, ok := args["max_results"].(float64); ok {
				opts.MaxResults = int(v)
			}
			res, err := g.fileSystem.Grep(pattern, opts)
			if err != nil {
				return nil, fmt.Errorf("failed to grep: %v", err)
			}
			return grepToolResult(res), nil
		},
	})

	g.toolRegistry.Register(&tool.Tool{
		Name:        "glob",
		ReadOnly:    true,
		Description: "List files in the workspace whose path matches a glob pattern (shell-style *, ?, [abc]; it does not cross directory boundaries, so prefer grep for recursive content search). Read-only and workspace-confined, so it runs without a permission prompt. Use it to discover files by name before reading them.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"pattern": map[string]interface{}{"type": "string", "description": "Glob pattern relative to the workspace root, e.g. \"*.txt\" or \"src/*.go\"."},
			},
			"required": []string{"pattern"},
		},
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			pattern, ok := args["pattern"].(string)
			if !ok || strings.TrimSpace(pattern) == "" {
				return nil, fmt.Errorf("pattern argument is required")
			}
			matches, err := g.fileSystem.Glob(pattern)
			if err != nil {
				return nil, fmt.Errorf("failed to glob: %v", err)
			}
			sort.Strings(matches) // deterministic order for stable display
			return map[string]interface{}{
				"pattern": pattern,
				"matches": matches,
				"count":   len(matches),
			}, nil
		},
	})

	g.toolRegistry.Register(&tool.Tool{
		Name:        "list",
		ReadOnly:    true,
		Description: "List the files and subdirectories immediately inside a workspace directory. Read-only and workspace-confined, so it runs without a permission prompt. Use it to explore a directory's layout before reading specific files.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{"type": "string", "description": "Directory to list, relative to the workspace root (default: the workspace root)."},
			},
		},
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			path := stringArg(args, "path")
			if path == "" {
				path = "."
			}
			entries, err := g.fileSystem.List(path)
			if err != nil {
				return nil, fmt.Errorf("failed to list: %v", err)
			}
			out := make([]map[string]interface{}, 0, len(entries))
			for _, e := range entries {
				out = append(out, map[string]interface{}{
					"name":   e.Name,
					"path":   e.Path,
					"is_dir": e.IsDir,
					"size":   e.Size,
				})
			}
			return map[string]interface{}{
				"path":    path,
				"entries": out,
				"count":   len(out),
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

	// Diagnostics tool (issue #42): runs the project's compiler/linter and returns
	// structured errors. The command is configurable; the zero-value config keeps
	// the Go default (`go vet ./...`), so it works out of the box.
	var diagCmd []string
	var diagWarn string
	if g.config != nil {
		diagCmd = g.config.Diagnostics.Command
		diagWarn = g.config.Diagnostics.WarningPattern
	}
	g.toolRegistry.RegisterDiagnosticsTool(diagCmd, diagWarn)

	// Verify tool (issue #44): runs the project's test command and returns
	// structured pass/fail results plus parsed failures — the tight
	// edit→test→read-failures loop. The command is configurable; the zero-value
	// config keeps the Go default (`go test ./...`), so it works out of the box.
	var verifyCmd []string
	if g.config != nil {
		verifyCmd = g.config.Verify.Command
	}
	g.toolRegistry.RegisterVerifyTool(verifyCmd)

	g.toolRegistry.Register(&tool.Tool{
		Name: "spawn_subagent",
		Description: "Delegate work to sub-agents to cut wall-clock latency. A SINGLE call " +
			"with a \"subtasks\" array runs every entry CONCURRENTLY and blocks only until the " +
			"slowest finishes — prefer it over investigating files or running checks one after " +
			"another yourself. Reach for it to parallelize multi-file/multi-module investigation, " +
			"to run several checks at once (e.g. diagnostics + verify + grep), or to research a " +
			"topic while you keep working. Batch the independent parts into the one call's " +
			"\"subtasks\" array; do NOT issue spawns one-per-turn (they then run serially with no " +
			"speed-up). Use \"name\"/\"task\" only for a single lone task. In one-shot mode (the " +
			"default) each sub-agent runs to completion and its result ends with SUCCESS: or " +
			"FAILURE:; in interactive mode it may return a CLARIFY: question instead.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "Sub-agent name (single-task mode)"},
				"task": map[string]interface{}{"type": "string", "description": "Task description (single-task mode)"},
				"subtasks": map[string]interface{}{
					"type": "array",
					"description": "Parallel batch of independent tasks; all run concurrently in " +
						"this one call. PREFER this over multiple separate calls. Each entry is " +
						"a {name, task} object (preferred) or a bare task string.",
					"items": map[string]interface{}{
						// An entry is normally a {name, task} object; a bare string is
						// also accepted and taken as the task. The union type keeps the
						// advertised contract honest with the tolerance in Execute, and
						// there is no item-level "required" because the string form has
						// no properties. (Array items are not enforced by validateArgs;
						// this schema is advisory wire guidance for the model.)
						"type": []string{"object", "string"},
						"properties": map[string]interface{}{
							"name": map[string]interface{}{"type": "string", "description": "Short label for this subtask."},
							"task": map[string]interface{}{"type": "string", "description": "What this sub-agent should do."},
						},
					},
				},
			},
		},
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			session := g.GetUserSession(ctx.SessionID)
			if session == nil {
				return nil, fmt.Errorf("session not found: %s", ctx.SessionID)
			}
			loopCtx := toolLoopContext(ctx)

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
				// Bound the goroutine fan-out via the session's shared concurrency
				// limiter: each subtask runs concurrently only while a global slot
				// is free, otherwise inline as backpressure (issue #23).
				tasks := make([]func(), 0, len(items))
				for i, raw := range items {
					i, raw := i, raw
					var name, task string
					switch v := raw.(type) {
					case map[string]interface{}:
						// Canonical shape: {"name": ..., "task": ...}.
						name, _ = v["name"].(string)
						task, _ = v["task"].(string)
					case string:
						// Tolerated weak-model shape: a bare string is the task, so a
						// ["do X", "do Y"] batch still fans out concurrently instead of
						// being rejected and retried one-per-turn (issue #282).
						task = v
					default:
						results[i].Error = "invalid subtask item"
						continue
					}
					results[i].Name = name
					results[i].Task = task
					if strings.TrimSpace(task) == "" {
						results[i].Error = "missing subtask.task"
						continue
					}
					tasks = append(tasks, func() {
						// A panic in a spawn worker must fail only its own
						// subtask, not crash the process and every other session
						// (issue #8).
						defer func() {
							if r := recover(); r != nil {
								results[i].Error = fmt.Sprintf("subagent panicked: %v", r)
							}
						}()
						// spawn_subagent is always the blocking one-shot primitive,
						// even when the session also exposes the async launch_agent
						// family (the "both" default, issue #284). Async, conversational
						// workers go through launch_agent, not here.
						text, err := session.SpawnSubAgent(loopCtx, ctx.AgentID, name, task, true)
						if err != nil {
							results[i].Error = err.Error()
							return
						}
						results[i].Result = text
					})
				}
				session.RunSubAgentsBounded(tasks)
				return map[string]interface{}{"success": true, "mode": map[string]bool{"one_shot": true, "interactive": false}, "results": results}, nil
			}

			name, _ := args["name"].(string)
			task, _ := args["task"].(string)
			if strings.TrimSpace(task) == "" {
				return nil, fmt.Errorf("task is required")
			}
			result, err := session.SpawnSubAgent(loopCtx, ctx.AgentID, name, task, true)
			if err != nil {
				return nil, fmt.Errorf("spawn sub-agent: %w", err)
			}
			return map[string]interface{}{
				"success": true,
				"name":    name,
				"task":    task,
				"mode":    map[string]bool{"one_shot": true, "interactive": false},
				"result":  result,
			}, nil
		},
	})

	// Interactive (fire-and-forget) sub-agent coordination tools. They are surfaced
	// to any session whose execution model exposes the interactive style — the
	// "both" default and "interactive" (the registry is filtered per session in
	// CreateUserSession by toolRegistryForMode) — but are registered globally here
	// so that filtering can include/exclude them by name.
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
				return nil, fmt.Errorf("launch interactive agent: %w", err)
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
				return nil, fmt.Errorf("query interactive agent status: %w", err)
			}
			return map[string]interface{}{"success": true, "agent_id": id, "status": string(status), "result": result}, nil
		},
	})

	g.toolRegistry.Register(&tool.Tool{
		Name:        "agent_send",
		Description: "Answer an interactive sub-agent's CLARIFY question. The agent must be awaiting input (status 'waiting'); drive this off a clarify event from wait_agent_event.",
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
				return nil, fmt.Errorf("send to interactive agent: %w", err)
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
				return nil, fmt.Errorf("terminate interactive agent: %w", err)
			}
			return map[string]interface{}{"success": true, "agent_id": id}, nil
		},
	})

	g.toolRegistry.Register(&tool.Tool{
		Name:        "wait_agent_event",
		Description: "Block until an interactive sub-agent finishes or asks for clarification, then return that event; returns {timed_out:true} if none arrives in time. timeout_ms bounds the wait (defaults to 30000ms when omitted). Call this only while agents are still running — waiting with nothing outstanding just times out.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"timeout_ms": map[string]interface{}{"type": "number"}},
		},
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			session := g.GetUserSession(ctx.SessionID)
			if session == nil {
				return nil, fmt.Errorf("session not found: %s", ctx.SessionID)
			}
			// Use the model's timeout_ms when given; otherwise apply a finite
			// default rather than 0, which NextAgentEvent treats as an unbounded
			// block that would hang the turn forever when nothing is pending.
			timeout := defaultWaitAgentEventTimeout
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

	// todo tracks the session's task checklist (issues #43, #263). The tool has
	// two modes. Write mode (todos present) replaces the whole list and echoes the
	// stored list back so the call's effect is unambiguous in the transcript. Read
	// mode (todos omitted) returns the current list without mutating it, so the
	// model can query live state before deciding its next step. The list is shown
	// live in the sidebar and injected into the system prompt every turn (so it
	// survives compaction). It is intentionally not read-only (write mode mutates
	// session state) so concurrent calls stay serial, but it is retained in plan
	// mode as a way to lay out a plan's steps.
	g.toolRegistry.Register(&tool.Tool{
		Name:        "todo",
		Description: "Record, update or read the session's task checklist. Pass `todos` (the full list) to replace the checklist; each item is {content, status?, note?} where status is pending, in_progress or completed (defaults to pending) and note is an optional finding/detail. Omit `todos` to read the current list back without changing it. Every call returns the stored list, its count and a summary. The list is shown live in the sidebar and stays in the system prompt; use it to lay out and track multi-step work.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"todos": map[string]interface{}{
					// No "type" is declared so the schema permits both an array
					// (write) and a null (read): validateArgs only enforces a
					// property's type when it is a string, and parseTodoItems still
					// type-checks the array on the write path. This lets a model send
					// {"todos": null} to mean "just read" without tripping schema
					// validation, while "items" + the description keep the array shape
					// advertised (issue #263).
					"description": "The complete checklist (a JSON array). Omit it, or pass null, to read the current list back without changing it. Each entry is {content: string, status?: pending|in_progress|completed, note?: string}.",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"content": map[string]interface{}{"type": "string"},
							"status":  map[string]interface{}{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
							"note":    map[string]interface{}{"type": "string", "description": "Optional finding, rationale or detail attached to this item."},
						},
						"required": []string{"content"},
					},
				},
			},
		},
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			session := g.GetUserSession(ctx.SessionID)
			if session == nil {
				return nil, fmt.Errorf("session not found: %s", ctx.SessionID)
			}
			// Read mode: no todos argument provided (absent or an explicit null).
			// Return the current list untouched so the model can query live state
			// without the read path tripping on a null it meant as "just read"
			// (issue #263).
			raw, present := args["todos"]
			if !present || raw == nil {
				items := session.Todos()
				return map[string]interface{}{
					"mode":    "read",
					"count":   len(items),
					"todos":   items,
					"summary": agent.TodoSummary(items),
				}, nil
			}
			// Write mode: replace the checklist and echo the stored list back.
			items, err := parseTodoItems(raw)
			if err != nil {
				return nil, err
			}
			session.SetTodos(items)
			stored := session.Todos()
			return map[string]interface{}{
				"mode":    "write",
				"success": true,
				"count":   len(stored),
				"todos":   stored,
				"summary": agent.TodoSummary(stored),
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

// grepToolResult renders a fileops.GrepResult as the grep tool's model-facing
// output: the mode, pattern and truncation flag, plus the mode-specific payload
// (matches / files / counts). It is a plain map so the value renders readably
// when the agent loop stringifies tool results for the model.
func grepToolResult(r *fileops.GrepResult) map[string]interface{} {
	out := map[string]interface{}{
		"mode":      r.Mode,
		"pattern":   r.Pattern,
		"truncated": r.Truncated,
	}
	switch r.Mode {
	case fileops.GrepModeContent:
		matches := make([]map[string]interface{}, 0, len(r.Matches))
		for _, m := range r.Matches {
			matches = append(matches, map[string]interface{}{
				"path":    m.Path,
				"line":    m.Line,
				"content": m.Content,
			})
		}
		out["matches"] = matches
		out["count"] = len(matches)
	case fileops.GrepModeFiles:
		out["files"] = r.Files
		out["count"] = len(r.Files)
	case fileops.GrepModeCount:
		counts := make([]map[string]interface{}, 0, len(r.Counts))
		total := 0
		for _, c := range r.Counts {
			counts = append(counts, map[string]interface{}{"path": c.Path, "count": c.Count})
			total += c.Count
		}
		out["counts"] = counts
		out["files"] = len(counts)
		out["total"] = total
	}
	return out
}

// stringArg returns args[key] as a string, or "" when it is absent or not a
// string. It mirrors the loose, inline coercion the other tools use for optional
// string parameters.
func stringArg(args map[string]interface{}, key string) string {
	s, _ := args[key].(string)
	return s
}

// boolArg returns args[key] as a bool, or false when it is absent or not a bool.
func boolArg(args map[string]interface{}, key string) bool {
	b, _ := args[key].(bool)
	return b
}

// CreateUserSession creates a new user session
func (g *Gogent) CreateUserSession(id string, rootAgent *agent.Agent) *agent.UserSession {
	g.mu.Lock()
	defer g.mu.Unlock()

	userSession := agent.NewUserSession(id, rootAgent)
	userSession.SetSubAgentConfig(g.config.SubAgents)
	// Apply the configurable per-task step cap (issue #249); 0 means unlimited.
	userSession.SetMaxSteps(g.config.MaxStepsOrDefault())
	// Mid-turn injection of a queued user note (issue #170, phase 2) is now always
	// on: it is the agent-side path behind the per-message Interject button (issue
	// #201), which replaced the removed experimental.inject_queued_input flag. The
	// UI's drain-on-idle queue (phase 1) still owns Enter/Queue.
	userSession.SetInjectQueuedInput(true)
	// Live thinking-token streaming (issue #217) is opt-in via the experimental
	// flag; the /thinking command can also toggle it per session at runtime.
	userSession.SetStreamThinking(g.config.Experimental.StreamThinking)
	userSession.SetSubAgentTimeout(time.Duration(g.config.Timeouts.SubAgentSecondsOrDefault()) * time.Second)
	// Share the process-wide concurrency limiter so sub-agent fan-out across all
	// sessions is globally bounded (issue #23).
	userSession.SetSubAgentLimiter(g.subAgentLimiter)
	// Share the process-wide request-rate limiter so the model request rate is
	// governed across all sessions, not per-session (issue #28).
	userSession.SetRateLimiter(g.rateLimiter)
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

	// Wire the lifecycle hooks that were defined but never fired (issue #47).
	// Root-agent state transitions surface as HookStateChange, and any callback
	// the model session already emits — today, context compaction — is bridged to
	// its matching HookEvent (HookCompression). HookResponseComplete/HookError are
	// fired by SendMessageToSessionWithModel at the turn boundary.
	rootAgentID := rootAgent.ID
	rootAgent.SetStateChangeCallback(func(old, new agent.AgentState) {
		g.NotifyHooks(HookEvent{
			Type:      HookStateChange,
			SessionID: id,
			AgentID:   rootAgentID,
			State:     new,
		})
	})
	if rootAgent.ThoughtTrain != nil {
		rootAgent.ThoughtTrain.AddCallback(func(ev model.CallbackEvent) {
			g.bridgeModelEvent(id, rootAgentID, ev)
		})
	}

	// Set tool callback to increment tool call count and record the invocation on
	// the audit trail (issue #51). Arguments are deliberately not logged — they
	// can carry file contents or secrets; the permission audit events capture the
	// resource a side-effecting tool touched.
	userSession.SetToolCallback(func(toolName string, args map[string]interface{}) error {
		userSession.IncrementToolCall()
		g.auditLog().ToolCall(id, "", toolName)
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

// NewEphemeralSession is like NewSession but marks the session as ephemeral, so
// it is never persisted to disk or auto-restored. The headless HTTP server uses
// it to create a session per client id (issue #25): such sessions live only for
// the process and are reclaimed by LRU/TTL eviction.
func (g *Gogent) NewEphemeralSession(id string) *agent.UserSession {
	g.mu.Lock()
	g.ephemeral[id] = true
	g.mu.Unlock()
	return g.NewSession(id)
}

// toolLoopContext returns the cancellation scope of the loop that invoked a
// tool, falling back to context.Background() for callers that pre-date context
// plumbing (issue #24).
func toolLoopContext(ctx tool.ToolContext) context.Context {
	if ctx.Context != nil {
		return ctx.Context
	}
	return context.Background()
}

// RemoveSession deletes a user session by id.
func (g *Gogent) RemoveSession(id string) {
	g.mu.Lock()
	us := g.userSessions[id]
	eph := g.ephemeral[id]
	delete(g.userSessions, id)
	delete(g.sessionTitles, id)
	delete(g.ephemeral, id)
	g.mu.Unlock()

	// Cancel any in-flight task loops so they stop mutating a session that is no
	// longer reachable (issue #24).
	if us != nil {
		us.Stop()
	}

	// Archive the on-disk transcript so it is not auto-restored next start.
	// Ephemeral (HTTP) sessions were never persisted, so there is nothing to
	// archive (issue #25).
	if g.store != nil && id != "default" && !eph {
		if err := g.store.Archive(id); err != nil {
			g.warnf("failed to archive session %s: %v", id, err)
		}
	}
}

// SetSessionTitle records a human-friendly title used when persisting a session.
func (g *Gogent) SetSessionTitle(id, title string) {
	g.mu.Lock()
	g.sessionTitles[id] = title
	g.mu.Unlock()
}

// RenameSession updates a session's title and persists it to the index right
// away (issue #272). The title is otherwise only written on the next message
// turn, so a rename was invisible to the on-disk index — and thus to the Sessions
// browser, which searches the index by title — until (and unless) another message
// was sent. Store.Save rewrites the index on a title-only change, so this records
// the new name immediately.
func (g *Gogent) RenameSession(id, title string) {
	g.SetSessionTitle(id, title)
	g.persistSession(id)
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
	eph := g.ephemeral[id]
	g.mu.RUnlock()
	if us == nil || eph {
		return
	}
	if title == "" {
		title = id
	}
	if err := g.store.Save(us, title); err != nil {
		g.warnf("failed to persist session %s: %v", id, err)
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
		g.warnf("failed to list sessions: %v", err)
		return nil
	}
	var restored []LoadedSession
	for _, ls := range loaded {
		if r, ok := g.adoptLoaded(ls); ok {
			restored = append(restored, r)
		}
	}
	return restored
}

// adoptLoaded rebuilds one loaded session's in-memory UserSession from its
// restored transcript (so the conversation can continue), seeds its title and
// re-attaches the store so later saves append to its active shard. It is shared
// by startup RestoreSessions and on-demand ContinueSession (issue #58). It is a
// no-op (ok=false) for the "default" session or one that already exists.
func (g *Gogent) adoptLoaded(ls LoadedSession) (LoadedSession, bool) {
	if ls.ID == "" || ls.ID == "default" {
		return LoadedSession{}, false
	}
	g.mu.RLock()
	_, exists := g.userSessions[ls.ID]
	g.mu.RUnlock()
	if exists {
		return LoadedSession{}, false
	}

	// Restore the session on the model it was last using (issue #266), not the
	// global default: resolve the recorded model name, falling back to the default
	// when it is empty (older sessions) or no longer in the config.
	conn := g.defaultConnection()
	primaryName := ""
	if g.config != nil && ls.Model != "" {
		if cfg := g.config.GetModelConfig(ls.Model); cfg != nil {
			conn = g.buildConnection(cfg)
			primaryName = cfg.Name
		}
	}
	sess := model.NewModelSession("main", conn)
	if msgs := ls.Transcripts["root"]; len(msgs) > 0 {
		sess.ReplaceTranscript(msgs)
	}
	rootAgent := agent.NewAgent("root", sess)
	rootAgent.SetState(agent.StateIdle)
	g.CreateUserSession(ls.ID, rootAgent)
	if primaryName != "" {
		// Report the restored model via PrimaryModel() from the first turn, so the
		// Statistics/Overall panels and the next persist read the right model
		// instead of the default.
		if us := g.GetUserSession(ls.ID); us != nil {
			us.SetPrimaryModel(primaryName)
		}
	}
	g.SetSessionTitle(ls.ID, ls.Title)
	if g.store != nil {
		g.store.Adopt(ls.ID, ls.File, rootAgent.ListAllAgents()) // continue appending to the active shard
	}
	return ls, true
}

// ContinueSession re-opens a single saved session by its index file path so the
// user can keep typing into it: it loads the transcript, adopts it into a live
// backend session (the next send appends rather than starting over) and returns
// the loaded session for a UI to re-open its window (issue #58). It returns
// ok=false when the store is unavailable, the file is missing, or the session
// is the "default"/already live.
func (g *Gogent) ContinueSession(file string) (LoadedSession, bool) {
	if g.store == nil {
		return LoadedSession{}, false
	}
	ls, err := g.store.LoadSession(file)
	if err != nil {
		g.warnf("failed to load session %s: %v", file, err)
		return LoadedSession{}, false
	}
	return g.adoptLoaded(ls)
}

// LoadSavedSession reads one saved session's transcript by its index file path
// for read-only analysis (issue #58) — unlike ContinueSession it builds no live
// backend session, so the returned transcript is a static snapshot.
func (g *Gogent) LoadSavedSession(file string) (LoadedSession, error) {
	if g.store == nil {
		return LoadedSession{}, fmt.Errorf("session persistence unavailable")
	}
	return g.store.LoadSession(file)
}

// UndoLastTurn reverts the most recent turn's file mutations for a session,
// restoring every file it touched to its pre-turn state (issue #41). It returns a
// human-readable summary and a non-nil error (with an empty summary) only when
// there is no checkpoint to undo.
func (g *Gogent) UndoLastTurn(sessionID string) (string, error) {
	if g.checkpoints == nil {
		return "", fmt.Errorf("checkpoints unavailable")
	}
	n, err := g.checkpoints.UndoLastTurn(sessionID)
	if err != nil {
		return "", fmt.Errorf("undo last turn: %w", err)
	}
	return fmt.Sprintf("reverted last turn (%d file(s) restored)", n), nil
}

// Rewind reverts the last turns turns for a session — all of them when turns <= 0
// — restoring each touched file to its state before the earliest reverted turn
// (issue #41). It returns a human-readable summary.
func (g *Gogent) Rewind(sessionID string, turns int) (string, error) {
	if g.checkpoints == nil {
		return "", fmt.Errorf("checkpoints unavailable")
	}
	files, reverted, err := g.checkpoints.Rewind(sessionID, turns)
	if err != nil {
		return "", fmt.Errorf("rewind: %w", err)
	}
	return fmt.Sprintf("reverted %d turn(s) (%d file(s) restored)", reverted, files), nil
}

// CheckpointCount reports how many committed (undoable) turns a session has
// recorded (issue #41).
func (g *Gogent) CheckpointCount(sessionID string) int {
	if g.checkpoints == nil {
		return 0
	}
	return g.checkpoints.Count(sessionID)
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

// GetCheckpointer returns the turn checkpoint store used for undo/rewind (issue
// #41), or nil when checkpointing is disabled.
func (g *Gogent) GetCheckpointer() *fileops.Checkpointer {
	return g.checkpoints
}

// snapshotBefore records path's pre-mutation state into the session's active
// checkpoint so the turn can be undone. It is a no-op when checkpointing is
// disabled or no turn is in progress; the checkpointer swallows read errors so
// this can never block or fail the write (issue #41).
func (g *Gogent) snapshotBefore(sessionID, path string, auth fileops.Authorization) {
	if g.checkpoints == nil {
		return
	}
	g.checkpoints.Snapshot(sessionID, path, auth)
}

// parseEditOps converts the multi_edit "edits" argument (a JSON array of
// {find, replace, replace_all?} objects) into fileops.EditOp values, validating
// that the array is present, non-empty, and well-shaped (issue #45).
func parseEditOps(raw interface{}) ([]fileops.EditOp, error) {
	arr, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("edits argument must be an array")
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("edits argument must not be empty")
	}
	edits := make([]fileops.EditOp, 0, len(arr))
	for i, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("edit %d must be an object", i+1)
		}
		find, ok := m["find"].(string)
		if !ok {
			return nil, fmt.Errorf("edit %d: find is required", i+1)
		}
		replace, ok := m["replace"].(string)
		if !ok {
			return nil, fmt.Errorf("edit %d: replace is required", i+1)
		}
		replaceAll, _ := m["replace_all"].(bool)
		edits = append(edits, fileops.EditOp{Find: find, Replace: replace, ReplaceAll: replaceAll})
	}
	return edits, nil
}

// parseTodoItems converts the todo tool's "todos" argument (a JSON array of
// {content, status?, note?} objects) into agent.TodoItem values, validating that
// the array is present and well-shaped (issues #43, #263). A missing/unknown
// status defaults to pending via agent.NormalizeTodoStatus; note is optional and
// trimmed.
func parseTodoItems(raw interface{}) ([]agent.TodoItem, error) {
	arr, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("todos argument must be an array")
	}
	items := make([]agent.TodoItem, 0, len(arr))
	for i, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("todo %d must be an object", i+1)
		}
		content, _ := m["content"].(string)
		if strings.TrimSpace(content) == "" {
			return nil, fmt.Errorf("todo %d: content is required", i+1)
		}
		status, _ := m["status"].(string)
		note, _ := m["note"].(string)
		items = append(items, agent.TodoItem{
			Content: content,
			Status:  agent.NormalizeTodoStatus(status),
			Note:    strings.TrimSpace(note),
		})
	}
	return items, nil
}

// applyPatch parses and applies a "*** Begin Patch" envelope (issue #45) in two
// phases so the whole patch is validated before anything is written. Phase one
// parses, resolves each op's before/after against current disk content,
// authorizes the path and runs the diff-review gate; phase two snapshots and
// writes. Any failure in phase one (bad envelope, missing/existing file, a hunk
// whose context does not match, or a rejected review) leaves the workspace
// untouched. Each file may be touched at most once per patch.
func (g *Gogent) applyPatch(patch string, ctx tool.ToolContext) (interface{}, error) {
	ops, err := fileops.ParsePatch(patch)
	if err != nil {
		return nil, fmt.Errorf("failed to parse patch: %v", err)
	}

	type plannedOp struct {
		op    fileops.PatchOp
		auth  fileops.Authorization
		after string
	}
	planned := make([]plannedOp, 0, len(ops))
	seen := make(map[string]bool, len(ops))

	for _, op := range ops {
		if seen[op.Path] {
			return nil, fmt.Errorf("patch touches %q more than once", op.Path)
		}
		seen[op.Path] = true

		auth, err := fileops.CheckFileAccess(g.permissions, g.locationMutation, true, op.Path,
			permission.RequestContext{SessionID: ctx.SessionID, Agent: ctx.AgentID})
		if err != nil {
			return nil, fmt.Errorf("check file access: %w", err)
		}

		exists, err := g.fileSystem.Exists(op.Path)
		if err != nil {
			return nil, fmt.Errorf("check file existence: %w", err)
		}
		switch op.Type {
		case fileops.PatchAdd:
			if exists {
				return nil, fmt.Errorf("add file %q: file already exists", op.Path)
			}
		case fileops.PatchUpdate, fileops.PatchDelete:
			if !exists {
				return nil, fmt.Errorf("%s file %q: file does not exist", op.Type, op.Path)
			}
		}

		before := ""
		if exists {
			data, err := g.fileSystem.Read(op.Path, auth)
			if err != nil {
				return nil, fmt.Errorf("failed to read %q: %v", op.Path, err)
			}
			before = strings.TrimPrefix(string(data), "\uFEFF")
		}

		after, err := op.ApplyTo(before)
		if err != nil {
			return nil, fmt.Errorf("%s file %q: %v", op.Type, op.Path, err)
		}

		// Diff-review gate (issue #64): surface this file's change and defer the
		// write until the user approves. Gating in phase one means a rejection
		// partway through never leaves a partially-written set on disk.
		if g.reviewActive(ctx.SessionID) {
			if err := g.reviewEdit(ctx, op.Type.String(), op.Path, before, after); err != nil {
				return nil, err
			}
		}

		planned = append(planned, plannedOp{op: op, auth: auth, after: after})
	}

	for _, p := range planned {
		// Snapshot the pre-turn state for undo (issue #41) before mutating.
		g.snapshotBefore(ctx.SessionID, p.op.Path, p.auth)
		if p.op.Type == fileops.PatchDelete {
			if err := g.fileMutation.Remove(p.op.Path); err != nil {
				return nil, fmt.Errorf("failed to delete %q: %v", p.op.Path, err)
			}
			continue
		}
		if err := g.fileMutation.WriteFile(p.op.Path, p.after, p.auth); err != nil {
			return nil, fmt.Errorf("failed to write %q: %v", p.op.Path, err)
		}
	}

	return map[string]interface{}{
		"success": true,
		"files":   len(planned),
	}, nil
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
Edit a file by replacing exact text. Use this for precise edits. The find string must match exactly once — include surrounding context to make it unique, or pass "replace_all": true to replace every occurrence.
- Input: {"path": "string", "find": "string", "replace": "string", "replace_all": "boolean (optional, default false)"}
- Example: {"tool": "edit", "args": {"path": "hello.txt", "find": "World", "replace": "Universe"}}

### multi_edit
Apply several exact text replacements to one file in a single call. Edits run in order (each against the result of the previous one) and each find must match exactly once unless that edit sets replace_all. The batch is all-or-nothing: if any edit fails, the file is left untouched. Prefer this over many edit calls when changing several spots in one file.
- Input: {"path": "string", "edits": [{"find": "string", "replace": "string", "replace_all": "boolean (optional)"}]}
- Example: {"tool": "multi_edit", "args": {"path": "main.go", "edits": [{"find": "foo", "replace": "bar"}, {"find": "old", "replace": "new"}]}}

### apply_patch
Apply a unified-diff patch in the "*** Begin Patch" / "*** End Patch" envelope to add, update and delete files in one call. Sections are "*** Add File: <path>" (followed by '+' content lines), "*** Delete File: <path>", and "*** Update File: <path>" (followed by '@@' hunks whose lines are prefixed ' ' for context, '-' to remove, '+' to add). Update hunks are located by their context, so include a few surrounding lines. The patch leaves the workspace untouched if it does not apply cleanly.
- Input: {"patch": "string"}
- Example: {"tool": "apply_patch", "args": {"patch": "*** Begin Patch\n*** Update File: main.go\n@@\n-old line\n+new line\n*** End Patch"}}

### grep
Search file contents across the workspace for a regular expression (Go regex syntax). Read-only and workspace-confined, so it runs without a permission prompt — prefer it over shelling out to grep/rg. It returns file:line references you can pass straight to read.
- Input: {"pattern": "string", "path": "string (optional)", "output_mode": "content|files_with_matches|count (optional)", "include": "string (optional glob)", "case_insensitive": "boolean (optional)", "max_results": "integer (optional)"}
- Example: {"tool": "grep", "args": {"pattern": "func.*List", "include": "*.go"}}

### glob
List workspace files whose path matches a shell-style glob (*, ?, [abc]; does not cross directory boundaries). Read-only and runs without a prompt. Use it to discover files by name.
- Input: {"pattern": "string"}
- Example: {"tool": "glob", "args": {"pattern": "*.go"}}

### list
List the files and subdirectories immediately inside a workspace directory. Read-only and runs without a prompt. Use it to explore layout before reading files.
- Input: {"path": "string (optional, default workspace root)"}
- Example: {"tool": "list", "args": {"path": "internal"}}

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

### diagnostics
Run the project's compiler/linter and return structured errors (file:line:column, severity, message). The default is "go vet ./..."; the command is fixed (configurable, not model-controlled). Prefer it over running the compiler through the shell: no shell quoting, output parsed into actionable diagnostics, and ok=true means the project builds. Call it after edits to catch compile/typecheck/lint breakage early.
- Input: {} (no arguments)
- Example: {"tool": "diagnostics", "args": {}}
- Returns: {"command": ["go","vet","./..."], "ok": false, "exit_code": 1, "count": 1, "diagnostics": [{"path": "foo.go", "line": 6, "column": 14, "severity": "error", "message": "undefined: undef"}]}

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

// SessionIDs returns the ids of every live in-memory session.
func (g *Gogent) SessionIDs() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()

	sessions := make([]string, 0, len(g.userSessions))
	for id := range g.userSessions {
		sessions = append(sessions, id)
	}
	return sessions
}

// ListSessions returns the metadata of every persisted session straight from the
// store's index files (issue #58): an O(sessions) listing for the Sessions
// browser that never replays a transcript. It returns nil when persistence is
// unavailable; a read error is warned and yields an empty slice rather than
// failing the caller.
func (g *Gogent) ListSessions() []SessionMeta {
	if g.store == nil {
		return nil
	}
	metas, err := g.store.ListSessions()
	if err != nil {
		g.warnf("failed to list saved sessions: %v", err)
		return nil
	}
	return metas
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
	models, err := lister.ListModels()
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	return models, nil
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

// Statistics assembles the detailed statistics report surfaced by the
// Statistics view (issue #57). It joins the per-session counters (turns, tokens,
// tool calls, context, compactions, connector stats), the per-tool usage/duration
// counters, the per-skill success/failure counters, and the per-model token
// attribution — most of which gogent already collects but only partially shows.
//
// The report is a point-in-time, in-memory snapshot; durable/queryable history
// arrives with the structured-logging/audit stream (issue #51). Per-model cost,
// cache-hit % and TTFT are likewise deferred (they depend on issues #48/#2 and a
// per-model pricing configuration).
func (g *Gogent) Statistics() stats.Report {
	// Snapshot the session pointers under the registry lock, then release it
	// before touching each session's own lock to avoid lock-ordering issues.
	g.mu.RLock()
	type entry struct {
		id string
		s  *agent.UserSession
	}
	items := make([]entry, 0, len(g.userSessions))
	for id, s := range g.userSessions {
		items = append(items, entry{id: id, s: s})
	}
	toolReg := g.toolRegistry
	skills := g.skills
	// Snapshot which sessions are ephemeral (on-demand HTTP/API sessions, issue
	// #25) so each row can be tagged. The report is not filtered here — GET /stats
	// must keep every session — but the tag lets the TUI drop windowless sessions
	// from its own Statistics surfaces (issue #278).
	ephemeral := make(map[string]bool, len(g.ephemeral))
	for id, eph := range g.ephemeral {
		ephemeral[id] = eph
	}
	g.mu.RUnlock()

	// Stable order: oldest session first (creation time, then id), matching how
	// the sidebar lists sessions.
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].s.CreatedAt != items[j].s.CreatedAt {
			return items[i].s.CreatedAt < items[j].s.CreatedAt
		}
		return items[i].id < items[j].id
	})

	rep := stats.Report{GeneratedAt: time.Now().Unix()}
	modelTotals := make(map[string]stats.ModelStat)
	for _, it := range items {
		snap := it.s.Snapshot()
		primary := stats.FromSnapshot(it.s.ConnectorStats())
		fast := stats.FromSnapshot(it.s.FastConnectorStats())
		primaryModel := it.s.PrimaryModel()
		subAgents := it.s.SubAgentCount()
		// Capture the per-model split once: it feeds both the session row's PerModel
		// (so a consumer excluding this session can back out its exact per-model
		// contribution, issue #278) and the grand per-model aggregation below.
		perModelStats := it.s.PerModelStats()
		perModel := make([]stats.SessionModelStat, 0, len(perModelStats))
		for _, m := range perModelStats {
			perModel = append(perModel, stats.SessionModelStat{
				Name:      m.Name,
				TokensIn:  m.TokensIn,
				TokensOut: m.TokensOut,
				Connector: stats.FromSnapshot(m.Connector),
			})
		}
		rep.Sessions = append(rep.Sessions, stats.SessionRow{
			ID:            it.id,
			Turns:         snap.Turns,
			TokensIn:      snap.TokensIn,
			TokensOut:     snap.TokensOut,
			ToolCalls:     snap.ToolCalls,
			ContextTokens: snap.ContextTokens,
			ContextWindow: snap.ContextWindow,
			Compactions:   it.s.CompactionCount(),
			PrimaryModel:  primaryModel,
			Primary:       primary,
			Fast:          fast,
			Ephemeral:     ephemeral[it.id],
			SubAgents:     subAgents,
			PerModel:      perModel,
		})
		rep.Totals.Sessions++
		rep.Totals.Turns += snap.Turns
		rep.Totals.TokensIn += snap.TokensIn
		rep.Totals.TokensOut += snap.TokensOut
		rep.Totals.ToolCalls += snap.ToolCalls
		rep.Totals.Compactions += it.s.CompactionCount()
		rep.Totals.Primary = rep.Totals.Primary.Add(primary)
		rep.Totals.Fast = rep.Totals.Fast.Add(fast)
		// Per-model breakdown (issue #191): tokens and connector metrics are
		// attributed to the model that actually incurred them (a session that
		// switched models contributes to several entries), while the session and
		// sub-agent counts are keyed by the session's current primary model — that is
		// the model the panel scopes "sessions/sub-agents using this model" to.
		for _, m := range perModel {
			mt := modelTotals[m.Name]
			mt.Name = m.Name
			mt.TokensIn += m.TokensIn
			mt.TokensOut += m.TokensOut
			mt.Connector = mt.Connector.Add(m.Connector)
			modelTotals[m.Name] = mt
		}
		if primaryModel != "" {
			mt := modelTotals[primaryModel]
			mt.Name = primaryModel
			mt.Sessions++
			mt.SubAgents += subAgents
			modelTotals[primaryModel] = mt
		}
	}

	if toolReg != nil {
		for _, ts := range toolReg.GetAllToolStats() {
			rep.Tools = append(rep.Tools, stats.ToolStat{
				Name:        ts.Name,
				Invocations: ts.Invocations,
				Success:     ts.Success,
				Failure:     ts.Failure,
				TotalMs:     ts.TotalMs,
			})
		}
	}
	if skills != nil {
		for _, sk := range skills.GetAllSkillStats() {
			rep.Skills = append(rep.Skills, stats.SkillStat{
				Name:       sk.SkillName,
				Success:    sk.Success,
				Failure:    sk.Failure,
				TotalCalls: sk.TotalCalls,
			})
		}
		sort.Slice(rep.Skills, func(i, j int) bool { return rep.Skills[i].Name < rep.Skills[j].Name })
	}

	rep.Models = make([]stats.ModelStat, 0, len(modelTotals))
	names := make([]string, 0, len(modelTotals))
	for n := range modelTotals {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		rep.Models = append(rep.Models, modelTotals[n])
	}
	return rep
}

// SendMessageToSession sends a message to a session and executes any tool calls
// This uses the ExecuteTaskLoop for proper multi-turn tool calling support
func (g *Gogent) SendMessageToSession(ctx context.Context, sessionID, agentID, message string) (*model.CompletionResponse, error) {
	return g.SendMessageToSessionWithModel(ctx, sessionID, agentID, message, "")
}

// SendMessageToSessionWithModel sends a message to a session with a specific model
// This uses the ExecuteTaskLoop for proper multi-turn tool calling support. ctx
// bounds the (potentially long) task loop: cancelling it — e.g. when an HTTP
// client disconnects — aborts the in-flight model work (issue #24).
func (g *Gogent) SendMessageToSessionWithModel(ctx context.Context, sessionID, agentID, message, modelName string) (*model.CompletionResponse, error) {
	return g.SendMessageToSessionWithModelAndEffort(ctx, sessionID, agentID, message, modelName, "")
}

// SendMessageToSessionWithModelAndEffort is SendMessageToSessionWithModel plus a
// per-request reasoning-effort override (issue #177). A non-empty effort takes
// precedence over the selected model config's ReasoningEffort for this turn only;
// an empty effort falls back to the model config default. The override is applied
// to a shallow copy of the model config (never the shared g.config), and the
// existing provider gate still drops the parameter where unsupported — so an
// effort sent to a model without supportsReasoningEffort is silently ignored.
func (g *Gogent) SendMessageToSessionWithModelAndEffort(ctx context.Context, sessionID, agentID, message, modelName, effort string) (*model.CompletionResponse, error) {
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

	// Apply the per-session reasoning-effort override (issue #177) onto a shallow
	// copy of the model config so the shared g.config is never mutated. The
	// provider gate in buildRequest still drops reasoning_effort where unsupported,
	// so overriding a model without supportsReasoningEffort is a safe no-op.
	if effort != "" && selectedConfig != nil {
		override := *selectedConfig
		override.ReasoningEffort = effort
		selectedConfig = &override
	}

	// Point the agent's existing session at the selected model, preserving the
	// conversation transcript across user messages. Only build a fresh session
	// if the agent somehow has none.
	if selectedConfig != nil {
		// Attribute this turn's token usage to the selected model for the
		// per-model breakdown in the Statistics view (issue #57).
		userSession.SetPrimaryModel(selectedConfig.Name)
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
	// The last response may be a tool call or a final response.
	//
	// Bracket the turn with the checkpointer (issue #41): a fresh active
	// checkpoint accumulates every file mutated by this turn (root agent and any
	// sub-agents share the session), and is committed at the end — even on error
	// or cancellation — so a partially-applied turn remains undoable. An empty
	// turn (no writes) is dropped by CommitTurn.
	//
	// Mark the agent thinking for the duration of the turn and idle once it ends
	// (issue #47): both transitions fire HookStateChange, and the turn's terminal
	// outcome fires HookError or HookResponseComplete below.
	ag.SetState(agent.StateThinking)
	if g.checkpoints != nil {
		g.checkpoints.BeginTurn(sessionID)
	}
	responses, err := userSession.ExecuteTaskLoopWithModel(ctx, agentID, message, selectedConfig)
	if g.checkpoints != nil {
		g.checkpoints.CommitTurn(sessionID)
	}
	ag.SetState(agent.StateIdle)
	if err != nil {
		g.NotifyHooks(HookEvent{
			Type:      HookError,
			SessionID: sessionID,
			AgentID:   agentID,
			Error:     &model.ModelError{Message: err.Error()},
		})
		return nil, fmt.Errorf("process message: %w", err)
	}

	// Persist the updated transcript (best-effort) for crash recovery.
	g.persistSession(sessionID)

	// Return the last response (this will be the final response from the model)
	// and announce the completed turn to hooks with its text and token usage.
	if len(responses) > 0 {
		final := responses[len(responses)-1]
		g.NotifyHooks(HookEvent{
			Type:      HookResponseComplete,
			SessionID: sessionID,
			AgentID:   agentID,
			Response:  final.Content,
			Usage:     final.Usage,
		})
		return final, nil
	}

	return nil, fmt.Errorf("no responses generated")
}

// SetPlanMode toggles plan mode for a session (issue #43): in plan mode the
// agent investigates with read-only tools and proposes a plan instead of making
// changes. It is a no-op for an unknown session.
func (g *Gogent) SetPlanMode(sessionID string, on bool) {
	if us := g.GetUserSession(sessionID); us != nil {
		us.SetPlanMode(on)
	}
}

// PlanMode reports whether a session is in plan mode (issue #43).
func (g *Gogent) PlanMode(sessionID string) bool {
	if us := g.GetUserSession(sessionID); us != nil {
		return us.PlanMode()
	}
	return false
}

// HasPendingPlan reports whether a session has a plan awaiting the user's
// approval (issue #43). It is the gate the UI's "approve plan" action checks.
func (g *Gogent) HasPendingPlan(sessionID string) bool {
	if us := g.GetUserSession(sessionID); us != nil {
		return strings.TrimSpace(us.PendingPlan()) != ""
	}
	return false
}

// approvedPlanPrefix introduces the message sent to the agent once the user
// approves a plan, so the model knows it may now make changes (issue #43).
const approvedPlanPrefix = "The user approved the plan below. Implement it now using the available tools.\n\n"

// ExecuteApprovedPlan runs the session's pending plan with the full tool set
// (issue #43): it leaves plan mode, then sends an "approved — execute" turn that
// carries the plan. It is synchronous (like SendMessageToSessionWithModel); the
// caller runs it on a background goroutine. It reuses the model the planning
// turn used (the session's primary model), or the default when none is known.
func (g *Gogent) ExecuteApprovedPlan(ctx context.Context, sessionID, agentID string) (*model.CompletionResponse, error) {
	us := g.GetUserSession(sessionID)
	if us == nil {
		return nil, &SessionNotFoundError{ID: sessionID}
	}
	plan := us.PendingPlan()
	if strings.TrimSpace(plan) == "" {
		return nil, fmt.Errorf("no plan awaiting approval in session %s", sessionID)
	}
	modelName := us.PrimaryModel()
	us.SetPlanMode(false) // exit plan mode (also clears the pending plan)
	return g.SendMessageToSessionWithModel(ctx, sessionID, agentID, approvedPlanPrefix+plan, modelName)
}

// RejectPlan discards a session's pending plan without executing it, leaving
// plan mode so the user can revise and re-plan (issue #43).
func (g *Gogent) RejectPlan(sessionID string) {
	if us := g.GetUserSession(sessionID); us != nil {
		us.ClearPendingPlan()
	}
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

// bridgeModelEvent translates a model-session CallbackEvent into the matching
// lifecycle HookEvent, so the callbacks the model layer already emits reach the
// hooks registered on Gogent (issue #47). Today only context compaction
// (EventCompression) is emitted by the model session; the response/error/token
// types are mapped too so they flow automatically if the model layer starts
// emitting them. An unknown event type is dropped.
func (g *Gogent) bridgeModelEvent(sessionID, agentID string, ev model.CallbackEvent) {
	out := HookEvent{
		SessionID:   sessionID,
		AgentID:     agentID,
		Token:       ev.Token,
		Response:    ev.Response,
		Usage:       ev.Usage,
		Error:       ev.Error,
		Compression: ev.Compression,
	}
	switch ev.Type {
	case model.EventTokenReceived:
		out.Type = HookTokenReceived
	case model.EventResponseComplete:
		out.Type = HookResponseComplete
	case model.EventError:
		out.Type = HookError
	case model.EventCompression:
		out.Type = HookCompression
	default:
		return
	}
	g.NotifyHooks(out)
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
				return &command.CommandResult{Success: false, Stderr: err.Error(), ExitCode: 1}, fmt.Errorf("read file: %w", err)
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
				return &command.CommandResult{Success: false, Stderr: err.Error(), ExitCode: 1}, fmt.Errorf("write file: %w", err)
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
				return &command.CommandResult{Success: false, Stderr: err.Error(), ExitCode: 1}, fmt.Errorf("edit file: %w", err)
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

// ParseToolCall parses a tool call from a model response, returning the first
// JSON tool call found. It delegates to the shared tolerant extractor
// (tool.ParseToolCalls) so prose-wrapped, fenced, pretty-printed or reordered
// calls are all recognised (issue #32).
func (g *Gogent) ParseToolCall(response string) (*tool.ToolCall, error) {
	if calls := tool.ParseToolCalls(response); len(calls) > 0 {
		return &calls[0], nil
	}
	return nil, fmt.Errorf("no valid tool call found in response")
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

	if err != nil {
		return result, fmt.Errorf("execute tool call: %w", err)
	}
	return result, nil
}

// defaultWaitAgentEventTimeout bounds a wait_agent_event call that the model
// issues without an explicit timeout_ms. NextAgentEvent treats a non-positive
// timeout as "block until an event arrives", which would hang the whole turn if
// the coordinator waits with nothing outstanding (e.g. after all agents have
// completed, or before any launch). Capping the omitted-timeout case keeps the
// parent unblockable forever while still giving a running agent ample time to
// report; the model simply re-issues wait_agent_event on a timed_out result
// (issue #284 — the parent must never be blocked).
const defaultWaitAgentEventTimeout = 30 * time.Second

// oneShotOnlyTools are the blocking coordination tools; interactiveOnlyTools are
// the asynchronous (fire-and-forget) ones. A session is handed a registry with
// the tools of any model it does NOT expose stripped out — the default "both"
// model strips neither, so both sets coexist (issue #284).
var (
	interactiveOnlyTools = []string{"launch_agent", "agent_status", "agent_send", "agent_terminate", "wait_agent_event"}
	oneShotOnlyTools     = []string{"spawn_subagent"}
)

// toolRegistryForMode returns a copy of the global tool registry tailored to the
// given sub-agent execution model. It strips only the coordination tools the
// model does NOT expose, so the "both" model (the default, issue #284) keeps both
// spawn_subagent and the launch_agent family registered in the same session, while
// the one_shot / interactive models keep exactly one set as before.
func (g *Gogent) toolRegistryForMode(cfg config.SubAgentConfig) *tool.ToolRegistry {
	var strip []string
	if !cfg.ExposesOneShotTools() {
		strip = append(strip, oneShotOnlyTools...)
	}
	if !cfg.ExposesInteractiveTools() {
		strip = append(strip, interactiveOnlyTools...)
	}
	return g.toolRegistry.CloneWithout(strip...)
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
		g.warnf("Failed to persist config: %v", err)
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
	if err := config.SaveConfig(home, cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	return nil
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
	diagCmd := g.config.Diagnostics.Command
	diagWarn := g.config.Diagnostics.WarningPattern
	verifyCmd := g.config.Verify.Command
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
	g.toolRegistry.RegisterDiagnosticsTool(diagCmd, diagWarn)
	g.toolRegistry.RegisterVerifyTool(verifyCmd)
	subAgentTO := time.Duration(t.SubAgentSecondsOrDefault()) * time.Second
	for _, s := range sessions {
		s.SetSubAgentTimeout(subAgentTO)
	}

	if err := g.SaveConfig(); err != nil {
		g.warnf("Failed to persist config: %v", err)
	}
}

// Notifications returns the desktop/terminal notification configuration, falling
// back to the built-in defaults when none is configured (e.g. an older
// config.json predating the setting). See issue #59.
func (g *Gogent) Notifications() config.NotifyConfig {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.config.NotifyConfig()
}

// SetNotifications updates the notification configuration and persists it to
// disk so it survives restarts.
func (g *Gogent) SetNotifications(n config.NotifyConfig) {
	g.mu.Lock()
	g.config.SetNotifyConfig(n)
	g.mu.Unlock()
	if err := g.SaveConfig(); err != nil {
		g.warnf("Failed to persist config: %v", err)
	}
}

// Theme returns the TUI colour-palette configuration (issue #66/#103). The zero
// value is the built-in "default" palette, so a config.json predating the
// setting yields the original colours.
func (g *Gogent) Theme() config.ThemeConfig {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.config.Theme
}

// SetTheme updates the colour-palette configuration and persists it to disk so
// the preferred theme is restored on the next launch (issue #103). The UI
// re-applies the palette to its live widgets separately.
func (g *Gogent) SetTheme(t config.ThemeConfig) {
	g.mu.Lock()
	g.config.Theme = t
	g.mu.Unlock()
	if err := g.SaveConfig(); err != nil {
		g.warnf("Failed to persist config: %v", err)
	}
}

// Budget returns the per-session token-budget configuration that drives the
// status-bar budget alert (issue #63, the UI side of #28). A zero TokenBudget
// means alerting is off.
func (g *Gogent) Budget() config.BudgetConfig {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.config.Budget
}

// SetBudget updates the token-budget configuration and persists it to disk so it
// survives restarts. The UI picks the new value up on its next status refresh.
func (g *Gogent) SetBudget(b config.BudgetConfig) {
	g.mu.Lock()
	g.config.Budget = b
	g.mu.Unlock()
	if err := g.SaveConfig(); err != nil {
		g.warnf("Failed to persist config: %v", err)
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
		g.warnf("Failed to persist config: %v", err)
	}
	return nil
}

// SetDefaultModel marks the named model as the default for new sessions and
// persists it to config (issue #296). The name must match a configured model;
// new sessions resolve their backend from it via defaultConnection().
func (g *Gogent) SetDefaultModel(name string) error {
	g.mu.Lock()
	known := false
	for _, m := range g.config.ModelConfigs {
		if m != nil && m.Name == name {
			known = true
			break
		}
	}
	if known {
		g.config.DefaultModel = name
	}
	g.mu.Unlock()

	if !known {
		return fmt.Errorf("model %q not found", name)
	}
	if err := g.SaveConfig(); err != nil {
		g.warnf("Failed to persist config: %v", err)
	}
	return nil
}

// DefaultModelName returns the configured default-model name for new sessions.
func (g *Gogent) DefaultModelName() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.config.DefaultModel
}

// ScanModels queries the given backend's model-listing endpoint and returns the
// advertised model ids, so the model editor can offer a pick-list instead of a
// free-text model id. The draft config need not be saved first; only its
// api_type, endpoint and api_key are used to reach the backend.
func (g *Gogent) ScanModels(cfg config.ModelConfig) ([]string, error) {
	conn := model.NewModelConnectionFromConfig(&cfg)
	infos, err := conn.ListModels()
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
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
