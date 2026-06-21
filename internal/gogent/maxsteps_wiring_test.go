package gogent

import (
	"testing"

	"gogent/internal/config"
)

// intptr is a local *int helper; config.intPtr is unexported and this package
// cannot reach it.
func intptr(v int) *int { return &v }

// newMaxStepsGogent builds a Gogent whose single model points at an unused
// endpoint, with the given configured max_steps, so CreateUserSession's wiring
// of the step cap can be asserted in isolation (no network is touched).
func newMaxStepsGogent(t *testing.T, maxSteps *int) *Gogent {
	t.Helper()
	g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())
	g.config = &config.Config{
		DefaultModel: "test",
		ModelConfigs: []*config.ModelConfig{{
			Name:     "test",
			Model:    "glm-5.2",
			APIType:  "zai",
			Endpoint: "http://unused.test/v1/chat/completions",
		}},
		MaxSteps: maxSteps,
	}
	return g
}

// TestCreateUserSessionWiresMaxStepsFromConfig verifies the gogent.go wiring
// line: CreateUserSession applies config.MaxStepsOrDefault() to the new session
// (issue #249). A nil/absent setting resolves to the default 25; an explicit 0
// (unlimited) and a positive N are passed through verbatim.
func TestCreateUserSessionWiresMaxStepsFromConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  *int
		want int
	}{
		{"nil resolves to default 25", nil, config.DefaultMaxSteps},
		{"explicit 0 wires as unlimited", intptr(0), 0},
		{"explicit 13 wires verbatim", intptr(13), 13},
		{"explicit 1 wires verbatim", intptr(1), 1},
		{"negative wires as unlimited", intptr(-2), -2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newMaxStepsGogent(t, tc.cfg)
			us := g.NewSession("s1")
			if us == nil {
				t.Fatal("NewSession returned nil")
			}
			if got := us.MaxSteps(); got != tc.want {
				t.Errorf("wired MaxSteps = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestCreateUserSessionDefaultConfigKeepsHistoricalBound confirms the full
// GetDefaultConfig -> CreateUserSession path yields the historical 25-step cap,
// so a user who never sets max_steps sees no behaviour change from before #249.
func TestCreateUserSessionDefaultConfigKeepsHistoricalBound(t *testing.T) {
	g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())
	g.config = config.GetDefaultConfig()
	// Keep a reachable model so defaultConnection() resolves during NewSession.
	g.config.DefaultModel = "test"
	g.config.ModelConfigs = []*config.ModelConfig{{
		Name:     "test",
		Model:    "glm-5.2",
		APIType:  "zai",
		Endpoint: "http://unused.test/v1/chat/completions",
	}}

	us := g.NewSession("default-cfg")
	if got := us.MaxSteps(); got != 25 {
		t.Errorf("default-config session MaxSteps = %d, want 25 (historical bound)", got)
	}
}
