package ui

import (
	"strings"
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/config"
)

// This file exercises issue #204: a live theme change (Config → Theme… editor, or
// editing theme in config.json at runtime) must FULLY re-apply to every open
// SessionWindow without a restart — not just repaint the desktop/sidebar/menu.
//
// Before the fix, transcript records froze their colours by value at creation, the
// window chrome was seeded once at construction, ApplyTheme never installed the
// turbotui chrome theme (tv.SetTheme), and RefreshTheme only rebuilt the menu. The
// fix has three parts, each covered here:
//
//	A. Transcript records/lines carry a semantic colorRole and resolve it against
//	   the *current* palette at render time (headerColor/effectiveColor/roleColor),
//	   so a re-render paints existing messages in the new colours.
//	B. ApplyTheme calls tv.SetTheme(tv.DefaultTheme) so turbotui's active chrome
//	   theme tracks the palette (what widgets seed from).
//	C. SessionWindow.refreshTheme re-renders the transcript and re-seeds the window
//	   chrome; Workbench.RefreshTheme walks every open window then redraws.
//
// The tests are organised in groups A–I below. Groups A–H verify behaviour the fix
// got right (they PASS against the implementation). Group I ("DEFECTS FOUND")
// documents real gaps the tester found in fix C — cached construction-time colours
// that refreshTheme still leaves frozen — and currently FAIL, as a defect report.

// ----------------------------------------------------------------------------
// Shared helpers.
// ----------------------------------------------------------------------------

// issue204RestoreTheme snapshots the live theme state and restores it on cleanup.
// It extends the package's withThemeRestore (gogent globals + tv.DefaultTheme) to
// also snapshot/restore tv.ActiveTheme(), because fix B makes ApplyTheme mutate it
// via tv.SetTheme — so a test that switches themes would otherwise leak the new
// chrome theme into later tests.
func issue204RestoreTheme(t *testing.T) {
	t.Helper()
	withThemeRestore(t)
	saved := tv.ActiveTheme()
	t.Cleanup(func() { tv.SetTheme(saved) })
}

// issue204Default/HighContrast/Dark resolve the built-in palettes at truecolor
// fidelity, the form ApplyTheme consumes. truecolor keeps the semantic roles as
// distinct colours across presets (default = 16-colour ANSI indices, high-contrast
// and dark = RGB), so a switch is guaranteed to change every role.
func issue204Default() Theme {
	return ResolveTheme(config.ThemeConfig{}, truecolorEnv, false)
}
func issue204HighContrast() Theme {
	return ResolveTheme(config.ThemeConfig{Name: themeHighContrast}, truecolorEnv, false)
}
func issue204Dark() Theme {
	return ResolveTheme(config.ThemeConfig{Name: themeDark}, truecolorEnv, false)
}

// populateAll adds one record of every transcript kind (plus a thought and a tool
// call with its result) so role/recolour behaviour can be asserted across the full
// set. It works on both live and read-only windows since it only touches the
// transcript model.
func populateAll(sw *SessionWindow) {
	sw.addUser("hello world")
	sw.addThought("considering the options")
	sw.beginToolCall("call-1", "Read", map[string]interface{}{"path": "main.go"})
	sw.finishToolCall("call-1", "Read", "package main")
	sw.addAssistant("done reading the file")
	sw.addError("disk on fire")
	sw.addNote("a system note")
}

// entryPtrs snapshots the live TextEntry pointer each record is currently rendered
// into. render() drops every record's entry to nil and rebuilds it, so a re-render
// is detected by the pointers changing.
func entryPtrs(sw *SessionWindow) []*tv.TextEntry {
	out := make([]*tv.TextEntry, 0, len(sw.transcript.records))
	for _, r := range sw.transcript.records {
		out = append(out, r.entry)
	}
	return out
}

// firstRecordOfKind returns the first record of the given kind, failing the test if
// none exists (populateAll is expected to add one of each).
func firstRecordOfKind(t *testing.T, sw *SessionWindow, k eventKind) *transcriptRecord {
	t.Helper()
	for _, r := range sw.transcript.records {
		if r.kind == k {
			return r
		}
	}
	t.Fatalf("no record of kind %v in transcript", k)
	return nil
}

// ----------------------------------------------------------------------------
// Group A: the colorRole → palette mapping (foundation of fix A).
// ----------------------------------------------------------------------------

