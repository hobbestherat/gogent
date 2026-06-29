package lsp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// docState is the Client's view of one open document. The Client owns the
// authoritative content it last synced so it can convert positions (the column
// conversion needs line text) and decide whether an on-disk change must be
// re-synced before a request (the LSP support design §11.1).
type docState struct {
	version    int32
	languageID string
	text       string
	size       int64
	mtime      time.Time
	hash       [32]byte
}

// Client is the generic, language-independent LSP client (the LSP support design
// §9). A single implementation serves every server; all per-language knowledge is
// in cfg. It owns the jsonrpc2 connection, the live capability table, the
// versioned open-document table, and the diagnostics cache, each guarded by mu
// (or, for diagnostics, by the store's own lock). Curated, capability-gated
// operations are the tool-facing surface.
//
// Cancellation (the LSP support design §9) is satisfied per request: every curated
// op issues exactly one request through the protocol ServerDispatcher, whose Call
// keeps the in-flight request→id mapping (the library's outgoing-call table) and,
// when the caller's context is cancelled before the response arrives, emits
// $/cancelRequest for that exact id over a detached context and unblocks the caller
// with context.Canceled; the id is cleared on response. The read loop runs on the
// Manager's long-lived baseCtx, never a tool-call context, so cancelling one tool
// call never tears down the client. TestCancellationEmitsCancelRequest pins this
// behavior.
type Client struct {
	cfg           ServerConfig
	rootURI       uri.URI
	workspaceRoot string
	host          Host

	conn   jsonrpc2.Conn
	server protocol.Server
	closer func() error

	mu   sync.Mutex
	caps *capabilities
	docs map[string]*docState

	diag *diagnosticsStore
}

// newClient builds a Client over stream, performs the initialize handshake, and
// returns a ready Client. closer releases the transport (terminates the stdio
// subprocess); it is called by Close. host may be nil (callbacks degrade to
// headless defaults).
//
// baseCtx is the long-lived context the jsonrpc2 read loop runs on: it must
// outlive individual tool calls (it is the Manager's session context, cancelled
// on Shutdown), NOT a tool-call context, or a cancelled tool call would tear down
// the whole client. Per-op cancellation is handled separately by passing each
// tool call's context to the cancel-aware request dispatcher.
func newClient(baseCtx context.Context, cfg ServerConfig, rootURI uri.URI, workspaceRoot string, host Host, stream jsonrpc2.Stream, closer func() error) (*Client, error) {
	c := &Client{
		cfg:           cfg,
		rootURI:       rootURI,
		workspaceRoot: workspaceRoot,
		host:          host,
		closer:        closer,
		caps:          newCapabilities(),
		docs:          map[string]*docState{},
		diag:          newDiagnosticsStore(),
	}
	// The handler (server→client surface) references c; wiring it before the conn
	// is fine because no inbound traffic is delivered until conn.Go runs inside
	// protocol.NewClient.
	_, conn, server := protocol.NewClient(baseCtx, &clientHandler{c: c}, stream)
	c.conn = conn
	c.server = server

	// The handshake is bounded so a wedged server cannot block startup forever.
	initCtx, cancel := context.WithTimeout(baseCtx, 30*time.Second)
	defer cancel()
	if err := c.initialize(initCtx); err != nil {
		_ = conn.Close()
		_ = closer()
		return nil, err
	}
	return c, nil
}

// initialize performs the LSP handshake: initialize → initialized, recording the
// negotiated capabilities (§10).
func (c *Client) initialize(ctx context.Context) error {
	params := &protocol.InitializeParams{
		ProcessID:    int32Ptr(int32(os.Getpid())), //nolint:gosec // pid fits int32 on supported platforms
		RootURI:      &c.rootURI,
		Capabilities: clientCapabilities(),
		Trace:        protocol.TraceValueOff,
	}
	if len(c.cfg.InitOptions) > 0 {
		if data, err := json.Marshal(c.cfg.InitOptions); err == nil {
			params.InitializationOptions = protocol.LSPAny(data)
		}
	}
	res, err := c.server.Initialize(ctx, params)
	if err != nil {
		return fmt.Errorf("lsp %s: initialize: %w", c.cfg.Name, err)
	}
	c.mu.Lock()
	c.caps.applyInitializeResult(res)
	c.mu.Unlock()
	if err := c.server.Initialized(ctx, &protocol.InitializedParams{}); err != nil {
		return fmt.Errorf("lsp %s: initialized: %w", c.cfg.Name, err)
	}
	c.pushConfiguration(ctx)
	return nil
}

// pushConfiguration sends workspace/didChangeConfiguration from the server's
// Settings so servers that read configuration eagerly see it (§10). Servers that
// pull instead are answered by clientHandler.Configuration.
func (c *Client) pushConfiguration(ctx context.Context) {
	if len(c.cfg.Settings) == 0 {
		return
	}
	data, err := json.Marshal(c.cfg.Settings)
	if err != nil {
		return
	}
	_ = c.conn.Notify(ctx, "workspace/didChangeConfiguration",
		&protocol.DidChangeConfigurationParams{Settings: protocol.LSPAny(data)})
}

// Name returns the configured server name.
func (c *Client) Name() string { return c.cfg.Name }

// logf forwards a server-originated message to the Host log, prefixed with the
// server name. A nil Host discards.
func (c *Client) logf(format string, args ...any) {
	if c.host != nil {
		c.host.Logf(c.cfg.Name, format, args...)
	}
}

