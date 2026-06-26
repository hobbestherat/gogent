package ui

// Tests for the Resources dialog's MCP tab population (issue #492). The tab is
// derived client-side from the registered mcp__<server>__<tool> tools, so the
// suite targets the pure derivers (loadMCPItems, splitMCPToolName,
// filterMCPItems, the label/detail renderers) and one end-to-end test that
// drives a real Gogent against an in-process stub MCP server to close the
// registry -> GetTools -> loadMCPItems seam. None of these depend on
// chrome-devtools-mcp/playwright-mcp.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gogent/internal/config"
	"gogent/internal/gogent"
	"gogent/internal/permission"
	"gogent/internal/tool"
)

// --- test helpers ---------------------------------------------------------

// mcpInfos builds []ToolInfo whose names are the given registered tool names.
// Each description carries the exact "use its full name" trailer that
// internal/gogent/newMCPTool appends, so the cleaner and detail renderers see
// realistic input. A separate description can be supplied for a row via the
// *Desc builders below.
func mcpInfos(names ...string) []ToolInfo {
	out := make([]ToolInfo, len(names))
	for i, n := range names {
		out[i] = ToolInfo{
			Name:        n,
			Description: "desc for " + n + "\n\nWhen calling this tool, use its full name \"" + n + "\".",
			InputSchema: `{"type":"object"}`,
		}
	}
	return out
}

// itemNames collects the .name of each resourceItem, preserving order.
func itemNames(items []resourceItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.name
	}
	return out
}

// findItem returns the first item with the given name, or fails the test.
func findItem(t *testing.T, items []resourceItem, name string) resourceItem {
	t.Helper()
	for _, it := range items {
		if it.name == name {
			return it
		}
	}
	t.Fatalf("item %q not found in %v", name, itemNames(items))
	return resourceItem{}
}

// newStubMCPServer stands up a minimal in-process MCP server over
// streamable-HTTP (plain JSON replies) exposing one "greet" tool with an input
// schema, mirroring internal/gogent/mcp_test.go's mcpTestServer. It is the
// stub chrome-devtools-mcp/playwright-mcp stand in for the automated gate.
func newStubMCPServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     int64                  `json:"id"`
			Method string                 `json:"method"`
			Params map[string]interface{} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		if req.ID == 0 { // notification
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result interface{}
		switch req.Method {
		case "initialize":
			result = map[string]interface{}{"protocolVersion": "2025-06-18"}
		case "tools/list":
			result = map[string]interface{}{"tools": []map[string]interface{}{{
				"name":        "greet",
				"description": "Greet someone",
				"inputSchema": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{"who": map[string]interface{}{"type": "string"}},
					"required":   []string{"who"},
				},
			}}}
		case "tools/call":
			result = map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "hi"}}}
		}
		resp, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": req.ID, "result": result})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	}))
}

// --- splitMCPToolName -----------------------------------------------------

func TestSplitMCPToolName(t *testing.T) {
	for _, tc := range []struct {
		name         string
		in           string
		server, tool string
		ok           bool
	}{
		{"plain", "mcp__srv__greet", "srv", "greet", true},
		{"tool name contains separator", "mcp__srv__do__thing", "srv", "do__thing", true},
		{"single segment after prefix", "mcp__srv", "", "", false},
		{"empty server segment", "mcp____tool", "", "", false},
		{"empty tool segment", "mcp__srv__", "", "", false},
		{"bare prefix", "mcp__", "", "", false},
		{"no prefix at all", "shell", "", "", false},
		{"separator but no prefix", "__srv__tool", "", "", false},
		{"prefix must be exact (mcpfoo)", "mcpfoo__srv__tool", "", "", false},
		{"prefix is case-sensitive", "MCP__srv__tool", "", "", false},
		{"empty string", "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, toolName, ok := splitMCPToolName(tc.in)
			if ok != tc.ok || s != tc.server || toolName != tc.tool {
				t.Errorf("splitMCPToolName(%q) = (%q,%q,%v), want (%q,%q,%v)",
					tc.in, s, toolName, ok, tc.server, tc.tool, tc.ok)
			}
		})
	}
}

