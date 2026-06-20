package ui

import (
	"strings"
	"testing"

	"gogent/internal/stats"
)

// findCommand returns the first command with the given name, or a zero command
// and false when none matches.
func findCommand(cmds []command, name string) (command, bool) {
	for _, c := range cmds {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

// commandNames extracts the names of a command slice in order.
func commandNames(cmds []command) []string {
	out := make([]string, len(cmds))
	for i, c := range cmds {
		out[i] = c.name
	}
	return out
}

// TestFuzzyScore covers subsequence matching, the empty/whitespace query
// (matches everything), case-insensitivity, and the non-match case.
func TestFuzzyScore(t *testing.T) {
	cases := []struct {
		pattern, text string
		wantOK        bool
	}{
		{"", "New session", true},
		{"   ", "New session", true},
		{"ns", "New session", true},
		{"NS", "New session", true},         // case-insensitive
		{"new", "New session", true},        // contiguous prefix
		{"sess", "New session", true},       // contiguous, offset
		{"nss", "New session", true},        // n, then the two s's of "session"
		{"newsession", "New session", true}, // every letter, spanning the space
		{"xyz", "New session", false},       // none present
		{"zsession", "New session", false},  // leading 'z' absent
	}
	for _, c := range cases {
		got, ok := fuzzyScore(c.pattern, c.text)
		if ok != c.wantOK {
			t.Errorf("fuzzyScore(%q, %q) ok = %v, want %v", c.pattern, c.text, ok, c.wantOK)
		}
		if ok && got < 0 {
			t.Errorf("fuzzyScore(%q, %q) score = %d, want >= 0", c.pattern, c.text, got)
		}
	}
}

// TestFuzzyScoreOrdering verifies the score ranks an earlier, tighter match above
// a later, looser one for the same pattern.
func TestFuzzyScoreOrdering(t *testing.T) {
	tight, ok1 := fuzzyScore("ns", "New session")          // n at 0, s at 4
	loose, ok2 := fuzzyScore("ns", "Close other sessions") // n later, big gap
	if !ok1 || !ok2 {
		t.Fatalf("expected both to match: %v %v", ok1, ok2)
	}
	if tight >= loose {
		t.Errorf("expected tighter match to score lower: tight=%d loose=%d", tight, loose)
	}
}

// TestCommandAvailability covers the visible/available predicates: a reference
// command (nil run) is visible but never offered in the palette; a gated command
// follows its predicate.
func TestCommandAvailability(t *testing.T) {
	cases := []struct {
		name          string
		cmd           command
		wantVisible   bool
		wantAvailable bool
	}{
		{"reference only", command{name: "x", keys: "?"}, true, false},
		{"runnable, no gate", command{name: "x", run: func() {}}, true, true},
		{"runnable, gate open", command{name: "x", run: func() {}, enabled: func() bool { return true }}, true, true},
		{"runnable, gate shut", command{name: "x", run: func() {}, enabled: func() bool { return false }}, false, false},
		{"reference, gate shut", command{name: "x", enabled: func() bool { return false }}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cmd.visible(); got != c.wantVisible {
				t.Errorf("visible() = %v, want %v", got, c.wantVisible)
			}
			if got := c.cmd.available(); got != c.wantAvailable {
				t.Errorf("available() = %v, want %v", got, c.wantAvailable)
			}
		})
	}
}

// TestFilterCommands covers the empty query (all available, table order), fuzzy
// narrowing with best-first ordering, exclusion of unavailable commands, and the
// no-match case.
func TestFilterCommands(t *testing.T) {
	cmds := []command{
		{category: "Session", name: "New session", run: func() {}},
		{category: "Session", name: "Close session", run: func() {}},
		{category: "Session", name: "Close other sessions", run: func() {}},
		{category: "App", name: "Command palette"},                                                        // reference only
		{category: "Config", name: "Theme editor", run: func() {}, enabled: func() bool { return false }}, // gated off
	}

	// Empty query lists every available command in table order (reference-only and
	// gated-off commands dropped).
	got := commandNames(filterCommands(cmds, ""))
	want := []string{"New session", "Close session", "Close other sessions"}
	if !sameStrings(got, want) {
		t.Fatalf("empty query = %v, want %v", got, want)
	}

	// A fuzzy query narrows and ranks the closest match first.
	got = commandNames(filterCommands(cmds, "ns"))
	if len(got) == 0 || got[0] != "New session" {
		t.Fatalf("query %q = %v, want \"New session\" first", "ns", got)
	}

	// No match yields an empty result.
	if got := filterCommands(cmds, "zzzz"); len(got) != 0 {
		t.Fatalf("non-matching query = %v, want empty", got)
	}
}

// TestCommandsTableShape checks the central table's integrity: every entry has a
// category and name, and the table exposes the expected core actions with their
// key hints.
func TestCommandsTableShape(t *testing.T) {
	w := &Workbench{}
	cmds := w.commands()
	if len(cmds) == 0 {
		t.Fatal("commands() returned no entries")
	}
	for _, c := range cmds {
		if strings.TrimSpace(c.category) == "" || strings.TrimSpace(c.name) == "" {
			t.Errorf("command %+v missing category or name", c)
		}
	}
	for _, name := range []string{"New session", "Close session", "Find in transcript", "Quit"} {
		if _, ok := findCommand(cmds, name); !ok {
			t.Errorf("expected command %q in table", name)
		}
	}
	if c, _ := findCommand(cmds, "New session"); c.keys != "Ctrl+N" {
		t.Errorf("New session keys = %q, want Ctrl+N", c.keys)
	}
}

// TestCommandsHandlerGating confirms handler-gated commands are hidden from the
// palette without their backend wiring and surface once it is present.
func TestCommandsHandlerGating(t *testing.T) {
	bare := filterCommands((&Workbench{}).commands(), "")
	if _, ok := findCommand(bare, "Saved sessions browser"); ok {
		t.Error("Saved sessions browser should be hidden without ListSavedSessions handler")
	}
	if _, ok := findCommand(bare, "Statistics"); ok {
		t.Error("Statistics should be hidden without GetStatistics handler")
	}

	wired := &Workbench{handlers: Handlers{
		ListSavedSessions: func() []SessionMeta { return nil },
		GetStatistics:     func() stats.Report { return stats.Report{} },
	}}
	got := filterCommands(wired.commands(), "")
	if _, ok := findCommand(got, "Saved sessions browser"); !ok {
		t.Error("Saved sessions browser should appear once ListSavedSessions is wired")
	}
	if _, ok := findCommand(got, "Statistics"); !ok {
		t.Error("Statistics should appear once GetStatistics is wired")
	}
}

// TestHelpText verifies the cheatsheet groups by category in table order,
// includes each binding's key hint and name, and omits handler-gated commands
// whose backend is absent.
func TestHelpText(t *testing.T) {
	text := helpText((&Workbench{}).commands())
	for _, want := range []string{"Session", "Transcript", "Config", "App", "Ctrl+N", "New session", "Quit"} {
		if !strings.Contains(text, want) {
			t.Errorf("helpText missing %q\n%s", want, text)
		}
	}
	// Session must appear before App (table grouping is preserved).
	if strings.Index(text, "Session\n") > strings.Index(text, "App\n") {
		t.Error("expected Session group before App group in help text")
	}
	// A gated-off command (no handler) is not listed.
	if strings.Contains(text, "Saved sessions browser") {
		t.Error("helpText should omit Saved sessions browser without its handler")
	}
}

// TestFormatCommandRow checks the palette row layout: the name is padded to the
// fixed column and the key hint follows, with no trailing padding when unbound.
func TestFormatCommandRow(t *testing.T) {
	withKeys := formatCommandRow(command{name: "New session", keys: "Ctrl+N"})
	if !strings.Contains(withKeys, "New session") || !strings.HasSuffix(withKeys, "Ctrl+N") {
		t.Errorf("row with keys = %q", withKeys)
	}
	noKeys := formatCommandRow(command{name: "Models"})
	if noKeys != "Models" {
		t.Errorf("row without keys = %q, want %q", noKeys, "Models")
	}
}

// TestCommandRunWiring exercises the table end-to-end: invoking the "New session"
// command's action on a real (headless) workbench opens a window, proving the
// palette entries drive the existing handlers rather than being inert labels.
func TestCommandRunWiring(t *testing.T) {
	w := newTestWorkbench(t)
	before := len(w.orderIDs())
	c, ok := findCommand(w.commands(), "New session")
	if !ok {
		t.Fatal("New session command missing")
	}
	c.run()
	if after := len(w.orderIDs()); after != before+1 {
		t.Fatalf("session count after New session = %d, want %d", after, before+1)
	}
}

// TestCommandPaletteOpensAndCloses confirms the palette modal is added on top and
// that the help overlay opens its own layer, using a real headless desktop.
func TestCommandPaletteOpensAndCloses(t *testing.T) {
	w := newTestWorkbench(t)

	w.showCommandPalette()
	if top := w.desktop.TopLayer(); top == nil || top.Name != "command-palette" {
		t.Fatalf("top layer after palette = %v, want command-palette", top)
	}

	w.showHelpOverlay()
	if top := w.desktop.TopLayer(); top == nil || top.Name != "help-overlay" {
		t.Fatalf("top layer after help = %v, want help-overlay", top)
	}
}
