package command

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gogent/internal/mathexpr"
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

			// Evaluate and format with the shared evaluator so /calc and the calc
			// tool agree on output (clean integers, full-precision fractionals).
			result, err := mathexpr.EvalFormatted(expr)
			if err != nil {
				return &CommandResult{
					Success:  false,
					Stderr:   err.Error(),
					ExitCode: 1,
				}, fmt.Errorf("evaluate expression: %w", err)
			}

			return &CommandResult{
				Success:  true,
				Stdout:   fmt.Sprintf("Result: %s", result),
				ExitCode: 0,
			}, nil
		},
	})

	// Echo command for testing
	r.Register(&InternalCommand{
		Name:        "echo",
		Description: "Echo arguments",
		Handler: func(ctx context.Context, args []string) (*CommandResult, error) {
			if len(args) == 0 {
				return &CommandResult{
					Success:  false,
					Stderr:   "missing arguments",
					ExitCode: 1,
				}, errors.New("missing arguments")
			}
			return &CommandResult{
				Success:  true,
				Stdout:   " " + strings.Join(args, " "),
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
