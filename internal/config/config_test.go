package config

import (
	"encoding/json"
	"testing"
)

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

// TestDefaultNotifyConfig guards the issue #59 defaults: notifications are on
// (master + every event), the bell and OSC desktop channels are on, and the
// native OS notifier is off by default.
func TestDefaultNotifyConfig(t *testing.T) {
	cfg := DefaultNotifyConfig()
	if !cfg.Enabled {
		t.Error("DefaultNotifyConfig: Enabled should be true")
	}
	if !cfg.Bell || !cfg.Desktop {
		t.Error("DefaultNotifyConfig: bell and desktop channels should be on")
	}
	if cfg.Native {
		t.Error("DefaultNotifyConfig: native notifier should be off (needs an external binary)")
	}
	for _, on := range []bool{cfg.OnComplete, cfg.OnError, cfg.OnApproval, cfg.OnClarify} {
		if !on {
			t.Error("DefaultNotifyConfig: every per-event toggle should be on")
		}
	}
	if cfg.SuppressWhenFocused {
		t.Error("DefaultNotifyConfig: SuppressWhenFocused should default off")
	}
}

// TestConfigNotifyConfig resolves the effective notification config: a nil
// pointer (older config.json without a "notify" block) yields the defaults, while
// an explicit block — even one that disables everything — is honored verbatim.
func TestConfigNotifyConfig(t *testing.T) {
	// No notify block -> defaults.
	c := &Config{}
	got := c.NotifyConfig()
	if !got.Enabled || !got.OnComplete {
		t.Errorf("nil Notify should resolve to defaults, got %+v", got)
	}

	// Explicit block is honored, including "everything off".
	off := NotifyConfig{Enabled: false}
	c.SetNotifyConfig(off)
	if c.Notify == nil {
		t.Fatal("SetNotifyConfig should store a non-nil pointer")
	}
	got = c.NotifyConfig()
	if got.Enabled || got.OnComplete || got.Bell {
		t.Errorf("explicit disabled config should be honored, got %+v", got)
	}

	// Nil config resolves to defaults too.
	var nilCfg *Config
	if nc := nilCfg.NotifyConfig(); !nc.Enabled {
		t.Error("nil Config should resolve to default notifications")
	}
}

// TestNotifyConfigRoundTrip ensures the notify block survives a JSON marshal /
// unmarshal cycle and that an absent block still resolves to the defaults on
// load (the backward-compatibility guarantee for issue #59).
func TestNotifyConfigRoundTrip(t *testing.T) {
	src := DefaultNotifyConfig()
	src.Native = true
	src.OnError = false
	in := &Config{Notify: &src}

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Config
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := out.NotifyConfig()
	if !got.Native || got.OnError {
		t.Errorf("round-trip lost fields, got %+v", got)
	}

	// A config blob with no "notify" key loads as nil and resolves to defaults.
	const legacy = `{"default_model": "x"}`
	var legacyCfg Config
	if err := json.Unmarshal([]byte(legacy), &legacyCfg); err != nil {
		t.Fatalf("legacy unmarshal: %v", err)
	}
	if legacyCfg.Notify != nil {
		t.Errorf("legacy config should leave Notify nil, got %+v", legacyCfg.Notify)
	}
	if nc := legacyCfg.NotifyConfig(); !nc.Enabled {
		t.Error("legacy config should resolve to default (enabled) notifications")
	}
}

