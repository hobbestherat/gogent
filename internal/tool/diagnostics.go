package tool

import (
	"fmt"
	"strings"

	"gogent/internal/diagnostics"
	"gogent/internal/permission"
)

// RegisterDiagnosticsTool registers the `diagnostics` tool (issue #42): it runs
// the configured compiler/linter and returns structured file:line:column
// findings, giving the model push-button "did it compile / typecheck?" feedback
// without shelling out to the compiler by hand. cmd is the argument vector
// (empty falls back to the Go default, `go vet ./...`); warningPattern
// optionally marks matching messages as warnings.
//
// The command is fixed by configuration, never model-controlled, and pinned to
// the workspace root — but executing it does run build-time code (a cgo
// package's C sources, vet analyzers), so each call is gated through a
// dedicated ActionDiagnostics permission. That keeps an "always" grant scoped to
// diagnostics alone: blessing diagnostics never also blesses the shell tool, and
// vice versa.
func (tr *ToolRegistry) RegisterDiagnosticsTool(cmd []string, warningPattern string) {
	// Resolve the command once so the result always reports exactly what ran,
	// even when it defaulted.
	resolved := cmd
	if len(resolved) == 0 {
		resolved = diagnostics.DefaultCommand
	}

	tr.Register(&Tool{
		Name: "diagnostics",
		Description: "Run the project's compiler/linter and return structured errors " +
			"(file:line:column, severity, message) — push-button \"did it compile / " +
			"typecheck?\" feedback. The default is `go vet ./...`, which typechecks Go " +
			"and reports vet findings; the command is configurable. Prefer it over " +
			"running the compiler through the shell: the command is fixed (no shell " +
			"quoting), output is parsed into actionable diagnostics, and an ok result " +
			"means the project builds. Call it after edits to catch breakage early.",
		Strict: true,
		InputSchema: map[string]interface{}{
			"type":                 "object",
			"properties":           map[string]interface{}{},
			"additionalProperties": false,
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			// Gate the run through a dedicated action so an "always" grant scopes
			// to diagnostics alone, never the shell.
			perm := ctx.PermissionService
			if perm == nil {
				perm = tr.Permission
			}
			if perm != nil {
				rc := permission.RequestContext{SessionID: ctx.SessionID, Agent: ctx.AgentID}
				detail := "diagnostics: " + strings.Join(resolved, " ")
				if err := perm.CheckWithContext(rc, permission.ActionDiagnostics, "", detail); err != nil {
					return nil, fmt.Errorf("permission check: %w", err)
				}
			}

			rep, err := diagnostics.Run(diagnostics.Config{
				Dir:            tr.WorkspaceRoot,
				Command:        cmd,
				WarningPattern: warningPattern,
				Timeout:        tr.ShellTimeout,
			})
			if err != nil {
				return nil, fmt.Errorf("failed to run diagnostics: %v", err)
			}
			return diagnosticsResult(rep), nil
		},
	})
}

// diagnosticsResult renders a diagnostics.Report as the tool's structured output.
func diagnosticsResult(r *diagnostics.Report) map[string]interface{} {
	out := map[string]interface{}{
		"command":   r.Command,
		"ok":        r.OK,
		"exit_code": r.ExitCode,
		"timeout":   r.Timeout,
		"count":     r.Count,
	}
	if r.Output != "" {
		out["output"] = r.Output
	}
	if r.Truncated {
		out["truncated"] = true
	}
	diags := make([]map[string]interface{}, 0, len(r.Diagnostics))
	for _, d := range r.Diagnostics {
		diags = append(diags, map[string]interface{}{
			"path":     d.Path,
			"line":     d.Line,
			"column":   d.Column,
			"severity": string(d.Severity),
			"message":  d.Message,
		})
	}
	out["diagnostics"] = diags
	return out
}
