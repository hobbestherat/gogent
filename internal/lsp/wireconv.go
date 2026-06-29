package lsp

import (
	"strings"

	"go.lsp.dev/protocol"
)

// This file converts the library's protocol.* result types into the thin,
// gogent-owned boundary types (config.go). It is the edge where the union
// response shapes servers may return are normalized (the LSP support design
// §7.2, §10): documentSymbol's flat vs. hierarchical arms, codeAction's Command
// vs. CodeAction arms, definition's Location vs. LocationLink arms, and the
// scalar/markup unions on hover and diagnostics.

// progressTokenString renders a ProgressToken union (used for a Diagnostic.Code)
// as a string. An absent code is "".
func progressTokenString(t protocol.ProgressToken) string {
	switch v := t.(type) {
	case protocol.String:
		return string(v)
	case protocol.Integer:
		return itoa(int(v))
	default:
		return ""
	}
}

// inlayHintTooltipString renders the String|MarkupContent union the library uses
// for a Diagnostic.Message as plain text.
func inlayHintTooltipString(t protocol.InlayHintTooltip) string {
	switch v := t.(type) {
	case protocol.String:
		return string(v)
	case *protocol.MarkupContent:
		if v != nil {
			return v.Value
		}
	}
	return ""
}

// diagFromWire converts a wire Diagnostic for path to the boundary type.
func diagFromWire(getLine lineProvider, path string, d protocol.Diagnostic) Diagnostic {
	source := ""
	if s, ok := d.Source.Get(); ok {
		source = s
	}
	return Diagnostic{
		Range:    fromWireRange(getLine, path, d.Range),
		Severity: int(d.Severity),
		Code:     progressTokenString(d.Code),
		Source:   source,
		Message:  inlayHintTooltipString(d.Message),
	}
}

// locationsFromDefinition normalizes a DefinitionResult/DeclarationResult union
// (a single Location, a Location slice, or a LocationLink slice) to []Location.
func locationsFromDefinition(getLine lineProvider, res any) []Location {
	var out []Location
	switch v := res.(type) {
	case *protocol.Location:
		if v != nil {
			out = append(out, fromWireLocation(getLine, *v))
		}
	case protocol.LocationSlice:
		for _, l := range v {
			out = append(out, fromWireLocation(getLine, l))
		}
	case protocol.DefinitionLinkSlice:
		for _, l := range v {
			out = append(out, locationFromLink(getLine, protocol.LocationLink(l)))
		}
	case protocol.DeclarationLinkSlice:
		for _, l := range v {
			out = append(out, locationFromLink(getLine, protocol.LocationLink(l)))
		}
	}
	return out
}

// locationFromLink reduces a LocationLink to a Location at its target range.
func locationFromLink(getLine lineProvider, l protocol.LocationLink) Location {
	path := uriToPath(l.TargetURI)
	return Location{Path: path, Range: fromWireRange(getLine, path, l.TargetRange)}
}

// symbolsFromResult normalizes a DocumentSymbolResult union: the hierarchical
// DocumentSymbol tree (preferred) is mapped directly, while a flat
// SymbolInformation list is synthesized into a flat []Symbol so the tree is never
// silently lost on a server that returns the flat shape (§7.2). path is the
// requested document; hierarchical symbols carry no URI of their own.
func symbolsFromResult(getLine lineProvider, path string, res protocol.DocumentSymbolResult) []Symbol {
	switch v := res.(type) {
	case protocol.DocumentSymbolSlice:
		return documentSymbols(getLine, path, v)
	case protocol.SymbolInformationSlice:
		return symbolInformationList(getLine, v)
	}
	return nil
}

// documentSymbols maps a hierarchical DocumentSymbol tree. The symbols carry no
// URI of their own (they belong to the requested document), so path is threaded
// down from the request.
func documentSymbols(getLine lineProvider, path string, syms []protocol.DocumentSymbol) []Symbol {
	out := make([]Symbol, 0, len(syms))
	for _, s := range syms {
		detail := ""
		if s.Detail != nil {
			detail = *s.Detail
		}
		out = append(out, Symbol{
			Name:     s.Name,
			Kind:     symbolKindName(s.Kind),
			Detail:   detail,
			Location: Location{Path: path, Range: fromWireRange(getLine, path, s.Range)},
			Children: documentSymbols(getLine, path, s.Children),
		})
	}
	return out
}

// symbolInformationList synthesizes a flat []Symbol from SymbolInformation.
func symbolInformationList(getLine lineProvider, syms []protocol.SymbolInformation) []Symbol {
	out := make([]Symbol, 0, len(syms))
	for _, s := range syms {
		out = append(out, Symbol{
			Name:     s.Name,
			Kind:     symbolKindName(s.Kind),
			Location: fromWireLocation(getLine, s.Location),
		})
	}
	return out
}

// workspaceSymbols normalizes a WorkspaceSymbolResult union to []Symbol.
func workspaceSymbols(getLine lineProvider, res protocol.WorkspaceSymbolResult) []Symbol {
	switch v := res.(type) {
	case protocol.SymbolInformationSlice:
		return symbolInformationList(getLine, v)
	case protocol.WorkspaceSymbolSlice:
		out := make([]Symbol, 0, len(v))
		for _, s := range v {
			out = append(out, Symbol{Name: s.Name, Kind: symbolKindName(s.Kind), Location: workspaceSymbolLocation(getLine, s.Location)})
		}
		return out
	}
	return nil
}

