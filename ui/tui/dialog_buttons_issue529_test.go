package ui

import (
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Issue #529 (gogent half): the Models… dialog opts its footer into 2-row-tall
// buttons via footerButtonRectsH, while EVERY other dialog footer stays on the
// 1-row footerButtonRects default. These unit tests pin the helper contract that
// keeps that opt-in local:
//   - the default path stays H==1 (so no other dialog grows taller buttons),
//   - the height-aware variant carries exactly the requested H,
//   - a non-positive H collapses to 1 (mirroring turbotui's ButtonHeight),
//   - the two variants share their horizontal layout, so the tall footer re-flows
//     nothing but the height (right-alignment / gap / clamp are unchanged).

// TestFooterButtonRectsDefaultIsOneRowTall is the cross-dialog regression guard:
// footerButtonRects (the signature every non-Models dialog footer uses — statistics,
// watchers, commands, sessions, command-history, keybindings) must keep yielding
// H==1 rects, so the tall-footer opt-in cannot leak into them. footerButtonRects is
// now a thin wrapper over footerButtonRectsH(..., 1); this also pins that delegation.
func TestFooterButtonRectsDefaultIsOneRowTall(t *testing.T) {
	labels := []string{"&OK", "Cancel", "Export &CSV"}
	rects := footerButtonRects(labels, 2, 60, 5, tv.DefaultButtonGap)
	if len(rects) != len(labels) {
		t.Fatalf("got %d rects, want %d", len(rects), len(labels))
	}
	for i, r := range rects {
		if r.H != 1 {
			t.Errorf("footerButtonRects rect[%d] = %+v, want H==1 (every non-Models dialog must stay 1-row)", i, r)
		}
	}
}

// TestFooterButtonRectsHCarriesExplicitHeight verifies the height-aware variant
// stamps exactly the requested H onto every rect, at any positive height — the
// mechanism the Models… footer uses to render 2 rows tall.
func TestFooterButtonRectsHCarriesExplicitHeight(t *testing.T) {
	labels := []string{"Add &Empty…", "&Edit…", "&Done"}
	for _, h := range []int{2, 3, 5} {
		rects := footerButtonRectsH(labels, 2, 80, 4, tv.DefaultButtonGap, h)
		if len(rects) != len(labels) {
			t.Fatalf("h=%d: got %d rects, want %d", h, len(rects), len(labels))
		}
		for i, r := range rects {
			if r.H != h {
				t.Errorf("h=%d: rect[%d].H = %d, want %d", h, i, r.H, h)
			}
		}
	}
}

// TestFooterButtonRectsHCollapsesNonPositiveToOne mirrors turbotui's ButtonHeight
// contract (bounds.H<=0 -> 1): a zero or negative height degrades to the ordinary
// single-row footer rather than an invisible (H<=0) button. This is the documented
// "default h==0 => 1" rule from the design.
func TestFooterButtonRectsHCollapsesNonPositiveToOne(t *testing.T) {
	labels := []string{"&Yes", "&No"}
	for _, h := range []int{0, -1, -7} {
		rects := footerButtonRectsH(labels, 2, 40, 1, tv.DefaultButtonGap, h)
		if len(rects) != len(labels) {
			t.Fatalf("h=%d: got %d rects, want %d", h, len(rects), len(labels))
		}
		for i, r := range rects {
			if r.H != 1 {
				t.Errorf("h=%d: rect[%d].H = %d, want collapsed to 1", h, i, r.H)
			}
		}
	}
}

// TestFooterButtonRectsHSharesHorizontalLayoutWithDefault proves the height-aware
// variant changes ONLY the rect height: for identical labels/edges/row/gap, the X,
// Y and W of footerButtonRectsH(..., h) equal footerButtonRects(...) exactly. A drift
// here would mean the tall footer re-flowed its buttons (right-alignment/gap/clamp)
// differently from every other dialog's footer — a silent layout regression.
func TestFooterButtonRectsHSharesHorizontalLayoutWithDefault(t *testing.T) {
	labels := []string{"Add from &Catalog…", "Add &Empty…", "&Edit…", "&Remove", "&Set Default", "&Done"}
	const leftX, rightX, y = 2, 100, 7
	gap := tv.DefaultButtonGap
	base := footerButtonRects(labels, leftX, rightX, y, gap)
	tall := footerButtonRectsH(labels, leftX, rightX, y, gap, 2)
	if len(base) != len(tall) {
		t.Fatalf("len base=%d tall=%d", len(base), len(tall))
	}
	for i := range base {
		if base[i].X != tall[i].X || base[i].Y != tall[i].Y || base[i].W != tall[i].W {
			t.Errorf("rect[%d] layout differs: base=%+v tall=%+v (only H should differ)", i, base[i], tall[i])
		}
		if base[i].H != 1 || tall[i].H != 2 {
			t.Errorf("rect[%d] H: base=%d tall=%d, want base=1 tall=2", i, base[i].H, tall[i].H)
		}
	}
}

// TestFooterButtonRectsHRightAlignmentUnchangedByHeight confirms the tall footer
// still right-aligns to rightX (the last button's right edge sits on rightX) and
// honours the inter-button gap — the single-row behaviour issue #529 explicitly
// preserves ("buttons may stay on ONE row if wide enough; the change is HEIGHT, not
// row count"). With a group that fits, no clamping occurs.
func TestFooterButtonRectsHRightAlignmentUnchangedByHeight(t *testing.T) {
	labels := []string{"&Edit…", "&Done"}
	const leftX, rightX, y = 2, 50, 9
	gap := tv.DefaultButtonGap
	rects := footerButtonRectsH(labels, leftX, rightX, y, gap, 2)
	if len(rects) != 2 {
		t.Fatalf("got %d rects, want 2", len(rects))
	}
	first, last := rects[0], rects[1]
	if last.X+last.W-1 != rightX {
		t.Errorf("rightmost button right edge = %d, want %d (right-alignment must survive the height change)",
			last.X+last.W-1, rightX)
	}
	// Blank columns between the first button's right edge and the last button's left
	// edge must equal gap (the layout loop reserves `gap` for the separator).
	if blank := last.X - first.X - first.W; blank != gap {
		t.Errorf("blank columns between buttons = %d, want gap %d (inter-button spacing unchanged by height)", blank, gap)
	}
}

// TestFooterButtonRectsHClampsToEdgesLikeDefault verifies the tall variant still
// clamps rects to [leftX, rightX] (via clampDialogRect) so a too-narrow dialog
// degrades to clipping instead of negative-width or border-crossing rects — and
// that the clamp is identical to the 1-row path. The height must not weaken it.
func TestFooterButtonRectsHClampsToEdgesLikeDefault(t *testing.T) {
	// A rightX too small to hold even one button: the single rect clamps to width
	// rightX-leftX+1 and never goes negative, at any height.
	labels := []string{"Add from &Catalog…"}
	const leftX, rightX, y = 2, 4, 3 // only 3 columns of room
	gap := tv.DefaultButtonGap
	for _, h := range []int{1, 2} {
		rects := footerButtonRectsH(labels, leftX, rightX, y, gap, h)
		r := rects[0]
		if r.W < 0 {
			t.Errorf("h=%d: clamped rect has negative width %+v", h, r)
		}
		if r.X < leftX {
			t.Errorf("h=%d: rect.X=%d before leftX=%d", h, r.X, leftX)
		}
		if right := r.X + r.W - 1; right > rightX {
			t.Errorf("h=%d: rect right edge %d past rightX=%d (clamp failed)", h, right, rightX)
		}
		if r.H != h {
			t.Errorf("h=%d: clamped rect H=%d, want the height preserved through clamping", h, r.H)
		}
	}
}
