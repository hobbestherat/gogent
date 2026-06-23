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
		{"(1+2)*(3+4)", "21"},
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

// TestCalcCommandMalformed ensures malformed expressions surface as errors
// instead of panicking (the original bug for inputs like "", "()", and "5+").
// Note: under the expr-lang evaluator "+5" is valid unary plus (-> 5) and "1/0"
// is a non-finite result (-> +Inf), not errors, so they are no longer listed
// here. These are genuinely malformed inputs that must surface as errors.
func TestCalcCommandMalformed(t *testing.T) {
	exprs := []string{"", "()", "5+", "5*", "(1+2", "1)(2", "abc", "5x", "sqrt(", "*"}

	registry := NewCommandRegistry()
	registry.RegisterBuiltInCommands()
	ctx := context.Background()

	for _, expr := range exprs {
		result, err := registry.Execute(ctx, "calc", []string{expr})
		if err == nil && result != nil && result.Success {
			t.Errorf("calc(%q) unexpectedly succeeded: %s", expr, result.Stdout)
		}
	}
}

// TestCalcCommandMissingArgs ensures calc reports an error when no expression
// is supplied rather than indexing args[0].
func TestCalcCommandMissingArgs(t *testing.T) {
	registry := NewCommandRegistry()
	registry.RegisterBuiltInCommands()
	ctx := context.Background()

	result, err := registry.Execute(ctx, "calc", []string{})
	if err == nil {
		t.Error("calc with no args should return an error")
	}
	if result != nil && result.Success {
		t.Error("calc with no args should not report success")
	}
}

// TestEchoCommand ensures echo joins its arguments and does not panic on empty
// args (the original bug indexed args[0] unchecked).
func TestEchoCommand(t *testing.T) {
	registry := NewCommandRegistry()
	registry.RegisterBuiltInCommands()
	ctx := context.Background()

	result, err := registry.Execute(ctx, "echo", []string{"hello"})
	if err != nil {
		t.Fatalf("echo returned unexpected error: %v", err)
	}
	if !strings.Contains(result.Stdout, "hello") {
		t.Errorf("echo should contain its argument, got: %q", result.Stdout)
	}

	// No arguments must error rather than panic.
	if _, err := registry.Execute(ctx, "echo", []string{}); err == nil {
		t.Error("echo with no args should return an error")
	}
}