// workspaceSymbolLocation normalizes the Location|{uri} union a WorkspaceSymbol
// carries; a URI-only location yields an empty range.
func workspaceSymbolLocation(getLine lineProvider, loc protocol.WorkspaceSymbolLocation) Location {
	switch v := loc.(type) {
	case *protocol.Location:
		if v != nil {
			return fromWireLocation(getLine, *v)
		}
	case *protocol.LocationUriOnly:
		if v != nil {
			return Location{Path: uriToPath(v.URI)}
		}
	}
	return Location{}
}

// textEditFromElement extracts a plain TextEdit from a TextDocumentEditElement
// union arm. SnippetTextEdit (cursor templating) has no plain-text form and is
// skipped — the curated mutation tools deal in concrete text, not snippets.
func textEditFromElement(el protocol.TextDocumentEditElement) *protocol.TextEdit {
	switch v := el.(type) {
	case *protocol.TextEdit:
		return v
	case *protocol.AnnotatedTextEdit:
		if v != nil {
			return &v.TextEdit
		}
	}
	return nil
}

// markedStringText renders a MarkedString union arm as plain text.
func markedStringText(m protocol.MarkedString) string {
	switch v := m.(type) {
	case protocol.String:
		return string(v)
	case *protocol.MarkedStringWithLanguage:
		if v != nil {
			return v.Value
		}
	}
	return ""
}

// symbolKindName renders a SymbolKind as a lowercase name for the model. Unknown
// kinds (a server using a value outside the standard set) render numerically.
func symbolKindName(k protocol.SymbolKind) string {
	switch k {
	case protocol.SymbolKindFile:
		return "file"
	case protocol.SymbolKindModule:
		return "module"
	case protocol.SymbolKindNamespace:
		return "namespace"
	case protocol.SymbolKindPackage:
		return "package"
	case protocol.SymbolKindClass:
		return "class"
	case protocol.SymbolKindMethod:
		return "method"
	case protocol.SymbolKindProperty:
		return "property"
	case protocol.SymbolKindField:
		return "field"
	case protocol.SymbolKindConstructor:
		return "constructor"
	case protocol.SymbolKindEnum:
		return "enum"
	case protocol.SymbolKindInterface:
		return "interface"
	case protocol.SymbolKindFunction:
		return "function"
	case protocol.SymbolKindVariable:
		return "variable"
	case protocol.SymbolKindConstant:
		return "constant"
	case protocol.SymbolKindString:
		return "string"
	case protocol.SymbolKindNumber:
		return "number"
	case protocol.SymbolKindBoolean:
		return "boolean"
	case protocol.SymbolKindArray:
		return "array"
	case protocol.SymbolKindObject:
		return "object"
	case protocol.SymbolKindKey:
		return "key"
	case protocol.SymbolKindNull:
		return "null"
	case protocol.SymbolKindEnumMember:
		return "enum-member"
	case protocol.SymbolKindStruct:
		return "struct"
	case protocol.SymbolKindEvent:
		return "event"
	case protocol.SymbolKindOperator:
		return "operator"
	case protocol.SymbolKindTypeParameter:
		return "type-parameter"
	default:
		return itoa(int(k))
	}
}

// hoverString renders a Hover's HoverContents union (markup, plain string, or the
// deprecated MarkedString forms) as plain text.
func hoverString(h *protocol.Hover) string {
	if h == nil {
		return ""
	}
	switch v := h.Contents.(type) {
	case *protocol.MarkupContent:
		if v != nil {
			return v.Value
		}
	case protocol.String:
		return string(v)
	case *protocol.MarkedStringWithLanguage:
		if v != nil {
			return v.Value
		}
	case protocol.MarkedStringSlice:
		var b strings.Builder
		for i, m := range v {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(markedStringText(m))
		}
		return b.String()
	}
	return ""
}

// workspaceEditFromWire converts a protocol.WorkspaceEdit to the boundary type,
// flattening both the `changes` map and `documentChanges` (text edits plus
// create/rename/delete resource operations) so a preview and an apply reproduce
// every effect, including renames and deletes (§12).
func workspaceEditFromWire(getLine lineProvider, e *protocol.WorkspaceEdit) WorkspaceEdit {
	out := WorkspaceEdit{}
	if e == nil {
		return out
	}
	add := func(path string, edits []protocol.TextEdit) {
		if out.Changes == nil {
			out.Changes = map[string][]TextEdit{}
		}
		for _, te := range edits {
			out.Changes[path] = append(out.Changes[path], TextEdit{
				Range:   fromWireRange(getLine, path, te.Range),
				NewText: te.NewText,
			})
		}
	}
	for u, edits := range e.Changes {
		add(uriToPath(u), edits)
	}
	for _, dc := range e.DocumentChanges {
		switch v := dc.(type) {
		case *protocol.TextDocumentEdit:
			path := uriToPath(v.TextDocument.URI)
			edits := make([]protocol.TextEdit, 0, len(v.Edits))
			for _, el := range v.Edits {
				if te := textEditFromElement(el); te != nil {
					edits = append(edits, *te)
				}
			}
			add(path, edits)
		case *protocol.CreateFile:
			out.ResourceOps = append(out.ResourceOps, ResourceOp{Kind: "create", Path: uriToPath(v.URI)})
		case *protocol.RenameFile:
			out.ResourceOps = append(out.ResourceOps, ResourceOp{
				Kind: "rename", Path: uriToPath(v.OldURI), NewPath: uriToPath(v.NewURI),
			})
		case *protocol.DeleteFile:
			out.ResourceOps = append(out.ResourceOps, ResourceOp{Kind: "delete", Path: uriToPath(v.URI)})
		}
	}
	return out
}

// itoa is a tiny strconv.Itoa to keep imports minimal in this conversion file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
