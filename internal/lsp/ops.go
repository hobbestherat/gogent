package lsp

import (
	"context"
	"fmt"

	"go.lsp.dev/protocol"
)

// This file is the curated, capability-gated operation surface the tools call
// (the LSP support design §7.2). Each op ensures the document is open and synced,
// checks the capability table, issues one cancel-aware request, and normalizes the
// union response shape at the edge. "Unsupported by this server" is ErrUnsupported,
// a clean expected result — never an assumption that a feature exists.

// Diagnostics returns the settled, deduped diagnostics for a file (Tier 1). It
// opens/re-syncs the file, waits for a version-correlated push (§11.4), and falls
// back to a pull (textDocument/diagnostic) when the server advertises it and the
// push wait did not settle.
func (c *Client) Diagnostics(ctx context.Context, file string) ([]Diagnostic, error) {
	path := cleanPath(file)
	if _, err := c.ensureOpen(ctx, path); err != nil {
		return nil, err
	}
	pullable := c.supports(methodPullDiagnostic)
	// A pull-capable server already observed to never version its pushes is treated
	// as pull-only: pull synchronously instead of blocking out the freshness ceiling
	// on every call (§11.4 — "pull sidesteps the question entirely"). gopls is
	// unaffected because it pushes versioned diagnostics, so it is never marked.
	if pullable && c.diag.pullOnly() {
		if pulled, err := c.pullDiagnostics(ctx, path); err == nil {
			return pulled, nil
		}
		// A pull failure falls through to the freshness wait below.
	}
	diags, settled := c.diag.wait(path)
	if settled || !pullable {
		return diags, nil
	}
	// The ceiling fired and the server advertises pull: it produced no versioned
	// push, so remember it as pull-only (future calls skip the ceiling) and pull now.
	c.diag.markPullOnly()
	pulled, err := c.pullDiagnostics(ctx, path)
	if err != nil {
		// A pull failure falls back to whatever the push wait returned.
		return diags, nil //nolint:nilerr // pull is a best-effort fallback to push
	}
	return pulled, nil
}

// pullDiagnostics issues a synchronous textDocument/diagnostic pull (§11.4) and
// returns the full report's items.
func (c *Client) pullDiagnostics(ctx context.Context, path string) ([]Diagnostic, error) {
	report, err := c.server.Diagnostic(ctx, &protocol.DocumentDiagnosticParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: pathToURI(path)},
	})
	if err != nil {
		return nil, fmt.Errorf("diagnostic pull: %w", err)
	}
	full, ok := report.(*protocol.RelatedFullDocumentDiagnosticReport)
	if !ok || full == nil {
		// An unchanged report carries no new items; the push cache stands.
		return c.diag.current(path), nil
	}
	out := make([]Diagnostic, 0, len(full.Items))
	for _, d := range full.Items {
		out = append(out, diagFromWire(c.lineText, path, d))
	}
	return dedupDiagnostics(out), nil
}

// Definition resolves the "go to" family member selected by kind (Tier 2).
func (c *Client) Definition(ctx context.Context, file string, pos Position, kind DefKind) ([]Location, error) {
	path := cleanPath(file)
	if _, err := c.ensureOpen(ctx, path); err != nil {
		return nil, err
	}
	res, err := c.definitionByKind(ctx, path, pos, kind)
	if err != nil {
		return nil, err
	}
	return locationsFromDefinition(c.lineText, res), nil
}

// definitionByKind dispatches to the right request for kind, gating on the
// corresponding capability.
func (c *Client) definitionByKind(ctx context.Context, path string, pos Position, kind DefKind) (any, error) {
	params := func() protocol.TextDocumentPositionParams { return c.posParams(path, pos) }
	var (
		res any
		err error
		op  string
	)
	switch kind {
	case DefDeclaration:
		if !c.supports(methodDeclaration) {
			return nil, ErrUnsupported
		}
		op = "declaration"
		res, err = c.server.Declaration(ctx, &protocol.DeclarationParams{TextDocumentPositionParams: params()})
	case DefTypeDefinition:
		if !c.supports(methodTypeDefinition) {
			return nil, ErrUnsupported
		}
		op = "type definition"
		res, err = c.server.TypeDefinition(ctx, &protocol.TypeDefinitionParams{TextDocumentPositionParams: params()})
	case DefImplementation:
		if !c.supports(methodImplementation) {
			return nil, ErrUnsupported
		}
		op = "implementation"
		res, err = c.server.Implementation(ctx, &protocol.ImplementationParams{TextDocumentPositionParams: params()})
	default:
		if !c.supports(methodDefinition) {
			return nil, ErrUnsupported
		}
		op = "definition"
		res, err = c.server.Definition(ctx, &protocol.DefinitionParams{TextDocumentPositionParams: params()})
	}
	if err != nil {
		return nil, fmt.Errorf("%s request: %w", op, err)
	}
	return res, nil
}

