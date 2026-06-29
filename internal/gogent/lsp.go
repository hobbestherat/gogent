package gogent

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"unicode/utf8"

	"gogent/internal/config"
	"gogent/internal/fileops"
	"gogent/internal/lsp"
	"gogent/internal/permission"
)

// lspSession is the synthetic checkpoint session under which server-driven
// (applyEdit) and tool-driven (rename/format apply) LSP edits are snapshotted, so
// they are undoable without clobbering a user session's in-flight turn.
const lspSession = "__lsp__"

// StartLSPServers builds the language-server Manager from configuration and
// registers the lsp_* tools (the LSP support design §8). Servers are launched
// lazily on first matching-file use — configuring five does not spawn five — and
// each first launch is permission-gated (ActionLSP) through the Manager's launch
// gate, exactly like an MCP server. A server whose command is missing from PATH
// (or that is disabled) is skipped with a warning so one misconfigured entry never
// blocks startup.
//
// It should be called once, after the permission prompter is installed (so the
// lazy ActionLSP gate can prompt rather than defaulting to deny), alongside
// StartMCPServers.
func (g *Gogent) StartLSPServers() {
	g.mu.RLock()
	cfg := g.config
	g.mu.RUnlock()
	if cfg == nil || len(cfg.LSPServers) == 0 {
		return
	}

	host := &lspHost{g: g, settings: map[string]map[string]any{}}
	var configs []lsp.ServerConfig
	for _, sc := range cfg.LSPServers {
		if sc.Disabled || strings.TrimSpace(sc.Name) == "" || strings.TrimSpace(sc.Command) == "" {
			continue
		}
		if _, err := exec.LookPath(sc.Command); err != nil {
			g.logger().Warn("lsp server skipped (command not found)", "server", sc.Name, "command", sc.Command)
			continue
		}
		configs = append(configs, lspServerConfig(sc))
		host.settings[sc.Name] = sc.Settings
	}
	if len(configs) == 0 {
		return
	}

	mgr := lsp.NewManager(g.workspaceRoot, configs, host)
	// Gate the first launch of each server (resource = server name), mirroring
	// ActionMCP. Queries against an already-running server need no further prompt.
	mgr.LaunchGate = func(sc lsp.ServerConfig) error {
		if g.permissions == nil {
			return nil
		}
		detail := "launch language server: " + strings.TrimSpace(sc.Command+" "+strings.Join(sc.Args, " "))
		return g.permissions.CheckWithDetail(permission.ActionLSP, sc.Name, detail)
	}

	g.mu.Lock()
	g.lspManager = mgr
	// Subscribe the Manager to gogent's own writes so a server's view tracks edits
	// and watched files (§11.2, §11.5). The hooks are best-effort and never block.
	if g.fileMutation != nil {
		g.fileMutation.OnMutation = func(path string, created bool) {
			kind := lsp.FileChanged
			if created {
				kind = lsp.FileCreated
			}
			mgr.FileChanged(path, kind)
		}
		g.fileMutation.OnRemove = func(path string) { mgr.FileChanged(path, lsp.FileDeleted) }
	}
	g.mu.Unlock()

	g.toolRegistry.RegisterLSPTools(mgr)
	g.refreshSessionRegistries()
	g.logger().Info("lsp servers configured", "count", len(configs))
}

// CloseLSPServers shuts down every running language server (clean shutdown → exit
// → kill) and unsubscribes the fileops hooks. It is safe to call when none are
// running, and should run alongside CloseMCPServers in the shutdown sequence.
func (g *Gogent) CloseLSPServers() {
	g.mu.Lock()
	mgr := g.lspManager
	g.lspManager = nil
	if g.fileMutation != nil {
		g.fileMutation.OnMutation = nil
		g.fileMutation.OnRemove = nil
	}
	g.mu.Unlock()
	if mgr != nil {
		mgr.Shutdown()
	}
}

// lspServerConfig maps a config.LSPServerConfig onto the transport-agnostic
// lsp.ServerConfig, keeping the lsp package free of any config dependency
// (mirroring mcpServerConfig).
func lspServerConfig(sc config.LSPServerConfig) lsp.ServerConfig {
	return lsp.ServerConfig{
		Name:            sc.Name,
		LanguageID:      sc.Language,
		Languages:       sc.Languages,
		Extensions:      sc.Extensions,
		Command:         sc.Command,
		Args:            sc.Args,
		Env:             sc.Env,
		RootMarkers:     sc.RootMarkers,
		InitOptions:     sc.InitOptions,
		Settings:        sc.Settings,
		AllowedCommands: sc.AllowedCommands,
	}
}

