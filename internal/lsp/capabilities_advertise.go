package lsp

import "go.lsp.dev/protocol"

// boolPtr returns a pointer to b, for the many *bool capability fields.
func boolPtr(b bool) *bool { return &b }

// clientCapabilities builds the ClientCapabilities gogent advertises in
// initialize (the LSP support design §10). It advertises only what the curated
// ops use, but advertises it correctly — including the result-shaping sub-options
// those ops depend on (§7.2): hierarchical document symbols, code-action literal +
// data + resolve support, rename prepareSupport, hover content formats, full text
// sync + didSave, workspace configuration/folders/watched-files (dynamic), and
// applyEdit with documentChanges + resource operations. positionEncodings is not
// advertised (unavailable in the library, §6); the wire encoding is utf-16.
func clientCapabilities() protocol.ClientCapabilities {
	dyn := protocol.HoverClientCapabilities{
		DynamicRegistration: boolPtr(true),
		ContentFormat:       []protocol.MarkupKind{protocol.MarkupKindMarkdown, protocol.MarkupKindPlainText},
	}
	return protocol.ClientCapabilities{
		Workspace: &protocol.WorkspaceClientCapabilities{
			ApplyEdit:        boolPtr(true),
			Configuration:    boolPtr(true),
			WorkspaceFolders: boolPtr(true),
			WorkspaceEdit: &protocol.WorkspaceEditClientCapabilities{
				DocumentChanges: boolPtr(true),
				ResourceOperations: []protocol.ResourceOperationKind{
					protocol.ResourceOperationKindCreate,
					protocol.ResourceOperationKindRename,
					protocol.ResourceOperationKindDelete,
				},
			},
			DidChangeConfiguration: &protocol.DidChangeConfigurationClientCapabilities{DynamicRegistration: boolPtr(true)},
			DidChangeWatchedFiles:  &protocol.DidChangeWatchedFilesClientCapabilities{DynamicRegistration: boolPtr(true)},
			ExecuteCommand:         &protocol.ExecuteCommandClientCapabilities{DynamicRegistration: boolPtr(true)},
			Symbol:                 &protocol.WorkspaceSymbolClientCapabilities{DynamicRegistration: boolPtr(true)},
		},
		TextDocument: &protocol.TextDocumentClientCapabilities{
			Synchronization: &protocol.TextDocumentSyncClientCapabilities{
				DynamicRegistration: boolPtr(true),
				DidSave:             boolPtr(true),
			},
			Hover:          &dyn,
			Declaration:    &protocol.DeclarationClientCapabilities{DynamicRegistration: boolPtr(true)},
			Definition:     &protocol.DefinitionClientCapabilities{DynamicRegistration: boolPtr(true)},
			TypeDefinition: &protocol.TypeDefinitionClientCapabilities{DynamicRegistration: boolPtr(true)},
			Implementation: &protocol.ImplementationClientCapabilities{DynamicRegistration: boolPtr(true)},
			References:     &protocol.ReferenceClientCapabilities{DynamicRegistration: boolPtr(true)},
			DocumentSymbol: &protocol.DocumentSymbolClientCapabilities{
				DynamicRegistration:               boolPtr(true),
				HierarchicalDocumentSymbolSupport: boolPtr(true),
			},
			CallHierarchy: &protocol.CallHierarchyClientCapabilities{DynamicRegistration: boolPtr(true)},
			CodeAction: &protocol.CodeActionClientCapabilities{
				DynamicRegistration: boolPtr(true),
				CodeActionLiteralSupport: protocol.ClientCodeActionLiteralOptions{
					CodeActionKind: protocol.ClientCodeActionKindOptions{
						ValueSet: []protocol.CodeActionKind{
							protocol.CodeActionKindQuickFix,
							protocol.CodeActionKindRefactor,
							protocol.CodeActionKindRefactorExtract,
							protocol.CodeActionKindRefactorInline,
							protocol.CodeActionKindRefactorRewrite,
							protocol.CodeActionKindSource,
							protocol.CodeActionKindSourceOrganizeImports,
						},
					},
				},
				DataSupport:    boolPtr(true),
				ResolveSupport: protocol.ClientCodeActionResolveOptions{Properties: []string{"edit"}},
			},
			Rename: &protocol.RenameClientCapabilities{
				DynamicRegistration: boolPtr(true),
				PrepareSupport:      boolPtr(true),
			},
			Formatting: &protocol.DocumentFormattingClientCapabilities{DynamicRegistration: boolPtr(true)},
			PublishDiagnostics: &protocol.PublishDiagnosticsClientCapabilities{
				VersionSupport: boolPtr(true),
			},
			Diagnostic: &protocol.DiagnosticClientCapabilities{DynamicRegistration: boolPtr(true)},
		},
		Window: &protocol.WindowClientCapabilities{
			WorkDoneProgress: boolPtr(true),
		},
	}
}
