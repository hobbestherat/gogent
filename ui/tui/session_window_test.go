package ui

import (
	"testing"
	"time"

	tui "github.com/hobbestherat/turbotui"
	"gogent/internal/agent"
	"gogent/internal/config"
)

// noLive / noBudget are the "nothing transient configured" zero values used by
// the static status-line cases (no turn in flight, no token budget).
var (
	noLive   = liveStats{}
	noBudget = config.BudgetConfig{}
)

// TestFormatStatusLine covers the status line composition: full line on wide
// windows, graceful right-most-segment-first truncation on narrow ones, zero
// suppression, and state truncation as a last resort. It also covers the live
// elapsed/throughput segments and the budget-exceeded marker.
func TestFormatStatusLine(t *testing.T) {
	full := agent.SessionStats{
		TokensIn: 12300, TokensOut: 4100,
		Turns:         7,
		ContextTokens: 38000, ContextWindow: 100000,
	}
	for _, tc := range []struct {
		name   string
		state  string
		stats  agent.SessionStats
		live   liveStats
		budget config.BudgetConfig
		width  int
		want   string
	}{
		{
			name:  "fresh idle session shows state only",
			state: "idle", stats: agent.SessionStats{}, width: 80,
			want: "idle",
		},
		{
			name:  "full stats on a wide window",
			state: "idle", stats: full, width: 80,
			want: "idle · 12.3k/4.1k tok · 7 turns · ctx ▰▰▱▱▱▱ 38%",
		},
		{
			name:  "working state with full stats",
			state: "working...", stats: full, width: 80,
			want: "working... · 12.3k/4.1k tok · 7 turns · ctx ▰▰▱▱▱▱ 38%",
		},
		{
			name:  "narrow drops context gauge first",
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
			want: "idle · ctx ▰▱▱▱▱▱ 5%",
		},
		{
			name:  "only tokens known (no turns, unknown window)",
			state: "idle", stats: agent.SessionStats{TokensIn: 400, TokensOut: 0}, width: 80,
			want: "idle · 400/0 tok",
		},
		{
			name:  "tokens suppressed when both in and out are zero",
			state: "idle", stats: agent.SessionStats{Turns: 1, ContextTokens: 0, ContextWindow: 1000}, width: 80,
			want: "idle · 1 turns · ctx ▱▱▱▱▱▱ 0%",
		},
		{
			name:  "live elapsed and throughput shown during generation",
			state: "working...", stats: agent.SessionStats{TokensIn: 400, TokensOut: 120},
			live:  liveStats{elapsed: 12 * time.Second, tokensPerSec: 10}, width: 80,
			want: "working... · 12s · 10 t/s · 400/120 tok",
		},
		{
			name:  "sub-one throughput renders as <1 t/s",
			state: "working...", stats: agent.SessionStats{TokensOut: 3},
			live:  liveStats{elapsed: 10 * time.Second, tokensPerSec: 0.3}, width: 80,
			want: "working... · 10s · <1 t/s · 0/3 tok",
		},
		{
			name:   "budget exceeded marker leads the stats",
			state:  "working...", stats: agent.SessionStats{TokensIn: 600, TokensOut: 400},
			budget: config.BudgetConfig{TokenBudget: 1000}, width: 80,
			want:   "working... · budget! · 600/400 tok",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := formatStatusLine(tc.state, tc.stats, tc.live, tc.budget, tc.width)
			if got != tc.want {
				t.Errorf("formatStatusLine(%q, %+v, %+v, %+v, %d) = %q, want %q",
					tc.state, tc.stats, tc.live, tc.budget, tc.width, got, tc.want)
			}
		})
	}
}

// TestContextGauge covers the bar fill: zero/unknown usage is empty, any nonzero
// usage shows at least one cell, mid usage rounds to the nearest cell, and usage
// at/over the window fills every cell.
func TestContextGauge(t *testing.T) {
	for _, tc := range []struct {
		name         string
		tokens, win  int
		want         string
	}{
		{"unknown window", 100, 0, "▱▱▱▱▱▱"},
		{"zero usage", 0, 1000, "▱▱▱▱▱▱"},
		{"tiny nonzero usage shows one cell", 1, 1000, "▰▱▱▱▱▱"},
		{"ten percent", 100, 1000, "▰▱▱▱▱▱"},
		{"thirty eight percent", 38000, 100000, "▰▰▱▱▱▱"},
		{"fifty percent", 500, 1000, "▰▰▰▱▱▱"},
		{"ninety percent", 900, 1000, "▰▰▰▰▰▱"},
		{"full window", 1000, 1000, "▰▰▰▰▰▰"},
		{"over full clamps", 2000, 1000, "▰▰▰▰▰▰"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := contextGauge(tc.tokens, tc.win); got != tc.want {
				t.Errorf("contextGauge(%d, %d) = %q, want %q", tc.tokens, tc.win, got, tc.want)
			}
		})
	}
}

