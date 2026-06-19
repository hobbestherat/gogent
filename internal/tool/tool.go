package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"gogent/internal/mathexpr"
	"gogent/internal/permission"
	"gogent/internal/shell"
	"gogent/internal/web"
)

type PermissionDecision string

const (
	Allow PermissionDecision = "allow"
	Deny  PermissionDecision = "deny"
	Ask   PermissionDecision = "ask"
)

type ToolContext struct {
	SessionID         string
	AgentID           string
	MessageID         string
	ToolCallID        string
	PermissionService *permission.Service
	ToolCallback      func(toolName string, args map[string]interface{}) error
	// Context carries the cancellation scope of the agent loop that invoked the
	// tool. Tools that themselves run a model loop (e.g. spawn_subagent) thread it
	// down so a stopped/closed parent also cancels its in-flight children (issue
	// #24). It may be nil for callers that pre-date context plumbing; treat nil as
	// context.Background().
	Context context.Context
}

type Tool struct {
	Name        string
	Description string
	InputSchema interface{}
	Execute     func(args map[string]interface{}, context ToolContext) (interface{}, error)
}

type ToolRegistry struct {
	tools map[string]*Tool
	// ShellTimeout bounds shell-tool executions. Zero falls back to a built-in
	// default (see RegisterShellTool).
	ShellTimeout time.Duration
	// WorkspaceRoot is the directory shell commands run in. Empty falls back to
	// the process working directory.
	WorkspaceRoot string
	// NetworkTimeout bounds web_fetch HTTP requests. Zero falls back to a
	// built-in default (see web.DefaultTimeout).
	NetworkTimeout time.Duration
	// Permission gates side-effecting tools (shell, etc.). May be nil.
	Permission *permission.Service
	// mu guards enabled, which is per-registry: a cloned registry starts with
	// every tool enabled regardless of its parent's toggles. The tools map itself
	// is populated once at startup and read thereafter, so it stays unlocked.
	mu      sync.RWMutex
	enabled map[string]bool
	// counts holds the per-tool invocation/outcome/duration counters. It is a
	// pointer so a clone family (see CloneWithout) shares one set: calls executed
	// on a session's mode-filtered clone aggregate into the same counters the
	// global registry exposes for the Statistics view.
	counts *toolCounts
}

// toolCounts holds the shared, mutex-guarded per-tool counters.
type toolCounts struct {
	mu          sync.RWMutex
	invocations map[string]int
	lastUsed    map[string]time.Time
	success     map[string]int
	failure     map[string]int
	totalMs     map[string]int64
}

func newToolCounts() *toolCounts {
	return &toolCounts{
		invocations: make(map[string]int),
		lastUsed:    make(map[string]time.Time),
		success:     make(map[string]int),
		failure:     make(map[string]int),
		totalMs:     make(map[string]int64),
	}
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools:   make(map[string]*Tool),
		enabled: make(map[string]bool),
		counts:  newToolCounts(),
	}
}

func (tr *ToolRegistry) Register(tool *Tool) {
	tr.tools[tool.Name] = tool
}

