package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ModelConfig represents a single model configuration
type ModelConfig struct {
	Name        string  `json:"name"`
	DisplayName string  `json:"display_name"`
	// APIType selects the provider conventions used to talk to this backend
	// ("openai" for any OpenAI-compatible server, "zai" for the Z.AI platform).
	// Empty defaults to "openai".
	APIType     string  `json:"api_type,omitempty"`
	Endpoint    string  `json:"endpoint"`
	Model       string  `json:"model"`
	APIKey      string  `json:"api_key,omitempty"`
	Temperature float32 `json:"temperature"`
	TopP        float32 `json:"top_p,omitempty"`
	MaxTokens   int     `json:"max_tokens"`
	Free        bool    `json:"free"`
}

// SubAgentExecutionModel selects how sub-agents are run.
type SubAgentExecutionModel string

const (
	// SubAgentOneShotModel runs sub-agents as blocking tool calls that must
	// finish with SUCCESS/FAILURE. This is the stable default.
	SubAgentOneShotModel SubAgentExecutionModel = "one_shot"
	// SubAgentInteractiveModel launches sub-agents as asynchronous workers that
	// return an id immediately and may ask the coordinator for clarification.
	// Experimental.
	SubAgentInteractiveModel SubAgentExecutionModel = "interactive"
)

// SubAgentConfig captures the user-facing execution-model settings for
// sub-agents (see the "Settings" section of the sub-agent design).
type SubAgentConfig struct {
	// ExecutionModel is "one_shot" (default) or "interactive" (experimental).
	ExecutionModel SubAgentExecutionModel `json:"execution_model"`
	// AllowRecursive permits spawned sub-agents to themselves spawn sub-agents.
	AllowRecursive bool `json:"allow_recursive"`
	// MaxSubAgents caps how many sub-agents a single agent may spawn. <=0 means
	// use the built-in default (see MaxSubAgentsOrDefault).
	MaxSubAgents int `json:"max_subagents"`
	// MaxDepth bounds how deeply sub-agents may recursively spawn further
	// sub-agents. <=0 means use the built-in default (see MaxDepthOrDefault).
	MaxDepth int `json:"max_depth"`
}

// defaultMaxSubAgents and defaultMaxDepth are the conservative built-in limits
// applied when the config leaves the corresponding field unset (<=0).
const (
	defaultMaxSubAgents = 4
	defaultMaxDepth     = 3
)

// MaxSubAgentsOrDefault returns the configured sub-agent fan-out limit, or the
// built-in default when unset.
func (c SubAgentConfig) MaxSubAgentsOrDefault() int {
	if c.MaxSubAgents <= 0 {
		return defaultMaxSubAgents
	}
	return c.MaxSubAgents
}

// MaxDepthOrDefault returns the configured recursion-depth limit, or the
// built-in default when unset.
func (c SubAgentConfig) MaxDepthOrDefault() int {
	if c.MaxDepth <= 0 {
		return defaultMaxDepth
	}
	return c.MaxDepth
}

// IsOneShot reports whether the configured execution model is one-shot. An empty
// (unset) value defaults to one-shot, the stable mode.
func (c SubAgentConfig) IsOneShot() bool {
	return c.ExecutionModel != SubAgentInteractiveModel
}

// DefaultSubAgentConfig returns the conservative defaults: one-shot agents, no
// recursion. One-shot stays the default until the interactive model is proven.
func DefaultSubAgentConfig() SubAgentConfig {
	return SubAgentConfig{
		ExecutionModel: SubAgentOneShotModel,
		AllowRecursive: false,
		MaxSubAgents:   defaultMaxSubAgents,
		MaxDepth:       defaultMaxDepth,
	}
}

// TimeoutConfig holds the configurable timeouts (in seconds) for the major
// blocking operations gogent performs. A zero value means "use the built-in
// default" via the *OrDefault accessors.
type TimeoutConfig struct {
	// ModelSeconds bounds a single model HTTP request.
	ModelSeconds int `json:"model_seconds"`
	// ToolSeconds bounds a single tool/shell execution.
	ToolSeconds int `json:"tool_seconds"`
	// SubAgentSeconds bounds how long a sub-agent may run before timing out.
	SubAgentSeconds int `json:"subagent_seconds"`
}

// Built-in timeout defaults (seconds) used when a field is left unset (<=0).
const (
	defaultModelTimeoutSec    = 300
	defaultToolTimeoutSec     = 300
	defaultSubAgentTimeoutSec = 300
)

// ModelSecondsOrDefault, ToolSecondsOrDefault and SubAgentSecondsOrDefault
// return the configured timeout or the built-in default when unset.
func (t TimeoutConfig) ModelSecondsOrDefault() int {
	if t.ModelSeconds <= 0 {
		return defaultModelTimeoutSec
	}
	return t.ModelSeconds
}

