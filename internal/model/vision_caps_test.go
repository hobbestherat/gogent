package model

import (
	"testing"

	"gogent/internal/config"
)

// TestSupportsVision verifies the connector reports its model's vision capability
// straight from the Caps snapshot, defaulting to false when caps are unset or the
// model config is absent (the safe default for the non-blocking warn-on-mismatch).
func TestSupportsVision(t *testing.T) {
	pc := &config.ProviderConnection{APIType: "openai", Endpoint: DefaultModelURL}

	visionOn := NewModelConnection(pc, &config.ModelConfig{
		Caps: config.ModelCapabilities{Vision: true},
	})
	if !visionOn.SupportsVision() {
		t.Error("SupportsVision() = false, want true for a vision-capable model")
	}

	visionOff := NewModelConnection(pc, &config.ModelConfig{
		Caps: config.ModelCapabilities{Vision: false},
	})
	if visionOff.SupportsVision() {
		t.Error("SupportsVision() = true, want false for a model with vision unset")
	}

	// A nil model config defaults Config to a zero value: vision is false.
	if NewModelConnection(pc, nil).SupportsVision() {
		t.Error("SupportsVision() = true, want false for a nil model config")
	}
}

// TestVisionModelName checks the name precedence used in the user-facing notice:
// DisplayName, then config Name, then the backend model id.
func TestVisionModelName(t *testing.T) {
	pc := &config.ProviderConnection{APIType: "openai", Endpoint: DefaultModelURL}

	tests := []struct {
		name string
		cfg  *config.ModelConfig
		want string
	}{
		{"display name wins", &config.ModelConfig{DisplayName: "GPT Vision", Name: "gpt", Model: "gpt-4o"}, "GPT Vision"},
		{"falls back to name", &config.ModelConfig{Name: "gpt", Model: "gpt-4o"}, "gpt"},
		{"falls back to model id", &config.ModelConfig{Model: "gpt-4o"}, "gpt-4o"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewModelConnection(pc, tt.cfg).VisionModelName()
			if got != tt.want {
				t.Errorf("VisionModelName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestModelConnectionIsVisionReporter is a compile-and-behavior check that the
// concrete connection satisfies the optional VisionReporter capability the agent
// loop type-asserts against.
func TestModelConnectionIsVisionReporter(t *testing.T) {
	var vr VisionReporter = NewModelConnection(
		&config.ProviderConnection{APIType: "openai", Endpoint: DefaultModelURL},
		&config.ModelConfig{Caps: config.ModelCapabilities{Vision: true}},
	)
	if !vr.SupportsVision() {
		t.Error("VisionReporter.SupportsVision() = false, want true")
	}
}