// TestIssue204RoleColorMapsEveryRole checks roleColor resolves every semantic role
// to its live package palette variable, and roleNone to the zero colour. renderOne
// paints via headerColor/effectiveColor which both delegate here, so a wrong or
// missing mapping would mis-colour (or drop the colour of) a whole record class.
func TestIssue204RoleColorMapsEveryRole(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204Default())

	for _, tc := range []struct {
		role colorRole
		want tui.Color
	}{
		{roleUser, colorUser},
		{roleAgent, colorAgent},
		{roleNote, colorNote},
		{roleTool, colorTool},
		{roleResult, colorResult},
		{roleInfo, colorInfo},
		{roleError, colorError},
	} {
		if got := roleColor(tc.role); got != tc.want {
			t.Errorf("roleColor(%v) = %+v, want %+v", tc.role, got, tc.want)
		}
	}
	if got := roleColor(roleNone); got != (tui.Color{}) {
		t.Errorf("roleColor(roleNone) = %+v, want zero colour", got)
	}
}

// TestIssue204RoleColorTracksLivePalette proves roleColor reads the *current* palette
// globals, not a snapshot: after a theme switch the same role resolves to a
// different colour. This is the property that makes a re-render recolour existing
// records.
func TestIssue204RoleColorTracksLivePalette(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204Default())
	before := roleColor(roleUser)

	ApplyTheme(issue204HighContrast())
	after := roleColor(roleUser)

	if after != colorUser {
		t.Errorf("roleColor(roleUser) = %+v, want the live colorUser %+v after the switch", after, colorUser)
	}
	if before == after {
		t.Fatalf("roleColor(roleUser) did not change across a default→high-contrast switch (before=after=%+v); it is not tracking the live palette", before)
	}
}

// ----------------------------------------------------------------------------
// Group B: every add site tags its role (fix A completeness).
// ----------------------------------------------------------------------------

// TestIssue204StyledChildLinesTagsRole checks the shared child-line builder records
// both the role (for live recolour) and a colour snapshot (back-compat: existing
// tests assert the snapshot equals the palette colour at creation).
func TestIssue204StyledChildLinesTagsRole(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204Default())

	lines := styledChildLines("a\nb", roleUser)
	if len(lines) != 2 {
		t.Fatalf("styledChildLines split %q into %d lines, want 2", "a\nb", len(lines))
	}
	for i, ln := range lines {
		if ln.role != roleUser {
			t.Errorf("line %d role = %v, want roleUser", i, ln.role)
		}
		if ln.color != colorUser {
			t.Errorf("line %d colour snapshot = %+v, want colorUser %+v", i, ln.color, colorUser)
		}
	}
}

// TestIssue204AddSitesTagRoles walks a fully-populated transcript and checks every
// record carries the role matching its kind, and that the frozen colour snapshot is
// still populated and matches the role's colour at creation. A missing role tag on
// any add site is a real defect: that record would keep its old colours after a
// theme change because headerColor/effectiveColor fall back to the frozen snapshot.
func TestIssue204AddSitesTagRoles(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204Default())

	sw := newTestSession()
	populateAll(sw)

	wantRole := map[eventKind]colorRole{
		kindUser:       roleUser,
		kindAssistant:  roleAgent,
		kindThinking:   roleNote,
		kindTool:       roleTool,
		kindError:      roleError,
		kindSystem:     roleInfo,
		kindCompaction: roleNote,
	}
	for i, r := range sw.transcript.records {
		want, ok := wantRole[r.kind]
		if !ok {
			t.Errorf("record %d: kind %v has no expected role mapping in the test", i, r.kind)
			continue
		}
		if r.role != want {
			t.Errorf("record %d (kind %v): role = %v, want %v — a wrong/missing tag means this record won't recolour on a theme change",
				i, r.kind, r.role, want)
		}
		// The colour snapshot must still be populated and equal the role's current
		// colour (created under the active default theme).
		if r.color == (tui.Color{}) {
			t.Errorf("record %d (kind %v): colour snapshot is zero; the back-compat colour field must stay populated", i, r.kind)
		}
		if r.color != roleColor(r.role) {
			t.Errorf("record %d (kind %v): colour snapshot %+v != roleColour %+v at creation", i, r.kind, r.color, roleColor(r.role))
		}
	}

	// A tool call record mixes child-line roles: the args block is roleTool and the
	// appended result block is roleResult. Both must be tagged so each recolours.
	tool := firstRecordOfKind(t, sw, kindTool)
	byText := map[string]colorRole{}
	for _, ln := range tool.lines {
		byText[strings.TrimSpace(ln.text)] = ln.role
	}
	if byText["args:"] != roleTool {
		t.Errorf("tool call 'args:' line role = %v, want roleTool", byText["args:"])
	}
	if byText["result:"] != roleResult {
		t.Errorf("tool call 'result:' line role = %v, want roleResult", byText["result:"])
	}
}

