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

// TestInfoDialogsOpenAtTop covers issue #174 Phase A: info/help/detail dialogs
// must open anchored at the TOP rather than following the bottom like chat/logs.
// The TextView's scroll state (scrollY/follow) is unexported, so this asserts the
// observable behaviour — that each info view constructs and opens its layer via
// the harness without panicking on the ScrollToTop() call wired in after the body
// is populated. The top-anchoring itself is covered by turbotui's own
// TestTextViewScrollToTopAnchorsAtTop.
func TestInfoDialogsOpenAtTop(t *testing.T) {
	w := newTestWorkbench(t)

	// Help overlay: a long, multi-line body that overflows and so genuinely
	// exercises ScrollToTop (a non-scrolling body would be at the top anyway).
	w.showHelpOverlay()
	if top := w.desktop.TopLayer(); top == nil || top.Name != "help-overlay" {
		t.Fatalf("top layer after help = %v, want help-overlay", top)
	}

	// Informational message dialog (onResult nil → OK-only): the body is filled
	// line by line and then anchored at the top. A long, overflowing body makes
	// the ScrollToTop call meaningful (a short body sits at the top regardless).
	long := strings.Repeat("line\n", 200)
	w.showConfirm("Info", long, nil)
	if top := w.desktop.TopLayer(); top == nil || top.Name != "confirm-dialog" {
		t.Fatalf("top layer after info message = %v, want confirm-dialog", top)
	}
}

// TestCloseableDialogAffordance verifies the visible close affordance (issue
// #173): the dialog exposes a title-bar [x] (ShowClose) whose OnClose runs the
// caller's close function, so the palette/help overlay no longer rely on the
// user guessing Esc.
func TestCloseableDialogAffordance(t *testing.T) {
	closed := false
	d := newCloseableDialog("X", 0, 0, 20, 10, func() { closed = true })
	if !d.Window.ShowClose {
		t.Error("ShowClose should be true so the [x] button is drawn")
	}
	if d.Window.OnClose == nil {
		t.Fatal("OnClose must be wired to the close function")
	}
	d.Window.OnClose(d.Window) // simulate the title-bar [x] click
	if !closed {
		t.Error("clicking [x] did not invoke the close function")
	}
}

// TestRunningTurnCommandsInPalette verifies the discoverability half of issue
// #201: /stop, /clearqueue, /goal and /markdown are present in the Session group
// with their slash-command key hints, and carry actions that are always available
// (no handler gate), so they show up in the palette even on a bare workbench.
func TestRunningTurnCommandsInPalette(t *testing.T) {
	cmds := (&Workbench{}).commands()
	want := map[string]string{
		"Stop turn":                    "/stop",
		"Clear queued message":         "/clearqueue",
		"Set / show goal (supervisor)": "/goal",
		"Toggle Markdown rendering":    "/markdown",
	}
	for name, keys := range want {
		c, ok := findCommand(cmds, name)
		if !ok {
			t.Errorf("expected command %q in the table for issue #201", name)
			continue
		}
		if c.category != "Session" {
			t.Errorf("%q category = %q, want Session", name, c.category)
		}
		if c.keys != keys {
			t.Errorf("%q keys = %q, want %q", name, c.keys, keys)
		}
		if c.run == nil {
			t.Errorf("%q has no run action", name)
		}
		// These are always available (no enabled predicate), unlike handler-gated
		// commands such as "Statistics".
		if !c.available() {
			t.Errorf("%q should be available without backend wiring", name)
		}
	}
}

// TestRunningTurnCommandsNotHiddenFromHelp confirms the new commands surface in
// the keybinding cheatsheet too (issue #201), since helpText lists every visible
// command and they carry no availability gate.
func TestRunningTurnCommandsNotHiddenFromHelp(t *testing.T) {
	text := helpText((&Workbench{}).commands())
	for _, name := range []string{"Stop turn", "Clear queued message", "Toggle Markdown rendering"} {
		if !strings.Contains(text, name) {
			t.Errorf("helpText missing %q\n%s", name, text)
		}
	}
	// The /stop key hint is shown alongside the name.
	if !strings.Contains(text, "/stop") {
		t.Errorf("helpText should show the /stop key hint\n%s", text)
	}
}