func (t TimeoutConfig) ToolSecondsOrDefault() int {
	if t.ToolSeconds <= 0 {
		return defaultToolTimeoutSec
	}
	return t.ToolSeconds
}

func (t TimeoutConfig) SubAgentSecondsOrDefault() int {
	if t.SubAgentSeconds <= 0 {
		return defaultSubAgentTimeoutSec
	}
	return t.SubAgentSeconds
}

// DefaultTimeoutConfig returns the built-in timeout defaults.
func DefaultTimeoutConfig() TimeoutConfig {
	return TimeoutConfig{
		ModelSeconds:    defaultModelTimeoutSec,
		ToolSeconds:     defaultToolTimeoutSec,
		SubAgentSeconds: defaultSubAgentTimeoutSec,
	}
}

// WindowConfig controls the appearance and behavior of session windows.
type WindowConfig struct {
	// Resizable allows users to drag the window corners to resize session windows.
	Resizable bool `json:"resizable"`
	// Minimizable allows users to collapse session windows to their title bar.
	Minimizable bool `json:"minimizable"`
	// MinWidth is the minimum allowed width for a window.
	MinWidth int `json:"min_width"`
	// MinHeight is the minimum allowed height for a window.
	MinHeight int `json:"min_height"`
}

// Auxiliary model roles. These name the lightweight, latency-sensitive, or
// high-volume tasks that may run on a smaller/cheaper "fast" model instead of
// the primary reasoning model. Use them with Config.ModelForRole.
const (
	RoleCompression       = "compression"
	RoleWebFetchSummarize = "web_fetch_summarize"
	RoleTitle             = "title"
	RoleJSONRepair        = "json_repair"
)

// FastModelRef is the sentinel a model_roles entry uses to point at the
// configured fast_model rather than naming a specific Models[] entry directly.
const FastModelRef = "fast_model"

// Config represents the full configuration
type Config struct {
	DefaultModel string `json:"default_model"`
	// FastModel optionally names a Models[] entry (by Name) used for auxiliary
	// tasks (compression, summarization, JSON repair, …). Empty means auxiliary
	// tasks run on the primary model, preserving prior behavior.
	FastModel string `json:"fast_model,omitempty"`
	// ModelRoles maps an auxiliary role (see the Role* constants) to either the
	// "fast_model" sentinel (FastModelRef) or a specific Models[] name. A role
	// absent from this map defaults to the fast model when one is configured,
	// otherwise to the primary model.
	ModelRoles   map[string]string `json:"model_roles,omitempty"`
	ModelConfigs []*ModelConfig    `json:"models"`
	SubAgents    SubAgentConfig    `json:"sub_agents"`
	Timeouts     TimeoutConfig     `json:"timeouts"`
	Window       WindowConfig      `json:"window"`
}

// LoadConfig loads configuration from the default location
func LoadConfig(homeDir string) (*Config, error) {
	configDir := filepath.Join(homeDir, ".gogent")
	configPath := filepath.Join(configDir, "config.json")

	// Check if config file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		// Return default config
		return GetDefaultConfig(), nil
	}

	// Load config from file
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config: %v", err)
	}

	return &config, nil
}

// SaveConfig saves configuration to the default location
func SaveConfig(homeDir string, config *Config) error {
	configDir := filepath.Join(homeDir, ".gogent")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %v", err)
	}

	configPath := filepath.Join(configDir, "config.json")
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %v", err)
	}

	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %v", err)
	}

	return nil
}

// EnvModelURL is the environment variable used to override gogent's default
// model endpoint at runtime, keeping environment-specific setups out of code.
const EnvModelURL = "GOGENT_MODEL_URL"

// FallbackModelURL is gogent's built-in default endpoint when neither the
// GOGENT_MODEL_URL env var nor a saved config provides one. This is the one
// place an environment-specific default lives — deliberately in the app's
// config layer, not in the reusable model connector.
const FallbackModelURL = "http://192.168.1.88:8080/v1/chat/completions"

// DefaultEndpoint returns gogent's default chat-completions endpoint, honoring
// the GOGENT_MODEL_URL environment variable and falling back to FallbackModelURL.
func DefaultEndpoint() string {
	if v := os.Getenv(EnvModelURL); v != "" {
		return v
	}
	return FallbackModelURL
}

