package model

import (
	"testing"

	"gogent/internal/config"
)

// Verifies #176: the default GLM-5.2 entry resolves to the Z.AI *coding* base,
// while GLM-4.6 stays on the general PaaS base.
func TestDefaultZAIEndpointsResolve(t *testing.T) {
	want := map[string]string{
		"zai-glm":     "https://api.z.ai/api/paas/v4/chat/completions",
		"zai-glm-5.2": "https://api.z.ai/api/coding/paas/v4/chat/completions",
	}
	seen := map[string]bool{}
	cfg := config.GetDefaultConfig()
	for _, m := range cfg.ModelConfigs {
		exp, ok := want[m.Name]
		if !ok {
			continue
		}
		seen[m.Name] = true
		got := NewModelConnection(cfg.ConnectionForModel(m), m).URL
		if got != exp {
			t.Errorf("%s: chat URL = %q, want %q", m.Name, got, exp)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("default model %q not found", name)
		}
	}
}