// TestPaletteStopCommandRunsAgainstActiveSession is an end-to-end check that the
// "Stop turn" palette entry dispatches to the active session (issue #201): on a
// workbench with a busy session holding a queued message, running the command
// cancels the turn and clears the queue, exactly like /stop.
func TestPaletteStopCommandRunsAgainstActiveSession(t *testing.T) {
	w := newTestWorkbench(t)
	stopped := recordStop(w)
	sw := w.openWindow("s", "S")
	sw.busy = true
	sw.enqueue("queued")
	if w.ActiveID() != "s" {
		t.Fatalf("active session = %q, want s", w.ActiveID())
	}

	c, ok := findCommand(w.commands(), "Stop turn")
	if !ok {
		t.Fatal("Stop turn command missing")
	}
	c.run()

	if sw.pending != "" {
		t.Errorf("palette Stop should clear the queue, pending = %q", sw.pending)
	}
	if id := waitStop(t, stopped); id != "s" {
		t.Errorf("OnStop id = %q, want s", id)
	}
}

// TestPaletteClearQueueCommandRunsAgainstActiveSession verifies the "Clear queued
// message" palette entry clears the active session's queue (issue #201).
func TestPaletteClearQueueCommandRunsAgainstActiveSession(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	sw.enqueue("to be cleared")

	c, ok := findCommand(w.commands(), "Clear queued message")
	if !ok {
		t.Fatal("Clear queued message command missing")
	}
	c.run()

	if sw.pending != "" {
		t.Errorf("palette Clear queued message should clear the queue, pending = %q", sw.pending)
	}
	if !noteContains(sw, "cleared") {
		t.Error("expected a 'cleared' note after clearing via the palette")
	}
}

// TestPaletteCommandNoOpWithoutActiveSession verifies the running-turn palette
// commands are safe when no session is open: withActiveTranscript is a no-op, so
// running them neither panics nor dispatches (issue #201).
func TestPaletteCommandNoOpWithoutActiveSession(t *testing.T) {
	w := newTestWorkbench(t) // no sessions opened
	for _, name := range []string{"Stop turn", "Clear queued message", "Set / show goal (supervisor)"} {
		c, ok := findCommand(w.commands(), name)
		if !ok {
			t.Fatalf("%s command missing", name)
		}
		// Must not panic with no active session.
		c.run()
	}
}

// TestPaletteGoalCommandOpensEditor verifies the "/goal" palette entry actually
// lets the user set the goal (issue #201): it opens an input dialog seeded with
// the current goal (editActiveGoal), not the read-only inline show that a bare
// sessionCmd("/goal") would produce. The three no-arg commands still act inline.
func TestPaletteGoalCommandOpensEditor(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.goal = "ship it"

	c, ok := findCommand(w.commands(), "Set / show goal (supervisor)")
	if !ok {
		t.Fatal("Set / show goal (supervisor) command missing")
	}
	c.run()

	top := w.desktop.TopLayer()
	if top == nil || top.Name != "input-dialog" {
		t.Fatalf("top layer = %v, want an input-dialog to edit the goal", top)
	}
}

// TestPaletteGoalCommandNoOpWithoutSession verifies editActiveGoal is a safe
// no-op (no dialog, no panic) when no session is open (issue #201).
func TestPaletteGoalCommandNoOpWithoutSession(t *testing.T) {
	w := newTestWorkbench(t) // no sessions opened
	c, ok := findCommand(w.commands(), "Set / show goal (supervisor)")
	if !ok {
		t.Fatal("Set / show goal (supervisor) command missing")
	}
	c.run()
	if top := w.desktop.TopLayer(); top != nil && top.Name == "input-dialog" {
		t.Error("goal editor should not open with no active session")
	}
}
