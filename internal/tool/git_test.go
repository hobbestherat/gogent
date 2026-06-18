package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogent/internal/permission"
	"gogent/internal/vcs"
)

// allowShell returns a permission service that allows all shell-gated actions.
func allowShell() *permission.Service {
	s := permission.New("")
	s.AddRule(permission.Rule{Action: string(permission.ActionShell), Resource: "*", Effect: "allow"})
	return s
}

// gitRepoRegistry creates a temp git repo and a registry whose git tool acts on
// it. It skips the test when git is unavailable.
func gitRepoRegistry(t *testing.T) (*ToolRegistry, string) {
	t.Helper()
	if !vcs.Available() {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
	} {
		if res, err := vcs.Run(dir, vcs.DefaultTimeout, args...); err != nil || !res.OK() {
			t.Fatalf("git %v: err=%v stderr=%s", args, err, resStderr(res))
		}
	}
	tr := NewToolRegistry()
	tr.WorkspaceRoot = dir
	tr.Permission = allowShell()
	tr.RegisterGitTool()
	return tr, dir
}

func resStderr(r *vcs.Result) string {
	if r == nil {
		return ""
	}
	return r.Stderr
}

func gitTool(t *testing.T, tr *ToolRegistry) *Tool {
	t.Helper()
	tool := tr.Get("git")
	if tool == nil {
		t.Fatal("git tool not registered")
	}
	return tool
}

func runGit(t *testing.T, tr *ToolRegistry, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	res, err := gitTool(t, tr).Execute(args, ToolContext{})
	if err != nil {
		t.Fatalf("git %v: Execute() error = %v", args["operation"], err)
	}
	out, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("result is %T, want map", res)
	}
	return out
}

func writeRepoFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestGitToolStatusAndCommit(t *testing.T) {
	tr, dir := gitRepoRegistry(t)
	writeRepoFile(t, dir, "a.txt", "hello\n")

	// status shows the untracked file.
	out := runGit(t, tr, map[string]interface{}{"operation": "status"})
	if out["success"] != true {
		t.Fatalf("status success = %v, stderr=%v", out["success"], out["stderr"])
	}
	if !strings.Contains(out["stdout"].(string), "a.txt") {
		t.Errorf("status stdout missing a.txt: %q", out["stdout"])
	}

	// commit with explicit paths stages and records the file.
	out = runGit(t, tr, map[string]interface{}{
		"operation": "commit",
		"message":   "feat: add a",
		"paths":     []interface{}{"a.txt"},
	})
	if out["success"] != true {
		t.Fatalf("commit success = %v, stderr=%v", out["success"], out["stderr"])
	}

	// log reflects the commit.
	out = runGit(t, tr, map[string]interface{}{"operation": "log"})
	if !strings.Contains(out["stdout"].(string), "feat: add a") {
		t.Errorf("log missing commit: %q", out["stdout"])
	}
}

func TestGitToolCommitAll(t *testing.T) {
	tr, dir := gitRepoRegistry(t)
	writeRepoFile(t, dir, "a.txt", "1\n")
	runGit(t, tr, map[string]interface{}{"operation": "commit", "message": "init", "paths": []interface{}{"a.txt"}})

	// Modify the tracked file and commit with all=true.
	writeRepoFile(t, dir, "a.txt", "2\n")
	out := runGit(t, tr, map[string]interface{}{"operation": "commit", "message": "chore: bump", "all": true})
	if out["success"] != true {
		t.Fatalf("commit -a success = %v stderr=%v", out["success"], out["stderr"])
	}
}

func TestGitToolDiffStaged(t *testing.T) {
	tr, dir := gitRepoRegistry(t)
	writeRepoFile(t, dir, "a.txt", "hello\n")
	if res, err := vcs.Run(dir, vcs.DefaultTimeout, "add", "a.txt"); err != nil || !res.OK() {
		t.Fatalf("add: %v %v", err, resStderr(res))
	}

	// Unstaged diff is empty; staged diff shows the addition.
	if out := runGit(t, tr, map[string]interface{}{"operation": "diff"}); strings.TrimSpace(out["stdout"].(string)) != "" {
		t.Errorf("unstaged diff = %q, want empty", out["stdout"])
	}
	out := runGit(t, tr, map[string]interface{}{"operation": "diff", "staged": true})
	if !strings.Contains(out["stdout"].(string), "hello") {
		t.Errorf("staged diff missing content: %q", out["stdout"])
	}
}

