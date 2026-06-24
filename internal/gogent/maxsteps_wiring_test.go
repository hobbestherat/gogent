package gogent

import (
	"os"
	"path/filepath"
	"testing"

	"gogent/internal/agent"
	"gogent/internal/config"
	"gogent/internal/permission"
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
// (issue #249). A nil/absent setting resolves to the built-in default
// (DefaultMaxSteps, raised to 100 in #449); an explicit 0 (unlimited) and a
// positive N are passed through verbatim.
func TestCreateUserSessionWiresMaxStepsFromConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  *int
		want int
	}{
		{"nil resolves to built-in default", nil, config.DefaultMaxSteps},
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
// GetDefaultConfig -> CreateUserSession path yields the built-in step cap
// (DefaultMaxSteps), so a user who never sets max_steps sees the documented
// backstop. #449 raised that backstop from the historical 25 to 100.
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
	if got := us.MaxSteps(); got != config.DefaultMaxSteps {
		t.Errorf("default-config session MaxSteps = %d, want %d (built-in default; raised to 100 in #449)",
			got, config.DefaultMaxSteps)
	}
}

func TestNewGogentLoadsRulesJSONGuardrailsAtStartup(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".gogent"), 0700); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gogent", "rules.json"), []byte(`{
		"rules": [
			{"action":"write","resource":"blocked*","effect":"deny"}
		]
	}`), 0600); err != nil {
		t.Fatalf("write rules.json: %v", err)
	}

	g := NewGogentWithWorkspace(home, t.TempDir())
	if err := g.GetPermissionService().Check(permission.ActionWrite, "blocked.txt"); err == nil {
		t.Fatal("rules.json deny should beat gogent's default write allow rule")
	}
	if err := g.GetPermissionService().Check(permission.ActionWrite, "allowed.txt"); err != nil {
		t.Fatalf("default write allow should still apply for non-guarded paths, got %v", err)
	}
}

func TestGlobalYoloWiresUnlimitedStepsAndPermissionAutoApprove(t *testing.T) {
	g := newMaxStepsGogent(t, intptr(7))
	g.SetGlobalYolo(true)

	us := g.NewSession("s-yolo")
	if us == nil {
		t.Fatal("NewSession returned nil")
	}
	if got := us.MaxSteps(); got != 0 {
		t.Fatalf("global yolo session MaxSteps = %d, want 0 (unlimited)", got)
	}
	if err := g.GetPermissionService().CheckWithContext(permission.RequestContext{SessionID: "s-yolo"}, permission.ActionShell, "", "echo hi"); err != nil {
		t.Fatalf("global yolo should auto-approve otherwise-ask shell permission, got %v", err)
	}
}

func TestConfigYoloWiresUnlimitedStepsAndPermissionAutoApprove(t *testing.T) {
	home := t.TempDir()
	cfg := config.GetDefaultConfig()
	cfg.Yolo = true
	cfg.MaxSteps = intptr(7)
	if err := config.SaveConfig(home, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	g := NewGogentWithWorkspace(home, t.TempDir())
	us := g.NewSession("s-config-yolo")
	if us == nil {
		t.Fatal("NewSession returned nil")
	}
	if got := us.MaxSteps(); got != 0 {
		t.Fatalf("config yolo session MaxSteps = %d, want 0 (unlimited)", got)
	}
	if err := g.GetPermissionService().CheckWithContext(permission.RequestContext{SessionID: "s-config-yolo"}, permission.ActionShell, "", "echo hi"); err != nil {
		t.Fatalf("config yolo should auto-approve otherwise-ask shell permission, got %v", err)
	}
}

func TestSetYoloModeTogglesSessionStepCapAndPermissions(t *testing.T) {
	g := newMaxStepsGogent(t, intptr(9))
	us := g.NewSession("s-toggle")
	if us == nil {
		t.Fatal("NewSession returned nil")
	}
	if got := us.MaxSteps(); got != 9 {
		t.Fatalf("initial MaxSteps = %d, want 9", got)
	}

	g.SetYoloMode("s-toggle", true)
	if !g.YoloMode("s-toggle") {
		t.Fatal("YoloMode should report enabled after SetYoloMode(true)")
	}
	if got := us.MaxSteps(); got != 0 {
		t.Fatalf("yolo-on MaxSteps = %d, want 0", got)
	}
	if err := g.GetPermissionService().CheckWithContext(permission.RequestContext{SessionID: "s-toggle"}, permission.ActionShell, "", "echo hi"); err != nil {
		t.Fatalf("session yolo should auto-approve shell ask, got %v", err)
	}

	g.SetYoloMode("s-toggle", false)
	if g.YoloMode("s-toggle") {
		t.Fatal("YoloMode should report disabled after SetYoloMode(false)")
	}
	if got := us.MaxSteps(); got != 9 {
		t.Fatalf("yolo-off MaxSteps = %d, want restored configured cap 9", got)
	}
	if err := g.GetPermissionService().CheckWithContext(permission.RequestContext{SessionID: "s-toggle"}, permission.ActionShell, "", "echo hi"); err == nil {
		t.Fatal("yolo-off shell ask should return to headless denial")
	}
}

func TestYoloSessionOverrideCreatedBeforeSessionAppliesAtCreation(t *testing.T) {
	g := newMaxStepsGogent(t, intptr(11))
	g.SetYoloMode("future", true)

	us := g.NewSession("future")
	if us == nil {
		t.Fatal("NewSession returned nil")
	}
	if got := us.MaxSteps(); got != 0 {
		t.Fatalf("pre-created yolo override MaxSteps = %d, want 0", got)
	}
}

func TestSetYoloModeEmitsBackendYoloEvent(t *testing.T) {
	g := newMaxStepsGogent(t, intptr(5))
	us := g.NewSession("s-events")
	if us == nil {
		t.Fatal("NewSession returned nil")
	}

	var events []agent.SessionEvent
	us.SetObserver(func(ev agent.SessionEvent) {
		events = append(events, ev)
	})

	g.SetYoloMode("s-events", true)
	g.SetYoloMode("s-events", false)

	if len(events) != 2 {
		t.Fatalf("yolo toggle emitted %d events, want 2: %+v", len(events), events)
	}
	if events[0].Type != agent.SessionEventYolo || !events[0].Yolo {
		t.Fatalf("first yolo event = %+v, want yolo=true", events[0])
	}
	if events[1].Type != agent.SessionEventYolo || events[1].Yolo {
		t.Fatalf("second yolo event = %+v, want yolo=false", events[1])
	}
}

func TestEmitYoloStateAnnouncesGlobalYoloAfterObserverInstalled(t *testing.T) {
	g := newMaxStepsGogent(t, intptr(5))
	g.SetGlobalYolo(true)
	us := g.NewSession("s-global-event")
	if us == nil {
		t.Fatal("NewSession returned nil")
	}

	var events []agent.SessionEvent
	us.SetObserver(func(ev agent.SessionEvent) {
		events = append(events, ev)
	})
	g.EmitYoloState("s-global-event")

	if len(events) != 1 {
		t.Fatalf("EmitYoloState emitted %d events, want 1: %+v", len(events), events)
	}
	if events[0].Type != agent.SessionEventYolo || !events[0].Yolo {
		t.Fatalf("initial yolo event = %+v, want yolo=true", events[0])
	}
}
