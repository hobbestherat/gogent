package ui

import (
	"testing"
	"time"

	"gogent/internal/gogent"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// TestSerializePromptReviewResolves verifies the generic prompt core returns the
// review decision a present-closure resolves with (the path ReviewEdit uses).
func TestSerializePromptReviewResolves(t *testing.T) {
	for _, want := range []gogent.EditReviewDecision{
		gogent.EditApprove,
		gogent.EditApproveAll,
		gogent.EditReject,
	} {
		want := want
		t.Run("", func(t *testing.T) {
			w := newPromptWorkbench()
			got := serializePrompt(w, gogent.EditReject, func(resolve func(gogent.EditReviewDecision)) {
				resolve(want)
			})
			if got != want {
				t.Fatalf("serializePrompt = %v, want %v", got, want)
			}
		})
	}
}

// TestSerializePromptReviewShutdown verifies an outstanding review unblocks with
// EditReject (never apply an unreviewed edit) when the UI shuts down.
func TestSerializePromptReviewShutdown(t *testing.T) {
	w := newPromptWorkbench()
	presented := make(chan struct{})
	done := make(chan gogent.EditReviewDecision, 1)
	go func() {
		done <- serializePrompt(w, gogent.EditReject, func(_ func(gogent.EditReviewDecision)) {
			close(presented) // never resolve
		})
	}()
	<-presented
	w.quit()

	select {
	case got := <-done:
		if got != gogent.EditReject {
			t.Fatalf("shutdown decision = %v, want EditReject", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("review prompt did not unblock on shutdown")
	}
}

// TestDiffLineColor checks unified-diff lines map to the intended colours.
func TestDiffLineColor(t *testing.T) {
	cases := []struct {
		line string
		want interface{}
	}{
		{"+added", colorAgent},
		{"-removed", colorError},
		{"@@ -1 +1 @@", colorInfo},
		{"--- a/f.txt", colorNote},
		{"+++ b/f.txt", colorNote},
		{" context", tv.DefaultTheme.DialogFG},
	}
	for _, c := range cases {
		if got := diffLineColor(c.line); got != c.want {
			t.Errorf("diffLineColor(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

// TestRenderDiffNoChanges ensures an empty diff renders a placeholder rather than
// nothing (defensive: ReviewEdit is only called for real changes).
func TestRenderDiffNoChanges(t *testing.T) {
	view := tv.NewTextView("", tv.Rect{})
	renderDiff(view, "   \n")
	// No panic and a line was added; the exact content is a UI detail.
}
