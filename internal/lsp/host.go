package lsp

// Host is the seam through which the generic Client reaches the host application
// for the few server→client callbacks that touch app state. It keeps internal/lsp
// free of any gogent dependency (the LSP support design §8): gogent implements
// Host, the Client only knows this interface.
//
// Every method must be safe to call from the single jsonrpc2 read goroutine and
// must not block indefinitely; a nil Host is tolerated by the Client (callbacks
// degrade to sane headless defaults).
type Host interface {
	// ApplyEdit applies a server-driven workspace edit (from workspace/applyEdit
	// or a Tier 3 tool) through gogent's write/edit permission and Checkpointer
	// (undo) machinery. It snapshots every path a documentChanges entry touches —
	// including both source and target of a rename — before performing any op, so
	// the edit round-trips under undo (§12). It returns whether the edit applied
	// and, on failure, a human-readable reason; a denied or stale edit returns
	// (false, reason, nil). A non-nil error is reserved for unexpected internal
	// failure.
	ApplyEdit(server string, edit WorkspaceEdit) (applied bool, failureReason string, err error)

	// Configuration answers a workspace/configuration pull for one requested
	// section (dotted path into the server's Settings), scoped to scopeURI when
	// the server provided one. A missing section returns (nil, false) so the
	// Client can answer null for that item.
	Configuration(server, section, scopeURI string) (value any, ok bool)

	// Logf records a server-originated log/show message (window/logMessage,
	// window/showMessage, telemetry, …) to gogent's log stream. No prompt.
	Logf(server, format string, args ...any)
}
