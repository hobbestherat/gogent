package lsp

import (
	"context"

	"go.lsp.dev/protocol"
)

// FileChangeKind classifies a filesystem mutation gogent performed, so the Client
// can keep the server's view honest: re-sync an open document and, for watched
// files, emit workspace/didChangeWatchedFiles (the LSP support design §11.5).
type FileChangeKind int

const (
	FileCreated FileChangeKind = iota
	FileChanged
	FileDeleted
)

// fileChangeType maps a FileChangeKind to the wire FileChangeType.
func (k FileChangeKind) fileChangeType() protocol.FileChangeType {
	switch k {
	case FileCreated:
		return protocol.FileChangeTypeCreated
	case FileDeleted:
		return protocol.FileChangeTypeDeleted
	default:
		return protocol.FileChangeTypeChanged
	}
}

// emitWatchedFileChange sends workspace/didChangeWatchedFiles for absPath when it
// matches a registered watcher glob. The globs come entirely from the server's
// dynamic registration, so there is no per-language code (§11.5). The caller
// holds no lock; capability reads take the Client mutex internally.
func (c *Client) emitWatchedFileChange(ctx context.Context, absPath string, kind FileChangeKind) {
	c.mu.Lock()
	matched := c.caps.matchesWatcher(absPath)
	c.mu.Unlock()
	if !matched {
		return
	}
	_ = c.conn.Notify(ctx, methodWatchedFiles, &protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{
			URI:  pathToURI(absPath),
			Type: kind.fileChangeType(),
		}},
	})
}