// References returns every reference to the symbol at pos (Tier 2).
func (c *Client) References(ctx context.Context, file string, pos Position, inclDecl bool) ([]Location, error) {
	path := cleanPath(file)
	if _, err := c.ensureOpen(ctx, path); err != nil {
		return nil, err
	}
	if !c.supports(methodReferences) {
		return nil, ErrUnsupported
	}
	locs, err := c.server.References(ctx, &protocol.ReferenceParams{
		TextDocumentPositionParams: c.posParams(path, pos),
		Context:                    protocol.ReferenceContext{IncludeDeclaration: inclDecl},
	})
	if err != nil {
		return nil, fmt.Errorf("references: %w", err)
	}
	out := make([]Location, 0, len(locs))
	for _, l := range locs {
		out = append(out, fromWireLocation(c.lineText, l))
	}
	return out, nil
}

// Hover returns the type/documentation summary at pos (Tier 2).
func (c *Client) Hover(ctx context.Context, file string, pos Position) (Hover, error) {
	path := cleanPath(file)
	if _, err := c.ensureOpen(ctx, path); err != nil {
		return Hover{}, err
	}
	if !c.supports(methodHover) {
		return Hover{}, ErrUnsupported
	}
	h, err := c.server.Hover(ctx, &protocol.HoverParams{TextDocumentPositionParams: c.posParams(path, pos)})
	if err != nil {
		return Hover{}, fmt.Errorf("hover: %w", err)
	}
	out := Hover{Contents: hoverString(h)}
	if h != nil && h.Range != nil {
		r := fromWireRange(c.lineText, path, *h.Range)
		out.Range = &r
	}
	return out, nil
}

// DocumentSymbols returns the symbol tree of a file (Tier 2), normalized across
// the hierarchical and flat response shapes (§7.2).
func (c *Client) DocumentSymbols(ctx context.Context, file string) ([]Symbol, error) {
	path := cleanPath(file)
	if _, err := c.ensureOpen(ctx, path); err != nil {
		return nil, err
	}
	if !c.supports(methodDocumentSymbol) {
		return nil, ErrUnsupported
	}
	res, err := c.server.DocumentSymbol(ctx, &protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: pathToURI(path)},
	})
	if err != nil {
		return nil, fmt.Errorf("document symbols: %w", err)
	}
	return symbolsFromResult(c.lineText, path, res), nil
}

// WorkspaceSymbols returns symbols across the workspace matching query (Tier 2).
func (c *Client) WorkspaceSymbols(ctx context.Context, query string) ([]Symbol, error) {
	if !c.supports(methodWorkspaceSymbol) {
		return nil, ErrUnsupported
	}
	res, err := c.server.Symbols(ctx, &protocol.WorkspaceSymbolParams{Query: query})
	if err != nil {
		return nil, fmt.Errorf("workspace symbols: %w", err)
	}
	return workspaceSymbols(c.lineText, res), nil
}

