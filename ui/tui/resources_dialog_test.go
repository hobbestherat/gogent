package ui

import (
	"reflect"
	"strings"
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// sameStrings reports whether two string slices hold the same values in order,
// treating nil and an empty slice as equal.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestFilterResources covers case-insensitive name/description matching, the
// all-match empty query, and the no-match case across resource kinds.
func TestFilterResources(t *testing.T) {
	items := []resourceItem{
		{kind: resourceTools, name: "shell", desc: "Execute shell commands"},
		{kind: resourceTools, name: "calc", desc: "Calculate math expressions"},
		{kind: resourceSkills, name: "writer", desc: "Long-form writing helper"},
	}
	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{"empty returns all", "", []string{"shell", "calc", "writer"}},
		{"whitespace is empty", "   ", []string{"shell", "calc", "writer"}},
		{"name match case-insensitive", "CALC", []string{"calc"}},
		{"description match", "math", []string{"calc"}},
		{"across kinds", "write", []string{"writer"}},
		{"no match", "zzz", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := filterResources(items, tc.query)
			names := make([]string, 0, len(got))
			for _, it := range got {
				names = append(names, it.name)
			}
			if !sameStrings(names, tc.want) {
				t.Errorf("filterResources(%q) = %v, want %v", tc.query, names, tc.want)
			}
		})
	}
}

