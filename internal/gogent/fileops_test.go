package gogent

import (
	"os"
	"path/filepath"
	"testing"

	"gogent/internal/fileops"
)

// TestFileOperations performs real model tests for file operations
func TestFileOperations(t *testing.T) {
	// Create a temporary workspace
	tempDir, err := os.MkdirTemp("", "gogent-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create workspace directory (must match NewGogent behavior)
	workspace := filepath.Join(tempDir, ".gogent", "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatalf("Failed to create workspace: %v", err)
	}

	// Create Gogent instance rooted at the test workspace
	g := NewGogentWithWorkspace(tempDir, workspace)

	// Add default allow rule for all operations in test
	g.GetPermissionService().AddRule(fileops.PermissionRule{
		Action:   "*",
		Resource: "*",
		Effect:   "allow",
	})

	// Create a test file
	testFile := filepath.Join(workspace, "test.txt")
	testContent := "Hello, World!"
	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Test 1: Read file
	t.Run("ReadFile", func(t *testing.T) {
		readTool := fileops.NewReadTool(g.GetFileSystem(), g.GetLocationMutation(), g.GetPermissionService())

		args := map[string]interface{}{
			"path": "test.txt",
		}

		result, err := readTool.Execute(args)
		if err != nil {
			t.Fatalf("Read failed: %v", err)
		}

		if result != testContent {
			t.Errorf("Expected '%s', got '%s'", testContent, result)
		}
	})

	// Test 2: Write file
	t.Run("WriteFile", func(t *testing.T) {
		writeTool := fileops.NewWriteTool(g.GetFileMutation(), g.GetLocationMutation(), g.GetPermissionService())

		newContent := "New content"
		args := map[string]interface{}{
			"path":    "write_test.txt",
			"content": newContent,
		}

		result, err := writeTool.Execute(args)
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		if resultMap, ok := result.(map[string]interface{}); ok {
			if resultMap["operation"] != "write" {
				t.Errorf("Expected operation 'write', got '%v'", resultMap["operation"])
			}
		} else {
			t.Errorf("Expected map result, got %T", result)
		}

		// Verify file was written
		writtenContent, err := os.ReadFile(filepath.Join(workspace, "write_test.txt"))
		if err != nil {
			t.Fatalf("Failed to read written file: %v", err)
		}

		if string(writtenContent) != newContent {
			t.Errorf("Expected '%s', got '%s'", newContent, string(writtenContent))
		}
	})

	// Test 3: Edit file
	t.Run("EditFile", func(t *testing.T) {
		// Create a file to edit
		editFile := filepath.Join(workspace, "edit_test.txt")
		originalContent := "Hello, World!"
		if err := os.WriteFile(editFile, []byte(originalContent), 0644); err != nil {
			t.Fatalf("Failed to create edit test file: %v", err)
		}

		editTool := fileops.NewEditTool(g.GetFileMutation(), g.GetLocationMutation(), g.GetPermissionService())

		args := map[string]interface{}{
			"path":    "edit_test.txt",
			"find":    "World",
			"replace": "Universe",
		}

		result, err := editTool.Execute(args)
		if err != nil {
			t.Fatalf("Edit failed: %v", err)
		}

		if resultMap, ok := result.(map[string]interface{}); ok {
			if resultMap["operation"] != "write" {
				t.Errorf("Expected operation 'write', got '%v'", resultMap["operation"])
			}
		} else {
			t.Errorf("Expected map result, got %T", result)
		}

		// Verify file was edited
		editedContent, err := os.ReadFile(editFile)
		if err != nil {
			t.Fatalf("Failed to read edited file: %v", err)
		}

		expectedContent := "Hello, Universe!"
		if string(editedContent) != expectedContent {
			t.Errorf("Expected '%s', got '%s'", expectedContent, string(editedContent))
		}
	})

	// Test 4: List directory
	t.Run("ListDirectory", func(t *testing.T) {
		fs := g.GetFileSystem()

		entries, err := fs.List(".")
		if err != nil {
			t.Fatalf("List failed: %v", err)
		}

		found := false
		for _, entry := range entries {
			if entry.Name == "test.txt" {
				found = true
				break
			}
		}

		if !found {
			t.Error("Expected to find test.txt in directory listing")
		}
	})

	// Test 5: Glob
	t.Run("Glob", func(t *testing.T) {
		fs := g.GetFileSystem()

		matches, err := fs.Glob("*.txt")
		if err != nil {
			t.Fatalf("Glob failed: %v", err)
		}

		if len(matches) < 1 {
			t.Errorf("Expected at least 1 match, got %d", len(matches))
		}
	})
}