// TestBudgetWarnFractionOrDefault covers the accessor's fallback and clamping:
// unset (<=0) and out-of-range (>1) values resolve to the built-in default,
// while an in-range value is returned verbatim.
func TestBudgetWarnFractionOrDefault(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    float64
		want float64
	}{
		{"unset zero", 0, defaultBudgetWarnFraction},
		{"negative", -0.2, defaultBudgetWarnFraction},
		{"over one", 1.5, defaultBudgetWarnFraction},
		{"explicit in range", 0.66, 0.66},
		{"boundary one", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := BudgetConfig{TokenBudget: 1000, WarnFraction: tc.f}
			if got := b.WarnFractionOrDefault(); got != tc.want {
				t.Errorf("WarnFractionOrDefault() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBudgetConfigZeroIsOff confirms a BudgetConfig with no token budget is the
// "alerting off" default, and that a budget block round-trips through JSON.
func TestBudgetConfigZeroIsOff(t *testing.T) {
	var zero BudgetConfig
	if zero.TokenBudget != 0 {
		t.Errorf("zero BudgetConfig should have no token budget, got %d", zero.TokenBudget)
	}

	// A populated budget survives a marshal/unmarshal cycle.
	in := &Config{Budget: BudgetConfig{TokenBudget: 50000, WarnFraction: 0.9}}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Config
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Budget.TokenBudget != 50000 || out.Budget.WarnFraction != 0.9 {
		t.Errorf("round-trip lost budget fields, got %+v", out.Budget)
	}

	// A legacy config blob with no "budget" key loads as the zero (off) value.
	const legacy = `{"default_model": "x"}`
	var legacyCfg Config
	if err := json.Unmarshal([]byte(legacy), &legacyCfg); err != nil {
		t.Fatalf("legacy unmarshal: %v", err)
	}
	if legacyCfg.Budget.TokenBudget != 0 {
		t.Errorf("legacy config should leave budget off, got %+v", legacyCfg.Budget)
	}
}

// TestDiagnosticsConfigRoundTrip confirms a configured diagnostics command and
// warning pattern survive a JSON marshal/unmarshal cycle, and that a legacy
// config blob with no "diagnostics" key loads as the zero value (the tool then
// applies its Go default).
func TestDiagnosticsConfigRoundTrip(t *testing.T) {
	in := &Config{Diagnostics: DiagnosticsConfig{
		Command:        []string{"go", "build", "./..."},
		WarningPattern: "^printf:",
	}}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Config
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Diagnostics.Command) != 3 || out.Diagnostics.Command[1] != "build" {
		t.Errorf("round-trip lost command, got %+v", out.Diagnostics.Command)
	}
	if out.Diagnostics.WarningPattern != "^printf:" {
		t.Errorf("round-trip lost warning_pattern, got %q", out.Diagnostics.WarningPattern)
	}

	// A legacy config blob with no "diagnostics" key loads as the zero value, so
	// the tool falls back to its built-in default (issue #42 backward compat).
	const legacy = `{"default_model": "x"}`
	var legacyCfg Config
	if err := json.Unmarshal([]byte(legacy), &legacyCfg); err != nil {
		t.Fatalf("legacy unmarshal: %v", err)
	}
	if len(legacyCfg.Diagnostics.Command) != 0 || legacyCfg.Diagnostics.WarningPattern != "" {
		t.Errorf("legacy config should leave diagnostics empty, got %+v", legacyCfg.Diagnostics)
	}
}

// TestVerifyConfigRoundTrip confirms a configured verify command survives a JSON
// marshal/unmarshal cycle, and that a legacy config blob with no "verify" key
// loads as the zero value (the tool then applies its Go default).
func TestVerifyConfigRoundTrip(t *testing.T) {
	in := &Config{Verify: VerifyConfig{
		Command: []string{"go", "test", "-count=1", "./..."},
	}}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Config
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Verify.Command) != 4 || out.Verify.Command[2] != "-count=1" {
		t.Errorf("round-trip lost command, got %+v", out.Verify.Command)
	}

	// A legacy config blob with no "verify" key loads as the zero value, so the
	// tool falls back to its built-in default (issue #44 backward compat).
	const legacy = `{"default_model": "x"}`
	var legacyCfg Config
	if err := json.Unmarshal([]byte(legacy), &legacyCfg); err != nil {
		t.Fatalf("legacy unmarshal: %v", err)
	}
	if len(legacyCfg.Verify.Command) != 0 {
		t.Errorf("legacy config should leave verify empty, got %+v", legacyCfg.Verify)
	}
}

func TestIsReasoningModel(t *testing.T) {
	on := true
	off := false
	cases := []struct {
		name string
		m    *ModelConfig
		want bool
	}{
		{"nil", nil, false},
		{"plain", &ModelConfig{Model: "gpt-4o"}, false},
		{"effort", &ModelConfig{ReasoningEffort: "high"}, true},
		{"thinking on", &ModelConfig{Thinking: &on}, true},
		{"thinking off still reasoning", &ModelConfig{Thinking: &off}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.m.IsReasoningModel(); got != c.want {
				t.Errorf("IsReasoningModel() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestThemeConfigUnmarshal checks the optional theme section round-trips,
// including the overrides map (issue #66), and that an absent section leaves the
// zero value (the coloured "default" palette).
func TestThemeConfigUnmarshal(t *testing.T) {
	const data = `{
		"default_model": "x",
		"models": [],
		"theme": {
			"name": "high-contrast",
			"no_color": true,
			"overrides": {"user": "#E69F00", "error": "9"}
		}
	}`
	var cfg Config
	if err := json.Unmarshal([]byte(data), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Theme.Name != "high-contrast" {
		t.Errorf("Name = %q, want high-contrast", cfg.Theme.Name)
	}
	if !cfg.Theme.NoColor {
		t.Error("NoColor = false, want true")
	}
	if got := cfg.Theme.Overrides["user"]; got != "#E69F00" {
		t.Errorf("overrides[user] = %q, want #E69F00", got)
	}

	var bare Config
	if err := json.Unmarshal([]byte(`{"default_model":"x","models":[]}`), &bare); err != nil {
		t.Fatalf("unmarshal bare: %v", err)
	}
	if bare.Theme.Name != "" || bare.Theme.NoColor || bare.Theme.Overrides != nil {
		t.Errorf("absent theme = %+v, want zero value", bare.Theme)
	}
}
