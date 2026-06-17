package tool

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gogent/internal/fileops"
	"gogent/internal/shell"
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
	PermissionService *fileops.PermissionService
	ToolCallback      func(toolName string, args map[string]interface{}) error
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
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools: make(map[string]*Tool),
	}
}

func (tr *ToolRegistry) Register(tool *Tool) {
	tr.tools[tool.Name] = tool
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

// CloneWithout returns a shallow copy of the registry with the named tools
// removed. It is used to hand sub-agents a registry that omits the sub-agent
// spawning tools when recursive sub-agents are disabled.
func (tr *ToolRegistry) CloneWithout(names ...string) *ToolRegistry {
	excluded := make(map[string]bool, len(names))
	for _, n := range names {
		excluded[n] = true
	}
	clone := NewToolRegistry()
	clone.ShellTimeout = tr.ShellTimeout
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

func (tr *ToolRegistry) ExecuteToolCall(toolCall *ToolCall, ctx ToolContext) (*ToolCallResponse, error) {
	tool := tr.tools[toolCall.Tool]
	if tool == nil {
		return &ToolCallResponse{
			Success: false,
			Error:   fmt.Sprintf("unknown tool: %s", toolCall.Tool),
		}, nil
	}

	if err := validateArgs(toolCall.Args, tool.InputSchema); err != nil {
		return &ToolCallResponse{
			Success: false,
			Error:   fmt.Sprintf("invalid args: %v", err),
		}, nil
	}

	// Track tool call
	if ctx.ToolCallback != nil {
		ctx.ToolCallback(toolCall.Tool, toolCall.Args)
	}

	result, err := tool.Execute(toolCall.Args, ctx)
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

			// Clean expression
			expr := expression
			expr = strings.ReplaceAll(expr, " ", "")

			// Evaluate using recursive descent parser
			result, err := evalMath(expr)
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

// evalMath evaluates a mathematical expression
func evalMath(expr string) (float64, error) {
	return evalExpr(expr)
}

// evalExpr handles + and -
func evalExpr(expr string) (float64, error) {
	// Find the last + or - that's not in parentheses
	level := 0
	for i := len(expr) - 1; i >= 0; i-- {
		c := expr[i]
		if c == ')' {
			level++
		} else if c == '(' {
			level--
		} else if level == 0 && (c == '+' || c == '-') {
			left, err := evalExpr(expr[:i])
			if err != nil {
				return 0, err
			}
			right, err := evalExpr(expr[i+1:])
			if err != nil {
				return 0, err
			}
			if c == '+' {
				return left + right, nil
			}
			return left - right, nil
		}
	}

	// Handle parentheses
	if len(expr) > 0 && expr[0] == '(' && expr[len(expr)-1] == ')' {
		return evalExpr(expr[1 : len(expr)-1])
	}

	// Handle * and /
	level = 0
	for i := len(expr) - 1; i >= 0; i-- {
		c := expr[i]
		if c == ')' {
			level++
		} else if c == '(' {
			level--
		} else if level == 0 && (c == '*' || c == '/') {
			left, err := evalExpr(expr[:i])
			if err != nil {
				return 0, err
			}
			right, err := evalExpr(expr[i+1:])
			if err != nil {
				return 0, err
			}
			if c == '*' {
				return left * right, nil
			}
			return left / right, nil
		}
	}

	// Parse number
	if len(expr) > 0 {
		// Try to parse as float
		var result float64
		_, err := fmt.Sscanf(expr, "%f", &result)
		if err == nil {
			return result, nil
		}
		// Try as int
		var intResult int
		_, err2 := fmt.Sscanf(expr, "%d", &intResult)
		if err2 == nil {
			return float64(intResult), nil
		}
		return 0, fmt.Errorf("invalid number: %s", expr)
	}
	return 0, fmt.Errorf("empty expression")
}

func validateArgs(args map[string]interface{}, schema interface{}) error {
	if args == nil {
		return fmt.Errorf("args cannot be nil")
	}
	return nil
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

			// Honor the configured shell timeout, defaulting to 5 minutes.
			timeout := tr.ShellTimeout
			if timeout <= 0 {
				timeout = 5 * time.Minute
			}

			// Execute the shell command
			result, err := shell.Execute(command, shell.ShellConfig{
				Timeout:   timeout,
				MaxOutput: 1024 * 1024,
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
