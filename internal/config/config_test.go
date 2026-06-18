package config

import "testing"

func TestDefaultEndpointEnvOverride(t *testing.T) {
	t.Setenv(EnvModelURL, "http://example.test:1234/v1/chat/completions")
	if got := DefaultEndpoint(); got != "http://example.test:1234/v1/chat/completions" {
		t.Errorf("Expected env override endpoint, got %q", got)
	}
	cfg := GetDefaultConfig()
	local := cfg.GetModelConfig("local-lan")
	if local == nil {
		t.Fatal("expected local-lan model config")
	}
	if local.Endpoint != "http://example.test:1234/v1/chat/completions" {
		t.Errorf("Expected local-lan endpoint to honor env override, got %q", local.Endpoint)
	}
}
func TestDefaultEndpointFallback(t *testing.T) {
	t.Setenv(EnvModelURL, "")
	if got := DefaultEndpoint(); got != FallbackModelURL {
		t.Errorf("Expected fallback %q, got %q", FallbackModelURL, got)
	}
}

func TestModelForRole(t *testing.T) {
	models := []*ModelConfig{
		{Name: "opus", Model: "claude-opus"},
		{Name: "haiku-fast", Model: "claude-haiku"},
		{Name: "sonnet", Model: "claude-sonnet"},
	}
	newCfg := func(fast string, roles map[string]string) *Config {
		return &Config{DefaultModel: "opus", FastModel: fast, ModelRoles: roles, ModelConfigs: models}
	}

	tests := []struct {
		name string
		cfg  *Config
		role string
		want string // expected resolved model Name
	}{
		{
			name: "no fast model falls back to primary",
			cfg:  newCfg("", nil),
			role: RoleCompression,
			want: "opus",
		},
		{
			name: "fast model set, role unmapped defaults to fast",
			cfg:  newCfg("haiku-fast", nil),
			role: RoleCompression,
			want: "haiku-fast",
		},
		{
			name: "role explicitly mapped to fast sentinel",
			cfg:  newCfg("haiku-fast", map[string]string{RoleCompression: FastModelRef}),
			role: RoleCompression,
			want: "haiku-fast",
		},
		{
			name: "role mapped to a specific model name",
			cfg:  newCfg("haiku-fast", map[string]string{RoleTitle: "sonnet"}),
			role: RoleTitle,
			want: "sonnet",
		},
		{
			name: "role mapped to unknown name falls back to primary",
			cfg:  newCfg("haiku-fast", map[string]string{RoleTitle: "ghost"}),
			role: RoleTitle,
			want: "opus",
		},
		{
			name: "fast sentinel but fast model unknown falls back to primary",
			cfg:  newCfg("missing", map[string]string{RoleCompression: FastModelRef}),
			role: RoleCompression,
			want: "opus",
		},
		{
			name: "empty mapping pins role to primary even when fast is set",
			cfg:  newCfg("haiku-fast", map[string]string{RoleCompression: ""}),
			role: RoleCompression,
			want: "opus",
		},
		{
			name: "unmapped role uses primary when no fast model",
			cfg:  newCfg("", map[string]string{RoleCompression: FastModelRef}),
			role: RoleWebFetchSummarize,
			want: "opus",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.ModelForRole(tt.role)
			if got == nil {
				t.Fatalf("ModelForRole(%q) = nil, want %q", tt.role, tt.want)
			}
			if got.Name != tt.want {
				t.Errorf("ModelForRole(%q) = %q, want %q", tt.role, got.Name, tt.want)
			}
		})
	}
}

func TestModelForRolePrimaryFallsBackToFirstModel(t *testing.T) {
	// An unknown default_model name should resolve to the first configured model.
	cfg := &Config{DefaultModel: "missing", ModelConfigs: []*ModelConfig{{Name: "only"}}}
	if got := cfg.ModelForRole(RoleCompression); got == nil || got.Name != "only" {
		t.Fatalf("expected first model %q, got %+v", "only", got)
	}
}

func TestModelForRoleNilConfig(t *testing.T) {
	var cfg *Config
	if got := cfg.ModelForRole(RoleCompression); got != nil {
		t.Errorf("expected nil for nil config, got %+v", got)
	}
}

func TestContextWindowOrDefault(t *testing.T) {
	tests := []struct {
		name string
		cfg  *ModelConfig
		want int
	}{
		{"nil receiver", nil, defaultContextWindow},
		{"empty struct defaults", &ModelConfig{}, defaultContextWindow},
		{"zero defaults", &ModelConfig{ContextWindow: 0}, defaultContextWindow},
		{"negative defaults", &ModelConfig{ContextWindow: -1024}, defaultContextWindow},
		{"configured returned as-is", &ModelConfig{ContextWindow: 131072}, 131072},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.ContextWindowOrDefault(); got != tt.want {
				t.Errorf("ContextWindowOrDefault() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestDefaultConfigSetsContextWindow guards the issue #4 fix: every shipped
// default model must declare an explicit context_window so compaction is
// calibrated against the input window, never against max_tokens (the output cap).
func TestDefaultConfigSetsContextWindow(t *testing.T) {
	cfg := GetDefaultConfig()
	if len(cfg.ModelConfigs) == 0 {
		t.Fatal("expected default config to define models")
	}
	for _, m := range cfg.ModelConfigs {
		if m.ContextWindow <= 0 {
			t.Errorf("model %q has no context_window set", m.Name)
		}
		// An explicitly set window must round-trip through the accessor; the
		// fallback default is only for hand-written configs that omit the field.
		if got := m.ContextWindowOrDefault(); got != m.ContextWindow {
			t.Errorf("model %q: ContextWindowOrDefault = %d, want %d", m.Name, got, m.ContextWindow)
		}
	}
}

// TestContextWindowDistinctFromMaxTokens documents the issue #4 invariant: a
// model with a sane output cap (e.g. 4096) has its compaction threshold driven by
// the much larger context window, so it does not compact at ~3.3K tokens.
func TestContextWindowDistinctFromMaxTokens(t *testing.T) {
	m := &ModelConfig{MaxTokens: 4096, ContextWindow: 131072}
	window := m.ContextWindowOrDefault()
	if window == m.MaxTokens {
		t.Fatalf("context window (%d) must be distinct from max_tokens output cap (%d)", window, m.MaxTokens)
	}
	// Compaction threshold (80% of the window) is far above the output cap.
	threshold := window * 4 / 5
	if threshold <= m.MaxTokens {
		t.Errorf("compaction threshold %d should exceed max_tokens %d", threshold, m.MaxTokens)
	}
}
