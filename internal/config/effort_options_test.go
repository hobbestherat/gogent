package config

import "testing"

// TestDefaultConfigEffortOptions verifies the per-model reasoning-effort options
// baked into the default config (issue #177): GLM-5.2 carries models.dev's
// effort values [high, max]; every other default model leaves EffortOptions empty
// (it has no effort-type reasoning, so its selector is greyed out in the UI).
func TestDefaultConfigEffortOptions(t *testing.T) {
	cfg := GetDefaultConfig()

	glm52 := cfg.GetModelConfig("zai-glm-5.2")
	if glm52 == nil {
		t.Fatal("expected zai-glm-5.2 model config")
	}
	want := []string{"high", "max"}
	if got := glm52.EffortOptions; len(got) != len(want) {
		t.Fatalf("zai-glm-5.2 EffortOptions = %v, want %v", got, want)
	}
	for i, v := range want {
		if glm52.EffortOptions[i] != v {
			t.Errorf("zai-glm-5.2 EffortOptions[%d] = %q, want %q", i, glm52.EffortOptions[i], v)
		}
	}

	for _, name := range []string{"zai-glm", "groq-free", "together-free", "openrouter-free", "local-lan"} {
		m := cfg.GetModelConfig(name)
		if m == nil {
			t.Fatalf("expected %s model config", name)
		}
		if len(m.EffortOptions) != 0 {
			t.Errorf("%s EffortOptions = %v, want empty", name, m.EffortOptions)
		}
	}
}
