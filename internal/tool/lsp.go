package tool

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"gogent/internal/lsp"
	"gogent/internal/permission"
)

// RegisterLSPTools registers the curated lsp_* tools over a single language-server
// Manager (the LSP support design §12). The tools are a thin model-facing layer:
// each resolves the file's server (lazily launching it, permission-gated inside
// the Manager), issues one capability-gated operation, and returns structured
// results. "Not supported by this server" and "no server configured" are clean,
// expected results — not errors. Tier 1-2 tools are read-only; Tier 3 mutations
// are preview-then-apply, routed through the Host's write/checkpoint machinery.
//
// A nil Manager registers nothing, so a build with LSP disabled simply omits the
// tools.
func (tr *ToolRegistry) RegisterLSPTools(mgr *lsp.Manager) {
	if mgr == nil {
		return
	}
	tr.registerLSPDiagnostics(mgr)
	tr.registerLSPDefinition(mgr)
	tr.registerLSPReferences(mgr)
	tr.registerLSPHover(mgr)
	tr.registerLSPDocumentSymbols(mgr)
	tr.registerLSPWorkspaceSymbols(mgr)
	tr.registerLSPCallHierarchy(mgr)
	tr.registerLSPCodeActions(mgr)
	tr.registerLSPRename(mgr)
	tr.registerLSPFormat(mgr)
	tr.registerLSPExecuteCommand(mgr)
}

// resolveLSPPath resolves a tool's path argument against the workspace root.
func (tr *ToolRegistry) resolveLSPPath(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(tr.WorkspaceRoot, path)
}

// clientForPath resolves the running client for a path, mapping the Manager's
// non-fatal routing/launch outcomes to a structured "no server" result the caller
// surfaces instead of an error.
func (tr *ToolRegistry) clientForPath(mgr *lsp.Manager, ctx context.Context, path string) (*lsp.Client, string, map[string]interface{}) {
	abs := tr.resolveLSPPath(path)
	client, err := mgr.ClientForFile(ctx, abs)
	if err != nil {
		if errors.Is(err, lsp.ErrNoServer) {
			return nil, abs, map[string]interface{}{
				"supported": false,
				"reason":    fmt.Sprintf("no LSP server configured for %s", filepath.Ext(abs)),
			}
		}
		return nil, abs, map[string]interface{}{
			"supported": false,
			"reason":    fmt.Sprintf("language server unavailable: %v", err),
		}
	}
	return client, abs, nil
}

// workspaceClientForHint resolves the client for a workspace-scoped tool
// (lsp_workspace_symbols). A path hint routes like any file; without one the
// Manager routes to the sole configured server, and an ambiguous routing (more
// than one server) is surfaced as a clean structured result asking for a path
// hint rather than silently biasing to one language.
func (tr *ToolRegistry) workspaceClientForHint(mgr *lsp.Manager, ctx context.Context, hint string) (*lsp.Client, map[string]interface{}) {
	if hint != "" {
		client, _, miss := tr.clientForPath(mgr, ctx, hint)
		return client, miss
	}
	client, err := mgr.WorkspaceClient(ctx, "")
	if err != nil {
		switch {
		case errors.Is(err, lsp.ErrAmbiguousServer):
			return nil, map[string]interface{}{
				"supported": false,
				"reason":    "multiple language servers are configured; pass \"path\" (any file in the target workspace) to select one",
			}
		case errors.Is(err, lsp.ErrNoServer):
			return nil, map[string]interface{}{
				"supported": false,
				"reason":    "no LSP server configured",
			}
		default:
			return nil, map[string]interface{}{
				"supported": false,
				"reason":    fmt.Sprintf("language server unavailable: %v", err),
			}
		}
	}
	return client, nil
}

// opCtx returns the operation's context, defaulting to Background.
func opCtx(ctx ToolContext) context.Context {
	if ctx.Context != nil {
		return ctx.Context
	}
	return context.Background()
}

