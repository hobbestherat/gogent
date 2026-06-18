package shell

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
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

	cmd := exec.Command("sh", "-c", command)
	if config.Dir != "" {
		cmd.Dir = config.Dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	done := make(chan error, 1)
	go func() {
		done <- cmd.Run()
	}()

	select {
	case err := <-done:
		if err != nil {
			// Command failed or timed out
			errMsg := err.Error()
			if strings.Contains(errMsg, "deadline exceeded") || strings.Contains(errMsg, "timeout") {
				return &ExecuteResult{
					Stdout:  stdout.String(),
					Stderr:  stderr.String(),
					Timeout: true,
				}, nil
			}
			return &ExecuteResult{
				Stdout:   stdout.String(),
				Stderr:   stderr.String(),
				ExitCode: 1,
				Error:    errMsg,
			}, nil
		}
		return &ExecuteResult{
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
			ExitCode: 0,
			Timeout:  false,
		}, nil
	case <-time.After(config.Timeout):
		cmd.Process.Kill()
		return &ExecuteResult{
			Stdout:  stdout.String(),
			Stderr:  stderr.String(),
			Timeout: true,
			Error:   fmt.Sprintf("command timed out after %v", config.Timeout),
		}, nil
	}
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
