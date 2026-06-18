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