// unsupportedResult maps ErrUnsupported to the clean, user-visible "not supported
// by this server for this file" result, and any other error to a tool error.
func unsupportedResult(err error) (interface{}, error, bool) {
	if errors.Is(err, lsp.ErrUnsupported) {
		return map[string]interface{}{
			"supported": false,
			"reason":    "operation not supported by this server for this file",
		}, nil, true
	}
	return nil, nil, false
}

func (tr *ToolRegistry) registerLSPDiagnostics(mgr *lsp.Manager) {
	tr.Register(&Tool{
		Name:     "lsp_diagnostics",
		ReadOnly: true,
		Description: "Return live, structured diagnostics (errors/warnings) for a single file from " +
			"its language server — the tight per-file feedback loop after an edit. Each finding has a " +
			"severity, code, source, message and 1-based range. Use this to check \"what's wrong with " +
			"THIS file right now?\"; for whole-project build/test use the shell.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{"type": "string", "description": "File path (relative to the workspace root or absolute)."},
			},
			"required": []string{"path"},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			path, _ := args["path"].(string)
			client, abs, miss := tr.clientForPath(mgr, opCtx(ctx), path)
			if miss != nil {
				return miss, nil
			}
			diags, err := client.Diagnostics(opCtx(ctx), abs)
			if r, e, ok := unsupportedResult(err); ok {
				return r, e
			}
			if err != nil {
				return nil, fmt.Errorf("lsp_diagnostics: %w", err)
			}
			return map[string]interface{}{"path": abs, "count": len(diags), "diagnostics": diags}, nil
		},
	})
}

func (tr *ToolRegistry) registerLSPDefinition(mgr *lsp.Manager) {
	tr.Register(&Tool{
		Name:     "lsp_definition",
		ReadOnly: true,
		Description: "Resolve where the symbol at a position is defined. The optional \"kind\" selects the " +
			"\"go to\" family member: definition (default), declaration, type, or implementation. " +
			"Positions are 1-based (line and column). Returns the target location(s).",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":   map[string]interface{}{"type": "string", "description": "File path."},
				"line":   map[string]interface{}{"type": "integer", "description": "1-based line."},
				"column": map[string]interface{}{"type": "integer", "description": "1-based column."},
				"kind":   map[string]interface{}{"type": "string", "description": "definition|declaration|type|implementation (default definition)."},
			},
			"required": []string{"path", "line", "column"},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			client, abs, miss := tr.clientForPath(mgr, opCtx(ctx), strArg(args["path"]))
			if miss != nil {
				return miss, nil
			}
			pos, ok := posArg(args)
			if !ok {
				return nil, fmt.Errorf("lsp_definition: line and column are required")
			}
			locs, err := client.Definition(opCtx(ctx), abs, pos, defKind(strArg(args["kind"])))
			if r, e, ok := unsupportedResult(err); ok {
				return r, e
			}
			if err != nil {
				return nil, fmt.Errorf("lsp_definition: %w", err)
			}
			return map[string]interface{}{"locations": locs, "count": len(locs)}, nil
		},
	})
}

func (tr *ToolRegistry) registerLSPReferences(mgr *lsp.Manager) {
	tr.Register(&Tool{
		Name:     "lsp_references",
		ReadOnly: true,
		Description: "Find every reference to the symbol at a 1-based position across the workspace. Set " +
			"include_declaration to also return the declaration. Returns locations.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":                map[string]interface{}{"type": "string", "description": "File path."},
				"line":                map[string]interface{}{"type": "integer", "description": "1-based line."},
				"column":              map[string]interface{}{"type": "integer", "description": "1-based column."},
				"include_declaration": map[string]interface{}{"type": "boolean", "description": "Include the symbol's declaration."},
			},
			"required": []string{"path", "line", "column"},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			client, abs, miss := tr.clientForPath(mgr, opCtx(ctx), strArg(args["path"]))
			if miss != nil {
				return miss, nil
			}
			pos, ok := posArg(args)
			if !ok {
				return nil, fmt.Errorf("lsp_references: line and column are required")
			}
			incl, _ := args["include_declaration"].(bool)
			locs, err := client.References(opCtx(ctx), abs, pos, incl)
			if r, e, ok := unsupportedResult(err); ok {
				return r, e
			}
			if err != nil {
				return nil, fmt.Errorf("lsp_references: %w", err)
			}
			return map[string]interface{}{"locations": locs, "count": len(locs)}, nil
		},
	})
}

