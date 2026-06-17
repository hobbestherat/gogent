package command

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ToolCall represents a tool call from the model
type ToolCall struct {
	Name      string
	Arguments map[string]interface{}
}

// InternalCommand represents an internal system command
type InternalCommand struct {
	Name        string
	Description string
	Handler     func(ctx context.Context, args []string) (*CommandResult, error)
}

// CommandRegistry manages internal commands
type CommandRegistry struct {
	commands map[string]*InternalCommand
}

// NewCommandRegistry creates a new command registry
func NewCommandRegistry() *CommandRegistry {
	return &CommandRegistry{
		commands: make(map[string]*InternalCommand),
	}
}

// Register registers a new command
func (r *CommandRegistry) Register(cmd *InternalCommand) {
	r.commands[cmd.Name] = cmd
}

// GetCommand gets a command by name
func (r *CommandRegistry) GetCommand(name string) *InternalCommand {
	return r.commands[name]
}

// ListCommands lists all registered commands
func (r *CommandRegistry) ListCommands() []*InternalCommand {
	commands := make([]*InternalCommand, 0, len(r.commands))
	for _, cmd := range r.commands {
		commands = append(commands, cmd)
	}
	return commands
}

// Execute executes a command by name
func (r *CommandRegistry) Execute(ctx context.Context, name string, args []string) (*CommandResult, error) {
	cmd, ok := r.commands[name]
	if !ok {
		return nil, fmt.Errorf("command not found: %s", name)
	}

	result, err := cmd.Handler(ctx, args)
	if err != nil {
		return &CommandResult{
			Success:    false,
			Stderr:     err.Error(),
			ExitCode:   1,
			DurationMs: 0,
		}, err
	}

	return result, nil
}

// RegisterBuiltInCommands registers built-in system commands
func (r *CommandRegistry) RegisterBuiltInCommands() {
	// Math command for calculations
	r.Register(&InternalCommand{
		Name:        "calc",
		Description: "Calculate mathematical expressions",
		Handler: func(ctx context.Context, args []string) (*CommandResult, error) {
			if len(args) == 0 {
				return &CommandResult{
					Success:  false,
					Stderr:   "missing expression",
					ExitCode: 1,
				}, errors.New("missing expression")
			}

			expr := args[0]

			// Parse and evaluate simple math
			result, err := evalMath(expr)
			if err != nil {
				return &CommandResult{
					Success:  false,
					Stderr:   err.Error(),
					ExitCode: 1,
				}, err
			}

			return &CommandResult{
				Success:  true,
				Stdout:   fmt.Sprintf("Result: %v", result),
				ExitCode: 0,
			}, nil
		},
	})

	// Echo command for testing
	r.Register(&InternalCommand{
		Name:        "echo",
		Description: "Echo arguments",
		Handler: func(ctx context.Context, args []string) (*CommandResult, error) {
			return &CommandResult{
				Success:  true,
				Stdout:   " " + args[0],
				ExitCode: 0,
			}, nil
		},
	})

	// Help command
	r.Register(&InternalCommand{
		Name:        "help",
		Description: "List available commands",
		Handler: func(ctx context.Context, args []string) (*CommandResult, error) {
			commands := r.ListCommands()
			helpText := "Available commands:\n"
			for _, cmd := range commands {
				helpText += fmt.Sprintf("  %s - %s\n", cmd.Name, cmd.Description)
			}
			return &CommandResult{
				Success:  true,
				Stdout:   helpText,
				ExitCode: 0,
			}, nil
		},
	})
}

// evalMath evaluates a simple mathematical expression using Go's eval
func evalMath(expr string) (float64, error) {
	// Clean expression
	expr = replaceAll(expr, " ", "")

	// For simple cases, use Go's eval
	// Handle basic cases: numbers, +, -, *, /, ()

	// Simple recursive descent parser
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
	if expr[0] == '(' && expr[len(expr)-1] == ')' {
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
			if right == 0 {
				return 0, errors.New("division by zero")
			}
			return left / right, nil
		}
	}

	// It's a number
	return strconv.ParseFloat(expr, 64)
}

// replaceAll replaces all occurrences of old with new
func replaceAll(s, old, new string) string {
	result := s
	for {
		pos := strings.Index(result, old)
		if pos == -1 {
			break
		}
		result = result[:pos] + new + result[pos+len(old):]
	}
	return result
}