// TestIssue204RestoreTagsRoles checks the session-restore path tags roles too, so a
// re-opened session's replayed records recolour on a later theme change.
func TestIssue204RestoreTagsRoles(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204Default())

	sw := newTestSession()
	sw.restore([]ChatMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello", Tool: "Read", Args: "path: x"},
		{Role: "tool", Content: "ok", Tool: "Read"},
		{Role: "system", Content: "booted"},
	})

	// user → roleUser, assistant text → roleAgent.
	if r := firstRecordOfKind(t, sw, kindUser); r.role != roleUser {
		t.Errorf("restored user record role = %v, want roleUser", r.role)
	}
	if r := firstRecordOfKind(t, sw, kindAssistant); r.role != roleAgent {
		t.Errorf("restored assistant record role = %v, want roleAgent", r.role)
	}
	// The restored tool-call and tool-result records are both kindTool; their child
	// lines carry roleTool / roleResult respectively.
	toolArgs, toolResult := false, false
	for _, r := range sw.transcript.records {
		if r.kind != kindTool {
			continue
		}
		for _, ln := range r.lines {
			if ln.role == roleTool {
				toolArgs = true
			}
			if ln.role == roleResult {
				toolResult = true
			}
		}
	}
	if !toolArgs {
		t.Error("restored tool-call lines missing roleTool tag")
	}
	if !toolResult {
		t.Error("restored tool-result lines missing roleResult tag")
	}
	// Restored system message → roleInfo.
	if r := firstRecordOfKind(t, sw, kindSystem); r.role != roleInfo {
		t.Errorf("restored system record role = %v, want roleInfo", r.role)
	}
}

// ----------------------------------------------------------------------------
// Group C: headerColor/effectiveColor resolve the role LIVE (the recolour
// mechanism). This is what makes a re-render paint existing messages in the new
// palette rather than their frozen creation-time colours.
// ----------------------------------------------------------------------------

// TestIssue204HeaderColorResolvesLiveAfterSwitch checks each record's header colour
// follows the live palette after a theme switch, and — crucially — differs from the
// frozen colour snapshot stored on the record. Before fix A, renderOne painted from
// the frozen r.color, so this would have stayed put.
func TestIssue204HeaderColorResolvesLiveAfterSwitch(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204Default())

	sw := newTestSession()
	populateAll(sw)

	before := make([]tui.Color, len(sw.transcript.records))
	for i, r := range sw.transcript.records {
		before[i] = r.headerColor()
	}

	ApplyTheme(issue204HighContrast())
	changed := false
	for i, r := range sw.transcript.records {
		got := r.headerColor()
		if got != roleColor(r.role) {
			t.Errorf("record %d (kind %v): headerColor = %+v, want live role colour %+v", i, r.kind, got, roleColor(r.role))
		}
		if got == r.color {
			t.Errorf("record %d (kind %v): headerColor %+v equals the frozen snapshot %+v; it must resolve from the role against the live palette",
				i, r.kind, got, r.color)
		}
		if got != before[i] {
			changed = true
		}
	}
	if !changed {
		t.Fatal("no record header changed colour across a default→high-contrast switch; the live resolution is not working")
	}
}

// TestIssue204EffectiveColorResolvesLiveAfterSwitch is the child-line analogue of
// the header test: every styled line follows its role to the new palette and leaves
// its frozen snapshot behind.
func TestIssue204EffectiveColorResolvesLiveAfterSwitch(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204Default())

	sw := newTestSession()
	populateAll(sw)

	// Snapshot every line's effective colour under the default theme.
	type lineRef struct {
		rec, ln int
		role    colorRole
		frozen  tui.Color
		before  tui.Color
	}
	var refs []lineRef
	for i, r := range sw.transcript.records {
		for j, ln := range r.lines {
			refs = append(refs, lineRef{i, j, ln.role, ln.color, ln.effectiveColor()})
		}
	}

	ApplyTheme(issue204HighContrast())
	changed := false
	for _, ref := range refs {
		ln := sw.transcript.records[ref.rec].lines[ref.ln]
		got := ln.effectiveColor()
		if ref.role != roleNone && got != roleColor(ref.role) {
			t.Errorf("record %d line %d: effectiveColor = %+v, want live role colour %+v", ref.rec, ref.ln, got, roleColor(ref.role))
		}
		if ref.role != roleNone && got == ref.frozen {
			t.Errorf("record %d line %d: effectiveColor %+v equals the frozen snapshot %+v; it must resolve from the role",
				ref.rec, ref.ln, got, ref.frozen)
		}
		if got != ref.before {
			changed = true
		}
	}
	if !changed {
		t.Fatal("no child line changed colour across a default→high-contrast switch")
	}
}