func (tr *ToolRegistry) registerLSPHover(mgr *lsp.Manager) {
	tr.Register(&Tool{
		Name:     "lsp_hover",
		ReadOnly: true,
		Description: "Return the type signature and documentation for the symbol at a 1-based position " +
			"(the \"hover\" an editor shows). Prefer it over guessing a symbol's type or signature.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":   map[string]interface{}{"type": "string", "description": "File path."},
				"line":   map[string]interface{}{"type": "integer", "description": "1-based line."},
				"column": map[string]interface{}{"type": "integer", "description": "1-based column."},
			},
			"required": []string{"path", "line", "column"},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			client, abs, miss := tr.clientForPath(mgr, opCtx(ctx), strArg(args["path"]))
			if miss != nil {
				return miss, nil
			}
			pos, ok := posArg(args)
			if !ok {
				return nil, fmt.Errorf("lsp_hover: line and column are required")
			}
			h, err := client.Hover(opCtx(ctx), abs, pos)
			if r, e, ok := unsupportedResult(err); ok {
				return r, e
			}
			if err != nil {
				return nil, fmt.Errorf("lsp_hover: %w", err)
			}
			return map[string]interface{}{"contents": h.Contents}, nil
		},
	})
}

func (tr *ToolRegistry) registerLSPDocumentSymbols(mgr *lsp.Manager) {
	tr.Register(&Tool{
		Name:        "lsp_document_symbols",
		ReadOnly:    true,
		Description: "Return the symbol tree of a file (functions, types, methods, fields, ...) for a quick structural map. The tree shape is the same whether the server answers hierarchically or flat.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{"type": "string", "description": "File path."},
			},
			"required": []string{"path"},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			client, abs, miss := tr.clientForPath(mgr, opCtx(ctx), strArg(args["path"]))
			if miss != nil {
				return miss, nil
			}
			syms, err := client.DocumentSymbols(opCtx(ctx), abs)
			if r, e, ok := unsupportedResult(err); ok {
				return r, e
			}
			if err != nil {
				return nil, fmt.Errorf("lsp_document_symbols: %w", err)
			}
			return map[string]interface{}{"symbols": syms, "count": len(syms)}, nil
		},
	})
}

func (tr *ToolRegistry) registerLSPWorkspaceSymbols(mgr *lsp.Manager) {
	tr.Register(&Tool{
		Name:        "lsp_workspace_symbols",
		ReadOnly:    true,
		Description: "Search the whole workspace for symbols matching a fuzzy query (functions, types, ...). Faster and more precise than grepping for a definition by name. With a single language server configured the query routes to it automatically; pass \"path\" (any file in the target workspace) to pick the server when more than one is configured.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{"type": "string", "description": "Symbol name query (fuzzy)."},
				"path":  map[string]interface{}{"type": "string", "description": "Any file in the target workspace, to select the server (optional; required only when more than one server is configured)."},
			},
			"required": []string{"query"},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			client, miss := tr.workspaceClientForHint(mgr, opCtx(ctx), strArg(args["path"]))
			if miss != nil {
				return miss, nil
			}
			syms, err := client.WorkspaceSymbols(opCtx(ctx), strArg(args["query"]))
			if r, e, ok := unsupportedResult(err); ok {
				return r, e
			}
			if err != nil {
				return nil, fmt.Errorf("lsp_workspace_symbols: %w", err)
			}
			return map[string]interface{}{"symbols": syms, "count": len(syms)}, nil
		},
	})
}

