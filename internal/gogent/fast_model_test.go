package gogent

import (
	"testing"

	"gogent/internal/config"
	"gogent/internal/model"
)

// fastModelConfig returns a config with a primary "opus" model and a fast
// "haiku" model so role resolution can be exercised end-to-end.
func fastModelConfig(roles map[string]string) *config.Config {
	return &config.Config{
		DefaultModel: "opus",
		FastModel:    "haiku",
		ModelRoles:   roles,
		Connections: []*config.ProviderConnection{
			{Name: "primary", APIType: "openai", Endpoint: "http://primary.test/v1/chat/completions"},
			{Name: "fast", APIType: "openai", Endpoint: "http://fast.test/v1/chat/completions"},
		},
		ModelConfigs: []*config.ModelConfig{
			{Name: "opus", Model: "claude-opus", Connection: "primary"},
			{Name: "haiku", Model: "claude-haiku", Connection: "fast"},
		},
	}
}

func TestCompleterForRole(t *testing.T) {
	g := NewGogent("/tmp/test")
	g.config = fastModelConfig(map[string]string{config.RoleCompression: config.FastModelRef})

	tests := []struct {
		role      string
		wantModel string
	}{
		{config.RoleCompression, "claude-haiku"}, // explicitly mapped to fast
		{config.RoleTitle, "claude-haiku"},       // unmapped defaults to fast
	}
	for _, tt := range tests {
		c := g.CompleterForRole(tt.role)
		conn, ok := c.(*model.ModelConnection)
		if !ok {
			t.Fatalf("CompleterForRole(%q) returned %T, want *model.ModelConnection", tt.role, c)
		}
		if conn.ModelName != tt.wantModel {
			t.Errorf("CompleterForRole(%q) model = %q, want %q", tt.role, conn.ModelName, tt.wantModel)
		}
	}
}

func TestCompleterForRoleNoFastModelUsesPrimary(t *testing.T) {
	g := NewGogent("/tmp/test")
	cfg := fastModelConfig(nil)
	cfg.FastModel = "" // no fast model configured
	g.config = cfg

	conn, ok := g.CompleterForRole(config.RoleCompression).(*model.ModelConnection)
	if !ok {
		t.Fatalf("expected *model.ModelConnection")
	}
	if conn.ModelName != "claude-opus" {
		t.Errorf("model = %q, want primary claude-opus", conn.ModelName)
	}
}

// TestCreateUserSessionWiresCompressionCompleter verifies that a configured fast
// model is injected as the compression completer, while leaving it unset when the
// role resolves back to the primary model (preserving prior behavior).
func TestCreateUserSessionWiresCompressionCompleter(t *testing.T) {
	t.Run("fast model wired in", func(t *testing.T) {
		g := NewGogent("/tmp/test")
		g.config = fastModelConfig(map[string]string{config.RoleCompression: config.FastModelRef})

		us := g.NewSession("s-fast")
		if !us.UsesFastCompression() {
			t.Error("expected compression to be wired to the fast model")
		}
		// The fresh fast connector reports zero (unused) usage so far.
		if snap := us.FastConnectorStats(); snap != (model.StatsSnapshot{}) {
			t.Errorf("fresh fast connector should report zero usage, got %+v", snap)
		}
	})

	t.Run("no fast model leaves compression on primary", func(t *testing.T) {
		g := NewGogent("/tmp/test")
		cfg := fastModelConfig(nil)
		cfg.FastModel = ""
		g.config = cfg

		us := g.NewSession("s-primary")
		if us.UsesFastCompression() {
			t.Error("expected compression to stay on the primary model")
		}
		if snap := us.FastConnectorStats(); snap != (model.StatsSnapshot{}) {
			t.Errorf("expected zero fast stats with no fast model, got %+v", snap)
		}
	})
}
