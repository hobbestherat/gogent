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
	// ReadOnly marks a tool as read-only/idempotent: it inspects the workspace
	// (or a remote resource) without mutating shared state, so several such calls
	// from one turn can run concurrently without racing on files or on call
	// ordering. Side-effecting tools (write, edit, shell, ...) leave it false and
	// are executed serially. Unknown/dynamic tools (e.g. MCP) default to false,
	// the safe choice. It is the property the parallel tool-call fast-path keys on
	// (issue #50).
	ReadOnly bool
	// Strict opts a tool into strict tool-use: its advertised schema is marked
	// strict on providers that enforce it (OpenAI structured outputs /
	// constrained decoding), so the model's arguments are guaranteed to validate
	// against InputSchema rather than merely prompted to — eliminating
	// type-coercion errors ("2" vs 2) and validate-and-retry rounds (issue #359).
	// It is opt-in and defaults false (the prior all-non-strict behavior). Only
	// set it for tools whose schema is in the supported subset: a closed object
	// (additionalProperties:false), no union-typed properties, no recursive $ref.
	// It is NOT derived from ReadOnly: a read-only tool with a union-typed schema
	// (e.g. spawn_subagent) must stay non-strict, since a strict tool forces
	// parallel_tool_calls:false on OpenAI-compatible providers and would suppress
	// batched spawns (issue #282, see toolDefsFromRegistry).
	Strict bool
	// InputExamples are optional, schema-conformant example argument objects for
	// format-sensitive tools (Anthropic's input_examples guidance, issue #361).
	// They are prompt-level documentation: surfaced verbatim in the
	// registry-rendered tool docs (RenderToolDocs) to steer the model toward the
	// right call shape, NOT authoritative schema metadata — they do not affect
	// validation or execution and are omitted from every wire format when empty.
	InputExamples []map[string]interface{}
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
	// resultBytes accumulates the serialized byte size of every tool result, so
	// the Statistics view can surface which tools dump the most context (issue
	// #361). It is a diagnostic counter only.
	resultBytes map[string]int64
}

