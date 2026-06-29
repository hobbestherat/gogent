package lsp

import (
	"encoding/json"
	"path"
	"strings"

	"go.lsp.dev/protocol"
)

// LSP method names for the operations the curated ops issue. They are the keys
// dynamic registration arrives under and the values the capability table gates.
const (
	methodDefinition      = "textDocument/definition"
	methodDeclaration     = "textDocument/declaration"
	methodTypeDefinition  = "textDocument/typeDefinition"
	methodImplementation  = "textDocument/implementation"
	methodReferences      = "textDocument/references"
	methodHover           = "textDocument/hover"
	methodDocumentSymbol  = "textDocument/documentSymbol"
	methodWorkspaceSymbol = "workspace/symbol"
	methodCallHierarchy   = "textDocument/prepareCallHierarchy"
	methodRename          = "textDocument/rename"
	methodCodeAction      = "textDocument/codeAction"
	methodFormatting      = "textDocument/formatting"
	methodPullDiagnostic  = "textDocument/diagnostic"
	methodExecuteCommand  = "workspace/executeCommand"
	methodWatchedFiles    = "workspace/didChangeWatchedFiles"
)

// methodProviderKey maps each gated method to the JSON key its support is
// announced under in the initialize result's ServerCapabilities. Dynamic
// registration (client/registerCapability) overrides this by method name.
var methodProviderKey = map[string]string{
	methodDefinition:      "definitionProvider",
	methodDeclaration:     "declarationProvider",
	methodTypeDefinition:  "typeDefinitionProvider",
	methodImplementation:  "implementationProvider",
	methodReferences:      "referencesProvider",
	methodHover:           "hoverProvider",
	methodDocumentSymbol:  "documentSymbolProvider",
	methodWorkspaceSymbol: "workspaceSymbolProvider",
	methodCallHierarchy:   "callHierarchyProvider",
	methodRename:          "renameProvider",
	methodCodeAction:      "codeActionProvider",
	methodFormatting:      "documentFormattingProvider",
	methodPullDiagnostic:  "diagnosticProvider",
	methodExecuteCommand:  "executeCommandProvider",
}

// capabilities is the live capability table (the LSP support design §7.2). It is
// built from the initialize result and kept current by register/unregister
// notifications. It also records the file-watcher globs a server registers so the
// Client can emit matching didChangeWatchedFiles (§11.5). It is not safe for
// concurrent use on its own; the Client mutex guards it.
type capabilities struct {
	// initial holds, per provider key, whether the initialize result advertised it
	// truthily (true bool or an options object).
	initial map[string]bool
	// dynamic holds, per method, whether client/registerCapability registered it.
	dynamic map[string]bool
	// codeActionResolve reports whether the server resolves lazy code actions via
	// codeAction/resolve (codeActionProvider.resolveProvider).
	codeActionResolve bool
	// renamePrepare reports whether the server validates renames via
	// textDocument/prepareRename (renameProvider.prepareProvider).
	renamePrepare bool
	// saveIncludeText reports whether the server asked for document text on save.
	saveIncludeText bool
	// watcherGlobs are the glob patterns the server registered interest in for
	// workspace/didChangeWatchedFiles.
	watcherGlobs []string
}

func newCapabilities() *capabilities {
	return &capabilities{initial: map[string]bool{}, dynamic: map[string]bool{}}
}