// TestIssue204RoleNoneFallsBackToStoredColor checks the back-compat path: a line
// with roleNone (no semantic slot, e.g. permission-dialog body text) paints from its
// stored colour and is deliberately NOT recoloured by a theme switch.
func TestIssue204RoleNoneFallsBackToStoredColor(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204Default())

	stored := tui.ANSIColor(3) // an arbitrary, non-palette colour
	ln := styledLine{text: "body", color: stored, role: roleNone}
	if got := ln.effectiveColor(); got != stored {
		t.Errorf("roleNone effectiveColor = %+v, want stored %+v", got, stored)
	}

	ApplyTheme(issue204HighContrast())
	if got := ln.effectiveColor(); got != stored {
		t.Errorf("roleNone effectiveColor changed to %+v after a theme switch, want it pinned to the stored %+v", got, stored)
	}
}

// ----------------------------------------------------------------------------
// Group D: fix B — ApplyTheme installs the turbotui active chrome theme.
// ----------------------------------------------------------------------------

// TestIssue204ApplyThemeUpdatesTVActiveTheme checks ApplyTheme calls tv.SetTheme so
// turbotui's activeTheme (what widgets, the menu bar and re-seeded windows read)
// tracks the palette. Before fix B, ApplyTheme only assigned tv.DefaultTheme (read
// solely by gogent's own dialogs), leaving activeTheme on the stock palette forever.
func TestIssue204ApplyThemeUpdatesTVActiveTheme(t *testing.T) {
	issue204RestoreTheme(t)

	ApplyTheme(issue204Default())
	defWindowBG := tv.ActiveTheme().WindowBG

	ApplyTheme(issue204HighContrast())
	hc := issue204HighContrast()
	if got := tv.ActiveTheme().WindowBG; got == defWindowBG {
		t.Errorf("ApplyTheme(high-contrast) did not change tv.ActiveTheme().WindowBG (still %+v); tv.SetTheme was not called (fix B missing)", got)
	} else if got != blackCanvasTVTheme(hc).WindowBG {
		t.Errorf("tv.ActiveTheme().WindowBG = %+v, want the black-canvas chrome %+v", got, blackCanvasTVTheme(hc).WindowBG)
	}

	// Switching back to default restores the stock chrome theme.
	ApplyTheme(issue204Default())
	if got := tv.ActiveTheme(); got != baseTVTheme {
		t.Errorf("after ApplyTheme(default), tv.ActiveTheme() = %+v, want the stock baseTVTheme", got)
	}
}

// ----------------------------------------------------------------------------
// Group E: fix C — SessionWindow.refreshTheme re-renders and re-seeds chrome.
// ----------------------------------------------------------------------------

// TestIssue204RefreshThemeReRendersTranscript checks refreshTheme re-runs the
// transcript render (so frozen colours resolve to the new palette via the roles).
// A re-render is detected by each record's live TextEntry pointer changing.
func TestIssue204RefreshThemeReRendersTranscript(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204Default())

	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	populateAll(sw)
	sw.transcript.render()
	before := entryPtrs(sw)

	ApplyTheme(issue204HighContrast())
	sw.refreshTheme()
	after := entryPtrs(sw)

	if len(before) != len(after) {
		t.Fatalf("record count changed across refreshTheme: %d → %d", len(before), len(after))
	}
	rerendered := false
	for i := range before {
		if after[i] == nil {
			t.Errorf("record %d has a nil entry after refreshTheme; it should be re-rendered", i)
			continue
		}
		if before[i] != nil && before[i] == after[i] {
			t.Errorf("record %d entry pointer unchanged by refreshTheme; the transcript was not re-rendered", i)
		} else {
			rerendered = true
		}
	}
	if !rerendered {
		t.Fatal("refreshTheme did not re-render any record's entry")
	}
}