// CallHierarchy returns the incoming or outgoing calls of the symbol at pos
// (Tier 2). It prepares the hierarchy item, then resolves one direction.
func (c *Client) CallHierarchy(ctx context.Context, file string, pos Position, dir Direction) ([]CallItem, error) {
	path := cleanPath(file)
	if _, err := c.ensureOpen(ctx, path); err != nil {
		return nil, err
	}
	if !c.supports(methodCallHierarchy) {
		return nil, ErrUnsupported
	}
	items, err := c.server.PrepareCallHierarchy(ctx, &protocol.CallHierarchyPrepareParams{
		TextDocumentPositionParams: c.posParams(path, pos),
	})
	if err != nil {
		return nil, fmt.Errorf("prepare call hierarchy: %w", err)
	}
	if len(items) == 0 {
		return nil, nil
	}
	item := items[0]
	var out []CallItem
	if dir == Outgoing {
		calls, err := c.server.OutgoingCalls(ctx, &protocol.CallHierarchyOutgoingCallsParams{Item: item})
		if err != nil {
			return nil, fmt.Errorf("outgoing calls: %w", err)
		}
		for _, call := range calls {
			out = append(out, callItemFromWire(c.lineText, call.To))
		}
		return out, nil
	}
	calls, err := c.server.IncomingCalls(ctx, &protocol.CallHierarchyIncomingCallsParams{Item: item})
	if err != nil {
		return nil, fmt.Errorf("incoming calls: %w", err)
	}
	for _, call := range calls {
		out = append(out, callItemFromWire(c.lineText, call.From))
	}
	return out, nil
}

// callItemFromWire converts a CallHierarchyItem to the boundary CallItem.
func callItemFromWire(getLine lineProvider, item protocol.CallHierarchyItem) CallItem {
	path := uriToPath(item.URI)
	detail := ""
	if item.Detail != nil {
		detail = *item.Detail
	}
	return CallItem{
		Name:     item.Name,
		Kind:     symbolKindName(item.Kind),
		Detail:   detail,
		Location: Location{Path: path, Range: fromWireRange(getLine, path, item.Range)},
	}
}

// Rename returns the proposed WorkspaceEdit that renames the symbol at pos to
// newName (Tier 3, preview). It validates the position via prepareRename when the
// server advertises prepareSupport (§7.2).
func (c *Client) Rename(ctx context.Context, file string, pos Position, newName string) (WorkspaceEdit, error) {
	path := cleanPath(file)
	if _, err := c.ensureOpen(ctx, path); err != nil {
		return WorkspaceEdit{}, err
	}
	if !c.supports(methodRename) {
		return WorkspaceEdit{}, ErrUnsupported
	}
	c.mu.Lock()
	prepare := c.caps.renamePrepare
	c.mu.Unlock()
	if prepare {
		if _, err := c.server.PrepareRename(ctx, &protocol.PrepareRenameParams{
			TextDocumentPositionParams: c.posParams(path, pos),
		}); err != nil {
			return WorkspaceEdit{}, fmt.Errorf("prepare rename: %w", err)
		}
	}
	edit, err := c.server.Rename(ctx, &protocol.RenameParams{
		TextDocumentPositionParams: c.posParams(path, pos),
		NewName:                    newName,
	})
	if err != nil {
		return WorkspaceEdit{}, fmt.Errorf("rename: %w", err)
	}
	return workspaceEditFromWire(c.lineText, edit), nil
}

// Format returns the proposed formatting edit for a file (Tier 3, preview).
func (c *Client) Format(ctx context.Context, file string) (WorkspaceEdit, error) {
	path := cleanPath(file)
	if _, err := c.ensureOpen(ctx, path); err != nil {
		return WorkspaceEdit{}, err
	}
	if !c.supports(methodFormatting) {
		return WorkspaceEdit{}, ErrUnsupported
	}
	// FormattingOptions are advisory: a server that has its own canonical style
	// (gopls, rustfmt, ...) ignores them and formats to the language's conventions.
	// The defaults below (tabs, width 4) match Go; a space-indented language served
	// by a formatter that honors these would want different values, which is a future
	// per-server config knob rather than a hardcode to revisit here.
	edits, err := c.server.Formatting(ctx, &protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: pathToURI(path)},
		Options:      protocol.FormattingOptions{TabSize: 4, InsertSpaces: false},
	})
	if err != nil {
		return WorkspaceEdit{}, fmt.Errorf("formatting: %w", err)
	}
	out := WorkspaceEdit{Changes: map[string][]TextEdit{}}
	for _, te := range edits {
		out.Changes[path] = append(out.Changes[path], TextEdit{
			Range:   fromWireRange(c.lineText, path, te.Range),
			NewText: te.NewText,
		})
	}
	return out, nil
}