// --- pluralTools ----------------------------------------------------------

func TestPluralTools(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{
		{0, "0 tools"},
		{1, "1 tool"},
		{2, "2 tools"},
		{7, "7 tools"},
	} {
		if got := pluralTools(tc.n); got != tc.want {
			t.Errorf("pluralTools(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// --- loadMCPItems ---------------------------------------------------------

// TestLoadMCPItems_NilGetter ensures a missing/empty backend degrades to no
// items rather than panicking.
func TestLoadMCPItems_NilGetter(t *testing.T) {
	if got := loadMCPItems(nil); got != nil {
		t.Errorf("loadMCPItems(nil) = %v, want nil", got)
	}
	if got := loadMCPItems(func() []ToolInfo { return nil }); got != nil {
		t.Errorf("loadMCPItems(empty) = %v, want nil", got)
	}
}

// TestLoadMCPItems_ExcludesNonMCP confirms only mcp__ tools are represented.
func TestLoadMCPItems_ExcludesNonMCP(t *testing.T) {
	got := loadMCPItems(func() []ToolInfo {
		return mcpInfos("shell", "calc", "mcp__srv__only")
	})
	if len(got) != 2 { // header + one tool
		t.Fatalf("expected only the mcp__ tool (header+row), got %v", itemNames(got))
	}
	if got[0].name != "srv" || !got[0].group {
		t.Errorf("expected srv header, got %q group=%v", got[0].name, got[0].group)
	}
	if got[1].name != "only" {
		t.Errorf("expected only tool, got %q", got[1].name)
	}
}

// TestLoadMCPItems_GroupingSortingReadOnly is the core structural test:
// servers sorted alphabetically, each followed by its tools sorted by bare
// name, every row read-only.
func TestLoadMCPItems_GroupingSortingReadOnly(t *testing.T) {
	items := loadMCPItems(func() []ToolInfo {
		// Deliberately unsorted and interleaved across servers.
		return mcpInfos(
			"mcp__srvB__nav",
			"mcp__srvA__greet",
			"mcp__srvA__echo",
			"mcp__srvA__alpha",
		)
	})
	want := []struct {
		name  string
		group bool
	}{
		{"srvA", true},
		{"alpha", false},
		{"echo", false},
		{"greet", false},
		{"srvB", true},
		{"nav", false},
	}
	if len(items) != len(want) {
		t.Fatalf("expected %d items, got %d: %v", len(want), len(items), itemNames(items))
	}
	for i, w := range want {
		if items[i].name != w.name {
			t.Errorf("item[%d].name = %q, want %q (full order: %v)", i, items[i].name, w.name, itemNames(items))
		}
		if items[i].group != w.group {
			t.Errorf("item[%d] (%q) group = %v, want %v", i, items[i].name, items[i].group, w.group)
		}
		if items[i].kind != resourceMCP {
			t.Errorf("item %q kind = %v, want resourceMCP", items[i].name, items[i].kind)
		}
		if items[i].canToggle {
			t.Errorf("item %q must be read-only (canToggle=false)", items[i].name)
		}
	}
	// Header usage counts: srvA advertises 3, srvB advertises 1.
	if h := findItem(t, items, "srvA"); h.usage != "3 tools" {
		t.Errorf("srvA header usage = %q, want %q", h.usage, "3 tools")
	}
	if h := findItem(t, items, "srvB"); h.usage != "1 tool" {
		t.Errorf("srvB header usage = %q, want %q", h.usage, "1 tool")
	}
	// A tool row carries no usage tail.
	if row := findItem(t, items, "alpha"); row.usage != "" {
		t.Errorf("alpha row usage = %q, want empty", row.usage)
	}
}

// TestLoadMCPItems_ToolNameWithSeparator verifies the first-__ split so a tool
// whose bare name contains "__" is not split into a bogus server.
func TestLoadMCPItems_ToolNameWithSeparator(t *testing.T) {
	items := loadMCPItems(func() []ToolInfo {
		return mcpInfos("mcp__srv__do__thing")
	})
	if len(items) != 2 {
		t.Fatalf("expected header + 1 tool, got %v", itemNames(items))
	}
	if items[0].name != "srv" || !items[0].group {
		t.Errorf("header = %q group=%v, want srv/group", items[0].name, items[0].group)
	}
	if items[1].name != "do__thing" || items[1].group {
		t.Errorf("tool = %q group=%v, want do__thing/leaf", items[1].name, items[1].group)
	}
}

// TestLoadMCPItems_SkipsMalformed confirms names that do not parse as
// mcp__<server>__<tool> are silently dropped (not grouped under an empty name).
func TestLoadMCPItems_SkipsMalformed(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"no second separator", "mcp__srv"},
		{"empty server", "mcp____tool"},
		{"empty tool", "mcp__srv__"},
		{"bare prefix", "mcp__"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := loadMCPItems(func() []ToolInfo { return mcpInfos(tc.in) })
			if len(got) != 0 {
				t.Errorf("loadMCPItems(%q) = %v, want no items", tc.in, itemNames(got))
			}
		})
	}
}

// TestLoadMCPItems_DescCleanedAndDetailRich checks the description trailer is
// stripped from the searchable row desc and that the detail carries the
// schema plus the agent-callable namespaced name.
func TestLoadMCPItems_DescCleanedAndDetailRich(t *testing.T) {
	items := loadMCPItems(func() []ToolInfo {
		return mcpInfos("mcp__srv__greet")
	})
	row := findItem(t, items, "greet")
	// Row desc is cleaned: no model-targeted trailer.
	if strings.Contains(row.desc, "When calling this tool") {
		t.Errorf("row desc leaked trailer: %q", row.desc)
	}
	if !strings.Contains(row.desc, "desc for") {
		t.Errorf("row desc lost real text: %q", row.desc)
	}
	// Detail surfaces the server, the namespaced name and the schema, but not
	// the trailer.
	for _, want := range []string{"MCP tool: greet", "Server: srv", "Registered as: mcp__srv__greet", "Input schema"} {
		if !strings.Contains(row.detail, want) {
			t.Errorf("detail missing %q:\n%s", want, row.detail)
		}
	}
	if strings.Contains(row.detail, "When calling this tool") {
		t.Errorf("detail leaked trailer:\n%s", row.detail)
	}
}

// --- cleanMCPDescription --------------------------------------------------

func TestCleanMCPDescription(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"strips trailer", "Greet someone.\n\nWhen calling this tool, use its full name \"mcp__srv__greet\".", "Greet someone."},
		{"no trailer just trims", "  Greet someone.\n", "Greet someone."},
		{"empty", "", ""},
		{"only whitespace", "   \n\t ", ""},
		{"trailer with surrounding blank lines", "Desc\n\n\nWhen calling this tool, use its full name \"x\".", "Desc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanMCPDescription(tc.in); got != tc.want {
				t.Errorf("cleanMCPDescription(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// --- filterMCPItems -------------------------------------------------------

// sampleMCPGroups is a hand-built two-tier list in the exact shape loadMCPItems
// emits (header, its tool rows, next header, ...). Used to control descriptions
// for the filter tests.
func sampleMCPGroups() []resourceItem {
	return []resourceItem{
		{kind: resourceMCP, group: true, name: "srvA", usage: "2 tools"},
		{kind: resourceMCP, group: false, name: "alpha", desc: "first tool"},
		{kind: resourceMCP, group: false, name: "beta", desc: "second tool"},
		{kind: resourceMCP, group: true, name: "srvB", usage: "1 tool"},
		{kind: resourceMCP, group: false, name: "gamma", desc: "third tool"},
	}
}

func TestFilterMCPItems(t *testing.T) {
	sample := sampleMCPGroups()

	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{"empty query keeps all", "", []string{"srvA", "alpha", "beta", "srvB", "gamma"}},
		{"whitespace is empty", "   ", []string{"srvA", "alpha", "beta", "srvB", "gamma"}},
		{"server name keeps whole group", "srva", []string{"srvA", "alpha", "beta"}},
		{"tool name keeps header + tool only", "alpha", []string{"srvA", "alpha"}},
		{"description match keeps header + tool", "second", []string{"srvA", "beta"}},
		{"match is case-insensitive", "GAMMA", []string{"srvB", "gamma"}},
		{"no match drops everything", "zzz", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := filterMCPItems(sample, tc.query)
			names := itemNames(got)
			if !sameStrings(names, tc.want) {
				t.Errorf("filterMCPItems(%q) = %v, want %v", tc.query, names, tc.want)
			}
		})
	}
}

// TestFilterMCPItems_PreservesGrouping asserts a matched tool is never orphaned
// from its server header: the row immediately preceding a kept tool row must be
// its group header.
func TestFilterMCPItems_PreservesGrouping(t *testing.T) {
	got := filterMCPItems(sampleMCPGroups(), "alpha")
	if len(got) != 2 {
		t.Fatalf("expected [srvA, alpha], got %v", itemNames(got))
	}
	if !got[0].group || got[0].name != "srvA" {
		t.Errorf("expected srvA header first, got %q group=%v", got[0].name, got[0].group)
	}
	if got[1].group || got[1].name != "alpha" {
		t.Errorf("expected alpha tool second, got %q group=%v", got[1].name, got[1].group)
	}
}

// TestFilterMCPItems_NilAndEmpty guards against panics on empty input.
func TestFilterMCPItems_NilAndEmpty(t *testing.T) {
	if got := filterMCPItems(nil, "x"); len(got) != 0 {
		t.Errorf("filterMCPItems(nil, x) = %v, want empty", itemNames(got))
	}
	if got := filterMCPItems(nil, ""); len(got) != 0 {
		t.Errorf("filterMCPItems(nil, empty) = %v, want empty", itemNames(got))
	}
}

// --- labels ---------------------------------------------------------------

func TestMCPListLabel(t *testing.T) {
	for _, tc := range []struct {
		name string
		item resourceItem
		want string
	}{
		{"header with usage", resourceItem{kind: resourceMCP, group: true, name: "srvA", usage: "2 tools"}, "srvA  (2 tools)"},
		{"header without usage", resourceItem{kind: resourceMCP, group: true, name: "srvA"}, "srvA"},
		{"tool row indented", resourceItem{kind: resourceMCP, group: false, name: "alpha"}, "  alpha"},
		// resourceListLabel routes resourceMCP to mcpListLabel regardless of
		// canToggle, so the bare-name row never carries a checkbox slot.
		{"resourceListLabel routes mcp tool", resourceItem{kind: resourceMCP, name: "alpha", canToggle: true}, "  alpha"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resourceListLabel(tc.item)
			if got != tc.want {
				t.Errorf("label = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResourceListLabel_ToolsUnchanged is a regression guard for criterion 3:
// Tools/Skills rows keep their checkbox + 22-col pad + usage tail, unaffected by
// the new resourceMCP branch.
func TestResourceListLabel_ToolsUnchanged(t *testing.T) {
	got := resourceListLabel(resourceItem{kind: resourceTools, name: "calc", enabled: true, canToggle: true, usage: "used:3"})
	want := "[x] calc" + strings.Repeat(" ", 22-4) + " used:3"
	if got != want {
		t.Errorf("tools label = %q, want %q", got, want)
	}
	disabled := resourceListLabel(resourceItem{kind: resourceTools, name: "sh", enabled: false, canToggle: true})
	if !strings.HasPrefix(disabled, "[ ] ") {
		t.Errorf("disabled tools label should start with [ ] : %q", disabled)
	}
}

// --- detail renderers -----------------------------------------------------

func TestMCPServerDetail(t *testing.T) {
	got := mcpServerDetail("srvA", []string{"alpha", "beta"})
	for _, want := range []string{
		"MCP server: srvA",
		"Tools: 2",
		" - alpha",
		" - beta",
		"mcp__srvA__<tool>",
		"callable by the agent",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("mcpServerDetail missing %q:\n%s", want, got)
		}
	}
}

func TestMCPToolDetail(t *testing.T) {
	desc := "Greet someone.\n\nWhen calling this tool, use its full name \"mcp__srvA__greet\"."
	got := mcpToolDetail("srvA", "greet", desc, `{"type":"object"}`)
	for _, want := range []string{
		"MCP tool: greet",
		"Server: srvA",
		"Registered as: mcp__srvA__greet",
		"Greet someone.",
		"Input schema",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("mcpToolDetail missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "When calling this tool") {
		t.Errorf("mcpToolDetail leaked the model trailer:\n%s", got)
	}
}

// TestMCPToolDetail_EmptySchema confirms an empty schema omits the section
// rather than printing an empty "Input schema" block.
func TestMCPToolDetail_EmptySchema(t *testing.T) {
	got := mcpToolDetail("srvA", "greet", "hi", "")
	if strings.Contains(got, "Input schema") {
		t.Errorf("empty schema should omit Input schema:\n%s", got)
	}
}

// --- empty state ----------------------------------------------------------

// TestMCPPlaceholder_NewWording verifies the empty state dropped the stale #36
// reference and points at the mcp_servers config key.
func TestMCPPlaceholder_NewWording(t *testing.T) {
	got := mcpPlaceholder()
	if strings.Contains(got, "#36") {
		t.Errorf("mcpPlaceholder still references #36: %q", got)
	}
	for _, want := range []string{"mcp_servers", "configured or connected"} {
		if !strings.Contains(got, want) {
			t.Errorf("mcpPlaceholder missing %q: %q", want, got)
		}
	}
	// The empty state is only for the genuinely-empty case: with a non-empty
	// item count emptyDetail must not return the placeholder.
	if emptyDetail(resourceMCP, 0, "abc") != "No matching items." {
		t.Errorf("search no-match should be 'No matching items.'")
	}
}

// --- render-path simulation (usability criterion) -------------------------

// TestMCPRenderSimulation_LongServerManyTools drives the exact pipeline render()
// uses (loadMCPItems -> filterMCPItems -> resourceListLabel) for a
// chrome-devtools-mcp-shaped server with several tools, asserting the server
// appears once as a header and every tool row shows its bare name (no server
// prefix eating the column) — the usability fix at the heart of issue #492.
func TestMCPRenderSimulation_LongServerManyTools(t *testing.T) {
	items := loadMCPItems(func() []ToolInfo {
		return mcpInfos(
			"mcp__chrome-devtools-mcp__navigate_page",
			"mcp__chrome-devtools-mcp__click",
			"mcp__chrome-devtools-mcp__fill",
			"mcp__chrome-devtools-mcp__performance_start_trace",
		)
	})
	visible := filterMCPItems(items, "")
	if len(visible) != 5 { // 1 header + 4 tools
		t.Fatalf("expected header + 4 tools, got %d: %v", len(visible), itemNames(visible))
	}
	// Exactly one server header, leading the group.
	if visible[0].name != "chrome-devtools-mcp" || !visible[0].group {
		t.Fatalf("expected chrome-devtools-mcp header first, got %q group=%v", visible[0].name, visible[0].group)
	}
	if got := resourceListLabel(visible[0]); got != "chrome-devtools-mcp  (4 tools)" {
		t.Errorf("header label = %q, want %q", got, "chrome-devtools-mcp  (4 tools)")
	}
	// Each tool row is the bare name, indented; the server prefix must not leak
	// into a tool row (that was the truncation defect).
	servers := 0
	for _, it := range visible[1:] {
		if it.group {
			servers++
		}
		label := resourceListLabel(it)
		if want := "  " + it.name; label != want {
			t.Errorf("tool %q label = %q, want bare %q", it.name, label, want)
		}
		if strings.Contains(label, "chrome-devtools-mcp") {
			t.Errorf("tool %q label should not embed the server: %q", it.name, label)
		}
	}
	if servers != 0 {
		t.Errorf("expected exactly one server header, found %d extra", servers)
	}
	// Searching a tool name keeps the server header attached (grouping survives).
	hit := filterMCPItems(items, "navigate_page")
	if !sameStrings(itemNames(hit), []string{"chrome-devtools-mcp", "navigate_page"}) {
		t.Errorf("search navigate_page = %v, want [chrome-devtools-mcp navigate_page]", itemNames(hit))
	}
}

// --- end-to-end stub-server test (required seam) --------------------------

// TestLoadMCPItems_EndToEndWithStubServer closes the
// registry -> GetTools -> ToolInfo -> loadMCPItems seam against a live (stub)
// MCP server: StartMCPServers dials the stub, registers mcp__demo__greet, and
// the deriver surfaces it under a server header with the schema and the
// agent-callable namespaced name.
func TestLoadMCPItems_EndToEndWithStubServer(t *testing.T) {
	ts := newStubMCPServer(t)
	defer ts.Close()

	g := gogent.NewGogent(t.TempDir())
	// Allow MCP launches (no interactive prompter is installed in tests).
	g.GetPermissionService().AddRule(permission.Rule{
		Action: string(permission.ActionMCP), Resource: "*", Effect: string(permission.EffectAllow),
	})
	g.GetConfig().MCPServers = []config.MCPServerConfig{
		{Name: "demo", Transport: "http", URL: ts.URL},
	}
	g.StartMCPServers()
	defer g.CloseMCPServers()

	// Build []ToolInfo exactly as the embedded GetTools handler does.
	getTools := func() []ToolInfo {
		reg := g.GetToolRegistry()
		if reg == nil {
			return nil
		}
		tools := reg.List()
		out := make([]ToolInfo, 0, len(tools))
		for _, tt := range tools {
			out = append(out, ToolInfo{
				Name:        tt.Name,
				Description: tt.Description,
				InputSchema: tool.SchemaJSON(tt.InputSchema),
			})
		}
		return out
	}

	items := loadMCPItems(getTools)
	if len(items) != 2 { // demo header + greet
		t.Fatalf("expected header + 1 tool, got %d: %v", len(items), itemNames(items))
	}
	if items[0].name != "demo" || !items[0].group {
		t.Errorf("expected demo header first, got %q group=%v", items[0].name, items[0].group)
	}
	if items[1].name != "greet" || items[1].group {
		t.Errorf("expected greet tool row, got %q group=%v", items[1].name, items[1].group)
	}
	// The tool is registered under the namespaced name (so it is agent-callable)
	// and its detail carries the schema and the registered-as line.
	registered := false
	for _, tt := range g.GetToolRegistry().List() {
		if tt.Name == "mcp__demo__greet" {
			registered = true
			break
		}
	}
	if !registered {
		t.Errorf("mcp__demo__greet not present in registry: not registered/agent-callable")
	}
	detail := items[1].detail
	for _, want := range []string{"Registered as: mcp__demo__greet", "who", "Input schema"} {
		if !strings.Contains(detail, want) {
			t.Errorf("tool detail missing %q:\n%s", want, detail)
		}
	}
}