// TestIssue204RefreshThemeReseedsChrome checks refreshTheme re-seeds every
// construction-time chrome colour from the freshly installed turbotui theme, and
// restores the gogent-set accents (error-red Stop button, divider rule, the cached
// effort-label colour). This is the window-chrome half of issue #204.
func TestIssue204RefreshThemeReseedsChrome(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204Default())

	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	// Capture the construction-time chrome under the default theme, then switch and
	// re-skin. Sentinel fields (WindowBG blue→black, ButtonBG green→black) must
	// change to prove the switch took effect and the reseed was meaningful.
	defContentBG := sw.window.Content.Background.BG
	defButtonBG := sw.sendButton.BG

	ApplyTheme(issue204HighContrast())
	sw.refreshTheme()
	th := tv.ActiveTheme()

	// Window frame + content surface.
	if got, want := sw.window.TitleFG, th.WindowTitleFG; got != want {
		t.Errorf("window.TitleFG = %+v, want %+v", got, want)
	}
	if got, want := sw.window.TitleBG, th.WindowTitleBG; got != want {
		t.Errorf("window.TitleBG = %+v, want %+v", got, want)
	}
	if got, want := sw.window.BorderFG, th.WindowBorderFG; got != want {
		t.Errorf("window.BorderFG = %+v, want %+v", got, want)
	}
	if got, want := sw.window.BorderBG, th.WindowBorderBG; got != want {
		t.Errorf("window.BorderBG = %+v, want %+v", got, want)
	}
	if got, want := sw.window.CloseFG, th.CloseButtonFG; got != want {
		t.Errorf("window.CloseFG = %+v, want %+v", got, want)
	}
	if got, want := sw.window.CloseBG, th.CloseButtonBG; got != want {
		t.Errorf("window.CloseBG = %+v, want %+v", got, want)
	}
	if got, want := sw.window.ShadowColor, th.WindowShadow; got != want {
		t.Errorf("window.ShadowColor = %+v, want %+v", got, want)
	}
	if got, want := sw.window.Content.Background.FG, th.WindowFG; got != want {
		t.Errorf("content background FG = %+v, want %+v", got, want)
	}
	if got, want := sw.window.Content.Background.BG, th.WindowBG; got != want {
		t.Errorf("content background BG = %+v, want %+v", got, want)
	}
	if defContentBG == th.WindowBG {
		t.Errorf("precondition: default content BG %+v should differ from high-contrast %+v", defContentBG, th.WindowBG)
	}

	// Header labels and selectors.
	if sw.modelLabel.FG != th.WindowFG || sw.modelLabel.BG != th.WindowBG || sw.modelLabel.HotFG != th.MnemonicFG {
		t.Errorf("modelLabel not reseeded: FG=%+v BG=%+v HotFG=%+v, want WindowFG/WindowBG/MnemonicFG",
			sw.modelLabel.FG, sw.modelLabel.BG, sw.modelLabel.HotFG)
	}
	if sw.effortLabel.BG != th.WindowBG || sw.effortLabel.HotFG != th.MnemonicFG {
		t.Errorf("effortLabel BG/HotFG not reseeded: BG=%+v HotFG=%+v", sw.effortLabel.BG, sw.effortLabel.HotFG)
	}
	for _, sel := range []*tv.Select{sw.modelSelect, sw.effortSelect} {
		if sel.FG != th.InputFG || sel.BG != th.InputBG || sel.FocusFG != th.InputFocusFG || sel.FocusBG != th.InputFocusBG {
			t.Errorf("select not reseeded: FG=%+v BG=%+v FocusFG=%+v FocusBG=%+v", sel.FG, sel.BG, sel.FocusFG, sel.FocusBG)
		}
	}
	// The cached enabled colour tracks the new window foreground.
	if sw.effortLabelEnabledFG != th.WindowFG {
		t.Errorf("effortLabelEnabledFG = %+v, want %+v", sw.effortLabelEnabledFG, th.WindowFG)
	}

	// Input box and the four input-row buttons.
	if sw.input.FG != th.InputFG || sw.input.BG != th.InputBG || sw.input.FocusFG != th.InputFocusFG || sw.input.FocusBG != th.InputFocusBG {
		t.Errorf("input box not reseeded: FG=%+v BG=%+v FocusFG=%+v FocusBG=%+v",
			sw.input.FG, sw.input.BG, sw.input.FocusFG, sw.input.FocusBG)
	}
	for _, b := range []*tv.Button{sw.sendButton, sw.queueButton, sw.interjectButton} {
		if b.FG != th.ButtonFG || b.BG != th.ButtonBG || b.FocusFG != th.ButtonFocusFG || b.FocusBG != th.ButtonFocusBG || b.ShadowColor != th.ButtonShadow {
			t.Errorf("button not reseeded: FG=%+v BG=%+v FocusFG=%+v FocusBG=%+v Shadow=%+v",
				b.FG, b.BG, b.FocusFG, b.FocusBG, b.ShadowColor)
		}
	}
	if defButtonBG == sw.sendButton.BG {
		t.Errorf("precondition: default button BG %+v should differ after the switch (now %+v)", defButtonBG, sw.sendButton.BG)
	}

	// gogent accents: Stop stays error-red even when focused; the divider rule uses
	// the chrome divider colour; the status line resolves its severity colour live.
	if sw.stopButton.FG != colorError || sw.stopButton.FocusFG != colorError {
		t.Errorf("stop button should stay error-red: FG=%+v FocusFG=%+v, want %+v", sw.stopButton.FG, sw.stopButton.FocusFG, colorError)
	}
	if sw.stopButton.BG != th.ButtonBG {
		t.Errorf("stop button BG = %+v, want reseeded %+v", sw.stopButton.BG, th.ButtonBG)
	}
	if sw.separator.FG != chromeDivider {
		t.Errorf("separator FG = %+v, want chromeDivider %+v", sw.separator.FG, chromeDivider)
	}
	wantStatus := statusColor(!sw.busy, sw.statusStats, sw.wb.budgetConfig())
	if sw.status.FG != wantStatus {
		t.Errorf("status FG = %+v, want live severity colour %+v", sw.status.FG, wantStatus)
	}
}