// TestContextSegment covers the full "ctx ‹bar› ‹pct›%" segment.
func TestContextSegment(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stats agent.SessionStats
		want  string
	}{
		{"38 percent", agent.SessionStats{ContextTokens: 38000, ContextWindow: 100000}, "ctx ▰▰▱▱▱▱ 38%"},
		{"zero usage", agent.SessionStats{ContextTokens: 0, ContextWindow: 1000}, "ctx ▱▱▱▱▱▱ 0%"},
		{"unknown window", agent.SessionStats{ContextTokens: 5, ContextWindow: 0}, "ctx ▱▱▱▱▱▱ 0%"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := contextSegment(tc.stats); got != tc.want {
				t.Errorf("contextSegment(%+v) = %q, want %q", tc.stats, got, tc.want)
			}
		})
	}
}

// TestStatusColor covers the severity-over-state colour mapping for both the
// context thresholds and the token budget.
func TestStatusColor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		idle   bool
		stats  agent.SessionStats
		budget config.BudgetConfig
		want   tui.Color
	}{
		{"idle no stats", true, agent.SessionStats{}, config.BudgetConfig{}, colorNote},
		{"working no stats", false, agent.SessionStats{}, config.BudgetConfig{}, colorInfo},
		{"context below warn stays state colour", false, agent.SessionStats{ContextTokens: 50000, ContextWindow: 100000}, config.BudgetConfig{}, colorInfo},
		{"context at warn turns amber", false, agent.SessionStats{ContextTokens: 60000, ContextWindow: 100000}, config.BudgetConfig{}, colorTool},
		{"context near threshold amber", false, agent.SessionStats{ContextTokens: 79000, ContextWindow: 100000}, config.BudgetConfig{}, colorTool},
		{"context at threshold turns red", false, agent.SessionStats{ContextTokens: 80000, ContextWindow: 100000}, config.BudgetConfig{}, colorError},
		{"context full red", false, agent.SessionStats{ContextTokens: 100000, ContextWindow: 100000}, config.BudgetConfig{}, colorError},
		{"severity overrides idle", true, agent.SessionStats{ContextTokens: 80000, ContextWindow: 100000}, config.BudgetConfig{}, colorError},
		{"budget approaching amber", false, agent.SessionStats{TokensIn: 800}, config.BudgetConfig{TokenBudget: 1000}, colorTool},
		{"budget exceeded red", false, agent.SessionStats{TokensIn: 1000}, config.BudgetConfig{TokenBudget: 1000}, colorError},
		{"budget exceeded overrides low context", false, agent.SessionStats{TokensIn: 1000, ContextTokens: 100, ContextWindow: 100000}, config.BudgetConfig{TokenBudget: 1000}, colorError},
		{"disabled budget ignored", true, agent.SessionStats{TokensIn: 9999}, config.BudgetConfig{TokenBudget: 0}, colorNote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusColor(tc.idle, tc.stats, tc.budget); got != tc.want {
				t.Errorf("statusColor(%v, %+v, %+v) = %v, want %v", tc.idle, tc.stats, tc.budget, got, tc.want)
			}
		})
	}
}

