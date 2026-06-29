package lsp

import (
	"encoding/json"
	"testing"

	"go.lsp.dev/protocol"
)

// TestRuneColToUTF16 covers the classic off-by-column bug class: a non-ASCII or
// astral character must count as the right number of UTF-16 code units, not runes
// or bytes (the LSP support design §11.3).
func TestRuneColToUTF16(t *testing.T) {
	cases := []struct {
		name string
		line string
		col1 int // 1-based rune column at the tool edge
		want uint32
	}{
		{"ascii start", "abc", 1, 0},
		{"ascii mid", "abc", 3, 2},
		{"bmp non-ascii", "héllo", 3, 2},   // é is one UTF-16 unit
		{"astral counts two", "a𐐀b", 3, 3}, // 𐐀 is a surrogate pair (2 units)
		{"clamp past end", "ab", 99, 2},    // out-of-range clamps to line length
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := runeColToUTF16(c.line, c.col1); got != c.want {
				t.Fatalf("runeColToUTF16(%q,%d) = %d, want %d", c.line, c.col1, got, c.want)
			}
		})
	}
}

// TestUTF16ColToRune is the inverse mapping used for incoming wire positions.
func TestUTF16ColToRune(t *testing.T) {
	cases := []struct {
		line  string
		units uint32
		want  int // 1-based rune column
	}{
		{"abc", 0, 1},
		{"abc", 2, 3},
		{"héllo", 2, 3},
		{"a𐐀b", 3, 3}, // past the surrogate pair lands on 'b'
	}
	for _, c := range cases {
		if got := utf16ColToRune(c.line, c.units); got != c.want {
			t.Fatalf("utf16ColToRune(%q,%d) = %d, want %d", c.line, c.units, got, c.want)
		}
	}
}

// TestRoundTripColumn confirms the two conversions compose to the identity on a
// line with mixed-width characters.
func TestRoundTripColumn(t *testing.T) {
	line := "a𐐀héllo b"
	for col := 1; col <= 9; col++ {
		units := runeColToUTF16(line, col)
		if got := utf16ColToRune(line, units); got != col {
			t.Fatalf("round-trip col %d -> units %d -> %d", col, units, got)
		}
	}
}

// TestDedupDiagnostics confirms duplicates keyed by (code, severity, message,
// source, range) collapse while first-seen order is preserved (§3).
func TestDedupDiagnostics(t *testing.T) {
	d := Diagnostic{Severity: 1, Code: "E1", Source: "go", Message: "boom",
		Range: Range{Start: Position{1, 1}, End: Position{1, 2}}}
	in := []Diagnostic{d, d, {Severity: 2, Message: "other"}, d}
	out := dedupDiagnostics(in)
	if len(out) != 2 {
		t.Fatalf("dedup len = %d, want 2 (%+v)", len(out), out)
	}
	if out[0].Message != "boom" || out[1].Message != "other" {
		t.Fatalf("dedup lost order: %+v", out)
	}
}

// TestCapabilityTruthiness checks the generic provider-truthiness gate over the
// bool-or-options union arms, plus the resolve/prepare sub-flags (§7.2).
func TestCapabilityTruthiness(t *testing.T) {
	raw := `{"capabilities":{
		"definitionProvider":true,
		"hoverProvider":{"workDoneProgress":true},
		"referencesProvider":false,
		"renameProvider":{"prepareProvider":true},
		"codeActionProvider":{"resolveProvider":true}
	}}`
	var res protocol.InitializeResult
	if err := protocol.Unmarshal(json.RawMessage(raw), &res); err != nil {
		t.Fatalf("unmarshal init result: %v", err)
	}
	caps := newCapabilities()
	caps.applyInitializeResult(&res)

	if !caps.supports(methodDefinition) {
		t.Error("definition should be supported (bool true)")
	}
	if !caps.supports(methodHover) {
		t.Error("hover should be supported (options object)")
	}
	if caps.supports(methodReferences) {
		t.Error("references should NOT be supported (bool false)")
	}
	if !caps.supports(methodCodeAction) {
		t.Error("codeAction should be supported (options object)")
	}
	if !caps.codeActionResolve {
		t.Error("resolveProvider sub-flag not detected")
	}
	if !caps.renamePrepare {
		t.Error("prepareProvider sub-flag not detected")
	}
}

