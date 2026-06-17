package command

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestShellCommandExecute(t *testing.T) {
	cmd := NewShellCommand()
	ctx := context.Background()

	result := cmd.Execute(ctx, "echo", "hello")
	if !result.Success {
		t.Errorf("Expected success, got error: %v", result.Error)
	}

	if !strings.Contains(result.Stdout, "hello") {
		t.Errorf("Expected 'hello' in output, got: %s", result.Stdout)
	}
}

func TestShellCommandExecuteWithTimeout(t *testing.T) {
	cmd := NewShellCommand()
	cmd.SetTimeout(100 * time.Millisecond)
	ctx := context.Background()

	result := cmd.ExecuteWithTimeout(ctx, "echo", "hello")
	if !result.Success {
		t.Errorf("Expected success, got error: %v", result.Error)
	}

	if !strings.Contains(result.Stdout, "hello") {
		t.Errorf("Expected 'hello' in output, got: %s", result.Stdout)
	}
}

func TestCommandRegistryRegisterAndExecute(t *testing.T) {
	registry := NewCommandRegistry()
	registry.RegisterBuiltInCommands()

	ctx := context.Background()

	// Test calc command
	result, err := registry.Execute(ctx, "calc", []string{"2+2"})
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if !result.Success {
		t.Errorf("Expected success, got: %s", result.Stderr)
	}

	if !strings.Contains(result.Stdout, "4") {
		t.Errorf("Expected '4' in output, got: %s", result.Stdout)
	}
}

func TestCommandRegistryListCommands(t *testing.T) {
	registry := NewCommandRegistry()
	registry.RegisterBuiltInCommands()

	commands := registry.ListCommands()
	if len(commands) == 0 {
		t.Error("Expected at least one command")
	}
}

func TestCommandRegistryNotFound(t *testing.T) {
	registry := NewCommandRegistry()

	ctx := context.Background()
	_, err := registry.Execute(ctx, "nonexistent", []string{})
	if err == nil {
		t.Error("Expected error for non-existent command")
	}
}

func TestCalcCommand(t *testing.T) {
	tests := []struct {
		expr     string
		expected string
	}{
		{"2+2", "4"},
		{"10-5", "5"},
		{"3*4", "12"},
		{"10/2", "5"},
		{"(2+3)*4", "20"},
	}

	registry := NewCommandRegistry()
	registry.RegisterBuiltInCommands()
	ctx := context.Background()

	for _, test := range tests {
		result, err := registry.Execute(ctx, "calc", []string{test.expr})
		if err != nil {
			t.Errorf("Error executing %s: %v", test.expr, err)
			continue
		}

		if !result.Success {
			t.Errorf("Failed to execute %s: %s", test.expr, result.Stderr)
			continue
		}

		if !strings.Contains(result.Stdout, test.expected) {
			t.Errorf("Expected %s in output for %s, got: %s", test.expected, test.expr, result.Stdout)
		}
	}
}
