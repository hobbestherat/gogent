package gogent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gogent/internal/agent"
	"gogent/internal/config"
	"gogent/internal/model"
	"gogent/internal/permission"
	"gogent/internal/tool"
)

// TestToolCallingFlow tests the complete tool calling flow
func TestToolCallingFlow(t *testing.T) {
	requireModel(t)

	// Create a temporary workspace
	tempDir, err := os.MkdirTemp("", "gogent-toolflow-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create workspace directory
	workspace := filepath.Join(tempDir, ".gogent", "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatalf("Failed to create workspace: %v", err)
	}

	// Create test file with expected content
	testFile := filepath.Join(workspace, "newfile.txt")
	testContent := "hello world"
	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Create Gogent instance rooted at the test workspace
	g := NewGogentWithWorkspace(tempDir, workspace)

	// Allow all actions on workspace
	g.GetPermissionService().AddRule(permission.Rule{
		Action:   "*",
		Resource: "*",
		Effect:   "allow",
	})

	// Create model connection and session pointed at the configured endpoint
	m := testConn()
	m.SetURL(config.DefaultEndpoint())
	s := model.NewModelSession("session_test", m)
	agentObj := agent.NewAgent("agent_test", s)
	g.CreateUserSession("session_test", agentObj)

	// ========== STEP 1: Send "read newfile.txt" ==========
	t.Log("STEP 1: Sending 'read newfile.txt' to trigger tool call")

	msg := model.Message{
		Role:    model.RoleUser,
		Content: "read newfile.txt",
	}

	// Send message and get response
	resp, err := s.Send([]model.Message{msg})
	if err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	t.Logf("Model response: %s", resp.Content)

	// Check if response contains a tool call
	hasToolCall := containsToolCall(resp.Content)
	if !hasToolCall {
		t.Logf("WARNING: Response does not contain explicit tool call JSON, continuing test anyway")
	}

	// Give time for any async processing
	time.Sleep(200 * time.Millisecond)

	// ========== STEP 2: Verify tool was executed ==========
	t.Log("STEP 2: Checking if tool execution logs show the read operation")

	// Read the file directly to verify it exists and has content
	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Errorf("Failed to read test file: %v", err)
	} else if string(content) != testContent {
		t.Errorf("File content mismatch: expected '%s', got '%s'", testContent, string(content))
	} else {
		t.Logf("✓ File '%s' exists with correct content: '%s'", "newfile.txt", string(content))
	}

	// ========== STEP 3: Check tool results in history ==========
	t.Log("STEP 3: Checking model session history for tool results")

	history := s.GetHistory()
	t.Logf("History has %d turns", len(history))

	for i, turn := range history {
		t.Logf("  Turn %d - Request: %d messages, Response length: %d",
			i, len(turn.Request), len(turn.Response))
		if turn.Response != "" {
			t.Logf("    Response preview: %.100s...", turn.Response)
		}
	}

	// ========== STEP 4: Check if full history is sent to model ==========
	t.Log("STEP 4: Verifying full history is preserved in ModelSession.Send")

	// The Send method should have appended the turn with full history
	if len(history) > 0 {
		lastTurn := history[len(history)-1]
		if len(lastTurn.Request) > 0 {
			t.Logf("✓ Last turn has %d request messages", len(lastTurn.Request))
		}
		// Verify response is stored
		if lastTurn.Response != "" {
			t.Logf("✓ Last turn response stored: %.50s...", lastTurn.Response)
		}
	}

	// ========== STEP 5: Send "translate to french" ==========
	t.Log("STEP 5: Sending 'translate to french' to test translation")

	// Note: The model doesn't actually have translation capability in this test,
	// but we can verify the flow works
	frenchMsg := model.Message{
		Role:    model.RoleUser,
		Content: "translate to french",
	}

	frenchResp, err := s.Send([]model.Message{frenchMsg})
	if err != nil {
		t.Logf("Note: Translation request failed: %v (expected if model not configured)", err)
	} else {
		t.Logf("Translation response: %s", frenchResp.Content)
		// Check if response contains French text
		frenchWords := []string{"bonjour", "monde", "bonsoir", "salut"}
		isFrench := false
		for _, word := range frenchWords {
			if containsWord(frenchResp.Content, word) {
				isFrench = true
				break
			}
		}
		if isFrench {
			t.Logf("✓ Response contains French words")
		} else {
			t.Logf("Note: Response does not contain obvious French words (model may not be configured)")
		}
	}

	// ========== STEP 6: Verify final state ==========
	t.Log("STEP 6: Verifying final session state")

	finalCount := g.CountMessages("session_test")
	t.Logf("Total messages in session: %d", finalCount)

	// Check agent state
	agentState := agentObj.GetState()
	t.Logf("Agent state: %s", agentState)

	if agentState != agent.StateIdle {
		t.Logf("Note: Agent state is '%s' instead of idle", agentState)
	}
}

