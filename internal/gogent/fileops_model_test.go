package gogent

import (
	"os"
	"path/filepath"
	"testing"

	"gogent/internal/agent"
	"gogent/internal/config"
	"gogent/internal/fileops"
	"gogent/internal/model"
	"gogent/internal/permission"
)

// TestFileOpsWithModel tests file operations through the model session
func TestFileOpsWithModel(t *testing.T) {
	// Create a temporary workspace
	tempDir, err := os.MkdirTemp("", "gogent-model-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create workspace directory (must match NewGogent behavior)
	workspace := filepath.Join(tempDir, ".gogent", "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatalf("Failed to create workspace: %v", err)
	}

	// Create test file with known content
	testFile := filepath.Join(workspace, "model_test.txt")
	testContent := "The quick brown fox jumps over the lazy dog."
	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Create Gogent instance rooted at the test workspace
	g := NewGogentWithWorkspace(tempDir, workspace)

	// Add default allow rule for all operations
	g.GetPermissionService().AddRule(permission.Rule{
		Action:   "*",
		Resource: "*",
		Effect:   "allow",
	})

	// Create model session pointed at the configured (LAN/env) endpoint
	m := testConn()
	m.SetURL(config.DefaultEndpoint())
	sess := model.NewModelSession("test", m)

	// Create root agent
	rootAgent := agent.NewAgent("root", sess)
	rootAgent.SetState(agent.StateIdle)

	// Create user session
	_ = g.CreateUserSession("default", rootAgent)

	// Test: Model can read and respond to file operations
	t.Run("ModelReadFile", func(t *testing.T) {
		requireModel(t)

		// Read the test file
		fs := g.GetFileSystem()
		content, err := fs.Read("model_test.txt", fileops.Authorization{})
		if err != nil {
			t.Fatalf("Failed to read test file: %v", err)
		}

		if string(content) != testContent {
			t.Errorf("Expected '%s', got '%s'", testContent, string(content))
		}

		// Send a message to the model that references the file content
		messages := []model.Message{
			{Role: model.RoleUser, Content: "I have a file with content: " + string(content)},
		}

		resp, err := m.Complete(messages)
		if err != nil {
			t.Fatalf("Model failed to respond: %v", err)
		}

		if resp == nil || resp.Content == "" {
			t.Error("Expected model response")
		}
	})

	// Test: Model can write to file via tool
	t.Run("ModelWriteFile", func(t *testing.T) {
		writeTool := fileops.NewWriteTool(
			g.GetFileMutation(),
			g.GetLocationMutation(),
			g.GetPermissionService(),
		)

		args := map[string]interface{}{
			"path":    "model_response.txt",
			"content": "Model response: " + testContent,
		}

		result, err := writeTool.Execute(args)
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		// Verify file was written
		writtenFile := filepath.Join(workspace, "model_response.txt")
		writtenContent, err := os.ReadFile(writtenFile)
		if err != nil {
			t.Fatalf("Failed to read written file: %v", err)
		}

		expectedContent := "Model response: " + testContent
		if string(writtenContent) != expectedContent {
			t.Errorf("Expected '%s', got '%s'", expectedContent, string(writtenContent))
		}

		// Verify write result structure
		if resultMap, ok := result.(map[string]interface{}); ok {
			if resultMap["operation"] != "write" {
				t.Errorf("Expected operation 'write', got '%v'", resultMap["operation"])
			}
			if resultMap["existed"].(bool) != false {
				t.Error("Expected new file (existed=false)")
			}
		}
	})

	// Test: Model can edit file via tool
	t.Run("ModelEditFile", func(t *testing.T) {
		editTool := fileops.NewEditTool(
			g.GetFileMutation(),
			g.GetLocationMutation(),
			g.GetPermissionService(),
		)

		args := map[string]interface{}{
			"path":    "model_response.txt",
			"find":    "Model response",
			"replace": "AI Response",
		}

		result, err := editTool.Execute(args)
		if err != nil {
			t.Fatalf("Edit failed: %v", err)
		}

		// Verify edit result
		if resultMap, ok := result.(map[string]interface{}); ok {
			if resultMap["operation"] != "write" {
				t.Errorf("Expected operation 'write', got '%v'", resultMap["operation"])
			}
		}

		// Verify file content was updated
		editedContent, err := os.ReadFile(filepath.Join(workspace, "model_response.txt"))
		if err != nil {
			t.Fatalf("Failed to read edited file: %v", err)
		}

		expectedContent := "AI Response: " + testContent
		if string(editedContent) != expectedContent {
			t.Errorf("Expected '%s', got '%s'", expectedContent, string(editedContent))
		}
	})
}