// lineText backs the lineProvider: it returns the text of the 0-based line for
// path, preferring the open document's synced content and falling back to a disk
// read for files the Client has not opened. It takes the Client mutex only to
// read the doc table, never while a conversion holds it.
func (c *Client) lineText(path string, line int) string {
	c.mu.Lock()
	d, ok := c.docs[path]
	var text string
	if ok {
		text = d.text
	}
	c.mu.Unlock()
	if !ok {
		if b, err := os.ReadFile(path); err == nil { //nolint:gosec // path resolved from a workspace tool request
			text = string(b)
		}
	}
	return lineOf(text, line)
}

// ensureOpen guarantees the server has a current view of path before a request
// (§11.1): it opens the document on first touch (didOpen) and re-syncs an
// already-open document whose on-disk content changed out-of-band (didChange). It
// returns whether a new version was sent (so a diagnostics caller knows to wait
// for a fresh, correlated push).
func (c *Client) ensureOpen(ctx context.Context, path string) (sentVersion bool, err error) {
	content, info, err := readFileStat(path)
	if err != nil {
		return false, err
	}
	hash := sha256.Sum256(content)

	c.mu.Lock()
	d, open := c.docs[path]
	switch {
	case !open:
		langID := c.cfg.languageIDFor(filepath.Ext(path))
		d = &docState{
			version:    1,
			languageID: langID,
			text:       string(content),
			size:       info.Size(),
			mtime:      info.ModTime(),
			hash:       hash,
		}
		c.docs[path] = d
		version := d.version
		c.mu.Unlock()
		// Register the freshness interest before sending didOpen so a fast push for
		// this version is never dropped (§9). didOpen is a notification.
		c.diag.expect(path, version)
		err := c.conn.Notify(ctx, "textDocument/didOpen", &protocol.DidOpenTextDocumentParams{
			TextDocument: protocol.TextDocumentItem{
				URI:        pathToURI(path),
				LanguageID: protocol.LanguageKind(langID),
				Version:    version,
				Text:       string(content),
			},
		})
		return true, err
	case d.hash != hash:
		// Out-of-band change to an open file: re-sync the full document (§11.1).
		d.version++
		d.text = string(content)
		d.size = info.Size()
		d.mtime = info.ModTime()
		d.hash = hash
		version := d.version
		c.mu.Unlock()
		c.diag.expect(path, version)
		err := c.sendDidChange(ctx, path, version, string(content))
		return true, err
	default:
		c.mu.Unlock()
		return false, nil
	}
}

// sendDidChange sends a full-document didChange for path at version (§11.1).
func (c *Client) sendDidChange(ctx context.Context, path string, version int32, text string) error {
	return c.conn.Notify(ctx, "textDocument/didChange", &protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: pathToURI(path)},
			Version:                version,
		},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{
			&protocol.TextDocumentContentChangeWholeDocument{Text: text},
		},
	})
}

// supports reports whether the server advertises method (capability gating, §7.2).
func (c *Client) supports(method string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.caps.supports(method)
}

// FileChanged keeps the server's view honest after gogent's own write to path
// (the fileops subscription, §11.2/§11.5): an open document is re-synced
// (didChange + didSave), and a watched file emits didChangeWatchedFiles. It is a
// no-op for an untouched, unopened, unwatched file.
func (c *Client) FileChanged(ctx context.Context, path string, kind FileChangeKind) {
	abs := cleanPath(path)
	c.mu.Lock()
	_, open := c.docs[abs]
	c.mu.Unlock()
	if open && kind != FileDeleted {
		if _, err := c.ensureOpen(ctx, abs); err == nil {
			c.sendDidSave(ctx, abs)
		}
	}
	if open && kind == FileDeleted {
		c.closeDoc(ctx, abs)
	}
	c.emitWatchedFileChange(ctx, abs, kind)
}

// sendDidSave notifies the server that an open document was saved, including its
// text when the server requested it on save (§11.1).
func (c *Client) sendDidSave(ctx context.Context, path string) {
	c.mu.Lock()
	d, open := c.docs[path]
	includeText := c.caps.saveIncludeText
	var text string
	if open {
		text = d.text
	}
	c.mu.Unlock()
	if !open {
		return
	}
	params := &protocol.DidSaveTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: pathToURI(path)},
	}
	if includeText {
		params.Text = &text
	}
	_ = c.conn.Notify(ctx, "textDocument/didSave", params)
}

// closeDoc sends didClose and drops the document from the open table.
func (c *Client) closeDoc(ctx context.Context, path string) {
	c.mu.Lock()
	_, open := c.docs[path]
	delete(c.docs, path)
	c.mu.Unlock()
	if !open {
		return
	}
	c.diag.invalidate(path)
	_ = c.conn.Notify(ctx, "textDocument/didClose", &protocol.DidCloseTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: pathToURI(path)},
	})
}

// Close shuts the server down cleanly: shutdown request → exit notification →
// release the transport (kill the subprocess if it lingers), mirroring the MCP
// stdio Close (§9). It tolerates a slow/unresponsive server via a bounded
// shutdown context.
func (c *Client) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = c.server.Shutdown(ctx)
	_ = c.server.Exit(ctx)
	_ = c.conn.Close()
	if c.closer != nil {
		return c.closer()
	}
	return nil
}

// posParams builds a wire TextDocumentPositionParams for path at the tool-edge
// position p.
func (c *Client) posParams(path string, p Position) protocol.TextDocumentPositionParams {
	return protocol.TextDocumentPositionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: pathToURI(path)},
		Position:     toWirePosition(c.lineText, path, p),
	}
}

// readFileStat reads a file's content and stat in one step.
func readFileStat(path string) ([]byte, os.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	content, err := os.ReadFile(path) //nolint:gosec // path resolved from a workspace tool request
	if err != nil {
		return nil, nil, err
	}
	return content, info, nil
}

// cleanPath returns the cleaned absolute form of path for stable doc-table keys.
func cleanPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

func int32Ptr(v int32) *int32 { return &v }