// TestBudgetStatus covers the OK/approaching/exceeded classification, including
// the default and a custom warn fraction, and the disabled case.
func TestBudgetStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stats  agent.SessionStats
		budget config.BudgetConfig
		want   budgetLevel
	}{
		{"no usage no budget", agent.SessionStats{}, config.BudgetConfig{TokenBudget: 1000}, budgetOK},
		{"well under budget", agent.SessionStats{TokensIn: 500}, config.BudgetConfig{TokenBudget: 1000}, budgetOK},
		{"just under warn", agent.SessionStats{TokensIn: 799}, config.BudgetConfig{TokenBudget: 1000}, budgetOK},
		{"at default warn (80%) approaching", agent.SessionStats{TokensIn: 800}, config.BudgetConfig{TokenBudget: 1000}, budgetApproaching},
		{"at budget exceeded", agent.SessionStats{TokensIn: 1000}, config.BudgetConfig{TokenBudget: 1000}, budgetExceeded},
		{"over budget exceeded", agent.SessionStats{TokensIn: 1500}, config.BudgetConfig{TokenBudget: 1000}, budgetExceeded},
		{"counts in plus out", agent.SessionStats{TokensIn: 400, TokensOut: 400}, config.BudgetConfig{TokenBudget: 1000}, budgetApproaching},
		{"disabled budget always ok", agent.SessionStats{TokensIn: 9999}, config.BudgetConfig{TokenBudget: 0}, budgetOK},
		{"custom warn fraction", agent.SessionStats{TokensIn: 500}, config.BudgetConfig{TokenBudget: 1000, WarnFraction: 0.5}, budgetApproaching},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := budgetStatus(tc.stats, tc.budget); got != tc.want {
				t.Errorf("budgetStatus(%+v, %+v) = %d, want %d", tc.stats, tc.budget, got, tc.want)
			}
		})
	}
}

// TestFormatDuration covers the under/over-a-minute forms and omission of
// non-positive durations.
func TestFormatDuration(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, ""},
		{-time.Second, ""},
		{1500 * time.Millisecond, "1s"}, // 1.5s truncates to 1s
		{time.Second, "1s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m00s"},
		{90 * time.Second, "1m30s"},
		{125 * time.Second, "2m05s"},
		{time.Hour, "60m00s"},
	} {
		if got := formatDuration(tc.d); got != tc.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestFormatTokensPerSec covers the rounding, the <1 t/s floor, and omission of
// non-positive throughput.
func TestFormatTokensPerSec(t *testing.T) {
	for _, tc := range []struct {
		tps  float64
		want string
	}{
		{0, ""},
		{-1, ""},
		{0.4, "<1 t/s"},
		{0.6, "1 t/s"},
		{1, "1 t/s"},
		{12.4, "12 t/s"},
		{12.5, "13 t/s"}, // rounds to nearest
		{12.7, "13 t/s"},
		{100, "100 t/s"},
	} {
		if got := formatTokensPerSec(tc.tps); got != tc.want {
			t.Errorf("formatTokensPerSec(%v) = %q, want %q", tc.tps, got, tc.want)
		}
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

// TestStatusSegments covers ordering and zero suppression of the stat segments,
// including the budget marker and live elapsed/throughput.
func TestStatusSegments(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stats  agent.SessionStats
		live   liveStats
		budget config.BudgetConfig
		want   []string
	}{
		{"empty", agent.SessionStats{}, noLive, noBudget, nil},
		{"all", agent.SessionStats{TokensIn: 1, TokensOut: 2, Turns: 3, ContextTokens: 4, ContextWindow: 10}, noLive, noBudget,
			[]string{"1/2 tok", "3 turns", "ctx ▰▰▱▱▱▱ 40%"}},
		{"tokens only", agent.SessionStats{TokensIn: 1000, TokensOut: 500}, noLive, noBudget, []string{"1.0k/500 tok"}},
		{"ctx only", agent.SessionStats{ContextTokens: 9, ContextWindow: 10}, noLive, noBudget, []string{"ctx ▰▰▰▰▰▱ 90%"}},
		{"live elapsed and throughput", agent.SessionStats{TokensOut: 120}, liveStats{elapsed: 12 * time.Second, tokensPerSec: 10}, noBudget,
			[]string{"12s", "10 t/s", "0/120 tok"}},
		{"budget exceeded marker first", agent.SessionStats{TokensIn: 600, TokensOut: 400}, noLive, config.BudgetConfig{TokenBudget: 1000},
			[]string{"budget!", "600/400 tok"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := statusSegments(tc.stats, tc.live, tc.budget)
			if len(got) != len(tc.want) {
				t.Fatalf("statusSegments(%+v, %+v, %+v) = %v, want %v", tc.stats, tc.live, tc.budget, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("statusSegments(%+v, %+v, %+v)[%d] = %q, want %q", tc.stats, tc.live, tc.budget, i, got[i], tc.want[i])
				}
			}
		})
	}
}