func (tr *ToolRegistry) registerLSPCallHierarchy(mgr *lsp.Manager) {
	tr.Register(&Tool{
		Name:        "lsp_call_hierarchy",
		ReadOnly:    true,
		Description: "Return the incoming or outgoing calls of the function at a 1-based position. direction is \"incoming\" (callers) or \"outgoing\" (callees).",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":      map[string]interface{}{"type": "string", "description": "File path."},
				"line":      map[string]interface{}{"type": "integer", "description": "1-based line."},
				"column":    map[string]interface{}{"type": "integer", "description": "1-based column."},
				"direction": map[string]interface{}{"type": "string", "description": "incoming|outgoing (default incoming)."},
			},
			"required": []string{"path", "line", "column"},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			client, abs, miss := tr.clientForPath(mgr, opCtx(ctx), strArg(args["path"]))
			if miss != nil {
				return miss, nil
			}
			pos, ok := posArg(args)
			if !ok {
				return nil, fmt.Errorf("lsp_call_hierarchy: line and column are required")
			}
			dir := lsp.Incoming
			if strArg(args["direction"]) == "outgoing" {
				dir = lsp.Outgoing
			}
			items, err := client.CallHierarchy(opCtx(ctx), abs, pos, dir)
			if r, e, ok := unsupportedResult(err); ok {
				return r, e
			}
			if err != nil {
				return nil, fmt.Errorf("lsp_call_hierarchy: %w", err)
			}
			return map[string]interface{}{"direction": string(dir), "calls": items, "count": len(items)}, nil
		},
	})
}

func (tr *ToolRegistry) registerLSPCodeActions(mgr *lsp.Manager) {
	tr.Register(&Tool{
		Name: "lsp_code_actions",
		Description: "List the available code actions (quick fixes, refactors) for a range, each resolved " +
			"to a concrete WorkspaceEdit you can preview, with a zero-based \"index\". By default this only " +
			"previews. To apply one, call again with apply:true and action_index set to the action's index; the " +
			"resolved edit goes through gogent's normal write permission + undo (checkpoint) machinery. Actions " +
			"that only carry a command are executeCommand candidates — run those with lsp_execute_command, which " +
			"is separately gated.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":         map[string]interface{}{"type": "string", "description": "File path."},
				"line":         map[string]interface{}{"type": "integer", "description": "1-based line of the range start."},
				"column":       map[string]interface{}{"type": "integer", "description": "1-based column of the range start."},
				"end_line":     map[string]interface{}{"type": "integer", "description": "1-based line of the range end (default = line)."},
				"end_column":   map[string]interface{}{"type": "integer", "description": "1-based column of the range end (default = column)."},
				"apply":        map[string]interface{}{"type": "boolean", "description": "Apply the action at action_index (default false = preview only)."},
				"action_index": map[string]interface{}{"type": "integer", "description": "Zero-based index of the action to apply (with apply:true)."},
			},
			"required": []string{"path", "line", "column"},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			client, abs, miss := tr.clientForPath(mgr, opCtx(ctx), strArg(args["path"]))
			if miss != nil {
				return miss, nil
			}
			rng, ok := rangeArg(args)
			if !ok {
				return nil, fmt.Errorf("lsp_code_actions: line and column are required")
			}
			actions, err := client.CodeActions(opCtx(ctx), abs, rng)
			if r, e, ok := unsupportedResult(err); ok {
				return r, e
			}
			if err != nil {
				return nil, fmt.Errorf("lsp_code_actions: %w", err)
			}
			if boolArg(args["apply"]) {
				return tr.applyCodeAction(mgr, client.Name(), actions, args)
			}
			return map[string]interface{}{"actions": withActionIndex(actions), "count": len(actions)}, nil
		},
	})
}

// applyCodeAction applies the resolved edit of the action selected by action_index
// through the Host (ActionWrite + Checkpointer). An action that carries only a
// command (no edit) is surfaced as an lsp_execute_command candidate rather than run
// silently (§12).
func (tr *ToolRegistry) applyCodeAction(mgr *lsp.Manager, server string, actions []lsp.CodeAction, args map[string]interface{}) (interface{}, error) {
	idx, _ := intArg(args["action_index"])
	if idx < 0 || idx >= len(actions) {
		return map[string]interface{}{
			"applied": false,
			"reason":  fmt.Sprintf("action_index %d out of range (%d actions available)", idx, len(actions)),
		}, nil
	}
	action := actions[idx]
	if action.Edit == nil {
		reason := "selected action has no editable WorkspaceEdit"
		if action.Command != "" {
			reason = "selected action runs the command '" + action.Command + "'; run it with lsp_execute_command (separately gated)"
		}
		return map[string]interface{}{"applied": false, "reason": reason, "command": action.Command}, nil
	}
	return tr.previewOrApply(mgr, server, *action.Edit, true)
}