// SchemaJSON serializes a tool's input schema to indented JSON for display (the
// Resources browser shows it in a tool's detail pane). It returns "" for a nil
// schema or a marshaling failure. Go's encoder sorts object keys, so the output
// is stable.
func SchemaJSON(schema interface{}) string {
	if schema == nil {
		return ""
	}
	b, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

func (tr *ToolRegistry) Get(name string) *Tool {
	return tr.tools[name]
}

func (tr *ToolRegistry) List() []*Tool {
	tools := make([]*Tool, 0, len(tr.tools))
	for _, t := range tr.tools {
		tools = append(tools, t)
	}
	return tools
}

// IsEnabled reports whether a tool is enabled. Tools are enabled by default;
// SetEnabled is the only way to disable one.
func (tr *ToolRegistry) IsEnabled(name string) bool {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	enabled, ok := tr.enabled[name]
	if !ok {
		return true
	}
	return enabled
}

// SetEnabled enables or disables a tool. A disabled tool is hidden from the
// model (it is omitted from the advertised tool set) and refused at execution
// time, so the agent neither sees nor can call it.
func (tr *ToolRegistry) SetEnabled(name string, enabled bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.enabled[name] = enabled
}

// ListEnabled returns the currently enabled tools. It backs the tool set
// advertised to the model, so disabling a tool drops it from the agent's view.
func (tr *ToolRegistry) ListEnabled() []*Tool {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	tools := make([]*Tool, 0, len(tr.tools))
	for name, t := range tr.tools {
		if enabled, ok := tr.enabled[name]; ok && !enabled {
			continue
		}
		tools = append(tools, t)
	}
	return tools
}

// Invocations returns how many times a tool has been invoked through the
// registry (validated calls only).
func (tr *ToolRegistry) Invocations(name string) int {
	tr.counts.mu.RLock()
	defer tr.counts.mu.RUnlock()
	return tr.counts.invocations[name]
}

// LastUsed returns the time of a tool's most recent invocation, or the zero
// time if it has never been used.
func (tr *ToolRegistry) LastUsed(name string) time.Time {
	tr.counts.mu.RLock()
	defer tr.counts.mu.RUnlock()
	return tr.counts.lastUsed[name]
}

// recordInvocation bumps a tool's invocation count and last-used timestamp. It
// is called once a tool call has passed validation and is about to run.
func (tr *ToolRegistry) recordInvocation(name string) {
	tr.counts.mu.Lock()
	defer tr.counts.mu.Unlock()
	tr.counts.invocations[name]++
	tr.counts.lastUsed[name] = time.Now()
}

// recordOutcome records the result and duration of a tool execution that already
// passed validation. success follows the returned ToolCallResponse.Success flag
// (an error or a non-success response counts as a failure). It pairs with
// recordInvocation, which bumped the invocation count just before execution.
func (tr *ToolRegistry) recordOutcome(name string, success bool, durationMs int64) {
	tr.counts.mu.Lock()
	defer tr.counts.mu.Unlock()
	if success {
		tr.counts.success[name]++
	} else {
		tr.counts.failure[name]++
	}
	tr.counts.totalMs[name] += durationMs
}

// ToolStats is a point-in-time view of one tool's usage: how many times it ran,
// the success/failure split and the cumulative/average execution time. It is the
// per-tool row the Statistics view (issue #57) renders.
type ToolStats struct {
	Name        string
	Invocations int
	Success     int
	Failure     int
	TotalMs     int64
}

// AvgMs returns the mean execution time per invocation, or 0 when the tool has
// never run.
func (s ToolStats) AvgMs() int64 {
	if s.Invocations == 0 {
		return 0
	}
	return s.TotalMs / int64(s.Invocations)
}

// GetAllToolStats returns a ToolStats row for every registered tool, sorted by
// name for a stable display. Tools that have never run report zero counters.
func (tr *ToolRegistry) GetAllToolStats() []ToolStats {
	tr.counts.mu.RLock()
	defer tr.counts.mu.RUnlock()
	out := make([]ToolStats, 0, len(tr.tools))
	names := make([]string, 0, len(tr.tools))
	for name := range tr.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, ToolStats{
			Name:        name,
			Invocations: tr.counts.invocations[name],
			Success:     tr.counts.success[name],
			Failure:     tr.counts.failure[name],
			TotalMs:     tr.counts.totalMs[name],
		})
	}
	return out
}

// CloneWithout returns a shallow copy of the registry with the named tools
// removed. It is used to hand sub-agents a registry that omits the sub-agent
// spawning tools when recursive sub-agents are disabled. The per-tool counters
// are shared with the parent (a fresh clone family aggregates into one set) so
// usage recorded on a session's mode-filtered clone reaches the global registry
// the Statistics view reads; the enabled map is intentionally not shared, so a
// clone starts with every tool enabled regardless of its parent's toggles.
func (tr *ToolRegistry) CloneWithout(names ...string) *ToolRegistry {
	excluded := make(map[string]bool, len(names))
	for _, n := range names {
		excluded[n] = true
	}
	clone := NewToolRegistry()
	clone.ShellTimeout = tr.ShellTimeout
	clone.WorkspaceRoot = tr.WorkspaceRoot
	clone.NetworkTimeout = tr.NetworkTimeout
	clone.Permission = tr.Permission
	clone.counts = tr.counts // share counters across the clone family
	for name, t := range tr.tools {
		if excluded[name] {
			continue
		}
		clone.tools[name] = t
	}
	return clone
}

type ToolCall struct {
	Tool   string                 `json:"tool"`
	Args   map[string]interface{} `json:"args"`
	CallID string                 `json:"call_id,omitempty"`
}

