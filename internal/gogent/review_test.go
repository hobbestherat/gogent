package gogent

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gogent/internal/permission"
	"gogent/internal/tool"
)

// fakeReviewer records the requests it receives and answers each with a
// preprogrammed decision, so the review gate can be driven without a UI.
type fakeReviewer struct {
	mu       sync.Mutex
	decision EditReviewDecision
	reqs     []EditReviewRequest
}

func (f *fakeReviewer) ReviewEdit(req EditReviewRequest) EditReviewDecision {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	return f.decision
}

func (f *fakeReviewer) calls() []EditReviewRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]EditReviewRequest, len(f.reqs))
	copy(out, f.reqs)
	return out
}

// newReviewGogent builds a Gogent rooted at an isolated temp workspace with all
// file operations permitted, returning the instance and the workspace path.
func newReviewGogent(t *testing.T) (*Gogent, string) {
	t.Helper()
	tempDir := t.TempDir()
	workspace := filepath.Join(tempDir, ".gogent", "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	g := NewGogentWithWorkspace(tempDir, workspace)
	g.GetPermissionService().AddRule(permission.Rule{Action: "*", Resource: "*", Effect: "allow"})
	return g, workspace
}

func writeCall(g *Gogent, session, path, content string) (*tool.ToolCallResponse, error) {
	return g.ExecuteToolCall(&tool.ToolCall{
		Tool: "write",
		Args: map[string]interface{}{"path": path, "content": content},
	}, session, "root", "")
}

func editCall(g *Gogent, session, path, find, replace string) (*tool.ToolCallResponse, error) {
	return g.ExecuteToolCall(&tool.ToolCall{
		Tool: "edit",
		Args: map[string]interface{}{"path": path, "find": find, "replace": replace},
	}, session, "root", "")
}

func multiEditCall(g *Gogent, session, path string, edits []interface{}) (*tool.ToolCallResponse, error) {
	return g.ExecuteToolCall(&tool.ToolCall{
		Tool: "multi_edit",
		Args: map[string]interface{}{"path": path, "edits": edits},
	}, session, "root", "")
}

func applyPatchCall(g *Gogent, session, patch string) (*tool.ToolCallResponse, error) {
	return g.ExecuteToolCall(&tool.ToolCall{
		Tool: "apply_patch",
		Args: map[string]interface{}{"patch": patch},
	}, session, "root", "")
}

// TestReviewDisabledByDefault confirms writes/edits apply immediately when the
// feature is off, even with a reviewer installed (no behavior change).
func TestReviewDisabledByDefault(t *testing.T) {
	g, workspace := newReviewGogent(t)
	rv := &fakeReviewer{decision: EditReject}
	g.SetReviewer(rv)

	if _, err := writeCall(g, "s1", "a.txt", "hello\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(workspace, "a.txt"))
	if err != nil || string(got) != "hello\n" {
		t.Fatalf("file = %q, err=%v; want %q", string(got), err, "hello\n")
	}
	if n := len(rv.calls()); n != 0 {
		t.Fatalf("reviewer consulted %d times while disabled, want 0", n)
	}
}

// TestReviewRejectBlocksWrite verifies a rejected review leaves the file
// untouched and surfaces a failing tool response.
func TestReviewRejectBlocksWrite(t *testing.T) {
	g, workspace := newReviewGogent(t)
	g.SetReviewer(&fakeReviewer{decision: EditReject})
	g.SetReviewEdits(true)

	resp, err := writeCall(g, "s1", "a.txt", "hello\n")
	if err == nil {
		t.Fatal("expected an error for a rejected write")
	}
	if resp != nil && resp.Success {
		t.Fatal("rejected write reported success")
	}
	if _, statErr := os.Stat(filepath.Join(workspace, "a.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("file should not exist after rejection, stat err = %v", statErr)
	}
}

func TestYoloBypassesEditReviewGate(t *testing.T) {
	g, workspace := newReviewGogent(t)
	rv := &fakeReviewer{decision: EditReject}
	g.SetReviewer(rv)
	g.SetReviewEdits(true)
	g.SetYoloMode("s1", true)

	if _, err := writeCall(g, "s1", "a.txt", "hello\n"); err != nil {
		t.Fatalf("yolo write should bypass review rejection, got %v", err)
	}
	if n := len(rv.calls()); n != 0 {
		t.Fatalf("yolo write consulted reviewer %d times, want 0", n)
	}
	got, err := os.ReadFile(filepath.Join(workspace, "a.txt"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != "hello\n" {
		t.Fatalf("file = %q, want %q", string(got), "hello\n")
	}
}

func TestYoloBypassesEditReviewGateForAllMutationTools(t *testing.T) {
	g, workspace := newReviewGogent(t)
	rv := &fakeReviewer{decision: EditReject}
	g.SetReviewer(rv)
	g.SetReviewEdits(true)
	g.SetYoloMode("s1", true)

	if err := os.WriteFile(filepath.Join(workspace, "edit.txt"), []byte("hello world\n"), 0644); err != nil {
		t.Fatalf("seed edit target: %v", err)
	}
	if _, err := editCall(g, "s1", "edit.txt", "world", "yolo"); err != nil {
		t.Fatalf("yolo edit should bypass review rejection, got %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "edit.txt")); err != nil || string(got) != "hello yolo\n" {
		t.Fatalf("edit target = %q, err=%v; want %q", string(got), err, "hello yolo\n")
	}

	if err := os.WriteFile(filepath.Join(workspace, "multi.txt"), []byte("one two three\n"), 0644); err != nil {
		t.Fatalf("seed multi_edit target: %v", err)
	}
	if _, err := multiEditCall(g, "s1", "multi.txt", []interface{}{
		map[string]interface{}{"find": "one", "replace": "1"},
		map[string]interface{}{"find": "three", "replace": "3"},
	}); err != nil {
		t.Fatalf("yolo multi_edit should bypass review rejection, got %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "multi.txt")); err != nil || string(got) != "1 two 3\n" {
		t.Fatalf("multi_edit target = %q, err=%v; want %q", string(got), err, "1 two 3\n")
	}

	if err := os.WriteFile(filepath.Join(workspace, "patch.txt"), []byte("old\n"), 0644); err != nil {
		t.Fatalf("seed patch target: %v", err)
	}
	patch := "*** Begin Patch\n" +
		"*** Update File: patch.txt\n@@\n-old\n+new\n" +
		"*** End Patch"
	if _, err := applyPatchCall(g, "s1", patch); err != nil {
		t.Fatalf("yolo apply_patch should bypass review rejection, got %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "patch.txt")); err != nil || string(got) != "new\n" {
		t.Fatalf("patch target = %q, err=%v; want %q", string(got), err, "new\n")
	}

	if n := len(rv.calls()); n != 0 {
		t.Fatalf("yolo mutation tools consulted reviewer %d times, want 0", n)
	}
}

// TestReviewApproveWrites verifies an approved review applies the write and that
// the reviewer was handed a unified diff of the change.
func TestReviewApproveWrites(t *testing.T) {
	g, workspace := newReviewGogent(t)
	rv := &fakeReviewer{decision: EditApprove}
	g.SetReviewer(rv)
	g.SetReviewEdits(true)

	if _, err := writeCall(g, "s1", "a.txt", "hello\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(workspace, "a.txt"))
	if string(got) != "hello\n" {
		t.Fatalf("file = %q, want %q", string(got), "hello\n")
	}
	calls := rv.calls()
	if len(calls) != 1 {
		t.Fatalf("reviewer consulted %d times, want 1", len(calls))
	}
	if calls[0].Op != "write" || calls[0].Path != "a.txt" {
		t.Fatalf("unexpected request: %+v", calls[0])
	}
	if !strings.Contains(calls[0].Diff, "+hello") {
		t.Fatalf("diff should show the added line, got:\n%s", calls[0].Diff)
	}
}

// TestReviewApproveAllSuppressesLaterPrompts verifies "approve all this session"
// is remembered: only the first edit prompts, later ones apply silently, and a
// different session is still gated independently.
func TestReviewApproveAllSuppressesLaterPrompts(t *testing.T) {
	g, workspace := newReviewGogent(t)
	rv := &fakeReviewer{decision: EditApproveAll}
	g.SetReviewer(rv)
	g.SetReviewEdits(true)

	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, err := writeCall(g, "s1", name, "x\n"); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if n := len(rv.calls()); n != 1 {
		t.Fatalf("session s1 consulted reviewer %d times, want 1 (approve-all)", n)
	}
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, err := os.Stat(filepath.Join(workspace, name)); err != nil {
			t.Fatalf("%s should have been written: %v", name, err)
		}
	}

	// A second session does not inherit s1's approve-all grant.
	if _, err := writeCall(g, "s2", "d.txt", "y\n"); err != nil {
		t.Fatalf("write s2: %v", err)
	}
	if n := len(rv.calls()); n != 2 {
		t.Fatalf("after a second session's write, reviewer calls = %d, want 2", n)
	}
}

// TestReviewEditApplied covers the edit tool path: an approved edit transforms
// the file and the reviewer sees a diff with both the removed and added lines.
func TestReviewEditApplied(t *testing.T) {
	g, workspace := newReviewGogent(t)
	if err := os.WriteFile(filepath.Join(workspace, "f.txt"), []byte("Hello, World!\n"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	rv := &fakeReviewer{decision: EditApprove}
	g.SetReviewer(rv)
	g.SetReviewEdits(true)

	if _, err := editCall(g, "s1", "f.txt", "World", "Universe"); err != nil {
		t.Fatalf("edit: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(workspace, "f.txt"))
	if string(got) != "Hello, Universe!\n" {
		t.Fatalf("file = %q, want %q", string(got), "Hello, Universe!\n")
	}
	calls := rv.calls()
	if len(calls) != 1 {
		t.Fatalf("reviewer consulted %d times, want 1", len(calls))
	}
	if !strings.Contains(calls[0].Diff, "-Hello, World!") || !strings.Contains(calls[0].Diff, "+Hello, Universe!") {
		t.Fatalf("diff missing expected lines:\n%s", calls[0].Diff)
	}
}

// TestReviewNoOpEditNotPrompted confirms a write that does not change the file
// (and therefore has an empty diff) is not sent for review.
func TestReviewNoOpEditNotPrompted(t *testing.T) {
	g, workspace := newReviewGogent(t)
	if err := os.WriteFile(filepath.Join(workspace, "f.txt"), []byte("same\n"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	rv := &fakeReviewer{decision: EditReject}
	g.SetReviewer(rv)
	g.SetReviewEdits(true)

	// Writing identical content is a no-op: no review, and the file survives.
	if _, err := writeCall(g, "s1", "f.txt", "same\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if n := len(rv.calls()); n != 0 {
		t.Fatalf("no-op write consulted reviewer %d times, want 0", n)
	}
	got, _ := os.ReadFile(filepath.Join(workspace, "f.txt"))
	if string(got) != "same\n" {
		t.Fatalf("file = %q, want %q", string(got), "same\n")
	}
}

// TestSetReviewEditsClearsApproveAll verifies disabling the feature drops any
// per-session approve-all grants, so re-enabling prompts again.
func TestSetReviewEditsClearsApproveAll(t *testing.T) {
	g, _ := newReviewGogent(t)
	rv := &fakeReviewer{decision: EditApproveAll}
	g.SetReviewer(rv)
	g.SetReviewEdits(true)

	if _, err := writeCall(g, "s1", "a.txt", "1\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !g.reviewApprovedAll["s1"] {
		t.Fatal("expected s1 to hold an approve-all grant")
	}

	g.SetReviewEdits(false)
	if len(g.reviewApprovedAll) != 0 {
		t.Fatalf("approve-all grants should be cleared on disable, got %v", g.reviewApprovedAll)
	}

	rv.decision = EditApprove
	g.SetReviewEdits(true)
	if _, err := writeCall(g, "s1", "b.txt", "2\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Two prompts total: the approve-all one before disable, and this one after.
	if n := len(rv.calls()); n != 2 {
		t.Fatalf("reviewer calls = %d, want 2 (grant cleared on disable)", n)
	}
}
