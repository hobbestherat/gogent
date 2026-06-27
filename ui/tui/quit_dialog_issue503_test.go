package ui

// Regression + coverage tests for the daemon-aware quit dialog (issue #503).
//
// Design criteria under test:
//   (1) GOAL MATCH — the dialog branches on DaemonMode into the six states with
//       the specified title/body/buttons/default-focus; attached warns the daemon
//       survives (local also offers Stop & quit), embedded warns quitting kills
//       everything (offers Start daemon & quit), nil falls back to today's box.
//   (2) USABILITY — opens instantly, enriches in place, never blocks on a
//       round-trip; default focus Cancel (No for nil); explicit buttons only.
//   (3) NO REGRESSIONS — w.quit() semantics unchanged; all gestures route through
//       confirmQuit; graceful degradation when handlers are nil; the factored
//       runStopDaemon/runStartDaemon keep the menu path identical (#478).
//   (4) HOLISTIC — confined to ui/tui; reuses newMessageLayer/showProgress/handoff;
//       the &-in-labels is escaped so turbotui's mnemonic parser renders them.
//
// Note on observability: turbotui exposes no public path from a VisualComponent
// back to its Widget, so the rendered body TextView's text and the Button labels
// cannot be read off the dialog. We therefore (a) assert the pure buildQuitModel
// for the exact title/body/labels (it IS the source the dialog renders), and (b)
// drive the real dialog via BubbleType(Enter) / OnTypeFn(Escape) for the action
// semantics. The in-place enriched body text itself is asserted at the unit level
// (buildQuitModel enriched + rewriteBody), since the live body TextView is a local
// in showQuitDialog and is not reachable from a test.

