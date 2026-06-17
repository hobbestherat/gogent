package command

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// CommandResult represents the result of a shell command
type CommandResult struct {
	Success    bool
	Stdout     string
	Stderr     string
	ExitCode   int
	Error      error
	DurationMs int64
}

// ShellCommand executes shell commands
type ShellCommand struct {
	Timeout time.Duration
}

// NewShellCommand creates a new shell command executor
func NewShellCommand() *ShellCommand {
	return &ShellCommand{
		Timeout: 30 * time.Second,
	}
}

// SetTimeout sets the command timeout
func (s *ShellCommand) SetTimeout(timeout time.Duration) {
	s.Timeout = timeout
}

// Execute runs a shell command
func (s *ShellCommand) Execute(ctx context.Context, cmd string, args ...string) *CommandResult {
	result := &CommandResult{
		Stdout:     "",
		Stderr:     "",
		ExitCode:   0,
		Error:      nil,
		DurationMs: 0,
	}

	start := time.Now()

	command := exec.CommandContext(ctx, cmd, args...)
	output, err := command.CombinedOutput()

	duration := time.Since(start).Milliseconds()

	if err != nil {
		result.Error = err
		result.ExitCode = -1
		result.Stdout = string(output)
		result.Success = false
	} else {
		result.Stdout = string(output)
		result.ExitCode = 0
		result.Success = true
	}

	result.DurationMs = duration
	return result
}

// ExecuteWithTimeout runs a shell command with timeout
func (s *ShellCommand) ExecuteWithTimeout(ctx context.Context, cmd string, args ...string) *CommandResult {
	// Create context with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()

	return s.Execute(timeoutCtx, cmd, args...)
}

// ExecuteShell executes a shell command as a string
func (s *ShellCommand) ExecuteShell(ctx context.Context, commandStr string) *CommandResult {
	// Split command into parts
	parts := strings.Fields(commandStr)
	if len(parts) == 0 {
		return &CommandResult{
			Success:    false,
			Stderr:     "empty command",
			ExitCode:   1,
			DurationMs: 0,
		}
	}

	cmd := parts[0]
	args := parts[1:]

	return s.ExecuteWithTimeout(ctx, cmd, args...)
}

// ExecuteBash executes a bash command
func (s *ShellCommand) ExecuteBash(ctx context.Context, script string) *CommandResult {
	return s.ExecuteWithTimeout(ctx, "bash", "-c", script)
}