type ToolCallResponse struct {
	Success bool        `json:"success"`
	Result  interface{} `json:"result,omitempty"`
	Error   string      `json:"error,omitempty"`
}

func (tr *ToolRegistry) ExecuteToolCall(toolCall *ToolCall, ctx ToolContext) (resp *ToolCallResponse, err error) {
	tool := tr.tools[toolCall.Tool]
	if tool == nil {
		return &ToolCallResponse{
			Success: false,
			Error:   fmt.Sprintf("unknown tool: %s", toolCall.Tool),
		}, nil
	}

	if !tr.IsEnabled(toolCall.Tool) {
		return &ToolCallResponse{
			Success: false,
			Error:   fmt.Sprintf("tool is disabled: %s", toolCall.Tool),
		}, nil
	}

	if err := validateArgs(toolCall.Args, tool.InputSchema); err != nil {
		return &ToolCallResponse{
			Success: false,
			Error:   fmt.Sprintf("invalid args: %v", err),
		}, nil
	}

	// Count the invocation now that it has been validated and is about to run.
	tr.recordInvocation(toolCall.Tool)

	// Make the registry's permission service available to tools that gate
	// through the context (the agent loop builds a context without it).
	if ctx.PermissionService == nil {
		ctx.PermissionService = tr.Permission
	}
	// Track tool call
	if ctx.ToolCallback != nil {
		ctx.ToolCallback(toolCall.Tool, toolCall.Args)
	}

	start := time.Now()
	// Contain a panicking tool (unchecked type assertion, parser slice index,
	// nil deref, ...) so one bad tool call surfaces as an ordinary tool error
	// instead of crashing the process and every concurrent session (issue #8).
	defer func() {
		if r := recover(); r != nil {
			tr.recordOutcome(toolCall.Tool, false, time.Since(start).Milliseconds())
			err = fmt.Errorf("tool %q panicked: %v", toolCall.Tool, r)
			resp = &ToolCallResponse{Success: false, Error: err.Error()}
		}
	}()
	result, err := tool.Execute(toolCall.Args, ctx)
	tr.recordOutcome(toolCall.Tool, err == nil, time.Since(start).Milliseconds())
	if err != nil {
		return &ToolCallResponse{
			Success: false,
			Error:   err.Error(),
		}, err
	}

	return &ToolCallResponse{
		Success: true,
		Result:  result,
	}, nil
}

// RegisterCalcTool registers the calc tool for calculations
func (tr *ToolRegistry) RegisterCalcTool() {
	tr.Register(&Tool{
		Name:        "calc",
		Description: "Calculate mathematical expressions like 5+5 or 10*20/5. Returns the result of the calculation.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"expression": map[string]interface{}{"type": "string"}},
			"required":   []string{"expression"},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			expression, ok := args["expression"].(string)
			if !ok {
				return nil, fmt.Errorf("expression argument is required")
			}

			// Evaluate using the shared, hardened evaluator.
			result, err := mathexpr.Eval(expression)
			if err != nil {
				return nil, fmt.Errorf("calculation error: %v", err)
			}

			return map[string]interface{}{
				"expression": expression,
				"result":     fmt.Sprintf("%.4f", result),
			}, nil
		},
	})
}

// RegisterShellTool registers the shell tool for executing shell commands
func (tr *ToolRegistry) RegisterShellTool() {
	tr.Register(&Tool{
		Name:        "shell",
		Description: "Execute a shell command. Use this when you need to run shell commands like curl, wget, ls, grep, etc.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"command": map[string]interface{}{"type": "string"}},
			"required":   []string{"command"},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			command, ok := args["command"].(string)
			if !ok {
				return nil, fmt.Errorf("command argument is required")
			}

			// Gate shell execution. The session-wide gate is asked once (the user
			// may choose "always"); then any path that escapes the workspace is
			// gated per external root folder.
			if tr.Permission != nil {
				rc := permission.RequestContext{SessionID: ctx.SessionID, Agent: ctx.AgentID}
				if err := tr.Permission.CheckWithContext(rc, permission.ActionShell, "", command); err != nil {
					return nil, err
				}
				for _, root := range shell.ExternalRoots(command, tr.WorkspaceRoot) {
					if err := tr.Permission.CheckWithContext(rc, permission.ActionExternal, root, command); err != nil {
						return nil, err
					}
				}
			}

			// Honor the configured shell timeout, defaulting to 5 minutes.
			timeout := tr.ShellTimeout
			if timeout <= 0 {
				timeout = 5 * time.Minute
			}

			// Execute the shell command
			result, err := shell.Execute(command, shell.ShellConfig{
				Timeout:   timeout,
				MaxOutput: 1024 * 1024,
				Dir:       tr.WorkspaceRoot,
			})

			if err != nil {
				return nil, fmt.Errorf("failed to execute command: %v", err)
			}

			return map[string]interface{}{
				"command":   command,
				"stdout":    result.Stdout,
				"stderr":    result.Stderr,
				"exit_code": result.ExitCode,
				"timeout":   result.Timeout,
				"error":     result.Error,
			}, nil
		},
	})
}

