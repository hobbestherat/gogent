package lsp

import (
	"context"
	"encoding/json"
	"fmt"

	"go.lsp.dev/protocol"
)

// clientHandler implements protocol.Client: the server→client request and
// notification surface (the LSP support design §10). It handles exactly what real
// servers need to function — capability registration, configuration pulls,
// diagnostics, applyEdit, workspace folders, progress — and degrades everything
// else to sane headless defaults (log; no prompts; success:false for showDocument;
// null for showMessageRequest). All inbound traffic is dispatched on the single
// jsonrpc2 read goroutine, so these methods are the only writers of some shared
// state and take the Client mutex where they touch tables.
type clientHandler struct {
	c *Client
}

// RegisterCapability maintains the live capability table and records watcher
// globs for didChangeWatchedFiles registrations (§7.2, §11.5).
func (h *clientHandler) RegisterCapability(_ context.Context, params *protocol.RegistrationParams) error {
	h.c.mu.Lock()
	for _, r := range params.Registrations {
		h.c.caps.register(r.Method, r.RegisterOptions)
	}
	h.c.mu.Unlock()
	return nil
}

// UnregisterCapability removes dynamic registrations from the capability table.
func (h *clientHandler) UnregisterCapability(_ context.Context, params *protocol.UnregistrationParams) error {
	h.c.mu.Lock()
	for _, r := range params.Unregisterations {
		h.c.caps.unregister(r.Method)
	}
	h.c.mu.Unlock()
	return nil
}

// PublishDiagnostics caches a push set, converting to the boundary type and
// feeding the version-keyed freshness machinery (§11.4).
func (h *clientHandler) PublishDiagnostics(_ context.Context, params *protocol.PublishDiagnosticsParams) error {
	path := uriToPath(params.URI)
	diags := make([]Diagnostic, 0, len(params.Diagnostics))
	for _, d := range params.Diagnostics {
		diags = append(diags, diagFromWire(h.c.lineText, path, d))
	}
	version, hasVersion := params.Version.Get()
	h.c.diag.publish(path, version, hasVersion, diags, params.Diagnostics)
	return nil
}

// Configuration answers a workspace/configuration pull from the server's Settings,
// scope-aware per requested section/scopeUri (§10). An unknown section answers
// null for that item rather than erroring.
func (h *clientHandler) Configuration(_ context.Context, params *protocol.ConfigurationParams) ([]protocol.LSPAny, error) {
	out := make([]protocol.LSPAny, 0, len(params.Items))
	for _, item := range params.Items {
		section := ""
		if item.Section != nil {
			section = *item.Section
		}
		scope := ""
		if item.ScopeURI != nil {
			scope = uriToPath(*item.ScopeURI)
		}
		var value any
		ok := false
		if h.c.host != nil {
			value, ok = h.c.host.Configuration(h.c.cfg.Name, section, scope)
		}
		out = append(out, marshalLSPAny(value, ok))
	}
	return out, nil
}

// WorkspaceFolders returns the Manager's known root for this client.
func (h *clientHandler) WorkspaceFolders(context.Context) ([]protocol.WorkspaceFolder, error) {
	return []protocol.WorkspaceFolder{{URI: h.c.rootURI, Name: h.c.cfg.Name}}, nil
}

// ApplyEdit routes a server-driven workspace edit through the Host (ActionWrite +
// Checkpointer); a denied or stale edit returns applied:false (§10, §12).
func (h *clientHandler) ApplyEdit(_ context.Context, params *protocol.ApplyWorkspaceEditParams) (*protocol.ApplyWorkspaceEditResult, error) {
	edit := workspaceEditFromWire(h.c.lineText, &params.Edit)
	if h.c.host == nil {
		reason := "no edit host configured"
		return &protocol.ApplyWorkspaceEditResult{Applied: false, FailureReason: &reason}, nil
	}
	applied, reason, err := h.c.host.ApplyEdit(h.c.cfg.Name, edit)
	if err != nil {
		msg := err.Error()
		return &protocol.ApplyWorkspaceEditResult{Applied: false, FailureReason: &msg}, nil
	}
	res := &protocol.ApplyWorkspaceEditResult{Applied: applied}
	if !applied && reason != "" {
		res.FailureReason = &reason
	}
	return res, nil
}