// GetDefaultConfig returns the default configuration with free models
// Note: Most free models require an API key. Users should add their own API keys
// to ~/.gogent/config.json after setting up accounts.
//
// The default LAN endpoint honors the GOGENT_MODEL_URL environment variable (via
// DefaultEndpoint) so the same binary works across machines.
func GetDefaultConfig() *Config {
	return &Config{
		DefaultModel: "local-lan",
		SubAgents:    DefaultSubAgentConfig(),
		Timeouts:     DefaultTimeoutConfig(),
		Window: WindowConfig{
			Resizable:   true,
			Minimizable: true,
			MinWidth:    50,
			MinHeight:   12,
		},
		ModelConfigs: []*ModelConfig{
			{
				Name:        "local-lan",
				DisplayName: "Local LAN (env: GOGENT_MODEL_URL)",
				Endpoint:    DefaultEndpoint(),
				Model:       "default",
				APIKey:      "",
				Temperature: 0.7,
				MaxTokens:   262144,
				Free:        true,
			},
			{
				Name:        "qemu-host",
				DisplayName: "Qemu Host (10.0.2.2)",
				Endpoint:    "http://10.0.2.2:8080/v1/chat/completions",
				Model:       "default",
				APIKey:      "",
				Temperature: 0.7,
				MaxTokens:   262144,
				Free:        true,
			},
			{
				Name:        "localhost",
				DisplayName: "Localhost (127.0.0.1)",
				Endpoint:    "http://127.0.0.1:8080/v1/chat/completions",
				Model:       "default",
				APIKey:      "",
				Temperature: 0.7,
				MaxTokens:   262144,
				Free:        true,
			},
			{
				Name:        "groq-free",
				DisplayName: "Groq (API Key Required)",
				Endpoint:    "https://api.groq.com/openai/v1/chat/completions",
				Model:       "llama-3.3-70b-versatile",
				APIKey:      "",
				Temperature: 0.7,
				MaxTokens:   262144,
				Free:        false,
			},
			{
				Name:        "together-free",
				DisplayName: "Together (API Key Required)",
				Endpoint:    "https://api.together.xyz/v1/chat/completions",
				Model:       "meta-llama/Llama-3.3-70B-Instruct-Turbo",
				APIKey:      "",
				Temperature: 0.7,
				MaxTokens:   262144,
				Free:        false,
			},
			{
				Name:        "openrouter-free",
				DisplayName: "OpenRouter (gemma-3-27b-it)",
				Endpoint:    "https://openrouter.ai/api/v1/chat/completions",
				Model:       "google/gemma-3-27b-it:free",
				APIKey:      "",
				Temperature: 0.7,
				MaxTokens:   262144,
				Free:        false,
			},
			{
				// Z.AI: api_type "zai" supplies the base URL automatically, so
				// only an API key (and model) are needed. See https://docs.z.ai.
				Name:        "zai-glm",
				DisplayName: "Z.AI (API Key Required)",
				APIType:     "zai",
				Endpoint:    "",
				Model:       "glm-4.6",
				APIKey:      "",
				Temperature: 0.7,
				MaxTokens:   131072,
				Free:        false,
			},
		},
	}
}

// ListModelNames returns a list of all model names for UI
func (c *Config) ListModelNames() []string {
	names := make([]string, len(c.ModelConfigs))
	for i, model := range c.ModelConfigs {
		names[i] = model.Name
	}
	return names
}

// GetModelConfig returns a model config by name
func (c *Config) GetModelConfig(name string) *ModelConfig {
	for _, model := range c.ModelConfigs {
		if model.Name == name {
			return model
		}
	}
	return nil
}

// primaryModel returns the configured default model, falling back to the first
// configured model when the default name is unknown.
func (c *Config) primaryModel() *ModelConfig {
	if c == nil {
		return nil
	}
	if m := c.GetModelConfig(c.DefaultModel); m != nil {
		return m
	}
	if len(c.ModelConfigs) > 0 {
		return c.ModelConfigs[0]
	}
	return nil
}

// ModelForRole resolves which model serves a given auxiliary role. An explicit
// model_roles entry takes precedence: the "fast_model" sentinel selects the
// configured fast model and any other value names a Models[] entry. With no
// explicit mapping, the role uses the fast model when one is configured,
// otherwise the primary model. Resolution always falls back to the primary
// model when a referenced model cannot be found, so a missing or misspelled
// reference degrades to current behavior rather than breaking the task.
func (c *Config) ModelForRole(role string) *ModelConfig {
	if c == nil {
		return nil
	}
	primary := c.primaryModel()
	fast := c.GetModelConfig(c.FastModel)

	if target, ok := c.ModelRoles[role]; ok {
		switch target {
		case "":
			return primary
		case FastModelRef:
			if fast != nil {
				return fast
			}
		default:
			if m := c.GetModelConfig(target); m != nil {
				return m
			}
		}
		return primary
	}

	if fast != nil {
		return fast
	}
	return primary
}
