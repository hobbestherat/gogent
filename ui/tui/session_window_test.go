package ui

import (
	"testing"

	"gogent/internal/agent"
)

// TestFormatStatusLine covers the status line composition: full line on wide
// windows, graceful right-most-segment-first truncation on narrow ones, zero
// suppression, and state truncation as a last resort.
func TestFormatStatusLine(t *testing.T) {
	full := agent.SessionStats{
		TokensIn: 12300, TokensOut: 4100,
		Turns:         7,
		ContextTokens: 38000, ContextWindow: 100000,
	}
	for _, tc := range []struct {
		name  string
		state string
		stats agent.SessionStats
		width int
		want  string
	}{
		{
			name:  "fresh idle session shows state only",
			state: "idle", stats: agent.SessionStats{}, width: 80,
			want: "idle",
		},
		{
			name:  "full stats on a wide window",
			state: "idle", stats: full, width: 80,
			want: "idle · 12.3k/4.1k tok · 7 turns · ctx 38%",
		},
		{
			name:  "working state with full stats",
			state: "working...", stats: full, width: 80,
			want: "working... · 12.3k/4.1k tok · 7 turns · ctx 38%",
		},
		{
			name:  "narrow drops context percent first",
			state: "working...", stats: full, width: 40,
			want: "working... · 12.3k/4.1k tok · 7 turns",
		},
		{
			name:  "narrower drops turns too",
			state: "working...", stats: full, width: 30,
			want: "working... · 12.3k/4.1k tok",
		},
		{
			name:  "only the state fits",
			state: "working...", stats: full, width: 12,
			want: "working...",
		},
		{
			name:  "state truncated when narrower than itself",
			state: "working...", stats: full, width: 5,
			want: "worki",
		},
		{
			name:  "zero width returns the state untouched",
			state: "idle", stats: full, width: 0,
			want: "idle",
		},
		{
			name:  "only context window known (no tokens/turns yet)",
			state: "idle", stats: agent.SessionStats{ContextTokens: 5000, ContextWindow: 100000}, width: 80,
			want: "idle · ctx 5%",
		},
		{
			name:  "only tokens known (no turns, unknown window)",
			state: "idle", stats: agent.SessionStats{TokensIn: 400, TokensOut: 0}, width: 80,
			want: "idle · 400/0 tok",
		},
		{
			name:  "tokens suppressed when both in and out are zero",
			state: "idle", stats: agent.SessionStats{Turns: 1, ContextTokens: 0, ContextWindow: 1000}, width: 80,
			want: "idle · 1 turns · ctx 0%",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatStatusLine(tc.state, tc.stats, tc.width); got != tc.want {
				t.Errorf("formatStatusLine(%q, %+v, %d) = %q, want %q",
					tc.state, tc.stats, tc.width, got, tc.want)
			}
		})
	}
}

// TestFormatTokens covers the k/M compaction and the under-a-thousand plain form.
func TestFormatTokens(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want string
	}{
		{0, "0"}, {999, "999"},
		{1000, "1.0k"}, {12300, "12.3k"}, {999999, "1000.0k"},
		{1_000_000, "1.0M"}, {1_500_000, "1.5M"}, {12_345_678, "12.3M"},
	} {
		if got := formatTokens(tc.in); got != tc.want {
			t.Errorf("formatTokens(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestContextPercent covers the percentage, clamping and the unknown-window case.
func TestContextPercent(t *testing.T) {
	for _, tc := range []struct {
		tokens, window int
		want           int
	}{
		{0, 1000, 0},
		{500, 1000, 50},
		{38000, 100000, 38},
		{80000, 100000, 80},
		{100000, 100000, 100},
		{150000, 100000, 100}, // clamped
		{500, 0, 0},           // unknown window
	} {
		if got := contextPercent(tc.tokens, tc.window); got != tc.want {
			t.Errorf("contextPercent(%d, %d) = %d, want %d", tc.tokens, tc.window, got, tc.want)
		}
	}
}

// TestStatusSegments covers ordering and zero suppression of the stat segments.
func TestStatusSegments(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stats agent.SessionStats
		want  []string
	}{
		{"empty", agent.SessionStats{}, nil},
		{"all", agent.SessionStats{TokensIn: 1, TokensOut: 2, Turns: 3, ContextTokens: 4, ContextWindow: 10},
			[]string{"1/2 tok", "3 turns", "ctx 40%"}},
		{"tokens only", agent.SessionStats{TokensIn: 1000, TokensOut: 500}, []string{"1.0k/500 tok"}},
		{"ctx only", agent.SessionStats{ContextTokens: 9, ContextWindow: 10}, []string{"ctx 90%"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := statusSegments(tc.stats)
			if len(got) != len(tc.want) {
				t.Fatalf("statusSegments(%+v) = %v, want %v", tc.stats, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("statusSegments(%+v)[%d] = %q, want %q", tc.stats, i, got[i], tc.want[i])
				}
			}
		})
	}
}