// Progress tracks work-done progress per token as the diagnostics-readiness
// fallback: a "begin" marks that token's stream in flight, an "end" clears it; the
// server is idle only when no stream is outstanding (§11.4, fallback 2). Tracking
// by token means one stream's "end" cannot mark the server idle while another
// concurrent stream is still running.
func (h *clientHandler) Progress(_ context.Context, params *protocol.ProgressParams) error {
	var v struct {
		Kind string `json:"kind"`
	}
	if len(params.Value) > 0 {
		_ = json.Unmarshal(params.Value, &v)
	}
	token := progressTokenKey(params.Token)
	switch v.Kind {
	case "begin":
		h.c.diag.progressBegin(token)
	case "end":
		h.c.diag.progressEnd(token)
	}
	return nil
}

// WorkDoneProgressCreate reserves a progress token; the work that token will report
// is starting, so the stream is marked in flight (idempotent with its later
// "begin").
func (h *clientHandler) WorkDoneProgressCreate(_ context.Context, params *protocol.WorkDoneProgressCreateParams) error {
	h.c.diag.progressBegin(progressTokenKey(params.Token))
	return nil
}

// progressTokenKey renders an LSP progress token (an int or a string on the wire)
// as a stable map key.
func progressTokenKey(token protocol.ProgressToken) string {
	return fmt.Sprintf("%v", token)
}

// DiagnosticRefresh invalidates the cached pull diagnostics so the next read
// re-pulls/re-waits (§10).
func (h *clientHandler) DiagnosticRefresh(context.Context) error {
	h.c.diag.invalidateAll()
	return nil
}

// The remaining inbound messages are logged or answered with sane headless
// defaults; gogent has no editor UI to surface them.

func (h *clientHandler) ShowMessage(_ context.Context, params *protocol.ShowMessageParams) error {
	h.c.logf("server message: %s", params.Message)
	return nil
}

func (h *clientHandler) LogMessage(_ context.Context, params *protocol.LogMessageParams) error {
	h.c.logf("server log: %s", params.Message)
	return nil
}

func (h *clientHandler) LogTrace(_ context.Context, params *protocol.LogTraceParams) error {
	h.c.logf("server trace: %s", params.Message)
	return nil
}

func (h *clientHandler) Telemetry(context.Context, protocol.LSPAny) error { return nil }

// ShowMessageRequest logs and returns null (no selection): we deliberately do not
// pick a default action, which could trigger a command/mutation as an un-gated
// decision for a headless agent (§10).
func (h *clientHandler) ShowMessageRequest(_ context.Context, params *protocol.ShowMessageRequestParams) (*protocol.MessageActionItem, error) {
	h.c.logf("server message (request): %s", params.Message)
	return nil, nil //nolint:nilnil // null is the deliberate "no selection" reply
}

// ShowDocument logs and reports failure: gogent is headless and cannot show a
// document in an editor.
func (h *clientHandler) ShowDocument(_ context.Context, params *protocol.ShowDocumentParams) (*protocol.ShowDocumentResult, error) {
	h.c.logf("server requested showDocument: %s", uriToPath(params.URI))
	return &protocol.ShowDocumentResult{Success: false}, nil
}

func (h *clientHandler) CodeLensRefresh(context.Context) error       { return nil }
func (h *clientHandler) FoldingRangeRefresh(context.Context) error   { return nil }
func (h *clientHandler) SemanticTokensRefresh(context.Context) error { return nil }
func (h *clientHandler) InlineValueRefresh(context.Context) error    { return nil }
func (h *clientHandler) InlayHintRefresh(context.Context) error      { return nil }

func (h *clientHandler) TextDocumentContentRefresh(context.Context, *protocol.TextDocumentContentRefreshParams) error {
	return nil
}

// marshalLSPAny encodes a configuration value as an LSPAny (raw JSON). A missing
// value (ok==false) or a marshal failure encodes as JSON null, the correct
// "no configuration" answer for an item.
func marshalLSPAny(value any, ok bool) protocol.LSPAny {
	if !ok {
		return protocol.LSPAny("null")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return protocol.LSPAny("null")
	}
	return protocol.LSPAny(data)
}