// withActionIndex annotates each previewed action with its zero-based index so the
// model can name one to apply.
func withActionIndex(actions []lsp.CodeAction) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(actions))
	for i, a := range actions {
		out = append(out, map[string]interface{}{
			"index":     i,
			"title":     a.Title,
			"kind":      a.Kind,
			"edit":      a.Edit,
			"command":   a.Command,
			"preferred": a.Preferred,
		})
	}
	return out
}

func (tr *ToolRegistry) registerLSPRename(mgr *lsp.Manager) {
	tr.Register(&Tool{
		Name: "lsp_rename",
		Description: "Rename the symbol at a 1-based position across the workspace. By default it returns " +
			"the proposed WorkspaceEdit for preview WITHOUT changing files; set apply:true to apply it through " +
			"gogent's normal write permission + undo (checkpoint) machinery.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":     map[string]interface{}{"type": "string", "description": "File path."},
				"line":     map[string]interface{}{"type": "integer", "description": "1-based line."},
				"column":   map[string]interface{}{"type": "integer", "description": "1-based column."},
				"new_name": map[string]interface{}{"type": "string", "description": "The new symbol name."},
				"apply":    map[string]interface{}{"type": "boolean", "description": "Apply the edit (default false = preview only)."},
			},
			"required": []string{"path", "line", "column", "new_name"},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			client, abs, miss := tr.clientForPath(mgr, opCtx(ctx), strArg(args["path"]))
			if miss != nil {
				return miss, nil
			}
			pos, ok := posArg(args)
			if !ok {
				return nil, fmt.Errorf("lsp_rename: line and column are required")
			}
			edit, err := client.Rename(opCtx(ctx), abs, pos, strArg(args["new_name"]))
			if r, e, ok := unsupportedResult(err); ok {
				return r, e
			}
			if err != nil {
				return nil, fmt.Errorf("lsp_rename: %w", err)
			}
			return tr.previewOrApply(mgr, client.Name(), edit, boolArg(args["apply"]))
		},
	})
}

func (tr *ToolRegistry) registerLSPFormat(mgr *lsp.Manager) {
	tr.Register(&Tool{
		Name: "lsp_format",
		Description: "Format a whole file with its language server's formatter. By default it returns the " +
			"proposed edit for preview; set apply:true to apply it through gogent's write permission + undo.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":  map[string]interface{}{"type": "string", "description": "File path."},
				"apply": map[string]interface{}{"type": "boolean", "description": "Apply the edit (default false = preview only)."},
			},
			"required": []string{"path"},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			client, abs, miss := tr.clientForPath(mgr, opCtx(ctx), strArg(args["path"]))
			if miss != nil {
				return miss, nil
			}
			edit, err := client.Format(opCtx(ctx), abs)
			if r, e, ok := unsupportedResult(err); ok {
				return r, e
			}
			if err != nil {
				return nil, fmt.Errorf("lsp_format: %w", err)
			}
			return tr.previewOrApply(mgr, client.Name(), edit, boolArg(args["apply"]))
		},
	})
}