// applyInitializeResult records the provider keys the server advertised. The
// ServerCapabilities is marshaled to generic JSON and each provider key tested
// for truthiness, which uniformly handles the bool-or-options union arms without
// per-field code.
func (c *capabilities) applyInitializeResult(res *protocol.InitializeResult) {
	if res == nil {
		return
	}
	data, err := protocol.Marshal(res.Capabilities)
	if err != nil {
		return
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	for _, key := range methodProviderKey {
		c.initial[key] = truthyProvider(m[key])
	}
	c.codeActionResolve = providerSubFlag(m["codeActionProvider"], "resolveProvider")
	c.renamePrepare = providerSubFlag(m["renameProvider"], "prepareProvider")
	c.saveIncludeText = textSyncIncludesText(m["textDocumentSync"])
}

// truthyProvider reports whether a provider value advertises support: a JSON
// `true`, or any object (the options form). `false`, `null` and absence are not.
func truthyProvider(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	switch raw[0] {
	case 't': // true
		return true
	case '{': // an options object
		return true
	default: // false, null, or anything else
		return false
	}
}

// providerSubFlag reports whether an options-form provider sets a boolean
// sub-flag (e.g. resolveProvider, prepareProvider) to true.
func providerSubFlag(raw json.RawMessage, field string) bool {
	if len(raw) == 0 || raw[0] != '{' {
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	v, ok := obj[field]
	return ok && len(v) > 0 && v[0] == 't'
}

// textSyncIncludesText reports whether the server's textDocumentSync requested
// document text on save (save.includeText), so didSave carries it.
func textSyncIncludesText(raw json.RawMessage) bool {
	if len(raw) == 0 || raw[0] != '{' {
		return false
	}
	var obj struct {
		Save json.RawMessage `json:"save"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	if len(obj.Save) == 0 || obj.Save[0] != '{' {
		return false
	}
	var save struct {
		IncludeText bool `json:"includeText"`
	}
	_ = json.Unmarshal(obj.Save, &save)
	return save.IncludeText
}

// supports reports whether the server advertises method, consulting dynamic
// registration first, then the initialize result.
func (c *capabilities) supports(method string) bool {
	if c.dynamic[method] {
		return true
	}
	if key, ok := methodProviderKey[method]; ok {
		return c.initial[key]
	}
	return false
}

// register records a client/registerCapability for method. For
// workspace/didChangeWatchedFiles it also extracts and records the watcher globs
// (so the Client can emit matching notifications, §11.5) rather than merely
// flipping a bit.
func (c *capabilities) register(method string, opts protocol.LSPAny) {
	c.dynamic[method] = true
	if method == methodWatchedFiles {
		c.watcherGlobs = append(c.watcherGlobs, parseWatcherGlobs(opts)...)
	}
}

// unregister clears a dynamic registration. A watched-files unregistration drops
// its globs.
func (c *capabilities) unregister(method string) {
	delete(c.dynamic, method)
	if method == methodWatchedFiles {
		c.watcherGlobs = nil
	}
}

// parseWatcherGlobs extracts the glob patterns from a didChangeWatchedFiles
// registration's options. Both the plain-string and relative-pattern glob forms
// are reduced to their pattern string; relative-pattern base URIs are ignored
// (the Client matches against absolute paths, see watch.go).
func parseWatcherGlobs(opts protocol.LSPAny) []string {
	if len(opts) == 0 {
		return nil
	}
	var reg struct {
		Watchers []struct {
			GlobPattern json.RawMessage `json:"globPattern"`
		} `json:"watchers"`
	}
	if err := json.Unmarshal(opts, &reg); err != nil {
		return nil
	}
	var globs []string
	for _, w := range reg.Watchers {
		raw := w.GlobPattern
		if len(raw) == 0 {
			continue
		}
		switch raw[0] {
		case '"':
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" {
				globs = append(globs, s)
			}
		case '{':
			var rp struct {
				Pattern string `json:"pattern"`
			}
			if json.Unmarshal(raw, &rp) == nil && rp.Pattern != "" {
				globs = append(globs, rp.Pattern)
			}
		}
	}
	return globs
}

// matchesWatcher reports whether absPath matches any registered watcher glob.
// LSP globs use path.Match semantics on the basename plus a coarse "**" handling:
// a "**" segment matches any number of path segments, so the common
// "**/*.go"-style patterns match by suffix. Matching is deliberately permissive
// — a false positive only sends a harmless extra notification.
func (c *capabilities) matchesWatcher(absPath string) bool {
	base := path.Base(absPath)
	for _, g := range c.watcherGlobs {
		if globMatch(g, absPath, base) {
			return true
		}
	}
	return false
}

// globMatch implements the coarse glob match watch-file matching needs.
func globMatch(glob, absPath, base string) bool {
	// Strip a leading "**/" so "**/*.go" reduces to matching the basename.
	if strings.HasPrefix(glob, "**/") {
		tail := strings.TrimPrefix(glob, "**/")
		if !strings.Contains(tail, "/") {
			if ok, _ := path.Match(tail, base); ok {
				return true
			}
		}
	}
	if ok, _ := path.Match(glob, absPath); ok {
		return true
	}
	if ok, _ := path.Match(glob, base); ok {
		return true
	}
	return false
}