// TestResourceListLabel covers the on/off checkbox prefix, name padding, the
// usage tail and long-name truncation.
func TestResourceListLabel(t *testing.T) {
	for _, tc := range []struct {
		name string
		item resourceItem
		want string
	}{
		{
			"enabled tool checked box",
			resourceItem{kind: resourceTools, name: "calc", enabled: true, canToggle: true},
			"[x] calc" + strings.Repeat(" ", 22-4),
		},
		{
			"enabled tool with usage",
			resourceItem{kind: resourceTools, name: "shell", enabled: true, canToggle: true, usage: "used:12"},
			"[x] shell" + strings.Repeat(" ", 22-5) + " used:12",
		},
		{
			"disabled tool empty box",
			resourceItem{kind: resourceTools, name: "shell", enabled: false, canToggle: true, usage: "used:1"},
			"[ ] shell" + strings.Repeat(" ", 22-5) + " used:1",
		},
		{
			"non-togglable aligned no box",
			resourceItem{kind: resourceMCP, name: "server", canToggle: false},
			"    server" + strings.Repeat(" ", 22-6),
		},
		{
			"long name truncated to width",
			resourceItem{kind: resourceTools, name: "a-very-long-tool-name-here", enabled: true, canToggle: true},
			"[x] " + "a-very-long-tool-name-here"[:22],
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resourceListLabel(tc.item); got != tc.want {
				t.Errorf("resourceListLabel = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestToolDetail covers the header, enabled/disabled state, the used/last line
// and that the description and schema are included.
func TestToolDetail(t *testing.T) {
	schema := `{
  "type": "object"
}`
	got := toolDetail("calc", "Calculate math.", schema, true, 5, "2026-06-19 09:00")
	for _, want := range []string{
		"Tool: calc",
		"State: enabled",
		"Used: 5 (last 2026-06-19 09:00)",
		"Description",
		"Calculate math.",
		"Input schema",
		`"type": "object"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("toolDetail missing %q in:\n%s", want, got)
		}
	}

	// Disabled state + no last-used time collapses the used line.
	disabled := toolDetail("shell", "Run commands.", "", false, 0, "")
	if !strings.Contains(disabled, "State: disabled") || !strings.Contains(disabled, "Used: 0\n") {
		t.Errorf("disabled toolDetail unexpected:\n%s", disabled)
	}
	if strings.Contains(disabled, "Input schema") {
		t.Errorf("empty schema should be omitted:\n%s", disabled)
	}
}

// TestSkillDetail covers the header, active/inactive state, the usage line, the
// on-disk file path and the SKILL.md preview.
func TestSkillDetail(t *testing.T) {
	got := skillDetail("writer", "Writing helper.", "/home/u/.gogent/skills/writer/SKILL.md",
		true, 3, 1, 4, "---\nname: writer\n---\nBody text.")
	for _, want := range []string{
		"Skill: writer",
		"State: active",
		"Usage: 3 ok / 1 fail (4 total)",
		"File: /home/u/.gogent/skills/writer/SKILL.md",
		"Description",
		"Writing helper.",
		"SKILL.md",
		"Body text.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skillDetail missing %q in:\n%s", want, got)
		}
	}

	// Inactive + empty content/empty path omit the preview and the file line.
	inactive := skillDetail("writer", "Writing helper.", "", false, 0, 0, 0, "")
	if !strings.Contains(inactive, "State: inactive") {
		t.Errorf("expected inactive state:\n%s", inactive)
	}
	if strings.Contains(inactive, "SKILL.md") {
		t.Errorf("empty content should omit SKILL.md preview:\n%s", inactive)
	}
	if strings.Contains(inactive, "File:") {
		t.Errorf("empty path should omit the file line:\n%s", inactive)
	}
}

// TestUsageStrings covers the usage tail formatters.
func TestUsageStrings(t *testing.T) {
	if toolUsage(0) != "" {
		t.Errorf("toolUsage(0) = %q, want empty", toolUsage(0))
	}
	if got, want := toolUsage(7), "used:7"; got != want {
		t.Errorf("toolUsage(7) = %q, want %q", got, want)
	}
	if got, want := skillUsage(3, 1), "ok:3 fail:1"; got != want {
		t.Errorf("skillUsage(3,1) = %q, want %q", got, want)
	}
}

// TestEmptyDetail covers the per-tab placeholder messages and the search
// no-match fallback.
func TestEmptyDetail(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  resourceKind
		count int
		query string
		want  string
	}{
		{"mcp placeholder", resourceMCP, 0, "", "#36"},
		{"tools none", resourceTools, 0, "", "No tools are registered."},
		{"skills none", resourceSkills, 0, "", "No skills are loaded."},
		{"search no match", resourceTools, 0, "abc", "No matching items."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := emptyDetail(tc.kind, tc.count, tc.query)
			if !strings.Contains(got, tc.want) {
				t.Errorf("emptyDetail(%v,%d,%q) = %q, want to contain %q", tc.kind, tc.count, tc.query, got, tc.want)
			}
		})
	}
}

// TestLoadToolItems verifies the backend ToolInfo list is mapped to sorted
// browser items whose detail embeds the schema and state.
func TestLoadToolItems(t *testing.T) {
	get := func() []ToolInfo {
		return []ToolInfo{
			{Name: "zebra", Description: "z", InputSchema: `{"type":"object"}`, Enabled: true, Invocations: 0},
			{Name: "alpha", Description: "a", InputSchema: "", Enabled: false, Invocations: 9, LastUsed: "2026-01-01 00:00"},
		}
	}
	items := loadToolItems(get)
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0].name != "alpha" || items[1].name != "zebra" {
		t.Fatalf("expected sorted order alpha,zebra; got %s,%s", items[0].name, items[1].name)
	}
	// alpha is disabled → its label carries an empty checkbox.
	if label := resourceListLabel(items[0]); !strings.Contains(label, "[ ]") || strings.Contains(label, "[x]") {
		t.Errorf("disabled item should show [ ] checkbox: %q", label)
	}
	// The enabled tool's detail embeds its schema.
	if !strings.Contains(items[1].detail, `"type":"object"`) {
		t.Errorf("zebra detail should embed schema: %q", items[1].detail)
	}
	// usage tail reflects invocations.
	if items[0].usage != "used:9" {
		t.Errorf("alpha usage = %q, want used:9", items[0].usage)
	}
	if items[1].usage != "" {
		t.Errorf("zebra usage = %q, want empty", items[1].usage)
	}
}

// TestLoadSkillItems verifies sorting and that the SKILL.md content is carried
// into the detail preview.
func TestLoadSkillItems(t *testing.T) {
	get := func() []SkillInfo {
		return []SkillInfo{
			{Name: "beta", Description: "b", Active: false, Success: 1, Failure: 0, TotalCalls: 1},
			{Name: "alpha", Description: "a", Active: true, Success: 2, Failure: 2, TotalCalls: 4, Content: "do the thing"},
		}
	}
	items := loadSkillItems(get)
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0].name != "alpha" || items[1].name != "beta" {
		t.Fatalf("expected sorted alpha,beta; got %s,%s", items[0].name, items[1].name)
	}
	if !strings.Contains(items[0].detail, "do the thing") {
		t.Errorf("alpha detail should include SKILL.md content: %q", items[0].detail)
	}
	if items[0].usage != "ok:2 fail:2" {
		t.Errorf("alpha usage = %q, want ok:2 fail:2", items[0].usage)
	}
}

// TestLoadItemsNilGetter ensures a missing backend handler degrades to no items
// rather than panicking.
func TestLoadItemsNilGetter(t *testing.T) {
	if items := loadToolItems(nil); items != nil {
		t.Errorf("loadToolItems(nil) = %v, want nil", items)
	}
	if items := loadSkillItems(nil); items != nil {
		t.Errorf("loadSkillItems(nil) = %v, want nil", items)
	}
}

// TestResourcesDialogSize covers clamping to the terminal and the min floors.
func TestResourcesDialogSize(t *testing.T) {
	for _, tc := range []struct {
		name         string
		screenW, H   int
		wantW, wantH int
	}{
		{"large screen caps at 96x32", 200, 100, 96, 32},
		{"fits narrow terminal", 70, 30, 68, 28},
		{"short terminal floors height", 120, 16, 96, 14},
		{"tiny terminal floors both", 50, 20, 60, 18},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotW, gotH := resourcesDialogSize(tc.screenW, tc.H)
			if gotW != tc.wantW || gotH != tc.wantH {
				t.Errorf("resourcesDialogSize(%d,%d) = %dx%d, want %dx%d",
					tc.screenW, tc.H, gotW, gotH, tc.wantW, tc.wantH)
			}
		})
	}
}

// TestSortResourceItems covers ordering by kind then name.
func TestSortResourceItems(t *testing.T) {
	items := []resourceItem{
		{kind: resourceSkills, name: "z"},
		{kind: resourceTools, name: "b"},
		{kind: resourceSkills, name: "a"},
		{kind: resourceTools, name: "a"},
	}
	sortResourceItems(items)
	kindName := func(k resourceKind) string {
		switch k {
		case resourceTools:
			return "Tools"
		case resourceSkills:
			return "Skills"
		default:
			return "MCP"
		}
	}
	got := make([]string, len(items))
	for i, it := range items {
		got[i] = kindName(it.kind) + ":" + it.name
	}
	want := []string{
		"Tools:a", "Tools:b",
		"Skills:a", "Skills:z",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sortResourceItems = %v, want %v", got, want)
	}
}

// TestSelectionColorsFor covers the collision fallback (invert the dialog
// colours so a selected row is visible) and passthrough for themes whose
// selection already differs from the dialog chrome.
func TestSelectionColorsFor(t *testing.T) {
	black := tui.ANSIColor(0)
	white := tui.ANSIColor(7)
	accent := tui.ANSIColor(11)
	def := tui.DefaultColor()
	for _, tc := range []struct {
		name                             string
		dialogFG, dialogBG, selFG, selBG tui.Color
		wantFG, wantBG                   tui.Color
	}{
		{
			"collision inverts dialog colours",
			black, white, black, white, // selection == dialog → invisible
			white, black,
		},
		{
			"distinct background passes through",
			black, white, black, accent,
			black, accent,
		},
		{
			"distinct foreground passes through",
			black, white, accent, white,
			accent, white,
		},
		{
			"no-colour defaults stay default",
			def, def, def, def,
			def, def,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotFG, gotBG := selectionColorsFor(tc.dialogFG, tc.dialogBG, tc.selFG, tc.selBG)
			if gotFG != tc.wantFG || gotBG != tc.wantBG {
				t.Errorf("selectionColorsFor = (FG %+v, BG %+v), want (FG %+v, BG %+v)",
					gotFG, gotBG, tc.wantFG, tc.wantBG)
			}
		})
	}
}

// TestBindToggleKeys verifies that Space and Enter are the explicit toggle
// gesture, while Escape bubbles up (so the dialog can close) and arrow
// navigation is delegated to the tree — none of which toggle.
func TestBindToggleKeys(t *testing.T) {
	list := tv.NewTree(tv.Rect{X: 0, Y: 0, W: 20, H: 5})
	list.AddRoot(tv.NewTreeNode("alpha"))
	var toggled int
	bindToggleKeys(list, func() { toggled++ })
	c := list.Component

	for name, ev := range map[string]tui.TypeEvent{
		"space": {Key: tui.KeyRune, Rune: ' '},
		"enter": {Key: tui.KeyEnter},
	} {
		toggled = 0
		if !c.BubbleType(ev) {
			t.Errorf("%s: expected the key to be consumed", name)
		}
		if toggled != 1 {
			t.Errorf("%s: expected toggle to fire once, got %d", name, toggled)
		}
	}

	// Escape is not consumed here (it must bubble to the dialog to close it) and
	// never toggles.
	toggled = 0
	if c.BubbleType(tui.TypeEvent{Key: tui.KeyEscape}) {
		t.Errorf("Escape should bubble up to the dialog, not be consumed by the list")
	}
	if toggled != 0 {
		t.Errorf("Escape must not toggle, got %d", toggled)
	}

	// Arrow navigation delegates to the tree (consumed) without toggling.
	toggled = 0
	if !c.BubbleType(tui.TypeEvent{Key: tui.KeyDown}) {
		t.Errorf("Down should be delegated to the tree and consumed")
	}
	if toggled != 0 {
		t.Errorf("Down must not toggle, got %d", toggled)
	}
}