func (tr *ToolRegistry) registerLSPExecuteCommand(mgr *lsp.Manager) {
	tr.Register(&Tool{
		Name: "lsp_execute_command",
		Description: "Ask the language server to run a workspace command (e.g. a gopls refactor command), for " +
			"commands surfaced by lsp_code_actions. This is HIGHER RISK than an edit: the server performs the work " +
			"out-of-band and its side effects are NOT checkpointable/undoable, so it is separately permission-gated " +
			"and only commands the server is configured to allow may run.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":      map[string]interface{}{"type": "string", "description": "Any file routing to the target server."},
				"command":   map[string]interface{}{"type": "string", "description": "The server command identifier."},
				"arguments": map[string]interface{}{"type": "array", "description": "Optional command arguments."},
			},
			"required": []string{"path", "command"},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			client, _, miss := tr.clientForPath(mgr, opCtx(ctx), strArg(args["path"]))
			if miss != nil {
				return miss, nil
			}
			command := strArg(args["command"])
			// Consult the server's allow-list before prompting so an off-list command
			// is declined up front rather than after a spurious ActionLSPCommand
			// request (ExecuteCommand re-checks it as defence in depth, §12).
			if !client.CommandAllowed(command) {
				return map[string]interface{}{
					"executed": false,
					"reason":   "command is not in this server's allow-list",
				}, nil
			}
			// Gate the higher-risk command through its own action (resource = server +
			// command id), distinct from ActionWrite — its effects are not undoable.
			perm := ctx.PermissionService
			if perm == nil {
				perm = tr.Permission
			}
			if perm != nil {
				rc := permission.RequestContext{SessionID: ctx.SessionID, Agent: ctx.AgentID}
				resource := client.Name() + ":" + command
				detail := "language server command (uncheckpointable): " + resource
				if err := perm.CheckWithContext(rc, permission.ActionLSPCommand, resource, detail); err != nil {
					return nil, fmt.Errorf("permission check: %w", err)
				}
			}
			var cmdArgs []any
			if raw, ok := args["arguments"].([]interface{}); ok {
				cmdArgs = raw
			}
			res, err := client.ExecuteCommand(opCtx(ctx), command, cmdArgs)
			if errors.Is(err, lsp.ErrCommandNotAllowed) {
				return map[string]interface{}{
					"executed": false,
					"reason":   "command is not in this server's allow-list",
				}, nil
			}
			if r, e, ok := unsupportedResult(err); ok {
				return r, e
			}
			if err != nil {
				return nil, fmt.Errorf("lsp_execute_command: %w", err)
			}
			return map[string]interface{}{"executed": true, "result": res}, nil
		},
	})
}

// previewOrApply returns a Tier 3 edit as a preview, or applies it through the
// Manager's Host (ActionWrite + Checkpointer) when apply is set (§12).
func (tr *ToolRegistry) previewOrApply(mgr *lsp.Manager, server string, edit lsp.WorkspaceEdit, apply bool) (interface{}, error) {
	if !apply {
		return map[string]interface{}{"applied": false, "preview": true, "edit": edit}, nil
	}
	applied, reason, err := mgr.ApplyEdit(server, edit)
	if err != nil {
		return nil, fmt.Errorf("apply edit: %w", err)
	}
	out := map[string]interface{}{"applied": applied, "edit": edit}
	if !applied && reason != "" {
		out["reason"] = reason
	}
	return out, nil
}

// strArg coerces a JSON value to a string ("" when absent/wrong type).
func strArg(v interface{}) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// boolArg coerces a JSON value to a bool.
func boolArg(v interface{}) bool {
	b, _ := v.(bool)
	return b
}

// posArg reads a 1-based line/column from the args.
func posArg(args map[string]interface{}) (lsp.Position, bool) {
	line, lok := intArg(args["line"])
	col, cok := intArg(args["column"])
	if !lok || !cok {
		return lsp.Position{}, false
	}
	return lsp.Position{Line: line, Character: col}, true
}

// rangeArg reads a range from line/column (start) and optional end_line/end_column.
func rangeArg(args map[string]interface{}) (lsp.Range, bool) {
	start, ok := posArg(args)
	if !ok {
		return lsp.Range{}, false
	}
	end := start
	if l, ok := intArg(args["end_line"]); ok {
		end.Line = l
	}
	if c, ok := intArg(args["end_column"]); ok {
		end.Character = c
	}
	return lsp.Range{Start: start, End: end}, true
}

// defKind maps a kind argument to a lsp.DefKind, defaulting to definition.
func defKind(s string) lsp.DefKind {
	switch s {
	case "declaration":
		return lsp.DefDeclaration
	case "type", "typeDefinition", "type_definition":
		return lsp.DefTypeDefinition
	case "implementation":
		return lsp.DefImplementation
	default:
		return lsp.DefDefinition
	}
}
