// Package lsp implements a single, language-agnostic Language Server Protocol
// client. It speaks LSP over a stdio subprocess (Content-Length framing,
// JSON-RPC 2.0, server→client callbacks) using go.lsp.dev/jsonrpc2 +
// go.lsp.dev/protocol, and exposes a small, curated, capability-gated set of
// operations an agent benefits from: per-file diagnostics, navigation, and
// preview-then-apply mutations.
//
// There is deliberately NO per-language Go code: everything a server needs is
// data in a ServerConfig (command, extensions, root markers, languageId, …).
// One generic Client serves gopls, rust-analyzer, pyright, … and every operation
// is gated on the server's negotiated capabilities, so "unsupported by this
// server" is a clean, expected result (ErrUnsupported) rather than an assumption.
//
// The package depends only on the two LSP libraries and the standard library; it
// holds no dependency on gogent's application packages. The host application
// supplies a Host for the few server→client callbacks that touch app state
// (workspace configuration, applyEdit, logging).
package lsp

import (
	"errors"
)

// ServerConfig is the complete, declarative description of one language server.
// It is the only place per-language knowledge lives (the LSP support design §7).
// It is a gogent-owned value with no dependency on the config package; the host
// maps its configuration onto this shape, mirroring the mcp package.
type ServerConfig struct {
	// Name is the server/process identity. Clients are cached by this name, so a
	// single process can serve several languageIds (Languages, below).
	Name string
	// LanguageID is the default LSP languageId sent in didOpen ("go", "rust", …).
	LanguageID string
	// Languages optionally overrides LanguageID per file extension (leading dot
	// included), e.g. ".tsx" → "typescriptreact", so one process can serve several
	// languageIds without spawning duplicate processes.
	Languages map[string]string
	// Extensions is the routing key (leading dot included): files with these
	// extensions are served by this server.
	Extensions []string
	// Command/Args/Env launch the stdio server subprocess.
	Command string
	Args    []string
	Env     map[string]string
	// RootMarkers name files that mark a project root, searched for by walking up
	// from the file (e.g. ["go.work", "go.mod"]). Empty falls back to the gogent
	// workspace root.
	RootMarkers []string
	// InitOptions feeds the initialize request's initializationOptions.
	InitOptions map[string]any
	// Settings answers workspace/configuration pulls and seeds
	// workspace/didChangeConfiguration.
	Settings map[string]any
	// AllowedCommands scopes the higher-risk workspace/executeCommand action; an
	// empty list means no command may run.
	AllowedCommands []string
}

// languageIDFor resolves the wire languageId for a file: the per-extension
// override in Languages if present, otherwise LanguageID.
func (c ServerConfig) languageIDFor(ext string) string {
	if id, ok := c.Languages[ext]; ok && id != "" {
		return id
	}
	return c.LanguageID
}

// commandAllowed reports whether command is on the executeCommand allow-list.
func (c ServerConfig) commandAllowed(command string) bool {
	for _, a := range c.AllowedCommands {
		if a == command {
			return true
		}
	}
	return false
}

var (
	// ErrUnsupported is returned by a curated operation when the server does not
	// advertise the capability for the file (the LSP support design §7.2). It is a
	// clean, expected result — the tool reports "not supported by this server for
	// this file" and the agent moves on, never a fatal error.
	ErrUnsupported = errors.New("operation not supported by this server")
	// ErrNoServer is returned by the Manager when no configured server matches a
	// file's extension. Like a declined MCP server, it is non-fatal.
	ErrNoServer = errors.New("no LSP server configured for this file")
	// ErrCommandNotAllowed is returned when a workspace/executeCommand is requested
	// for a command absent from the server's AllowedCommands allow-list (§12).
	ErrCommandNotAllowed = errors.New("command not in the server's allow-list")
)

// DefKind selects which "go to" family member a Definition call resolves.
type DefKind string

const (
	DefDefinition     DefKind = "definition"
	DefDeclaration    DefKind = "declaration"
	DefTypeDefinition DefKind = "type"
	DefImplementation DefKind = "implementation"
)

// Direction selects the call-hierarchy traversal direction.
type Direction string

const (
	Incoming Direction = "incoming"
	Outgoing Direction = "outgoing"
)

// Position is a location in a text document. At the tool boundary it is 1-based
// (line and character); on the wire it is 0-based with UTF-16 character offsets.
// The edge layer (convert.go) owns the rune→UTF-16 conversion (§11.3).
type Position struct {
	Line      int `json:"line"`
	Character int `json:"column"`
}

// Range is a half-open span between two positions.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is a range within a file, identified by path (not URI).
type Location struct {
	Path  string `json:"path"`
	Range Range  `json:"range"`
}

// Diagnostic is a single finding for a file.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity"` // 1=error 2=warning 3=info 4=hint
	Code     string `json:"code,omitempty"`
	Source   string `json:"source,omitempty"`
	Message  string `json:"message"`
}

// Hover is the type/documentation summary for a position.
type Hover struct {
	Contents string `json:"contents"`
	Range    *Range `json:"range,omitempty"`
}

// Symbol is one node of a document/workspace symbol tree.
type Symbol struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	Detail   string   `json:"detail,omitempty"`
	Location Location `json:"location"`
	Children []Symbol `json:"children,omitempty"`
}

// CallItem is one node of a call-hierarchy result.
type CallItem struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	Detail   string   `json:"detail,omitempty"`
	Location Location `json:"location"`
}

// TextEdit is a single textual change to a file (positions at the tool edge).
type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"new_text"`
}

// WorkspaceEdit is a proposed set of changes, keyed by file path. ResourceOps
// carries create/rename/delete operations from documentChanges so a preview and
// an apply can reproduce them.
type WorkspaceEdit struct {
	Changes     map[string][]TextEdit `json:"changes,omitempty"`
	ResourceOps []ResourceOp          `json:"resource_ops,omitempty"`
}

// ResourceOp is a create/rename/delete file operation within a WorkspaceEdit.
type ResourceOp struct {
	Kind    string `json:"kind"` // "create" | "rename" | "delete"
	Path    string `json:"path"`
	NewPath string `json:"new_path,omitempty"` // for rename
}

// CodeAction is a proposed fix/refactor. Edit is the materialized WorkspaceEdit
// (resolved via codeAction/resolve when the server returned it lazily). Command,
// when set, names a workspace/executeCommand candidate gated by AllowedCommands.
type CodeAction struct {
	Title     string         `json:"title"`
	Kind      string         `json:"kind,omitempty"`
	Edit      *WorkspaceEdit `json:"edit,omitempty"`
	Command   string         `json:"command,omitempty"`
	Preferred bool           `json:"preferred,omitempty"`
}