// CodeActions returns the available fixes/refactors for a range (Tier 3,
// preview). Actions a server returned lazily (no edit, only data) are resolved via
// codeAction/resolve so the preview shows a real WorkspaceEdit; actions that carry
// only a command become executeCommand candidates (§12).
func (c *Client) CodeActions(ctx context.Context, file string, rng Range) ([]CodeAction, error) {
	path := cleanPath(file)
	if _, err := c.ensureOpen(ctx, path); err != nil {
		return nil, err
	}
	if !c.supports(methodCodeAction) {
		return nil, ErrUnsupported
	}
	c.mu.Lock()
	canResolve := c.caps.codeActionResolve
	c.mu.Unlock()
	wireRange := toWireRange(c.lineText, path, rng)
	// Carry the cached diagnostics that intersect the requested range in the request
	// context: diagnostic-bound quick fixes (gopls add-missing-import, quickfix, …)
	// are computed by the server from CodeActionContext.Diagnostics, so an empty
	// context omits exactly the highest-value "fix this error" actions (§12). The wire
	// diagnostics retain their Data payload, which gopls preserves to match the fix.
	ctxDiags := c.diag.rawIntersecting(path, wireRange)
	if ctxDiags == nil {
		ctxDiags = []protocol.Diagnostic{}
	}
	results, err := c.server.CodeAction(ctx, &protocol.CodeActionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: pathToURI(path)},
		Range:        wireRange,
		Context:      protocol.CodeActionContext{Diagnostics: ctxDiags},
	})
	if err != nil {
		return nil, fmt.Errorf("code action: %w", err)
	}
	out := make([]CodeAction, 0, len(results))
	for _, r := range results {
		switch v := r.(type) {
		case *protocol.Command:
			if v != nil {
				out = append(out, CodeAction{Title: v.Title, Command: v.Command})
			}
		case *protocol.CodeAction:
			if v != nil {
				out = append(out, c.codeActionFromWire(ctx, canResolve, *v))
			}
		}
	}
	return out, nil
}

// codeActionFromWire converts a wire CodeAction, resolving a lazy edit via
// codeAction/resolve when the action arrived with only a data payload (§12).
func (c *Client) codeActionFromWire(ctx context.Context, canResolve bool, a protocol.CodeAction) CodeAction {
	if a.Edit == nil && len(a.Data) > 0 && canResolve {
		if resolved, err := c.server.CodeActionResolve(ctx, &a); err == nil && resolved != nil {
			a = *resolved
		}
	}
	out := CodeAction{Title: a.Title}
	if a.Kind != nil {
		out.Kind = string(*a.Kind)
	}
	if a.IsPreferred != nil {
		out.Preferred = *a.IsPreferred
	}
	if a.Edit != nil {
		e := workspaceEditFromWire(c.lineText, a.Edit)
		out.Edit = &e
	}
	if a.Command.Command != "" {
		out.Command = a.Command.Command
	}
	return out
}

// ExecuteCommand asks the server to run a workspace command (the higher-risk
// Tier 3 action, §12). The command must be on the server's AllowedCommands
// allow-list (defense in depth alongside the tool's ActionLSPCommand gate); an
// off-list command returns ErrCommandNotAllowed and nothing runs. The raw result
// is returned for the tool to surface.
func (c *Client) ExecuteCommand(ctx context.Context, command string, args []any) (any, error) {
	if !c.cfg.commandAllowed(command) {
		return nil, ErrCommandNotAllowed
	}
	if !c.supports(methodExecuteCommand) {
		return nil, ErrUnsupported
	}
	lspArgs := make([]protocol.LSPAny, 0, len(args))
	for _, a := range args {
		lspArgs = append(lspArgs, marshalLSPAny(a, true))
	}
	res, err := c.server.ExecuteCommand(ctx, &protocol.ExecuteCommandParams{
		Command:   command,
		Arguments: lspArgs,
	})
	if err != nil {
		return nil, fmt.Errorf("execute command: %w", err)
	}
	return rawJSONString(res), nil
}

// rawJSONString renders an LSPAny result as a string for tool output, or "" when
// it is empty/null.
func rawJSONString(v protocol.LSPAny) string {
	s := string(v)
	if s == "" || s == "null" {
		return ""
	}
	return s
}