// TestIssue204RefreshThemeReadOnlySkipsInputChrome checks a read-only analysis
// window (no input chrome) is refreshed without panicking: it re-renders its
// transcript and re-seeds its frame, returning before the nil input widgets.
func TestIssue204RefreshThemeReadOnlySkipsInputChrome(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204Default())

	w := newTestWorkbench(t)
	ro := newSessionWindow(w, "analysis-1", "Saved", tv.Rect{}, true)
	populateAll(ro)
	ro.transcript.render()
	before := entryPtrs(ro)

	ApplyTheme(issue204HighContrast())
	ro.refreshTheme() // must not panic
	after := entryPtrs(ro)

	// Read-only windows never build input chrome.
	if ro.input != nil || ro.sendButton != nil || ro.status != nil || ro.separator != nil {
		t.Errorf("read-only window should have no input chrome: input=%v send=%v status=%v separator=%v",
			ro.input != nil, ro.sendButton != nil, ro.status != nil, ro.separator != nil)
	}
	// Its frame is still re-seeded…
	th := tv.ActiveTheme()
	if ro.window.Content.Background.BG != th.WindowBG {
		t.Errorf("read-only content BG = %+v, want %+v", ro.window.Content.Background.BG, th.WindowBG)
	}
	// …and its transcript is re-rendered.
	rerendered := false
	for i := range before {
		if before[i] != nil && after[i] != nil && before[i] != after[i] {
			rerendered = true
			break
		}
	}
	if !rerendered {
		t.Error("read-only window transcript was not re-rendered by refreshTheme")
	}
}

// TestIssue204RefreshThemeIdempotent checks calling refreshTheme twice is stable and
// does not corrupt state (no duplicate entries, no stale reseed).
func TestIssue204RefreshThemeIdempotent(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204Default())

	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	populateAll(sw)
	sw.transcript.render()
	wantRecords := len(sw.transcript.records)

	sw.refreshTheme()
	sw.refreshTheme()

	if got := len(sw.transcript.records); got != wantRecords {
		t.Errorf("record count changed after double refreshTheme: %d → %d", wantRecords, got)
	}
	all := sw.history.AllText()
	if !strings.Contains(all, "hello world") || !strings.Contains(all, "disk on fire") {
		t.Errorf("transcript content lost after double refreshTheme:\n%s", all)
	}
	th := tv.ActiveTheme()
	if sw.window.Content.Background.BG != th.WindowBG {
		t.Errorf("chrome drifted after double refreshTheme: content BG = %+v, want %+v", sw.window.Content.Background.BG, th.WindowBG)
	}
}

// ----------------------------------------------------------------------------
// Group F: Workbench.RefreshTheme walks every open window (fix C entry point).
// ----------------------------------------------------------------------------

// TestIssue204WorkbenchRefreshThemeReSkinsAllWindows checks RefreshTheme re-skins
// every open session — each window's transcript is re-rendered and its chrome
// re-seeded — not just the desktop/menu. This is the exact chain the SetTheme
// handler runs (ApplyTheme; RefreshTheme).
func TestIssue204WorkbenchRefreshThemeReSkinsAllWindows(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204Default())

	w := newTestWorkbench(t)
	sws := []*SessionWindow{w.openWindow("a", "A"), w.openWindow("b", "B"), w.openWindow("c", "C")}
	for _, sw := range sws {
		populateAll(sw)
		sw.transcript.render()
	}
	before := make([][]*tv.TextEntry, len(sws))
	for i, sw := range sws {
		before[i] = entryPtrs(sw)
	}

	ApplyTheme(issue204HighContrast())
	w.RefreshTheme()
	th := tv.ActiveTheme()

	for i, sw := range sws {
		// Transcript re-rendered.
		rerendered := false
		now := entryPtrs(sw)
		for j := range before[i] {
			if before[i][j] != nil && now[j] != nil && before[i][j] != now[j] {
				rerendered = true
			}
		}
		if !rerendered {
			t.Errorf("window %d (%q) transcript was not re-rendered by Workbench.RefreshTheme", i, sw.title)
		}
		// Chrome re-seeded.
		if sw.window.Content.Background.BG != th.WindowBG {
			t.Errorf("window %d (%q) chrome not re-seeded: content BG = %+v, want %+v", i, sw.title, sw.window.Content.Background.BG, th.WindowBG)
		}
		if sw.sendButton.BG != th.ButtonBG {
			t.Errorf("window %d (%q) send button BG = %+v, want %+v", i, sw.title, sw.sendButton.BG, th.ButtonBG)
		}
	}
}