// lspHost implements lsp.Host: the server→client callbacks that touch gogent
// state (configuration, applyEdit, logging). It is a thin adapter so internal/lsp
// carries no gogent dependency (§8).
type lspHost struct {
	g        *Gogent
	settings map[string]map[string]any // server name -> Settings
}

// Logf records a server-originated message to gogent's log stream (§10).
func (h *lspHost) Logf(server, format string, args ...any) {
	h.g.logger().Info("lsp: "+fmt.Sprintf(format, args...), "server", server)
}

// Configuration answers a workspace/configuration pull from the server's
// configured Settings, resolving a dotted section path (§10). scopeURI is ignored
// (gogent has a single workspace scope).
func (h *lspHost) Configuration(server, section, _ string) (any, bool) {
	settings := h.settings[server]
	if settings == nil {
		return nil, false
	}
	if section == "" {
		return settings, true
	}
	var cur any = settings
	for _, part := range strings.Split(section, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// ApplyEdit applies a server/tool-driven WorkspaceEdit through gogent's write
// permission and the Checkpointer (§12). It snapshots every path a change touches
// — including BOTH source and target of a rename and the target of a
// create/delete — before performing any op, so undo round-trips. A path denied by
// the permission gate aborts the whole edit (applied:false) before anything is
// written.
func (h *lspHost) ApplyEdit(_ string, edit lsp.WorkspaceEdit) (bool, string, error) {
	g := h.g
	if g.fileMutation == nil {
		return false, "file mutation unavailable", nil
	}
	rc := permission.RequestContext{}

	// Collect every affected path and authorize it up front; a denial aborts.
	paths := edit.AffectedPaths()
	auths := map[string]fileops.Authorization{}
	for _, path := range paths {
		auth, err := fileops.CheckFileAccess(g.permissions, g.locationMutation, true, path, rc)
		if err != nil {
			return false, err.Error(), nil
		}
		auths[path] = auth
	}

	// Snapshot every affected path before any op so undo can restore content,
	// re-create a deleted file, or delete a created one — including BOTH ends of a
	// rename (§12).
	if g.checkpoints != nil {
		g.checkpoints.BeginTurn(lspSession)
		for _, path := range paths {
			g.checkpoints.Snapshot(lspSession, path, auths[path])
		}
	}

	if reason, err := h.applyChanges(edit, auths); err != nil {
		if g.checkpoints != nil {
			g.checkpoints.CommitTurn(lspSession)
		}
		return false, reason, nil
	}
	if g.checkpoints != nil {
		g.checkpoints.CommitTurn(lspSession)
	}
	return true, "", nil
}

// applyChanges performs an edit's operations in order. It honors the server's
// original documentChanges sequence (edit.Ordered) when present — so a create runs
// before the edits that populate it and a rename runs before edits to the renamed
// file — and falls back to a synthesized order (creates, then text edits, then
// renames/deletes) for a legacy `changes`-map edit. A create never truncates a file
// it was told to leave alone (§12).
func (h *lspHost) applyChanges(edit lsp.WorkspaceEdit, auths map[string]fileops.Authorization) (string, error) {
	for _, op := range orderedChanges(edit) {
		if reason, err := h.applyOne(op, auths); err != nil {
			return reason, err
		}
	}
	return "", nil
}

// orderedChanges returns the operations of edit in apply order: edit.Ordered as-is
// when the server used documentChanges, otherwise a safe synthesized order for a
// legacy `changes`-map / ResourceOps edit (creates first so a later text edit fills
// them, then text edits, then renames and deletes).
func orderedChanges(edit lsp.WorkspaceEdit) []lsp.DocumentChange {
	if len(edit.Ordered) > 0 {
		return edit.Ordered
	}
	var creates, texts, tail []lsp.DocumentChange
	for _, op := range edit.ResourceOps {
		switch op.Kind {
		case "create":
			creates = append(creates, lsp.DocumentChange{Kind: lsp.ChangeCreate, Path: op.Path})
		case "rename":
			tail = append(tail, lsp.DocumentChange{Kind: lsp.ChangeRename, Path: op.Path, NewPath: op.NewPath})
		case "delete":
			tail = append(tail, lsp.DocumentChange{Kind: lsp.ChangeDelete, Path: op.Path})
		}
	}
	for path, edits := range edit.Changes {
		texts = append(texts, lsp.DocumentChange{Kind: lsp.ChangeText, Path: path, Edits: edits})
	}
	out := make([]lsp.DocumentChange, 0, len(creates)+len(texts)+len(tail))
	out = append(out, creates...)
	out = append(out, texts...)
	out = append(out, tail...)
	return out
}

// applyOne performs a single ordered operation, returning a short failure tag and
// the error on failure.
func (h *lspHost) applyOne(op lsp.DocumentChange, auths map[string]fileops.Authorization) (string, error) {
	g := h.g
	switch op.Kind {
	case lsp.ChangeText:
		auth := auths[op.Path]
		before, _, err := g.fileMutation.PreviewWrite(op.Path, "", auth)
		if err != nil {
			return "read " + op.Path, err
		}
		updated, err := applyTextEdits(before, op.Edits)
		if err != nil {
			return "apply edits to " + op.Path, err
		}
		if err := g.fileMutation.WriteFile(op.Path, updated, auth); err != nil {
			return "write " + op.Path, err
		}
	case lsp.ChangeCreate:
		exists, err := fileExists(op.Path)
		if err != nil {
			return "create " + op.Path, err
		}
		// Never clobber an existing file unless the op explicitly opted into
		// overwrite; ignoreIfExists and the default both leave existing content
		// intact so a preceding/following text edit is not truncated (§12).
		if exists && !op.Overwrite {
			return "", nil
		}
		if err := g.fileMutation.WriteFile(op.Path, "", auths[op.Path]); err != nil {
			return "create " + op.Path, err
		}
	case lsp.ChangeDelete:
		if err := g.fileMutation.Remove(op.Path); err != nil {
			return "delete " + op.Path, err
		}
	case lsp.ChangeRename:
		content, _, err := g.fileMutation.PreviewWrite(op.Path, "", auths[op.Path])
		if err != nil {
			return "read " + op.Path, err
		}
		if err := g.fileMutation.WriteFile(op.NewPath, content, auths[op.NewPath]); err != nil {
			return "write " + op.NewPath, err
		}
		if err := g.fileMutation.Remove(op.Path); err != nil {
			return "remove " + op.Path, err
		}
	}
	return "", nil
}

// fileExists reports whether path exists on disk; an unexpected stat error is
// surfaced so a create does not silently mis-decide.
func fileExists(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return true, nil
	} else if os.IsNotExist(err) {
		return false, nil
	} else {
		return false, err
	}
}

// applyTextEdits applies a set of LSP text edits (1-based, rune-column ranges at
// the tool edge) to content. Edits are applied from the last position to the
// first so earlier offsets stay valid as the buffer changes; overlapping edits
// (which the spec forbids) are applied in that order without further reconciliation.
func applyTextEdits(content string, edits []lsp.TextEdit) (string, error) {
	if len(edits) == 0 {
		return content, nil
	}
	lineStarts := lineStartOffsets(content)
	type resolved struct {
		start, end int
		text       string
	}
	res := make([]resolved, 0, len(edits))
	for _, e := range edits {
		start, err := offsetOf(content, lineStarts, e.Range.Start)
		if err != nil {
			return "", err
		}
		end, err := offsetOf(content, lineStarts, e.Range.End)
		if err != nil {
			return "", err
		}
		if end < start {
			start, end = end, start
		}
		res = append(res, resolved{start: start, end: end, text: e.NewText})
	}
	sort.SliceStable(res, func(i, j int) bool { return res[i].start > res[j].start })
	b := []byte(content)
	for _, r := range res {
		if r.start < 0 || r.end > len(b) || r.start > r.end {
			return "", fmt.Errorf("edit range out of bounds")
		}
		b = append(b[:r.start], append([]byte(r.text), b[r.end:]...)...)
	}
	return string(b), nil
}

// lineStartOffsets returns the byte offset at which each 0-based line begins.
func lineStartOffsets(content string) []int {
	offsets := []int{0}
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			offsets = append(offsets, i+1)
		}
	}
	return offsets
}

// offsetOf converts a 1-based line / 1-based rune-column edge position to a byte
// offset in content.
func offsetOf(content string, lineStarts []int, p lsp.Position) (int, error) {
	line := p.Line - 1
	if line < 0 {
		line = 0
	}
	if line >= len(lineStarts) {
		return len(content), nil
	}
	off := lineStarts[line]
	col := p.Character - 1
	for col > 0 && off < len(content) && content[off] != '\n' {
		_, size := utf8.DecodeRuneInString(content[off:])
		off += size
		col--
	}
	return off, nil
}