// TestDynamicRegistrationGatesAndWatchers confirms dynamic registration flips a
// capability on and that a didChangeWatchedFiles registration records its globs
// for matching (§7.2, §11.5).
func TestDynamicRegistrationGatesAndWatchers(t *testing.T) {
	caps := newCapabilities()
	if caps.supports(methodCallHierarchy) {
		t.Fatal("call hierarchy should start unsupported")
	}
	caps.register(methodCallHierarchy, nil)
	if !caps.supports(methodCallHierarchy) {
		t.Fatal("call hierarchy should be supported after dynamic registration")
	}

	opts := protocol.LSPAny(`{"watchers":[{"globPattern":"**/*.go"},{"globPattern":{"pattern":"go.mod"}}]}`)
	caps.register(methodWatchedFiles, opts)
	if !caps.matchesWatcher("/ws/internal/foo.go") {
		t.Error("**/*.go glob should match a .go file")
	}
	if !caps.matchesWatcher("/ws/go.mod") {
		t.Error("relative-pattern go.mod glob should match go.mod")
	}
	if caps.matchesWatcher("/ws/readme.md") {
		t.Error("no glob should match readme.md")
	}
	caps.unregister(methodWatchedFiles)
	if caps.matchesWatcher("/ws/internal/foo.go") {
		t.Error("globs should be cleared after unregister")
	}
}

// TestWorkspaceEditPreservesOrder confirms the edge keeps the server's
// documentChanges sequence (so a create precedes the edits that populate it) and
// carries the create/rename options through the boundary type (§12).
func TestWorkspaceEditPreservesOrder(t *testing.T) {
	raw := `{"documentChanges":[
		{"kind":"create","uri":"file:///ws/new.go","options":{"ignoreIfExists":true}},
		{"textDocument":{"uri":"file:///ws/new.go","version":1},"edits":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}},"newText":"package x\n"}]},
		{"kind":"rename","oldUri":"file:///ws/a.go","newUri":"file:///ws/b.go","options":{"overwrite":true}},
		{"kind":"delete","uri":"file:///ws/c.go"}
	]}`
	var e protocol.WorkspaceEdit
	if err := protocol.Unmarshal(json.RawMessage(raw), &e); err != nil {
		t.Fatalf("unmarshal workspace edit: %v", err)
	}
	out := workspaceEditFromWire(func(string, int) string { return "" }, &e)
	if len(out.Ordered) != 4 {
		t.Fatalf("ordered len = %d, want 4 (%+v)", len(out.Ordered), out.Ordered)
	}
	if out.Ordered[0].Kind != ChangeCreate || !out.Ordered[0].IgnoreIfExists {
		t.Errorf("first op should be a create with ignoreIfExists: %+v", out.Ordered[0])
	}
	if out.Ordered[1].Kind != ChangeText || len(out.Ordered[1].Edits) != 1 {
		t.Errorf("second op should be the text edit that fills the created file: %+v", out.Ordered[1])
	}
	if out.Ordered[2].Kind != ChangeRename || !out.Ordered[2].Overwrite {
		t.Errorf("third op should be a rename with overwrite: %+v", out.Ordered[2])
	}
	if out.Ordered[3].Kind != ChangeDelete {
		t.Errorf("fourth op should be a delete: %+v", out.Ordered[3])
	}
}

// TestServerConfigLanguageID covers the per-extension languageId override that
// lets one process serve several languageIds (§7.1).
func TestServerConfigLanguageID(t *testing.T) {
	cfg := ServerConfig{
		LanguageID: "typescript",
		Languages:  map[string]string{".tsx": "typescriptreact"},
	}
	if got := cfg.languageIDFor(".ts"); got != "typescript" {
		t.Errorf(".ts languageId = %q, want typescript", got)
	}
	if got := cfg.languageIDFor(".tsx"); got != "typescriptreact" {
		t.Errorf(".tsx languageId = %q, want typescriptreact", got)
	}
}
