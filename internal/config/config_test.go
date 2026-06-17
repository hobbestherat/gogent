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
