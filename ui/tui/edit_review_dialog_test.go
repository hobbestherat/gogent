package ui

import "testing"

// TestDiffLineColor checks the unified-diff line classifier the review dialog
// uses to colour additions, deletions, headers and context distinctly.
func TestDiffLineColor(t *testing.T) {
	tests := []struct {
		line string
		want interface{}
	}{
		{"--- path", colorInfo},
		{"+++ path", colorInfo},
		{"@@ -1,3 +1,4 @@", colorInfo},
		{"+added line", colorAgent},
		{"-removed line", colorError},
		{" context line", colorNote},
		{"", colorNote},
	}
	for _, tt := range tests {
		if got := diffLineColor(tt.line); got != tt.want {
			t.Errorf("diffLineColor(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}
