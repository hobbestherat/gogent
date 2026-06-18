package gogent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gogent/internal/tool"
)

// fakeApprover is a scripted EditApprover that records the previews it sees and
// returns a fixed decision (or a per-call decision via decideFn).
type fakeApprover struct {
	decision EditDecision
	decideFn func(EditPreview) EditDecision
	calls    int
	last     EditPreview
}

func (f *fakeApprover) ApproveEdit(p EditPreview) EditDecision {
	f.calls++
	f.last = p
	if f.decideFn != nil {
		return f.decideFn(p)
	}
	return f.decision
}

// newReviewGogent builds a Gogent rooted at a fresh temp workspace with edit
// review enabled and the given approver installed.
func newReviewGogent(t *testing.T, ap EditApprover) (*Gogent, string) {
	t.Helper()
	home := t.TempDir()
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	g := NewGogentWithWorkspace(home, workspace)
	g.SetEditApprover(ap)
	g.SetReviewEdits(true)
	return g, workspace
}

// callTool invokes a registered tool by name and returns its error.
func callTool(t *testing.T, g *Gogent, name string, args map[string]interface{}) error {
	t.Helper()
	tl := g.GetToolRegistry().Get(name)
	if tl == nil {
		t.Fatalf("tool %q not registered", name)
	}
	_, err := tl.Execute(args, tool.ToolContext{})
	return err
}

func TestReviewEditAccept(t *testing.T) {
	ap := &fakeApprover{decision: EditAccept}
	g, ws := newReviewGogent(t, ap)

	if err := callTool(t, g, "write", map[string]interface{}{
		"path": "hello.txt", "content": "hi\n",
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if ap.calls != 1 {
		t.Fatalf("approver calls = %d, want 1", ap.calls)
	}
	if ap.last.Path != "hello.txt" || ap.last.Stat.Added != 1 {
		t.Fatalf("preview = %+v, want path=hello.txt added=1", ap.last)
	}
	got, err := os.ReadFile(filepath.Join(ws, "hello.txt"))
	if err != nil || string(got) != "hi\n" {
		t.Fatalf("file = %q err=%v, want %q", got, err, "hi\n")
	}
}

func TestReviewEditReject(t *testing.T) {
	ap := &fakeApprover{decision: EditReject}
	g, ws := newReviewGogent(t, ap)

	path := filepath.Join(ws, "keep.txt")
	if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := callTool(t, g, "edit", map[string]interface{}{
		"path": "keep.txt", "find": "original", "replace": "changed",
	})
	if !errors.Is(err, ErrEditRejected) {
		t.Fatalf("edit err = %v, want ErrEditRejected", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "original\n" {
		t.Fatalf("rejected edit modified the file: %q", got)
	}
}

// TestReviewEditAcceptAll verifies the "accept all this session" latch: the
// first change is reviewed, later ones apply without consulting the approver.
func TestReviewEditAcceptAll(t *testing.T) {
	ap := &fakeApprover{decision: EditAcceptAll}
	g, ws := newReviewGogent(t, ap)

	if err := callTool(t, g, "write", map[string]interface{}{
		"path": "a.txt", "content": "one\n",
	}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := callTool(t, g, "write", map[string]interface{}{
		"path": "b.txt", "content": "two\n",
	}); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if ap.calls != 1 {
		t.Fatalf("approver calls = %d, want 1 (latched after accept-all)", ap.calls)
	}
	for name, want := range map[string]string{"a.txt": "one\n", "b.txt": "two\n"} {
		got, _ := os.ReadFile(filepath.Join(ws, name))
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
}

// TestReviewEditNoOpSkipsApprover ensures a write/edit that changes nothing does
// not bother the user.
func TestReviewEditNoOpSkipsApprover(t *testing.T) {
	ap := &fakeApprover{decision: EditReject}
	g, ws := newReviewGogent(t, ap)

	path := filepath.Join(ws, "same.txt")
	if err := os.WriteFile(path, []byte("body\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Editing a substring that is not present leaves the content identical.
	if err := callTool(t, g, "edit", map[string]interface{}{
		"path": "same.txt", "find": "absent", "replace": "x",
	}); err != nil {
		t.Fatalf("no-op edit: %v", err)
	}
	if ap.calls != 0 {
		t.Fatalf("approver calls = %d, want 0 for a no-op", ap.calls)
	}
}

// TestReviewEditDisabledBypasses ensures the gate is inert when review is off.
func TestReviewEditDisabledBypasses(t *testing.T) {
	ap := &fakeApprover{decision: EditReject}
	g, ws := newReviewGogent(t, ap)
	g.SetReviewEdits(false)

	if err := callTool(t, g, "write", map[string]interface{}{
		"path": "free.txt", "content": "written\n",
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if ap.calls != 0 {
		t.Fatalf("approver calls = %d, want 0 when review disabled", ap.calls)
	}
	got, _ := os.ReadFile(filepath.Join(ws, "free.txt"))
	if string(got) != "written\n" {
		t.Fatalf("file = %q, want written", got)
	}
}