// TestIssue204WorkbenchRefreshThemeEmptyNoPanic checks RefreshTheme is safe with no
// open sessions (the loop over w.sessions is empty; redraw still runs).
func TestIssue204WorkbenchRefreshThemeEmptyNoPanic(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204Default())

	w := newTestWorkbench(t)
	ApplyTheme(issue204HighContrast())
	w.RefreshTheme() // must not panic with zero sessions
	if got := len(w.sessions); got != 0 {
		t.Errorf("expected zero sessions, got %d", got)
	}
}

// ----------------------------------------------------------------------------
// Group G: end-to-end — the exact scenario from the issue.
// ----------------------------------------------------------------------------

// TestIssue204LiveSwitchRecoloursOpenWindowAndChrome simulates the live theme
// switch a user triggers (the SetTheme handler's ApplyTheme; RefreshTheme chain)
// and asserts an OPEN window — including messages added BEFORE the switch — takes
// on the new palette, not just the desktop. It runs for both presets the issue
// names (high-contrast and dark).
func TestIssue204LiveSwitchRecoloursOpenWindowAndChrome(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target func() Theme
	}{
		{"high-contrast", issue204HighContrast},
		{"dark", issue204Dark},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue204RestoreTheme(t)
			ApplyTheme(issue204Default())

			w := newTestWorkbench(t)
			sw := w.openWindow("s", "S")
			populateAll(sw)
			sw.transcript.render()

			// Snapshot the pre-switch resolved colours and chrome sentinel.
			headerBefore := make([]tui.Color, len(sw.transcript.records))
			for i, r := range sw.transcript.records {
				headerBefore[i] = r.headerColor()
			}
			contentBGBefore := sw.window.Content.Background.BG
			deskBGBefore := chromeDesktopBG

			// The SetTheme handler chain: persist + re-resolve/install + refresh.
			ApplyTheme(tc.target())
			w.RefreshTheme()
			th := tv.ActiveTheme()

			if contentBGBefore == th.WindowBG {
				t.Fatalf("precondition: default content BG %+v should differ from %s WindowBG %+v", contentBGBefore, tc.name, th.WindowBG)
			}

			// Existing records (added before the switch) now resolve to the new palette.
			recChanged := false
			for i, r := range sw.transcript.records {
				got := r.headerColor()
				if got != roleColor(r.role) {
					t.Errorf("record %d (kind %v) header not at live role colour: %+v, want %+v", i, r.kind, got, roleColor(r.role))
				}
				if got == headerBefore[i] {
					t.Errorf("record %d (kind %v) kept its pre-switch colour — messages added before the change must be recoloured", i, r.kind)
				}
				if got != headerBefore[i] {
					recChanged = true
				}
			}
			if !recChanged {
				t.Error("no record header changed colour across the switch")
			}

			// The window chrome and the desktop both moved to the new palette.
			if sw.window.Content.Background.BG != th.WindowBG {
				t.Errorf("content BG = %+v, want %+v", sw.window.Content.Background.BG, th.WindowBG)
			}
			if sw.sendButton.BG != th.ButtonBG {
				t.Errorf("send button BG = %+v, want %+v", sw.sendButton.BG, th.ButtonBG)
			}
			if chromeDesktopBG == deskBGBefore {
				t.Error("desktop background did not change — the desktop repaint half is broken too")
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Group H: rich-Markdown assistant body recolours (re-render + mdPaletteGen bump).
// ----------------------------------------------------------------------------

// TestIssue204AssistantRichBodyRecoloursAfterSwitch checks the rich-Markdown body of
// an assistant answer recomputes against the new palette when the transcript is
// re-rendered after a theme change (ApplyTheme bumps mdPaletteGen, invalidating the
// cached spans). This is the one record class that already re-derived its body
// colours before #204 — the fix makes render() actually run so it takes effect.
func TestIssue204AssistantRichBodyRecoloursAfterSwitch(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204Default())
	if !richMarkdownEnabled() {
		t.Skip("rich Markdown disabled in this environment; body recolour path not exercisable")
	}

	sw := newTestSession()
	sw.addAssistant("# Heading\n\nsome **bold** text and `code`")
	rec := firstRecordOfKind(t, sw, kindAssistant)

	beforeFGs := flatSpanFGs(rec.markdownSpans())

	ApplyTheme(issue204HighContrast())
	afterFGs := flatSpanFGs(rec.markdownSpans())

	if len(beforeFGs) == 0 {
		t.Fatal("rich body produced no coloured spans under the default theme")
	}
	same := len(beforeFGs) == len(afterFGs)
	if same {
		for i := range beforeFGs {
			if beforeFGs[i] != afterFGs[i] {
				same = false
				break
			}
		}
	}
	if same {
		t.Errorf("rich body span colours did not change across a default→high-contrast switch (before=after=%+v); the body did not recompute against the new palette", beforeFGs)
	}
}

// flatSpanFGs flattens a record's styled spans into the ordered list of span
// foreground colours, skipping spans that carry no foreground colour.
func flatSpanFGs(spans [][]tv.StyledSpan) []tui.Color {
	var out []tui.Color
	for _, line := range spans {
		for _, s := range line {
			if s.HasFG {
				out = append(out, s.FG)
			}
		}
	}
	return out
}

// ----------------------------------------------------------------------------
// Group I: DEFECTS FOUND — cached construction-time colours refreshTheme leaves
// frozen. These tests currently FAIL and document real gaps in fix C. Per the issue
// spec, refreshTheme must refresh "any cached construction-time colours" and "cover
// the same regions a restart fixes"; a restart re-seeds these from ActiveTheme at
// construction (NewTextView / NewLabel), so the live switch should too.
// ----------------------------------------------------------------------------

// TestIssue204DefectTranscriptViewColorsNotRefreshed: the transcript TextView
// (sw.history) caches FG/BG/FocusFG from tv.ActiveTheme at construction and draws
// with them (TextView.draw fills its whole bounds with BG). refreshTheme re-renders
// the entries and re-seeds the window content surface, but never touches the view's
// own colours, so the transcript AREA keeps the old window background after a live
// theme switch — e.g. a blue transcript panel inside a black window on default→dark.
// The fix is to reseed sw.history.{FG,BG,FocusFG} from tv.ActiveTheme() in
// refreshTheme (and Clear() does not reset them, so render() alone is not enough).
func TestIssue204DefectTranscriptViewColorsNotRefreshed(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204Default())

	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	defBG := sw.history.BG

	ApplyTheme(issue204HighContrast())
	sw.refreshTheme()
	th := tv.ActiveTheme()

	// Precondition: the theme really switched (so a stale value is a real defect,
	// not a no-op).
	if defBG == th.WindowBG {
		t.Fatalf("precondition failed: default WindowBG %+v == high-contrast %+v", defBG, th.WindowBG)
	}

	if sw.history.FG != th.WindowFG {
		t.Errorf("DEFECT: transcript view FG not reseeded by refreshTheme: got %+v, want %+v — the transcript area keeps the old foreground",
			sw.history.FG, th.WindowFG)
	}
	if sw.history.BG != th.WindowBG {
		t.Errorf("DEFECT: transcript view BG not reseeded by refreshTheme: got %+v, want %+v — the transcript area keeps the old background after a live switch",
			sw.history.BG, th.WindowBG)
	}
	if sw.history.FocusFG != th.MnemonicFG {
		t.Errorf("DEFECT: transcript view FocusFG not reseeded by refreshTheme: got %+v, want %+v", sw.history.FocusFG, th.MnemonicFG)
	}
}

