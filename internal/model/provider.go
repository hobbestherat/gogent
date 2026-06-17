package model

import "strings"

// APIType identifies which provider/wire conventions a backend speaks. Every
// provider supported today is OpenAI-compatible over HTTP + SSE and differs only
// in its base-URL layout and default endpoint, which providerSpec captures. This
// is the seam to extend when a genuinely different protocol has to be added:
// introduce a new APIType, give it a providerSpec, and (if the request/response
// shape differs) branch on the type where the wire format is built.
type APIType string

const (
	// APITypeOpenAI is the generic OpenAI-compatible API: the caller supplies a
	// base URL (or a full chat-completions URL) and we talk plain OpenAI.
	APITypeOpenAI APIType = "openai"
	// APITypeZAI is the Z.AI platform (https://docs.z.ai). It is OpenAI
	// compatible; only the default base URL differs, so the user can leave the
	// endpoint empty and just provide an API key.
	APITypeZAI APIType = "zai"
)

var stringToAPITypeMap = map[string]APIType{
	"openai": APITypeOpenAI,
	"zai":    APITypeZAI,
	"z.ai":   APITypeZAI,
}

// StringToAPIType resolves a config string to an APIType, defaulting to the
// generic OpenAI-compatible provider when empty or unknown.
func StringToAPIType(s string) APIType {
	if t, ok := stringToAPITypeMap[strings.ToLower(strings.TrimSpace(s))]; ok {
		return t
	}
	return APITypeOpenAI
}

// providerSpec describes how to derive concrete endpoints for an APIType from a
// (possibly empty) user-supplied base URL.
type providerSpec struct {
	// defaultBaseURL is used when the config endpoint is left empty, so simple
	// providers only need an API key.
	defaultBaseURL string
	// chatPath and modelsPath are appended to the base URL to reach the
	// chat-completions and model-listing endpoints.
	chatPath   string
	modelsPath string
}

var providerSpecs = map[APIType]providerSpec{
	APITypeOpenAI: {
		// Neutral local default; apps with their own default resolve it upstream
		// (see config.DefaultEndpoint) and pass a full endpoint in.
		defaultBaseURL: "http://localhost:8080/v1",
		chatPath:       "/chat/completions",
		modelsPath:     "/models",
	},
	APITypeZAI: {
		defaultBaseURL: "https://api.z.ai/api/paas/v4",
		chatPath:       "/chat/completions",
		modelsPath:     "/models",
	},
}

// specFor returns the providerSpec for an APIType, falling back to OpenAI.
func specFor(t APIType) providerSpec {
	if s, ok := providerSpecs[t]; ok {
		return s
	}
	return providerSpecs[APITypeOpenAI]
}

// APITypeIDs lists the selectable api_type values in display order (first is the
// default). Config UIs use this to populate an API-type dropdown.
func APITypeIDs() []string {
	return []string{string(APITypeOpenAI), string(APITypeZAI)}
}

// normalizeBaseURL reduces whatever the user put in the config endpoint to a
// bare base URL: an empty value falls back to the provider default, a full
// chat-completions URL is trimmed back to its base, and trailing slashes are
// dropped. This is what lets a user supply just a base URL and have the rest
// filled in automatically.
func normalizeBaseURL(endpoint string, spec providerSpec) string {
	e := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if e == "" {
		e = strings.TrimRight(spec.defaultBaseURL, "/")
	}
	if i := strings.LastIndex(e, spec.chatPath); i >= 0 && i == len(e)-len(spec.chatPath) {
		e = strings.TrimRight(e[:i], "/")
	}
	return e
}

// chatURL and modelsURL build the concrete endpoints for a base URL.
func (s providerSpec) chatURL(base string) string   { return base + s.chatPath }
func (s providerSpec) modelsURL(base string) string { return base + s.modelsPath }
