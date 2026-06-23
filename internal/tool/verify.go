package tool

import (
	"fmt"
	"strings"

	"gogent/internal/permission"
	"gogent/internal/verify"
)

// RegisterVerifyTool registers the `verify` tool (issue #44): it runs the
// configured test command and returns structured pass/fail results plus parsed
// failures, giving the model push-button "did the suite go green?" feedback and
// the tight edit→test→read-failures loop without shelling out to the runner by
// hand. cmd is the argument vector (empty falls back to the Go default,
// `go test ./...`).
//
// The command is fixed by configuration, never model-controlled, and pinned to
// the workspace root — but executing it runs arbitrary test code (and any
// build-time code it compiles), so each call is gated through a dedicated
// ActionVerify permission. That keeps an "always" grant scoped to verify alone:
// blessing verify never also blesses the shell or diagnostics tools, and vice
// versa.
func (tr *ToolRegistry) RegisterVerifyTool(cmd []string) {
	// Resolve the command once so the result always reports exactly what ran,
	// even when it defaulted.
	resolved := cmd
	if len(resolved) == 0 {
		resolved = verify.DefaultCommand
	}

	tr.Register(&Tool{
		Name: "verify",
		Description: "Run the project's test suite and return structured pass/fail " +
			"results plus the parsed failures (failing package, test name, message) " +
			"— push-button \"did the tests go green?\" feedback for the edit→test " +
			"loop. The default is `go test ./...`, whose output is parsed into " +
			"per-test and per-package failures (build failures and panics included); " +
			"the command is configurable. Prefer it over running the suite through " +
			"the shell: the command is fixed (no shell quoting), failures are parsed " +
			"into actionable items, and a passing result means the suite is green. " +
			"Call it after edits to confirm they did not break anything, and to get " +
			"the exact failures to fix.",
		Strict: true,
		InputSchema: map[string]interface{}{
			"type":                 "object",
			"properties":           map[string]interface{}{},
			"additionalProperties": false,
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			// Gate the run through a dedicated action so an "always" grant scopes
			// to verify alone, never the shell or diagnostics.
			perm := ctx.PermissionService
			if perm == nil {
				perm = tr.Permission
			}
			if perm != nil {
				rc := permission.RequestContext{SessionID: ctx.SessionID, Agent: ctx.AgentID}
				detail := "verify: " + strings.Join(resolved, " ")
				if err := perm.CheckWithContext(rc, permission.ActionVerify, "", detail); err != nil {
					return nil, fmt.Errorf("permission check: %w", err)
				}
			}

			rep, err := verify.Run(verify.Config{
				Dir:     tr.WorkspaceRoot,
				Command: cmd,
				Timeout: tr.ShellTimeout,
			})
			if err != nil {
				return nil, fmt.Errorf("failed to run verify: %v", err)
			}
			return verifyResult(rep), nil
		},
	})
}

// verifyResult renders a verify.Report as the tool's structured output.
func verifyResult(r *verify.Report) map[string]interface{} {
	out := map[string]interface{}{
		"command":           r.Command,
		"pass":              r.Pass,
		"exit_code":         r.ExitCode,
		"timeout":           r.Timeout,
		"count":             r.Count,
		"packages_ok":       r.PackagesOK,
		"packages_failed":   r.PackagesFailed,
		"packages_no_tests": r.PackagesNoTests,
	}
	if r.Output != "" {
		out["output"] = r.Output
	}
	if r.Truncated {
		out["truncated"] = true
	}
	failures := make([]map[string]interface{}, 0, len(r.Failures))
	for _, f := range r.Failures {
		m := map[string]interface{}{
			"package": f.Package,
			"message": f.Message,
		}
		if f.Test != "" {
			m["test"] = f.Test
		}
		failures = append(failures, m)
	}
	out["failures"] = failures
	return out
}