// RegisterWebFetchTool registers the web_fetch tool: it downloads an http(s)
// URL, extracts the main content as readability-style Markdown, caps the size,
// and serves repeat requests from a short-lived cache. Network access is gated
// per domain through the permission service (ActionNetwork), so an "always"
// grant is scoped to a single host. The fetcher (and its cache) is created once
// here and captured by the tool closure, so it is shared across calls and any
// sub-agent registries cloned from this one.
func (tr *ToolRegistry) RegisterWebFetchTool() {
	fetcher := web.NewFetcher(web.Config{Timeout: tr.NetworkTimeout})
	tr.Register(&Tool{
		Name: "web_fetch",
		Description: "Fetch a web page over http(s) and return its main content as Markdown " +
			"(readability-extracted, size-capped, short-TTL cached). Prefer this over running " +
			"curl in the shell for reading docs, API references and error lookups: it returns " +
			"clean Markdown instead of raw HTML. Network access is gated per domain.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"url":        map[string]interface{}{"type": "string", "description": "Absolute http(s) URL to fetch."},
				"max_length": map[string]interface{}{"type": "integer", "description": "Optional cap on the number of Markdown characters returned."},
			},
			"required": []string{"url"},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			rawURL, ok := args["url"].(string)
			if !ok || strings.TrimSpace(rawURL) == "" {
				return nil, fmt.Errorf("url argument is required")
			}
			rawURL = strings.TrimSpace(rawURL)
			u, err := url.Parse(rawURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return nil, fmt.Errorf("url must be an absolute http(s) URL")
			}

			// Gate network access per host so an "always" grant covers one domain.
			perm := ctx.PermissionService
			if perm == nil {
				perm = tr.Permission
			}
			if perm != nil {
				rc := permission.RequestContext{SessionID: ctx.SessionID, Agent: ctx.AgentID}
				if err := perm.CheckWithContext(rc, permission.ActionNetwork, u.Host, rawURL); err != nil {
					return nil, err
				}
			}

			res, err := fetcher.Fetch(rawURL)
			if err != nil {
				return nil, fmt.Errorf("web_fetch failed: %v", err)
			}

			markdown, truncated := res.Markdown, res.Truncated
			if max, ok := intArg(args["max_length"]); ok && max > 0 {
				if cut, didCut := web.TruncateChars(markdown, max); didCut {
					markdown, truncated = cut, true
				}
			}

			return map[string]interface{}{
				"url":        res.URL,
				"title":      res.Title,
				"markdown":   markdown,
				"truncated":  truncated,
				"from_cache": res.FromCache,
			}, nil
		},
	})
}

// intArg coerces a JSON-decoded argument to an int. JSON numbers decode to
// float64; integer Go types are accepted defensively for non-JSON callers.
func intArg(v interface{}) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

type StructuredOutput struct {
	Response string    `json:"response"`
	ToolCall *ToolCall `json:"tool_call,omitempty"`
	Final    bool      `json:"final,omitempty"`
}

func (tr *ToolRegistry) ParseToolCall(response string) (*ToolCall, error) {
	var toolCall ToolCall
	if err := json.Unmarshal([]byte(response), &toolCall); err == nil {
		return &toolCall, nil
	}

	if extracted := extractJSON(response); extracted != "" {
		if err := json.Unmarshal([]byte(extracted), &toolCall); err == nil {
			return &toolCall, nil
		}
	}

	return nil, fmt.Errorf("no valid tool call found in response")
}

func extractJSON(text string) string {
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

	if idx := strings.Index(text, "{"); idx != -1 {
		return extractJSONFrom(text[idx:])
	}

	return ""
}

func extractJSONFrom(text string) string {
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

// UnmarshalJSON is a helper to unmarshal JSON
func UnmarshalJSON(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}