func TestGitToolCreateBranch(t *testing.T) {
	tr, dir := gitRepoRegistry(t)
	writeRepoFile(t, dir, "a.txt", "x\n")
	runGit(t, tr, map[string]interface{}{"operation": "commit", "message": "init", "paths": []interface{}{"a.txt"}})

	out := runGit(t, tr, map[string]interface{}{"operation": "create_branch", "branch": "feature/x"})
	if out["success"] != true {
		t.Fatalf("create_branch success = %v stderr=%v", out["success"], out["stderr"])
	}
	cur, _ := vcs.Run(dir, vcs.DefaultTimeout, "rev-parse", "--abbrev-ref", "HEAD")
	if strings.TrimSpace(cur.Stdout) != "feature/x" {
		t.Errorf("current branch = %q, want feature/x", strings.TrimSpace(cur.Stdout))
	}
}

func TestGitToolRestore(t *testing.T) {
	tr, dir := gitRepoRegistry(t)
	writeRepoFile(t, dir, "a.txt", "original\n")
	runGit(t, tr, map[string]interface{}{"operation": "commit", "message": "init", "paths": []interface{}{"a.txt"}})

	// Dirty the file, then restore it.
	writeRepoFile(t, dir, "a.txt", "changed\n")
	out := runGit(t, tr, map[string]interface{}{"operation": "restore", "paths": []interface{}{"a.txt"}})
	if out["success"] != true {
		t.Fatalf("restore success = %v stderr=%v", out["success"], out["stderr"])
	}
	data, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(data) != "original\n" {
		t.Errorf("file content = %q, want restored original", string(data))
	}
}

func TestGitToolValidation(t *testing.T) {
	tr, _ := gitRepoRegistry(t)
	tool := gitTool(t, tr)

	cases := []map[string]interface{}{
		{},                             // missing operation
		{"operation": "commit"},        // missing message
		{"operation": "create_branch"}, // missing branch
		{"operation": "restore"},       // missing paths
		{"operation": "frobnicate"},    // unknown operation
	}
	for _, args := range cases {
		if _, err := tool.Execute(args, ToolContext{}); err == nil {
			t.Errorf("Execute(%v) expected error, got nil", args)
		}
	}
}

func TestGitToolGatesMutation(t *testing.T) {
	if !vcs.Available() {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{{"init"}, {"config", "user.email", "t@e.com"}, {"config", "user.name", "T"}} {
		vcs.Run(dir, vcs.DefaultTimeout, args...)
	}
	tr := NewToolRegistry()
	tr.WorkspaceRoot = dir
	tr.Permission = permission.New("") // no prompter, no rule: shell "ask" -> deny
	tr.RegisterGitTool()
	tool := gitTool(t, tr)

	// A mutating op is denied.
	if _, err := tool.Execute(map[string]interface{}{"operation": "create_branch", "branch": "x"}, ToolContext{}); err == nil {
		t.Error("create_branch expected permission denial, got nil")
	}
	// A read-only op is allowed without a permission rule.
	if _, err := tool.Execute(map[string]interface{}{"operation": "status"}, ToolContext{}); err != nil {
		t.Errorf("status should not be gated, got error: %v", err)
	}
}

func TestGitToolNotARepo(t *testing.T) {
	if !vcs.Available() {
		t.Skip("git not installed")
	}
	tr := NewToolRegistry()
	tr.WorkspaceRoot = t.TempDir() // plain dir, not a repo
	tr.Permission = allowShell()
	tr.RegisterGitTool()

	if _, err := gitTool(t, tr).Execute(map[string]interface{}{"operation": "status"}, ToolContext{}); err == nil {
		t.Error("expected error for non-repo workspace, got nil")
	}
}