import (
	"fmt"
	"strings"
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"

	"gogent/internal/config"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// newQuitWorkbench builds a headless workbench with the given daemon Handlers and a
// wide terminal so the three-button row lays out without the narrow-terminal drop
// (tests that want the drop call w.app.Resize themselves afterwards).
func newQuitWorkbench(t *testing.T, h Handlers) *Workbench {
	t.Helper()
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	w.SetHandlers(h)
	w.app.Resize(120, 40)
	return w
}

// quitTriggered reports whether the workbench's quit (w.quit, the sole teardown)
// has been invoked, by observing the shutdown context it cancels.
func quitTriggered(w *Workbench) bool { return w.shutdown.Err() != nil }

// quitButtons returns the quit dialog's button components in left-to-right (model)
// order. A button is the only component whose OnActivateFn is set — turbotui's
// NewButton wires it to the press hook, while the body TextView never does (it has
// no activation). This is deterministic and theme-independent, so len(result) ==
// len(model.Buttons).
func quitButtons(w *Workbench) []*tv.VisualComponent {
	var out []*tv.VisualComponent
	var walk func(*tv.VisualComponent)
	walk = func(c *tv.VisualComponent) {
		if c == nil {
			return
		}
		if c.OnActivateFn != nil {
			out = append(out, c)
		}
		for _, ch := range c.Children() {
			walk(ch)
		}
	}
	if w.quitDialogLayer != nil {
		walk(w.quitDialogLayer.Root)
	}
	return out
}

// quitOpenModel rebuilds the exact model showQuitDialog built (same inputs) so a
// test can map a button kind to its position in the rendered row.
func quitOpenModel(w *Workbench) quitDialogModel {
	return buildQuitModel(
		w.handlers.DaemonMode(),
		DaemonStatusReport{}, false,
		w.reconnectHost, w.reconnectAddress(),
		w.handlers.StopDaemon != nil, w.handlers.StartDaemon != nil,
	)
}

// pressQuitButton focuses and activates (Enter) the quit-dialog button of the given
// kind, mirroring what the desktop does on a keypress. It mirrors showQuitDialog's
// narrow drop (keyed off the content-sized dialog width) so it maps the kind to the
// correct component whether or not the middle handoff button was dropped. The kind
// must actually be rendered (Quit client and Cancel always are; Stop/Start are only
// when the row fits).
func pressQuitButton(t *testing.T, w *Workbench, kind quitButtonKind) {
	t.Helper()
	m := quitOpenModel(w)
	rendered := m.Buttons
	if len(rendered) == 3 {
		dw := w.quitDialogLayer.Root.Bounds.W
		if !quitButtonRowFits(dw, quitLabels(rendered)...) {
			rendered = []quitButton{rendered[0], rendered[2]} // drop the middle
		}
	}
	btns := quitButtons(w)
	if len(btns) != len(rendered) {
		t.Fatalf("model renders %d buttons but dialog has %d", len(rendered), len(btns))
	}
	idx := -1
	for i, qb := range rendered {
		if qb.Kind == kind {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("button kind %d is not rendered (dropped on this width)", kind)
	}
	comp := btns[idx]
	w.desktop.SetFocus(comp)
	if !comp.BubbleType(tui.TypeEvent{Key: tui.KeyEnter}) {
		t.Fatalf("quit button kind %d (%q) did not consume Enter", kind, rendered[idx].Label)
	}
}

// sendEscape mimics the desktop dispatching Escape to the quit dialog's root, which
// showQuitDialog wires to dismiss (Cancel).
func sendEscape(t *testing.T, w *Workbench) {
	t.Helper()
	if w.quitDialogLayer == nil || w.quitDialogLayer.Root.OnTypeFn == nil {
		t.Fatal("quit dialog has no Escape (OnTypeFn) handler")
	}
	w.quitDialogLayer.Root.OnTypeFn(w.quitDialogLayer.Root, tui.TypeEvent{Key: tui.KeyEscape})
}

// labelsOf extracts the ordered labels from a button slice.
func labelsOf(btns []quitButton) []string {
	out := make([]string, len(btns))
	for i, b := range btns {
		out[i] = b.Label
	}
	return out
}

// hasLabel reports whether the model has a button whose label renders as want (i.e.
// the &&-escaped source label, which is what newButton receives).
func hasLabel(btns []quitButton, want string) bool {
	for _, b := range btns {
		if b.Label == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// (1) GOAL MATCH — pure buildQuitModel over the six states + edges
// ---------------------------------------------------------------------------

func TestIssue503BuildModelAttachedLocalEnriched(t *testing.T) {
	report := DaemonStatusReport{
		LiveSessions: 3, Watchers: 1, MCPServers: []string{"git", "fs"},
	}
	m := buildQuitModel(DaemonModeAttachedLocal, report, true, "", "", true, false)

	if m.Title != "Quit Gogent (daemon stays running)" {
		t.Fatalf("title = %q", m.Title)
	}
	wantBody := "Quitting closes this TUI client only.\n" +
		"The local daemon keeps running:\n" +
		"\n" +
		"  • 3 live sessions\n" +
		"  • 1 watcher\n" +
		"  • 2 MCP servers\n" +
		"\n" +
		"Re-attach later with:  gogent"
	if m.Body != wantBody {
		t.Fatalf("enriched local body mismatch:\nwant:\n%s\ngot:\n%s", wantBody, m.Body)
	}
	if got := labelsOf(m.Buttons); !equalStrings(got, []string{"Quit client", "Stop daemon && quit", "Cancel"}) {
		t.Fatalf("local buttons = %v", got)
	}
	// The literal "&" must be escaped as "&&" for turbotui's mnemonic parser.
	if !hasLabel(m.Buttons, "Stop daemon && quit") {
		t.Fatal("Stop label must be &&-escaped so ParseMnemonic renders one '&'")
	}
	if m.DefaultIdx != 2 || m.Buttons[m.DefaultIdx].Kind != quitCancel {
		t.Fatalf("default idx = %d (kind %d), want Cancel", m.DefaultIdx, m.Buttons[m.DefaultIdx].Kind)
	}
}

func TestIssue503BuildModelAttachedLocalFallback(t *testing.T) {
	m := buildQuitModel(DaemonModeAttachedLocal, DaemonStatusReport{}, false, "", "", true, false)

	if m.Title != "Quit Gogent (daemon stays running)" {
		t.Fatalf("title = %q", m.Title)
	}
	wantBody := "Quitting closes this TUI client only.\n" +
		"The local daemon keeps running — your sessions, watchers and\n" +
		"MCP servers continue in the background.\n" +
		"\n" +
		"Re-attach later with:  gogent"
	if m.Body != wantBody {
		t.Fatalf("fallback local body mismatch:\nwant:\n%s\ngot:\n%s", wantBody, m.Body)
	}
	// Fallback body must NOT contain enriched counts/bullets.
	if strings.Contains(m.Body, "•") {
		t.Fatalf("fallback body should have no count bullets:\n%s", m.Body)
	}
	if got := labelsOf(m.Buttons); !equalStrings(got, []string{"Quit client", "Stop daemon && quit", "Cancel"}) {
		t.Fatalf("local buttons = %v", got)
	}
	if m.DefaultIdx != 2 {
		t.Fatalf("default = %d, want 2 (Cancel)", m.DefaultIdx)
	}
}

func TestIssue503BuildModelAttachedLocalOmitsStopWhenHandlerNil(t *testing.T) {
	m := buildQuitModel(DaemonModeAttachedLocal, DaemonStatusReport{}, false, "", "", false, false)
	if got := labelsOf(m.Buttons); !equalStrings(got, []string{"Quit client", "Cancel"}) {
		t.Fatalf("buttons with StopDaemon nil = %v, want no Stop button", got)
	}
	if m.DefaultIdx != 1 || m.Buttons[m.DefaultIdx].Kind != quitCancel {
		t.Fatalf("default = %d, want Cancel", m.DefaultIdx)
	}
}

func TestIssue503BuildModelAttachedRemoteEnriched(t *testing.T) {
	report := DaemonStatusReport{LiveSessions: 2, Watchers: 0, MCPServers: []string{"x"}}
	m := buildQuitModel(DaemonModeAttachedRemote, report, true, "db.example:8080", "ssh://u@db.example", false, false)

	if m.Title != "Quit Gogent (daemon stays running)" {
		t.Fatalf("title = %q", m.Title)
	}
	// Host (display) and addr (connect string) are distinct sources — both surface.
	if !strings.Contains(m.Body, "The daemon at db.example:8080 keeps running:") {
		t.Fatalf("remote body missing host phrase:\n%s", m.Body)
	}
	if !strings.Contains(m.Body, "Re-attach later with:  gogent --connect ssh://u@db.example") {
		t.Fatalf("remote body missing --connect addr line:\n%s", m.Body)
	}
	// NO Stop button: Stop only ever drives the local daemon.
	if got := labelsOf(m.Buttons); !equalStrings(got, []string{"Quit client", "Cancel"}) {
		t.Fatalf("remote buttons = %v, want NO Stop", got)
	}
	if m.DefaultIdx != 1 || m.Buttons[m.DefaultIdx].Kind != quitCancel {
		t.Fatalf("default = %d, want Cancel", m.DefaultIdx)
	}
}

func TestIssue503BuildModelAttachedRemoteFallback(t *testing.T) {
	m := buildQuitModel(DaemonModeAttachedRemote, DaemonStatusReport{}, false, "db.example:8080", "ssh://u@db.example", false, false)
	wantBody := "Quitting closes this TUI client only.\n" +
		"The daemon at db.example:8080 keeps running — your sessions and watchers\n" +
		"continue in the background.\n" +
		"\n" +
		"Re-attach later with:  gogent --connect ssh://u@db.example"
	if m.Body != wantBody {
		t.Fatalf("fallback remote body mismatch:\nwant:\n%s\ngot:\n%s", wantBody, m.Body)
	}
}

func TestIssue503BuildModelAttachedRemoteOmitsReattachWhenAddrEmpty(t *testing.T) {
	m := buildQuitModel(DaemonModeAttachedRemote, DaemonStatusReport{}, false, "db.example:8080", "", false, false)
	if strings.Contains(m.Body, "--connect") {
		t.Fatalf("addr empty must omit the re-attach line:\n%s", m.Body)
	}
	if !strings.Contains(m.Body, "The daemon at db.example:8080 keeps running") {
		t.Fatalf("host phrase should still show when host known:\n%s", m.Body)
	}
}

func TestIssue503BuildModelAttachedRemoteHostFallback(t *testing.T) {
	// host == "" → "The daemon keeps running:" (no "at {host}").
	m := buildQuitModel(DaemonModeAttachedRemote, DaemonStatusReport{LiveSessions: 1}, true, "", "host:7", false, false)
	if !strings.Contains(m.Body, "The daemon keeps running:") {
		t.Fatalf("empty host should fall back to 'The daemon keeps running:':\n%s", m.Body)
	}
	if strings.Contains(m.Body, "The daemon at") {
		t.Fatalf("empty host must not render 'The daemon at':\n%s", m.Body)
	}
}

func TestIssue503BuildModelEmbedded(t *testing.T) {
	m := buildQuitModel(DaemonModeEmbedded, DaemonStatusReport{}, false, "", "", false, true)

	if m.Title != "Quit Gogent — stops all sessions" {
		t.Fatalf("title = %q", m.Title)
	}
	wantBody := "You are running embedded (no daemon).\n" +
		"Quitting stops ALL sessions and watchers in this process;\n" +
		"in-flight turns are cancelled.\n" +
		"\n" +
		"To keep your work running after you leave, start the\n" +
		"daemon first."
	if m.Body != wantBody {
		t.Fatalf("embedded body mismatch:\nwant:\n%s\ngot:\n%s", wantBody, m.Body)
	}
	if got := labelsOf(m.Buttons); !equalStrings(got, []string{"Quit (stops all)", "Start daemon && quit", "Cancel"}) {
		t.Fatalf("embedded buttons = %v", got)
	}
	if !hasLabel(m.Buttons, "Start daemon && quit") {
		t.Fatal("Start label must be &&-escaped")
	}
	// Embedded's "Quit (stops all)" is the destructive quit (same quitClient kind).
	if m.Buttons[0].Kind != quitClient {
		t.Fatalf("embedded primary button kind = %d, want quitClient", m.Buttons[0].Kind)
	}
	if m.DefaultIdx != 2 || m.Buttons[m.DefaultIdx].Kind != quitCancel {
		t.Fatalf("default = %d, want Cancel", m.DefaultIdx)
	}
}

func TestIssue503BuildModelEmbeddedOmitsStartWhenHandlerNil(t *testing.T) {
	m := buildQuitModel(DaemonModeEmbedded, DaemonStatusReport{}, false, "", "", false, false)
	if got := labelsOf(m.Buttons); !equalStrings(got, []string{"Quit (stops all)", "Cancel"}) {
		t.Fatalf("buttons with StartDaemon nil = %v, want no Start button", got)
	}
}

// Defensive default (nil mode never reaches here in confirmQuit, but the function
// must not panic and must produce a sane confirmation).
func TestIssue503BuildModelDefaultIsGeneric(t *testing.T) {
	m := buildQuitModel(DaemonMode(999), DaemonStatusReport{}, false, "", "", false, false)
	if m.Title != "Quit Gogent" {
		t.Fatalf("defensive title = %q", m.Title)
	}
	if m.Body != "Are you sure you want to quit?" {
		t.Fatalf("defensive body = %q", m.Body)
	}
	if m.DefaultIdx != 1 || m.Buttons[m.DefaultIdx].Kind != quitCancel {
		t.Fatalf("defensive default = %d, want Cancel", m.DefaultIdx)
	}
}

// Pluralisation helper: 0 and N>1 pluralise, exactly 1 is singular.
func TestIssue503CountLinePluralisation(t *testing.T) {
	cases := map[int]string{
		0: "0 live sessions",
		1: "1 live session",
		2: "2 live sessions",
		7: "7 live sessions",
	}
	for n, want := range cases {
		if got := quitCountLine(n, "live session"); got != want {
			t.Errorf("quitCountLine(%d) = %q, want %q", n, got, want)
		}
	}
	// All three nouns pluralise by a trailing "s".
	for _, noun := range []string{"live session", "watcher", "MCP server"} {
		if got := quitCountLine(3, noun); got != "3 "+noun+"s" {
			t.Errorf("quitCountLine(3,%q) = %q", noun, got)
		}
	}
}

// The literal "&" in the button labels is escaped as "&&" so turbotui's
// ParseMnemonic (which every Button label runs through) renders exactly one "&"
// instead of consuming it as a mnemonic marker. This pins the criterion-4 fix.
func TestIssue503ButtonLabelsEscapeAmpersandForMnemonicParser(t *testing.T) {
	for _, src := range []string{"Stop daemon && quit", "Start daemon && quit"} {
		clean, hot := tv.ParseMnemonic(src)
		if clean != "Stop daemon & quit" && clean != "Start daemon & quit" {
			t.Errorf("ParseMnemonic(%q) clean = %q, want a single literal '&'", src, clean)
		}
		if hot >= 0 {
			t.Errorf("ParseMnemonic(%q) flagged a mnemonic at %d; the && must be a literal, not a marker", src, hot)
		}
	}
}

// Default focus is the safe Cancel choice in every non-nil mode.
func TestIssue503DefaultFocusIsCancelInAllModes(t *testing.T) {
	modes := []struct {
		mode              DaemonMode
		canStop, canStart bool
	}{
		{DaemonModeAttachedLocal, true, false},
		{DaemonModeAttachedLocal, false, false},
		{DaemonModeAttachedRemote, false, false},
		{DaemonModeEmbedded, false, true},
		{DaemonModeEmbedded, false, false},
	}
	for _, tc := range modes {
		m := buildQuitModel(tc.mode, DaemonStatusReport{}, false, "h", "a", tc.canStop, tc.canStart)
		if m.Buttons[m.DefaultIdx].Kind != quitCancel {
			t.Fatalf("mode %d canStop=%v canStart=%v: default button kind = %d, want Cancel",
				tc.mode, tc.canStop, tc.canStart, m.Buttons[m.DefaultIdx].Kind)
		}
	}
}

// ---------------------------------------------------------------------------
// (1) GOAL MATCH — confirmQuit choke point
// ---------------------------------------------------------------------------

func TestIssue503ConfirmQuitNilDaemonModeKeepsYesNoBox(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	// No daemon wiring at all (DaemonMode nil) — the generic Yes/No box, unchanged.
	w.confirmQuit()

	if got := topLayerName(w); got != "confirm-dialog" {
		t.Fatalf("nil DaemonMode top layer = %q, want confirm-dialog (today's Yes/No box)", got)
	}
	if got := w.quitDialogLayer; got != nil {
		t.Fatalf("nil DaemonMode must not open a quit-dialog, got %v", got)
	}
	if quitTriggered(w) {
		t.Fatal("opening the confirmation must not quit")
	}
}

func TestIssue503ConfirmQuitWithDaemonModeOpensQuitDialog(t *testing.T) {
	w := newQuitWorkbench(t, Handlers{DaemonMode: func() DaemonMode { return DaemonModeAttachedLocal }})
	w.confirmQuit()

	if got := topLayerName(w); got != "quit-dialog" {
		t.Fatalf("daemon-mode top layer = %q, want quit-dialog", got)
	}
	if w.quitDialogLayer == nil {
		t.Fatal("quitDialogLayer not tracked")
	}
	if quitTriggered(w) {
		t.Fatal("opening the quit dialog must not itself quit")
	}
}

// ---------------------------------------------------------------------------
// (2)/(3) showQuitDialog wiring: blocking modal, default focus, button count
// ---------------------------------------------------------------------------

func TestIssue503QuitDialogIsBlockingModalNamed(t *testing.T) {
	w := newQuitWorkbench(t, Handlers{
		DaemonMode: func() DaemonMode { return DaemonModeAttachedLocal },
		StopDaemon: func() error { return nil },
	})
	w.showQuitDialog()

	top := w.desktop.TopLayer()
	if top == nil || top.Name != "quit-dialog" {
		t.Fatalf("top = %+v, want quit-dialog", top)
	}
	if !top.Modal || !top.AcceptInput {
		t.Fatalf("quit layer modal/acceptInput = %v/%v, want a blocking modal", top.Modal, top.AcceptInput)
	}
	if w.quitDialogLayer != top {
		t.Fatal("quitDialogLayer must reference the top layer")
	}
}

func TestIssue503QuitDialogDefaultFocusIsCancel(t *testing.T) {
	for _, mode := range []DaemonMode{DaemonModeAttachedLocal, DaemonModeAttachedRemote, DaemonModeEmbedded} {
		t.Run(modeName(mode), func(t *testing.T) {
			w := newQuitWorkbench(t, Handlers{
				DaemonMode:  func() DaemonMode { return mode },
				StopDaemon:  func() error { return nil },
				StartDaemon: func() error { return nil },
				DaemonStatusInfo: func() (DaemonStatusReport, error) {
					return DaemonStatusReport{Mode: mode, LiveSessions: 1}, nil
				},
			})
			w.showQuitDialog()

			f := quitButtons(w)
			if len(f) < 2 {
				t.Fatalf("expected at least Quit + Cancel rendered, got %d buttons", len(f))
			}
			focused := focusedComponent(w.quitDialogLayer.Root)
			if focused == nil {
				t.Fatal("no focused component in the quit dialog")
			}
			// Cancel is always the last rendered button (even after a middle drop),
			// so the default-focused component must be the last one.
			if focused != f[len(f)-1] {
				t.Fatalf("default focus is not the Cancel (last) button (mode %s)", modeName(mode))
			}
		})
	}
}

// Button counts for the cases that render correctly at the dialog's content width.
// (Embedded+Start and attached-local+status drop the middle button on a normal
// terminal — that is a defect surfaced by TestIssue503Defect_MiddleButtonDropped,
// not asserted here.)
func TestIssue503QuitDialogButtonCountPerMode(t *testing.T) {
	cases := []struct {
		mode              DaemonMode
		canStop, canStart bool
		wantButtons       int
	}{
		{DaemonModeAttachedLocal, true, false, 3},  // Quit / Stop / Cancel (fallback body is wide enough)
		{DaemonModeAttachedLocal, false, false, 2}, // Stop omitted
		{DaemonModeAttachedRemote, true, true, 2},  // never Stop
		{DaemonModeEmbedded, false, false, 2},      // Start omitted → Quit / Cancel
	}
	for _, tc := range cases {
		t.Run(modeName(tc.mode), func(t *testing.T) {
			h := Handlers{DaemonMode: func() DaemonMode { return tc.mode }}
			if tc.canStop {
				h.StopDaemon = func() error { return nil }
			}
			if tc.canStart {
				h.StartDaemon = func() error { return nil }
			}
			w := newQuitWorkbench(t, h)
			w.showQuitDialog()
			f := quitButtons(w)
			if got := len(f); got != tc.wantButtons {
				t.Fatalf("buttons = %d, want %d", got, tc.wantButtons)
			}
		})
	}
}

// DEFECT (intentionally failing — surfaces a real bug against the acceptance
// criteria). The narrow-terminal button drop is keyed off the CONTENT-SIZED dialog
// width (the body's longest line + messagePad), NOT the terminal width. So on a
// normal 120-column terminal:
//   - Embedded: the body caps the dialog at ~61 cols, but its three buttons need
//     ~65, so "Start daemon & quit" is dropped and never shown.
//   - Attached-local once a status snapshot is fetched: the dialog is pre-sized to
//     the (narrower) enriched shape (~41 cols), so "Stop daemon & quit" is dropped.
//
// Both violate "embedded offers Start daemon & quit" and "attached-local buttons:
// Quit client / Stop daemon & quit / Cancel". The dialog width should be at least
// the button-row width (max of body width and row width); today it is body-only.
func TestIssue503Defect_MiddleButtonDroppedOnNormalWidthDialog(t *testing.T) {
	cases := []struct {
		name string
		h    Handlers
		want int // acceptance-criteria button count
	}{
		{
			"embedded-with-start",
			Handlers{
				DaemonMode:  func() DaemonMode { return DaemonModeEmbedded },
				StartDaemon: func() error { return nil },
			},
			3,
		},
		{
			"attached-local-with-status",
			Handlers{
				DaemonMode: func() DaemonMode { return DaemonModeAttachedLocal },
				StopDaemon: func() error { return nil },
				DaemonStatusInfo: func() (DaemonStatusReport, error) {
					return DaemonStatusReport{Mode: DaemonModeAttachedLocal, LiveSessions: 2}, nil
				},
			},
			3,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newQuitWorkbench(t, c.h)
			w.showQuitDialog()
			got := len(quitButtons(w))
			dw := w.quitDialogLayer.Root.Bounds.W
			if got != c.want {
				t.Fatalf("DEFECT: on a 120-col terminal %s rendered %d buttons (dialog width %d), "+
					"want %d — the middle handoff button was dropped because the body-sized dialog "+
					"is narrower than the button row. Fix: size the dialog to max(body, buttonRow).",
					c.name, got, dw, c.want)
			}
		})
	}
}

// Narrow terminal: three buttons don't fit → drop the middle handoff button, keep
// Quit + Cancel, and keep default focus on Cancel. (documented degradation)
func TestIssue503QuitDialogDropsMiddleButtonOnNarrowTerminal(t *testing.T) {
	w := newQuitWorkbench(t, Handlers{
		DaemonMode: func() DaemonMode { return DaemonModeAttachedLocal },
		StopDaemon: func() error { return nil },
	})
	// Three labels "Quit client"/"Stop daemon && quit"/"Cancel" fit at the wide
	// default width but not in a narrow interior; the predicate agrees.
	labels := []string{"Quit client", "Stop daemon && quit", "Cancel"}
	if !quitButtonRowFits(120, labels...) {
		t.Fatal("precondition: the row should fit at width 120")
	}
	if quitButtonRowFits(20, labels...) {
		t.Fatal("precondition: the row should NOT fit at width 20")
	}
	w.app.Resize(20, 10) // narrow
	w.showQuitDialog()

	f := quitButtons(w)
	if got := len(f); got != 2 {
		t.Fatalf("narrow buttons = %d, want 2 (middle dropped)", got)
	}
	// Default focus is still Cancel (the last remaining focusable).
	if focused := focusedComponent(w.quitDialogLayer.Root); focused != f[len(f)-1] {
		t.Fatal("default focus should remain Cancel after the middle button is dropped")
	}
}

// ---------------------------------------------------------------------------
// (2)/(3) Action semantics — driven via real Enter/Esc on the rendered dialog
// ---------------------------------------------------------------------------

func TestIssue503CancelDismissesWithoutQuitting(t *testing.T) {
	w := newQuitWorkbench(t, Handlers{
		DaemonMode: func() DaemonMode { return DaemonModeAttachedRemote },
	})
	w.showQuitDialog()
	pressQuitButton(t, w, quitCancel)

	if w.quitDialogLayer != nil {
		t.Fatal("Cancel must dismiss the quit dialog")
	}
	if quitTriggered(w) {
		t.Fatal("Cancel must not quit")
	}
}

func TestIssue503EscapeDismissesWithoutQuitting(t *testing.T) {
	w := newQuitWorkbench(t, Handlers{
		DaemonMode: func() DaemonMode { return DaemonModeAttachedLocal },
		StopDaemon: func() error { return nil },
	})
	w.showQuitDialog()
	sendEscape(t, w)

	if w.quitDialogLayer != nil {
		t.Fatal("Escape must dismiss the quit dialog")
	}
	if quitTriggered(w) {
		t.Fatal("Escape must not quit")
	}
}

func TestIssue503QuitClientQuits(t *testing.T) {
	w := newQuitWorkbench(t, Handlers{
		DaemonMode: func() DaemonMode { return DaemonModeAttachedRemote },
	})
	w.showQuitDialog()
	pressQuitButton(t, w, quitClient)

	if w.quitDialogLayer != nil {
		t.Fatal("Quit client must dismiss the quit dialog first")
	}
	if !quitTriggered(w) {
		t.Fatal("Quit client must invoke w.quit() (the only teardown)")
	}
}

func TestIssue503QuitStopsAllEmbeddedQuits(t *testing.T) {
	w := newQuitWorkbench(t, Handlers{
		DaemonMode:  func() DaemonMode { return DaemonModeEmbedded },
		StartDaemon: func() error { return nil },
	})
	w.showQuitDialog()
	// The embedded primary button is "Quit (stops all)", kind quitClient.
	pressQuitButton(t, w, quitClient)
	if !quitTriggered(w) {
		t.Fatal("embedded Quit (stops all) must invoke w.quit()")
	}
}

// Stop daemon & quit: progress modal shows during the handoff, then on SUCCESS it
// quits; the daemon is stopped only via this explicit button (no implicit stop).
func TestIssue503StopDaemonAndQuitSucceedsThenQuits(t *testing.T) {
	release := make(chan struct{})
	w := newQuitWorkbench(t, Handlers{
		DaemonMode: func() DaemonMode { return DaemonModeAttachedLocal },
		StopDaemon: func() error { <-release; return nil },
	})
	w.showQuitDialog()

	pressQuitButton(t, w, quitStopAndQuit)

	// The quit dialog is dismissed; the handoff progress modal is up and blocking.
	if w.quitDialogLayer != nil {
		t.Fatal("Stop-and-quit must dismiss the quit dialog before the handoff")
	}
	if w.daemonHandoffLayer == nil {
		t.Fatal("daemonHandoffLayer must be set while the handoff runs")
	}
	if got := topLayerName(w); got != "daemon-progress" {
		t.Fatalf("in-flight top = %q, want daemon-progress", got)
	}

	close(release)
	drainPostedEventually(t, w)

	if w.daemonHandoffLayer != nil {
		t.Fatal("progress modal must be dismissed after the handoff")
	}
	if !quitTriggered(w) {
		t.Fatal("successful Stop-and-quit must invoke w.quit()")
	}
}

// Stop daemon & quit on FAILURE stays alive with an error dialog — no quit.
func TestIssue503StopDaemonAndQuitFailureStaysAlive(t *testing.T) {
	w := newQuitWorkbench(t, Handlers{
		DaemonMode: func() DaemonMode { return DaemonModeAttachedLocal },
		StopDaemon: func() error { return fmt.Errorf("daemon did not exit") },
	})
	w.showQuitDialog()
	pressQuitButton(t, w, quitStopAndQuit)
	drainPostedEventually(t, w)

	if quitTriggered(w) {
		t.Fatal("failed Stop-and-quit must NOT quit (stay alive)")
	}
	if got := topLayerName(w); got != "confirm-dialog" {
		t.Fatalf("failed Stop-and-quit top = %q, want a confirm-dialog error", got)
	}
	if w.daemonHandoffLayer != nil {
		t.Fatal("progress modal must be dismissed on the failure path too (#478)")
	}
}

// Start daemon & quit on SUCCESS quits (daemon stays up, work survives). Driven
// through runStartDaemon with the exact continuation showQuitDialog wires to its
// Start button, because the embedded dialog drops that button on a normal-width
// terminal (see TestIssue503Defect_MiddleButtonDropped) so it cannot be click-
// tested. This still validates the handoff + continuation contract end-to-end.
func TestIssue503StartDaemonAndQuitSucceedsThenQuits(t *testing.T) {
	release := make(chan struct{})
	w := newQuitWorkbench(t, Handlers{
		DaemonMode:  func() DaemonMode { return DaemonModeEmbedded },
		StartDaemon: func() error { <-release; return nil },
	})
	quitAndQuit := func(err error) { // mirrors showQuitDialog's quitStartAndQuit action
		if err != nil {
			w.showConfirm("Start daemon", "Could not start the daemon:\n"+err.Error(), nil)
			return
		}
		if w.quit != nil {
			w.quit()
		}
	}
	if !w.runStartDaemon(quitAndQuit) {
		t.Fatal("runStartDaemon did not start a handoff")
	}
	if got := topLayerName(w); got != "daemon-progress" {
		t.Fatalf("in-flight top = %q, want daemon-progress", got)
	}

	close(release)
	drainPostedEventually(t, w)

	if w.daemonHandoffLayer != nil {
		t.Fatal("progress modal must be dismissed after the handoff")
	}
	if !quitTriggered(w) {
		t.Fatal("successful Start-and-quit must invoke w.quit()")
	}
}

// Start daemon & quit on FAILURE stays alive with an error dialog — no quit.
func TestIssue503StartDaemonAndQuitFailureStaysAlive(t *testing.T) {
	w := newQuitWorkbench(t, Handlers{
		DaemonMode:  func() DaemonMode { return DaemonModeEmbedded },
		StartDaemon: func() error { return fmt.Errorf("bind refused") },
	})
	w.runStartDaemon(func(err error) { // mirrors showQuitDialog's quitStartAndQuit action
		if err != nil {
			w.showConfirm("Start daemon", "Could not start the daemon:\n"+err.Error(), nil)
			return
		}
		if w.quit != nil {
			w.quit()
		}
	})
	drainPostedEventually(t, w)

	if quitTriggered(w) {
		t.Fatal("failed Start-and-quit must NOT quit (stay alive)")
	}
	if got := topLayerName(w); got != "confirm-dialog" {
		t.Fatalf("failed Start-and-quit top = %q, want confirm-dialog error", got)
	}
	if w.daemonHandoffLayer != nil {
		t.Fatal("progress modal must be dismissed on the failure path too (#478)")
	}
}

// ---------------------------------------------------------------------------
// (2) Non-blocking enrichment: opens immediately, fetch off-thread, error stays
// ---------------------------------------------------------------------------

// The dialog opens instantly even when the status fetch never returns.
func TestIssue503QuitDialogOpensImmediatelyWithoutBlocking(t *testing.T) {
	w := newQuitWorkbench(t, Handlers{
		DaemonMode: func() DaemonMode { return DaemonModeAttachedLocal },
		StopDaemon: func() error { return nil },
		DaemonStatusInfo: func() (DaemonStatusReport, error) {
			<-make(chan struct{}) // never returns
			return DaemonStatusReport{}, nil
		},
	})
	w.showQuitDialog()

	// The dialog is already up and interactive despite the fetch being stuck.
	if got := topLayerName(w); got != "quit-dialog" {
		t.Fatalf("dialog must open immediately; top = %q", got)
	}
	if w.quitDialogLayer == nil {
		t.Fatal("quitDialogLayer must be set on open")
	}
}

// On a fetch error, the fallback body stays and the dialog remains up.
func TestIssue503QuitDialogKeepsFallbackOnStatusError(t *testing.T) {
	w := newQuitWorkbench(t, Handlers{
		DaemonMode: func() DaemonMode { return DaemonModeAttachedLocal },
		StopDaemon: func() error { return nil },
		DaemonStatusInfo: func() (DaemonStatusReport, error) {
			return DaemonStatusReport{}, fmt.Errorf("unreachable")
		},
	})
	w.showQuitDialog()
	drainPostedEventually(t, w)

	// Error path: the quit dialog stays (not replaced by an error box).
	if got := topLayerName(w); got != "quit-dialog" {
		t.Fatalf("on status error top = %q, want quit-dialog (fallback kept)", got)
	}
	if quitTriggered(w) {
		t.Fatal("status fetch error must not quit")
	}
}

// The background fetch is actually invoked (non-blocking, off-thread).
func TestIssue503QuitDialogInvokesStatusFetch(t *testing.T) {
	calls := 0
	w := newQuitWorkbench(t, Handlers{
		DaemonMode: func() DaemonMode { return DaemonModeAttachedRemote },
		DaemonStatusInfo: func() (DaemonStatusReport, error) {
			calls++
			return DaemonStatusReport{Mode: DaemonModeAttachedRemote, LiveSessions: 5}, nil
		},
	})
	w.showQuitDialog()
	drainPostedEventually(t, w)

	if calls == 0 {
		t.Fatal("DaemonStatusInfo must be invoked for attached modes")
	}
}

// The enrichment callback no-ops once the user has already dismissed the dialog
// (no use-after-dismiss / late mutation). Drain is forced after dismiss.
func TestIssue503QuitDialogEnrichNoOpsAfterDismiss(t *testing.T) {
	results := make(chan DaemonStatusReport, 1)
	w := newQuitWorkbench(t, Handlers{
		DaemonMode: func() DaemonMode { return DaemonModeAttachedLocal },
		StopDaemon: func() error { return nil },
		DaemonStatusInfo: func() (DaemonStatusReport, error) {
			r := <-results
			return r, nil
		},
	})
	w.showQuitDialog()
	// Dismiss before the fetch completes (Escape = Cancel).
	sendEscape(t, w)
	if w.quitDialogLayer != nil {
		t.Fatal("dialog should be dismissed")
	}
	// Now release the fetch; the Posted enrichment must no-op (layer guard).
	results <- DaemonStatusReport{LiveSessions: 9}
	drainPostedEventually(t, w)
	// Nothing to assert visibly (body is unreachable), but the drain must not panic
	// and the workbench must remain in a consistent (not-quit) state.
	if quitTriggered(w) {
		t.Fatal("late enrichment after dismiss must not quit")
	}
}

// rewriteBody is the in-place enrichment mechanism: clear, re-add each line, and
// scroll to the top. Asserted directly on a fresh TextView (the live body is a local
// in showQuitDialog and unreachable from tests).
func TestIssue503RewriteBodyReplacesContentAndScrollsTop(t *testing.T) {
	body := tv.NewTextView("initial", tv.Rect{X: 0, Y: 0, W: 40, H: 5})
	body.AddLine("kept")
	rewriteBody(body, "first line\nsecond line")
	if got := body.AllText(); !strings.Contains(got, "first line") || !strings.Contains(got, "second line") {
		t.Fatalf("rewriteBody did not set the new text: %q", got)
	}
	if strings.Contains(body.AllText(), "initial") || strings.Contains(body.AllText(), "kept") {
		t.Fatalf("rewriteBody did not clear the old text: %q", body.AllText())
	}
	// nil body must be safe (defensive).
	rewriteBody(nil, "anything")
}

// reconnectAddress is nil-safe: "" when ReconnectAddress is unwired.
func TestIssue503ReconnectAddressNilSafe(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	if got := w.reconnectAddress(); got != "" {
		t.Fatalf("nil ReconnectAddress = %q, want empty", got)
	}
	called := false
	w.SetHandlers(Handlers{ReconnectAddress: func() string { called = true; return "ssh://h" }})
	if got := w.reconnectAddress(); got != "ssh://h" {
		t.Fatalf("ReconnectAddress = %q, want ssh://h", got)
	}
	if !called {
		t.Fatal("ReconnectAddress closure was not invoked")
	}
}

// ---------------------------------------------------------------------------
// (3) Factored handoff helpers (shared with the menu path) — no #478 regression
// ---------------------------------------------------------------------------

func TestIssue503RunStopDaemonShowsProgressThenInvokesOnResult(t *testing.T) {
	release := make(chan struct{})
	var gotErr error
	w := newQuitWorkbench(t, Handlers{
		DaemonMode: func() DaemonMode { return DaemonModeAttachedLocal },
		StopDaemon: func() error { <-release; return nil },
	})
	started := w.runStopDaemon(func(err error) { gotErr = err })

	if !started {
		t.Fatal("runStopDaemon should report it started a handoff")
	}
	if w.daemonHandoffLayer == nil {
		t.Fatal("progress modal must be up while the handoff runs")
	}
	if got := topLayerName(w); got != "daemon-progress" {
		t.Fatalf("in-flight top = %q, want daemon-progress", got)
	}

	close(release)
	drainPostedEventually(t, w)

	if w.daemonHandoffLayer != nil {
		t.Fatal("progress modal must be dismissed after completion")
	}
	if gotErr != nil {
		t.Fatalf("onResult err = %v, want nil", gotErr)
	}
}

func TestIssue503RunStopDaemonPassesErrorToOnResult(t *testing.T) {
	var gotErr error
	w := newQuitWorkbench(t, Handlers{
		DaemonMode: func() DaemonMode { return DaemonModeAttachedLocal },
		StopDaemon: func() error { return fmt.Errorf("nope") },
	})
	w.runStopDaemon(func(err error) { gotErr = err })
	drainPostedEventually(t, w)

	if gotErr == nil || gotErr.Error() != "nope" {
		t.Fatalf("onResult err = %v, want 'nope'", gotErr)
	}
	if w.daemonHandoffLayer != nil {
		t.Fatal("progress modal must be dismissed on the error path")
	}
}

func TestIssue503RunStopDaemonGuardedAgainstDoubleHandoffAndNilHandler(t *testing.T) {
	w := newQuitWorkbench(t, Handlers{
		DaemonMode: func() DaemonMode { return DaemonModeAttachedLocal },
		StopDaemon: func() error { return nil },
	})
	// Nil handler: no-op, reports not-started.
	w.handlers.StopDaemon = nil
	if w.runStopDaemon(func(error) { t.Fatal("nil handler must not run") }) {
		t.Fatal("runStopDaemon with nil handler should report not-started")
	}
	// Already in flight: guarded no-op.
	w.handlers.StopDaemon = func() error { return nil }
	w.daemonHandoffLayer = w.showProgress("x", "y") // pretend a handoff is running
	if w.runStopDaemon(func(error) { t.Fatal("double handoff must be guarded") }) {
		t.Fatal("runStopDaemon during an in-flight handoff should report not-started")
	}
}

func TestIssue503RunStartDaemonGuardedAgainstNilHandler(t *testing.T) {
	w := newQuitWorkbench(t, Handlers{
		DaemonMode: func() DaemonMode { return DaemonModeEmbedded },
	})
	if w.runStartDaemon(func(error) { t.Fatal("nil handler must not run") }) {
		t.Fatal("runStartDaemon with nil handler should report not-started")
	}
}

// The menu path is unchanged by the factoring: Stop from the menu still rebuilds
// the menu and shows a single result dialog (success), per #478.
func TestIssue503MenuStopDaemonStillShowsSingleResultOnSuccess(t *testing.T) {
	w := newQuitWorkbench(t, Handlers{
		DaemonMode: func() DaemonMode { return DaemonModeAttachedLocal },
		StopDaemon: func() error { return nil },
	})
	w.stopDaemonFromMenu()
	drainPostedEventually(t, w)

	if w.daemonHandoffLayer != nil {
		t.Fatal("progress modal must be dismissed after the menu Stop")
	}
	if got := topLayerName(w); got != "confirm-dialog" {
		t.Fatalf("menu Stop result top = %q, want a single confirm-dialog", got)
	}
	// Nothing stacked beneath (the #478 single-dialog guarantee).
	assertNoDaemonDialogStackedBeneath(t, w)
}

// ---------------------------------------------------------------------------
// (3) Layout helpers — centring, clamping, narrow-fit predicate
// ---------------------------------------------------------------------------

func TestIssue503QuitButtonRowCentresAndClamps(t *testing.T) {
	const width, btnY = 80, 10
	rects := quitButtonRow(width, btnY, "Quit client", "Stop daemon && quit", "Cancel")
	if len(rects) != 3 {
		t.Fatalf("rects = %d, want 3", len(rects))
	}
	for i, r := range rects {
		if r.Y != btnY {
			t.Errorf("rect[%d].Y = %d, want %d", i, r.Y, btnY)
		}
		if r.H != 1 {
			t.Errorf("rect[%d].H = %d, want 1", i, r.H)
		}
		if r.X < 2 || r.X+r.W > width-3 {
			t.Errorf("rect[%d] = %+v escapes content margins [2,%d]", i, r, width-3)
		}
	}
	// No two rects overlap (constant gap of >=4 between them).
	for i := 1; i < len(rects); i++ {
		if rects[i].X < rects[i-1].X+rects[i-1].W {
			t.Errorf("rect[%d] overlaps rect[%d]: %+v vs %+v", i, i-1, rects[i], rects[i-1])
		}
	}
}

func TestIssue503QuitButtonRowReturnsOneRectPerLabel(t *testing.T) {
	rects := quitButtonRow(60, 5, "Quit client", "Cancel")
	if len(rects) != 2 {
		t.Fatalf("2-label row = %d rects, want 2", len(rects))
	}
}

func TestIssue503QuitButtonRowFitsPredicate(t *testing.T) {
	labels := []string{"Quit client", "Stop daemon && quit", "Cancel"}
	if !quitButtonRowFits(120, labels...) {
		t.Error("row should fit at width 120")
	}
	if quitButtonRowFits(20, labels...) {
		t.Error("row should NOT fit at width 20")
	}
	// Two buttons always fit at a reasonable width.
	if !quitButtonRowFits(40, "Quit client", "Cancel") {
		t.Error("two-button row should fit at width 40")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func modeName(m DaemonMode) string {
	switch m {
	case DaemonModeEmbedded:
		return "embedded"
	case DaemonModeAttachedLocal:
		return "attached-local"
	case DaemonModeAttachedRemote:
		return "attached-remote"
	default:
		return fmt.Sprintf("mode(%d)", m)
	}
}

func equalStrings(a, b []string) bool {
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