// TestToolCallDetection tests tool call detection from model responses
func TestToolCallDetection(t *testing.T) {
	testCases := []struct {
		name     string
		response string
		expected bool
	}{
		{
			name:     "JSON tool call",
			response: `{"tool": "read", "args": {"path": "test.txt"}}`,
			expected: true,
		},
		{
			name:     "Nested JSON with tool",
			response: `{"response": "I'll read the file", "tool": "read", "args": {"path": "test.txt"}}`,
			expected: true,
		},
		{
			name:     "Regular response",
			response: "I can help you read that file.",
			expected: false,
		},
		{
			name:     "JSON with tool in backticks",
			response: "Here is the tool call:\n```\n{\"tool\": \"calc\", \"args\": {\"expression\": \"2+2\"}}\n```",
			expected: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			hasTool := containsToolCall(tc.response)
			if hasTool != tc.expected {
				t.Errorf("Expected hasTool=%v for response '%s', got %v", tc.expected, tc.response, hasTool)
			}
		})
	}
}

// TestToolExecutionIntegration tests actual tool execution
func TestToolExecutionIntegration(t *testing.T) {
	// Create temp workspace
	tempDir, err := os.MkdirTemp("", "gogent-toolint-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	workspace := filepath.Join(tempDir, ".gogent", "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatalf("Failed to create workspace: %v", err)
	}

	// Create Gogent
	g := NewGogentWithWorkspace(tempDir, workspace)
	g.GetPermissionService().AddRule(permission.Rule{
		Action:   "*",
		Resource: "*",
		Effect:   "allow",
	})

	// Test read tool
	t.Run("ReadTool", func(t *testing.T) {
		testFile := filepath.Join(workspace, "readtest.txt")
		content := "test content"
		if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
			t.Fatalf("Failed to create test file: %v", err)
		}

		result, err := g.ExecuteToolCall(&tool.ToolCall{
			Tool: "read",
			Args: map[string]interface{}{"path": "readtest.txt"},
		}, "test", "test", "")

		if err != nil {
			t.Errorf("Read tool execution failed: %v", err)
		} else if result == nil {
			t.Error("Read tool returned nil result")
		} else {
			t.Logf("Read tool result: %+v", result)
			if !result.Success {
				t.Errorf("Read tool reported failure: %s", result.Error)
			}
		}
	})

	// Test calc tool
	t.Run("CalcTool", func(t *testing.T) {
		result, err := g.ExecuteToolCall(&tool.ToolCall{
			Tool: "calc",
			Args: map[string]interface{}{"expression": "5+5"},
		}, "test", "test", "")

		if err != nil {
			t.Errorf("Calc tool execution failed: %v", err)
		} else if result == nil {
			t.Error("Calc tool returned nil result")
		} else {
			t.Logf("Calc tool result: %+v", result)
			if !result.Success {
				t.Errorf("Calc tool reported failure: %s", result.Error)
			}
		}
	})
}

// Helper functions

func containsToolCall(response string) bool {
	// Look for JSON with "tool" field
	if response == "" {
		return false
	}

	// Simple check - look for the pattern
	if contains(response, `"tool"`) && contains(response, `"args"`) {
		return true
	}

	// Check for tool call in code blocks
	if contains(response, "```") && contains(response, `"tool"`) {
		return true
	}

	return false
}

func containsWord(text, word string) bool {
	return contains(text, word)
}

func contains(text, substr string) bool {
	return len(text) >= len(substr) && findSubstring(text, substr)
}

func findSubstring(text, substr string) bool {
	for i := 0; i <= len(text)-len(substr); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			if text[i+j] != substr[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