func newToolCounts() *toolCounts {
	return &toolCounts{
		invocations: make(map[string]int),
		lastUsed:    make(map[string]time.Time),
		success:     make(map[string]int),
		failure:     make(map[string]int),
		totalMs:     make(map[string]int64),
		resultBytes: make(map[string]int64),
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
	// Normalize the advertised schema once, here, so validation, the Resources
	// display, and every provider's wire format all share one portable schema
	// (object root with a properties map, no provider-rejected keywords) — see
	// NormalizeSchema. MCP servers in particular hand us arbitrary schemas.
	tool.InputSchema = NormalizeSchema(tool.InputSchema)
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
	// Sort by name so the listing is deterministic — tr.tools is a Go map, whose
	// iteration order is randomized (issue #361).
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
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
	// Sort by name so the advertised tool set has a stable order across runs
	// (tr.tools is a randomized Go map); a deterministic order also keeps the
	// provider's cached tool-definition prefix stable (issue #361).
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools
}

// RenderToolDocs renders a deterministic, registry-derived plain-text listing of
// the enabled tools — each tool's name, full Description, and its top-level
// schema parameters with their descriptions (nested array-item fields are
// summarized by the parameter's own description, not expanded). It exists so
// that any prompt wanting an inline tool
// summary can generate it from the registry instead of hand-maintaining a list
// that drifts from Tool.Description/InputSchema (issue #357). The authoritative
// contract sent to the model is still the native function definitions built from
// Tool.Description + InputSchema; this is only a human-readable echo for the
// legacy single-shot prompt paths, and being generated it can never drift.
//
// Output is sorted by tool name for stable, diff-friendly rendering.
func (tr *ToolRegistry) RenderToolDocs() string {
	tools := tr.ListEnabled()
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

	var b strings.Builder
	for _, t := range tools {
		fmt.Fprintf(&b, "### %s\n", t.Name)
		if t.Description != "" {
			b.WriteString(t.Description)
			b.WriteString("\n")
		}
		for _, line := range renderSchemaParams(t.InputSchema) {
			b.WriteString(line)
			b.WriteString("\n")
		}
		for _, line := range renderInputExamples(t.InputExamples) {
			b.WriteString(line)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderSchemaParams turns a tool's top-level InputSchema properties into stable,
// name-sorted "- name (required, type): description" lines. It reads only the
// schema, so it stays in lockstep with the advertised contract.
func renderSchemaParams(rawSchema interface{}) []string {
	schema, ok := rawSchema.(map[string]interface{})
	if !ok {
		return nil
	}
	props, ok := schema["properties"].(map[string]interface{})
	if !ok || len(props) == 0 {
		return nil
	}

	required := map[string]bool{}
	switch req := schema["required"].(type) {
	case []string:
		for _, r := range req {
			required[r] = true
		}
	case []interface{}:
		for _, r := range req {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
	}

	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)

	lines := make([]string, 0, len(names))
	for _, name := range names {
		sub, _ := props[name].(map[string]interface{})
		typ, _ := sub["type"].(string)
		desc, _ := sub["description"].(string)

		var meta []string
		if required[name] {
			meta = append(meta, "required")
		}
		if typ != "" {
			meta = append(meta, typ)
		}
		line := "- " + name
		if len(meta) > 0 {
			line += " (" + strings.Join(meta, ", ") + ")"
		}
		if desc != "" {
			line += ": " + desc
		}
		lines = append(lines, line)
	}
	return lines
}

// renderInputExamples turns a tool's optional InputExamples into stable
// "  {...}" lines under an "Examples:" header, each example compact-JSON encoded
// (Go sorts object keys, so the output is deterministic). It returns nil when the
// tool declares no examples, so unchanged tools render exactly as before.
func renderInputExamples(examples []map[string]interface{}) []string {
	if len(examples) == 0 {
		return nil
	}
	lines := make([]string, 0, len(examples)+1)
	lines = append(lines, "Examples:")
	for _, ex := range examples {
		b, err := json.Marshal(ex)
		if err != nil {
			continue
		}
		lines = append(lines, "  "+string(b))
	}
	// All examples failed to marshal: drop the dangling header.
	if len(lines) == 1 {
		return nil
	}
	return lines
}

// Invocations returns how many times a tool has been invoked through the
// registry (validated calls only).
func (tr *ToolRegistry) Invocations(name string) int {
	tr.counts.mu.RLock()
	defer tr.counts.mu.RUnlock()
	return tr.counts.invocations[name]
}

// ResultBytes returns the cumulative serialized byte size of a tool's results
// (issue #361). It is 0 for a tool that has never produced a result.
func (tr *ToolRegistry) ResultBytes(name string) int64 {
	tr.counts.mu.RLock()
	defer tr.counts.mu.RUnlock()
	return tr.counts.resultBytes[name]
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
// (an error or a non-success response counts as a failure). resultBytes is the
// serialized size of the raw value the tool returned (0 when it returned no
// result at all, e.g. a panic); a payload returned alongside an error still
// counts. It is accumulated per tool for the Statistics view (issue #361). It
// pairs with recordInvocation, which bumped the invocation count just before
// execution.
func (tr *ToolRegistry) recordOutcome(name string, success bool, durationMs, resultBytes int64) {
	tr.counts.mu.Lock()
	defer tr.counts.mu.Unlock()
	if success {
		tr.counts.success[name]++
	} else {
		tr.counts.failure[name]++
	}
	tr.counts.totalMs[name] += durationMs
	tr.counts.resultBytes[name] += resultBytes
}

// resultByteLen measures the serialized byte size of a tool result the way it is
// handed to the model: JSON encoding, falling back to fmt for the rare value that
// does not marshal. A nil result (a failed or panicking call) is zero bytes.
func resultByteLen(result interface{}) int64 {
	if result == nil {
		return 0
	}
	if b, err := json.Marshal(result); err == nil {
		return int64(len(b))
	}
	return int64(len(fmt.Sprintf("%v", result)))
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
	// ResultBytes is the cumulative serialized size of this tool's results, a
	// diagnostic for which tools dump the most context (issue #361).
	ResultBytes int64
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
			ResultBytes: tr.counts.resultBytes[name],
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

// CloneForPlanMode returns a clone exposing only read-only tools plus the named
// extras, stripping every side-effecting tool so a planning agent cannot mutate
// the workspace (issue #43). It backs the plan-mode turn: write/edit/shell and
// the sub-agent coordination tools are removed, leaving the read-only
// investigation tools (and the named extras such as "todo" and
// "structured_output"). The per-tool counters are shared with the source (see
// CloneWithout) so plan-mode calls still aggregate into the Statistics view.
func (tr *ToolRegistry) CloneForPlanMode(keep ...string) *ToolRegistry {
	keepSet := make(map[string]bool, len(keep))
	for _, n := range keep {
		keepSet[n] = true
	}
	var strip []string
	for name, t := range tr.tools {
		if t.ReadOnly || keepSet[name] {
			continue
		}
		strip = append(strip, name)
	}
	if len(strip) == 0 {
		// Every tool is read-only or kept; CloneWithout with no exclusions still
		// produces an independent clone sharing the counters.
		return tr.CloneWithout()
	}
	return tr.CloneWithout(strip...)
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
	// Resolve the called name to a registered tool. The direct lookup wins; only
	// on a miss do we attempt the MCP bare-name fallback (see resolveMCPBareName),
	// which re-targets `name` to the namespaced tool. `name` — not toolCall.Tool —
	// is used for every downstream step (enabled check, validation, counters,
	// callback) so stats and gating attribute to the resolved tool.
	name := toolCall.Tool
	tool := tr.tools[name]
	if tool == nil {
		// Bare-name fallback is scoped to the JSON-text tool-call path (models
		// without native tool-calling), which is exactly the path that carries no
		// CallID — native calls always have one and are already constrained to the
		// advertised namespaced name. Gating on CallID keeps native tool-calling
		// semantics byte-identical: a native call with an unknown bare name still
		// errors rather than silently routing to a same-suffix MCP tool (issue #360).
		if toolCall.CallID == "" {
			resolved, candidates := tr.resolveMCPBareName(name)
			if len(candidates) > 1 {
				return &ToolCallResponse{
					Success: false,
					Error: fmt.Sprintf("unknown tool: %s (ambiguous MCP bare name; candidates: %s)",
						name, strings.Join(candidates, ", ")),
				}, nil
			}
			if resolved != "" {
				name = resolved
				tool = tr.tools[name]
			}
		}
		if tool == nil {
			return &ToolCallResponse{
				Success: false,
				Error:   fmt.Sprintf("unknown tool: %s", name),
			}, nil
		}
	}

	if !tr.IsEnabled(name) {
		return &ToolCallResponse{
			Success: false,
			Error:   fmt.Sprintf("tool is disabled: %s", name),
		}, nil
	}

	if err := validateArgs(toolCall.Args, tool.InputSchema); err != nil {
		return &ToolCallResponse{
			Success: false,
			Error:   fmt.Sprintf("invalid args: %v", err),
		}, nil
	}

	// Count the invocation now that it has been validated and is about to run.
	tr.recordInvocation(name)

	// Make the registry's permission service available to tools that gate
	// through the context (the agent loop builds a context without it).
	if ctx.PermissionService == nil {
		ctx.PermissionService = tr.Permission
	}
	// Track tool call
	if ctx.ToolCallback != nil {
		_ = ctx.ToolCallback(name, toolCall.Args)
	}

	start := time.Now()
	// Contain a panicking tool (unchecked type assertion, parser slice index,
	// nil deref, ...) so one bad tool call surfaces as an ordinary tool error
	// instead of crashing the process and every concurrent session (issue #8).
	defer func() {
		if r := recover(); r != nil {
			tr.recordOutcome(name, false, time.Since(start).Milliseconds(), 0)
			err = fmt.Errorf("tool %q panicked: %v", name, r)
			resp = &ToolCallResponse{Success: false, Error: err.Error()}
		}
	}()
	result, err := tool.Execute(toolCall.Args, ctx)
	// Measure the raw result the tool returned. A tool that returns a payload
	// alongside an error still produced bytes, so they are counted; a call with no
	// result at all (a panic, see the deferred recover above) contributes zero.
	tr.recordOutcome(name, err == nil, time.Since(start).Milliseconds(), resultByteLen(result))
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

// mcpToolPrefix is the namespace MCP tools are registered under
// (mcp__<server>__<tool>). It is duplicated here from internal/gogent because the
// tool package must not import gogent (gogent imports tool); the two must stay in
// sync.
const mcpToolPrefix = "mcp__"

// resolveMCPBareName resolves a bare (unprefixed) tool name to a registered MCP
// tool's namespaced name "mcp__<server>__<bare>". It is the fallback for the
// JSON-text tool-call path: models without native tool-calling emit the bare name
// they saw in the MCP server's own tools/list rather than the namespaced form, so
// the direct registry lookup misses. It runs only after that miss, so exact and
// native names always win first.
//
// It considers only tools registered under the mcp__ prefix and matches the final
// "__<segment>" against the bare name (suffix "__"+bare). The double underscore
// prevents partial-word false matches (suffix "__greet" does not match
// "mcp__demo__do_greet"). It returns:
//   - (resolved, nil) when exactly one MCP tool carries that bare name;
//   - ("", candidates) when more than one does — the caller errors with the sorted
//     candidate list rather than silently mis-routing;
//   - ("", nil) when none does.
func (tr *ToolRegistry) resolveMCPBareName(bare string) (resolved string, candidates []string) {
	if bare == "" {
		return "", nil
	}
	suffix := "__" + bare
	var matches []string
	for name := range tr.tools {
		if strings.HasPrefix(name, mcpToolPrefix) && strings.HasSuffix(name, suffix) {
			matches = append(matches, name)
		}
	}
	switch len(matches) {
	case 0:
		return "", nil
	case 1:
		return matches[0], nil
	default:
		sort.Strings(matches)
		return "", matches
	}
}

// RegisterCalcTool registers the calc tool for calculations
func (tr *ToolRegistry) RegisterCalcTool() {
	tr.Register(&Tool{
		Name: "calc",
		Description: "Evaluate a math expression and return the exact result. " +
			"Prefer this over guessing arithmetic or shelling out to python/bc. " +
			"Operators: + - * /, power (** or ^), unary minus, parentheses; % is integer modulo only (for non-integers use mod(x,y)); " +
			"a comparison (>, <, ==, !=) is only valid as a ternary condition, e.g. (a>b ? a : b). " +
			"Functions: sqrt cbrt pow hypot exp log log2 log10; sin cos tan asin acos atan atan2 (radians) with deg()/rad() converters; " +
			"sinh cosh tanh; abs floor ceil round trunc sign mod min max; factorial (or fact, also postfix n!) gcd lcm; sum mean median. " +
			"Constants: pi e tau phi sqrt2; physics c G g h hbar k Na R sigma epsilon0 mu0 echarge me mp. " +
			"Integer results print cleanly (2+2 -> 4); fractionals keep full precision (1/3 -> 0.3333333333333333). " +
			`Examples: {"expression":"sqrt(2)"}, {"expression":"sin(pi/2)"}, {"expression":"factorial(5)"}, {"expression":"G*5.97e24/(6.371e6)^2"}.`,
		ReadOnly: true,
		Strict:   true,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"expression": map[string]interface{}{
					"type":        "string",
					"description": "Math expression, e.g. \"2**10\", \"sqrt(2)\", \"sin(pi/2)\", \"log(e)\", \"factorial(5)\", \"G*5.97e24/(6.371e6)^2\".",
				},
			},
			"required":             []string{"expression"},
			"additionalProperties": false,
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			expression, ok := args["expression"].(string)
			if !ok {
				return nil, fmt.Errorf("expression argument is required")
			}

			// Evaluate using the shared, hardened evaluator and format the result
			// cleanly (integers without a trailing ".0000", full precision for
			// fractionals). The /calc command uses the same formatter so both
			// consumers agree.
			result, err := mathexpr.EvalFormatted(expression)
			if err != nil {
				return nil, fmt.Errorf("calculation error: %v", err)
			}

			return map[string]interface{}{
				"expression": expression,
				"result":     result,
			}, nil
		},
	})
}

// RegisterShellTool registers the shell tool for executing shell commands
func (tr *ToolRegistry) RegisterShellTool() {
	tr.Register(&Tool{
		Name: "shell",
		Description: "Execute a shell command from the workspace root and return its stdout, stderr and exit code. " +
			"Use it for tasks the dedicated tools do not cover — running build scripts, package managers, " +
			"git porcelain beyond the git tool, or arbitrary CLIs. Prefer the purpose-built tools over shell " +
			"whenever one fits: grep for searching file contents, glob/list for finding files, git for version " +
			"control, diagnostics for compiling/linting and verify for running tests — they need no permission " +
			"prompt, no shell quoting, and return structured results. Commands run with a timeout (5 minutes by " +
			"default) and a 1 MB output cap, are permission-gated (a command touching paths outside the workspace " +
			"is additionally gated per external root), and cannot be interactive.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string", "description": "The shell command line to execute from the workspace root."},
			},
			"required": []string{"command"},
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
					return nil, fmt.Errorf("permission check: %w", err)
				}
				for _, root := range shell.ExternalRoots(command, tr.WorkspaceRoot) {
					if err := tr.Permission.CheckWithContext(rc, permission.ActionExternal, root, command); err != nil {
						return nil, fmt.Errorf("permission check: %w", err)
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
		Name:     "web_fetch",
		ReadOnly: true,
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
					return nil, fmt.Errorf("permission check: %w", err)
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
	if calls := ParseToolCalls(response); len(calls) > 0 {
		return &calls[0], nil
	}
	return nil, fmt.Errorf("no valid tool call found in response")
}

// ParseToolCalls is the tolerant fallback for models without native
// tool-calling: it returns every JSON tool call embedded in a model response,
// in order of appearance. It scans for balanced {...} objects and keeps each one
// that decodes to a call naming a tool, so it is robust to the formatting
// variations small/local models produce — surrounding prose, Markdown code
// fences, pretty-printing, key reordering, whitespace around colons, and several
// calls in one reply. Returns nil when no tool call is present.
func ParseToolCalls(response string) []ToolCall {
	var calls []ToolCall
	for _, obj := range ExtractJSONObjects(response) {
		var tc ToolCall
		if err := json.Unmarshal([]byte(obj), &tc); err == nil && tc.Tool != "" {
			calls = append(calls, tc)
		}
	}
	return calls
}

// ExtractJSONObjects scans text for balanced, top-level {...} JSON objects and
// returns their source substrings in order of appearance. It is the single
// tolerant extractor shared by every JSON-text tool-call fallback (issue #32),
// replacing the brittle substring matching that only recognised one exact
// `{"tool":` shape.
//
// Braces inside JSON string literals (including escaped quotes) are ignored, so
// a value like {"content":"a } b"} is extracted whole. Non-JSON characters
// between objects — prose, Markdown ```json fences, list markers — are skipped,
// which is why fenced and prose-wrapped calls are handled without a separate
// fence-stripping pass. Nested objects are returned as part of their enclosing
// top-level object, not separately.
func ExtractJSONObjects(text string) []string {
	var objs []string
	depth := 0
	start := -1
	inString := false
	escaped := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start != -1 {
					objs = append(objs, text[start:i+1])
					start = -1
				}
			}
		}
	}
	return objs
}

// UnmarshalJSON is a helper to unmarshal JSON
func UnmarshalJSON(data []byte, v interface{}) error {
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("unmarshal json: %w", err)
	}
	return nil
}