// TestIssue204DefectLabelBackgroundsNotRefreshed: modelLabel/effortLabel get their
// FG/BG/HotFG reseeded via reseedLabel, but the status and separator labels are only
// given a fresh FG (refreshStatus / the explicit chromeDivider set). Their BG/HotFG
// stay frozen at the construction-time window background, inconsistent with both the
// other labels and a restart.
func TestIssue204DefectLabelBackgroundsNotRefreshed(t *testing.T) {
	issue204RestoreTheme(t)
	ApplyTheme(issue204Default())

	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	defStatusBG, defSepBG := sw.status.BG, sw.separator.BG

	ApplyTheme(issue204HighContrast())
	sw.refreshTheme()
	th := tv.ActiveTheme()

	if defStatusBG == th.WindowBG {
		t.Fatalf("precondition failed: default status BG %+v == high-contrast WindowBG %+v", defStatusBG, th.WindowBG)
	}

	if sw.status.BG != th.WindowBG {
		t.Errorf("DEFECT: status label BG not reseeded: got %+v, want %+v (modelLabel/effortLabel ARE reseeded — this is inconsistent)",
			sw.status.BG, th.WindowBG)
	}
	if sw.status.HotFG != th.MnemonicFG {
		t.Errorf("DEFECT: status label HotFG not reseeded: got %+v, want %+v", sw.status.HotFG, th.MnemonicFG)
	}
	if defSepBG == th.WindowBG {
		t.Fatalf("precondition failed: default separator BG %+v == high-contrast WindowBG %+v", defSepBG, th.WindowBG)
	}
	if sw.separator.BG != th.WindowBG {
		t.Errorf("DEFECT: separator label BG not reseeded: got %+v, want %+v", sw.separator.BG, th.WindowBG)
	}
	if sw.separator.HotFG != th.MnemonicFG {
		t.Errorf("DEFECT: separator label HotFG not reseeded: got %+v, want %+v", sw.separator.HotFG, th.MnemonicFG)
	}
}
