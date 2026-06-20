package tool

import (
	"fmt"
	"strings"

	"gogent/internal/permission"
	"gogent/internal/vcs"
)

// gitDefaultLogCount bounds `git log` output when the caller does not ask for a
// specific number of commits.
const gitDefaultLogCount = 20

// RegisterGitTool registers the native `git` tool: a thin, dispatched wrapper
// over the git binary (status / diff / log / commit / create_branch / restore).
// It runs git with explicit argument vectors via internal/vcs, so there is no
// shell-injection surface and no dependency on the gated shell. Read-only
// operations (status, diff, log) run freely; mutating operations (commit,
// create_branch, restore) are gated through the permission service so they are
// asked once like any other command. The workspace root is the repository the
// operations act on.
func (tr *ToolRegistry) RegisterGitTool() {
	tr.Register(&Tool{
		Name: "git",
		Description: "Run native git operations in the workspace repository. Prefer this over " +
			"running git through the shell: arguments are passed directly (no shell quoting) " +
			"and output is returned structured. operation is one of: " +
			"status (working-tree summary), diff (unstaged or, with staged=true, staged changes), " +
			"log (recent commits), commit (record staged changes; stage paths first, or all=true " +
			"to stage tracked modifications), create_branch (create and switch to a branch), " +
			"restore (discard working-tree changes for the given paths — destructive).",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"operation": map[string]interface{}{
					"type":        "string",
					"enum":        []string{"status", "diff", "log", "commit", "create_branch", "restore"},
					"description": "The git operation to perform.",
				},
				"message": map[string]interface{}{"type": "string", "description": "Commit message (required for commit)."},
				"branch":  map[string]interface{}{"type": "string", "description": "Branch name (required for create_branch)."},
				"paths": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "Files to act on. commit: stage these before committing. diff: limit to these. restore: paths to discard (required).",
				},
				"staged":    map[string]interface{}{"type": "boolean", "description": "diff: show staged changes instead of unstaged."},
				"all":       map[string]interface{}{"type": "boolean", "description": "commit: stage all tracked modified files before committing."},
				"max_count": map[string]interface{}{"type": "integer", "description": "log: number of commits to show (default 20)."},
			},
			"required": []string{"operation"},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			op, ok := args["operation"].(string)
			if !ok || strings.TrimSpace(op) == "" {
				return nil, fmt.Errorf("operation argument is required")
			}
			op = strings.TrimSpace(op)

			if !vcs.Available() {
				return nil, fmt.Errorf("git is not installed or not on PATH")
			}
			if !vcs.IsRepo(tr.WorkspaceRoot) {
				return nil, fmt.Errorf("workspace is not a git repository")
			}

			plan, err := buildGitPlan(op, args)
			if err != nil {
				return nil, err
			}

			// Gate mutating operations once, the same way shell is gated. The detail
			// is the exact git command so the user sees what they are approving.
			if plan.mutating {
				perm := ctx.PermissionService
				if perm == nil {
					perm = tr.Permission
				}
				if perm != nil {
					detail := "git " + strings.Join(plan.args, " ")
					rc := permission.RequestContext{SessionID: ctx.SessionID, Agent: ctx.AgentID}
					if err := perm.CheckWithContext(rc, permission.ActionShell, "", detail); err != nil {
						return nil, fmt.Errorf("permission check: %w", err)
					}
				}
			}

			// Some operations stage files first (e.g. commit with explicit paths):
			// `git commit -- <path>` only records already-tracked files, so a new
			// file must be added before it can be committed. If staging fails, return
			// its result rather than attempting the primary command.
			if len(plan.stage) > 0 {
				addArgs := append([]string{"add", "--"}, plan.stage...)
				addRes, err := vcs.Run(tr.WorkspaceRoot, tr.ShellTimeout, addArgs...)
				if err != nil {
					return nil, fmt.Errorf("git add failed: %v", err)
				}
				if !addRes.OK() {
					return gitResult("add", addRes), nil
				}
			}

			res, err := vcs.Run(tr.WorkspaceRoot, tr.ShellTimeout, plan.args...)
			if err != nil {
				return nil, fmt.Errorf("git %s failed: %v", op, err)
			}

			return gitResult(op, res), nil
		},
	})
}

// gitResult renders a vcs.Result as the tool's structured output.
func gitResult(op string, res *vcs.Result) map[string]interface{} {
	return map[string]interface{}{
		"operation": op,
		"stdout":    res.Stdout,
		"stderr":    res.Stderr,
		"exit_code": res.ExitCode,
		"timeout":   res.Timeout,
		"success":   res.OK(),
	}
}

// gitPlan is the resolved execution plan for a git tool invocation: an optional
// `git add` staging step, the primary git command, and whether the operation
// mutates the repository (and so must be gated).
type gitPlan struct {
	stage    []string // pathspecs to `git add --` before running args; may be empty
	args     []string // the primary git command
	mutating bool
}

// buildGitPlan translates a tool invocation into its execution plan.
func buildGitPlan(op string, args map[string]interface{}) (gitPlan, error) {
	switch op {
	case "status":
		return gitPlan{args: []string{"status", "--short", "--branch"}}, nil

	case "diff":
		a := []string{"diff"}
		if b, _ := args["staged"].(bool); b {
			a = append(a, "--staged")
		}
		if paths := stringSliceArg(args["paths"]); len(paths) > 0 {
			a = append(a, "--")
			a = append(a, paths...)
		}
		return gitPlan{args: a}, nil

	case "log":
		n := gitDefaultLogCount
		if v, ok := intArg(args["max_count"]); ok && v > 0 {
			n = v
		}
		return gitPlan{args: []string{"log", "--oneline", "--decorate", fmt.Sprintf("-n%d", n)}}, nil

	case "commit":
		msg, ok := args["message"].(string)
		if !ok || strings.TrimSpace(msg) == "" {
			return gitPlan{}, fmt.Errorf("message argument is required for commit")
		}
		a := []string{"commit"}
		if b, _ := args["all"].(bool); b {
			a = append(a, "-a")
		}
		a = append(a, "-m", msg)
		// Explicit paths are staged first (so new files are included), then a plain
		// commit records the index.
		return gitPlan{stage: stringSliceArg(args["paths"]), args: a, mutating: true}, nil

	case "create_branch":
		branch, ok := args["branch"].(string)
		if !ok || strings.TrimSpace(branch) == "" {
			return gitPlan{}, fmt.Errorf("branch argument is required for create_branch")
		}
		return gitPlan{args: []string{"checkout", "-b", strings.TrimSpace(branch)}, mutating: true}, nil

	case "restore":
		paths := stringSliceArg(args["paths"])
		if len(paths) == 0 {
			return gitPlan{}, fmt.Errorf("paths argument is required for restore")
		}
		return gitPlan{args: append([]string{"restore", "--"}, paths...), mutating: true}, nil
	}
	return gitPlan{}, fmt.Errorf("unknown git operation %q", op)
}

// stringSliceArg coerces a JSON-decoded argument into a slice of non-empty
// strings. JSON arrays decode to []interface{}; a single string is accepted as a
// one-element slice for convenience.
func stringSliceArg(v interface{}) []string {
	switch val := v.(type) {
	case []interface{}:
		out := make([]string, 0, len(val))
		for _, e := range val {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return val
	case string:
		if strings.TrimSpace(val) != "" {
			return []string{val}
		}
	}
	return nil
}
