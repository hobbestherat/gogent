package fileops

import (
	"os"
	"path/filepath"
	"testing"

	"gogent/internal/permission"
)

// fakePrompter returns a fixed decision for every request, exercising the
// interactive (AskPermission) path of the permission service.
type fakePrompter struct{ decision permission.Decision }

func (f *fakePrompter) AskPermission(_ permission.Request) permission.Decision {
	return f.decision
}

// newAuthScaffold builds a FileSystem, LocationMutation and permission Service
// rooted at a fresh tempdir workspace, plus an unrelated external tempdir. The
// service mirrors NewGogentWithWorkspace's default posture: workspace reads and
// writes are allowed without prompting; external access falls through to "ask".
func newAuthScaffold(t *testing.T) (fsys *FileSystem, loc *LocationMutation, perm *permission.Service, externalDir string) {
	t.Helper()
	root := t.TempDir()
	externalDir = t.TempDir() // deliberately outside the workspace root
	fsys = NewFileSystem(root)
	loc = NewLocationMutation(root)
	perm = permission.New("") // no persistence
	perm.AddRule(permission.Rule{Action: string(permission.ActionRead), Resource: "*", Effect: string(permission.EffectAllow)})
	perm.AddRule(permission.Rule{Action: string(permission.ActionWrite), Resource: "*", Effect: string(permission.EffectAllow)})
	return fsys, loc, perm, externalDir
}

// TestCheckFileAccess_ExternalApproved is the regression for issue #13: once an
// external path is approved, CheckFileAccess returns an external Authorization
// and the file operation succeeds instead of failing with "escapes workspace".
func TestCheckFileAccess_ExternalApproved(t *testing.T) {
	fsys, loc, perm, externalDir := newAuthScaffold(t)

	// Grant covers the whole external root folder.
	perm.AddRule(permission.Rule{
		Action:   string(permission.ActionExternal),
		Resource: externalDir,
		Effect:   string(permission.EffectAllow),
	})

	target := filepath.Join(externalDir, "notes.txt")
	auth, err := CheckFileAccess(perm, loc, true, target)
	if err != nil {
		t.Fatalf("external write denied despite grant: %v", err)
	}
	if !auth.AllowsExternal() {
		t.Fatal("expected AllowsExternal authorization for approved external path")
	}

	if err := fsys.Write(target, []byte("hello"), auth); err != nil {
		t.Fatalf("write of approved external path failed: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("external file not written: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

// TestCheckFileAccess_ExternalInteractiveAllow exercises the prompter path:
// approving the prompt yields an external authorization.
func TestCheckFileAccess_ExternalInteractiveAllow(t *testing.T) {
	fsys, loc, perm, externalDir := newAuthScaffold(t)
	perm.SetPrompter(&fakePrompter{decision: permission.DecisionAllow})

	target := filepath.Join(externalDir, "data.bin")
	auth, err := CheckFileAccess(perm, loc, false, target)
	if err != nil {
		t.Fatalf("unexpected deny after interactive allow: %v", err)
	}
	if !auth.AllowsExternal() {
		t.Fatal("expected AllowsExternal after interactive allow")
	}

	// A read against the (non-existent) external path must reach the OS layer
	// rather than being rejected at the workspace boundary.
	if _, err := fsys.Read(target, auth); err == nil {
		// File does not exist, so we expect an OS read error — never a boundary
		// ("escapes workspace") error.
		t.Fatal("expected OS read error for missing file, got nil")
	}
}

// TestCheckFileAccess_ExternalDenied ensures an external path with no grant and
// no prompter is rejected (headless safe default) and yields no authorization.
func TestCheckFileAccess_ExternalDenied(t *testing.T) {
	fsys, loc, perm, externalDir := newAuthScaffold(t)

	target := filepath.Join(externalDir, "secret")
	auth, err := CheckFileAccess(perm, loc, true, target)
	if err == nil {
		t.Fatal("expected external path without grant to be denied")
	}
	if auth.AllowsExternal() {
		t.Fatal("denied request must not carry an external authorization")
	}

	// And the boundary still rejects the path when no authorization is present.
	if err := fsys.Write(target, []byte("x"), auth); err == nil {
		t.Fatal("expected workspace boundary to reject denied external path")
	}
}

// TestCheckFileAccess_WorkspacePathNoExternalAuth confirms an ordinary
// workspace path produces a workspace-only authorization.
func TestCheckFileAccess_WorkspacePathNoExternalAuth(t *testing.T) {
	_, loc, perm, _ := newAuthScaffold(t)

	auth, err := CheckFileAccess(perm, loc, true, "inside.txt")
	if err != nil {
		t.Fatalf("workspace write denied: %v", err)
	}
	if auth.AllowsExternal() {
		t.Fatal("workspace path must not carry an external authorization")
	}
}
