package shell

import (
	"bytes"
	"context"
	"fmt"
	"time"
)

// ShellConfig represents shell execution configuration
type ShellConfig struct {
	Timeout   time.Duration
	MaxOutput int
	// Dir is the working directory for the command. Empty uses the process cwd.
	Dir string
}

// DefaultTimeout is the default timeout for shell commands
const DefaultTimeout = 5 * time.Minute

// DefaultMaxOutput is the default max output size
const DefaultMaxOutput = 1024 * 1024 // 1MB

// Execute executes a shell command and returns the result
func Execute(command string, config ShellConfig) (*ExecuteResult, error) {
	if config.Timeout == 0 {
		config.Timeout = DefaultTimeout
	}
	if config.MaxOutput == 0 {
		config.MaxOutput = DefaultMaxOutput
	}

	ctx, cancel := context.WithTimeout(context.Background(), config.Timeout)
	defer cancel()

	// newShellCommand wires up the platform shell invocation plus the
	// cancellation strategy (process-group kill on Unix, default process kill
	// on Windows) so a timeout tears down the command and its children.
	cmd := newShellCommand(ctx, command)
	if config.Dir != "" {
		cmd.Dir = config.Dir
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Run blocks until the process has exited and os/exec has finished copying
	// output, so the buffers are only read after writes are complete (no race).
	err := cmd.Run()

	if ctx.Err() == context.DeadlineExceeded {
		return &ExecuteResult{
			Stdout:  stdout.String(),
			Stderr:  stderr.String(),
			Timeout: true,
			Error:   fmt.Sprintf("command timed out after %v", config.Timeout),
		}, nil
	}
	if err != nil {
		return &ExecuteResult{
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
			ExitCode: 1,
			Error:    err.Error(),
		}, nil
	}
	return &ExecuteResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: 0,
		Timeout:  false,
	}, nil
}

// ExecuteResult represents the result of a shell command execution
type ExecuteResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code,omitempty"`
	Timeout  bool   `json:"timeout,omitempty"`
	Error    string `json:"error,omitempty"`
}

// GetOutput returns combined stdout and stderr
func (r *ExecuteResult) GetOutput() string {
	if r.Stderr == "" {
		return r.Stdout
	}
	if r.Stdout == "" {
		return "stderr:\n" + r.Stderr
	}
	return r.Stdout + "\n\nstderr:\n" + r.Stderr
}
