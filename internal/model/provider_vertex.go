package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"gogent/internal/config"
)

// Google Vertex AI providers. Three api_types share Vertex's ADC auth and
// project/location-derived base URL but differ in wire format and route:
//
//   - vertex          → Gemini via the OpenAI-compatible shim (openAIAdapter)
//   - vertex-native   → native Gemini :generateContent (geminiAdapter)
//   - vertex-anthropic→ Claude :rawPredict (anthropicAdapter; in provider_anthropic.go)
//
// All three discover models through the Vertex Model Garden publisher listing
// (vertexPublisherLister), which is why the Scan button works for them.

// vertexAnthropicVersion is the anthropic_version body value for Claude on Vertex
// (the direct API uses the anthropic-version header instead; see anthropicVersion).
const vertexAnthropicVersion = "vertex-2023-10-16"

// vertexModelGardenBase is the base URL of the Vertex Model Garden listing API. It
// is a package var (not a const) only so tests can point it at a local server; in
// production it is always the global aiplatform host (publisher-model listing is a
// location-independent catalog).
var vertexModelGardenBase = "https://aiplatform.googleapis.com/v1beta1"

func init() {
	registerProvider(&provider{
		apiType: APITypeVertex,
		adapter: openAIAdapter{},
		// OpenAI-compatible surface: response_format works; reasoning_effort /
		// thinking are native-Gemini features not exposed through the compat shim.
		caps:      Capabilities{SupportsResponseFormat: true},
		endpoints: staticBaseEndpoints{baseURLFunc: vertexOpenAIBaseURL, chatPath: "/endpoints/openapi/chat/completions"},
		auth:      adcAuth{},
		lister:    vertexPublisherLister{publisher: "google", format: googlePublisherModelID, keep: hasPrefixAny("gemini", "gemma")},
		validate:  vertexValidate,
		// Defense in depth: a bare "gemini-…" that escaped ValidateModelConfig is
		// auto-qualified to "google/gemini-…" at the send seam, so it can never reach
		// Vertex bare and 400 opaquely (issue #574). Set only on the shim, whose model
		// travels in the request body; the native route carries the model in the URL.
		normalizeModelID: ensureGooglePublisher,
		derivesBase:      true, // base URL is derived from project/location; an empty endpoint is routable
	})

	registerProvider(&provider{
		apiType: APITypeVertexNative,
		adapter: geminiAdapter{},
		// Native Gemini: structured output via responseSchema and thinking via
		// thinkingConfig; Gemini accepts temperature normally. Cache: context-cache
		// reads (cachedContentTokenCount) at ~0.25x, no write count; explicit
		// cachedContent is a client-side directive declared for #547.
		caps: Capabilities{
			SupportsResponseFormat: true,
			SupportsThinking:       true,
			CacheControl:           CacheControlCachedContent,
			CacheReadMultiplier:    0.25,
		},
		endpoints:   modelURLEndpoints{baseURLFunc: vertexNativeBaseURL, chatURLFunc: vertexNativeChatURL, streamURLFunc: vertexNativeStreamURL},
		auth:        adcAuth{},
		lister:      vertexPublisherLister{publisher: "google", format: bareModelID, keep: hasPrefixAny("gemini", "gemma")},
		validate:    vertexValidate,
		derivesBase: true, // base URL is derived from project/location; an empty endpoint is routable
	})
}

// vertexValidate defers a clear config error when a Vertex base URL would be
// derived from a missing project/location (which would otherwise fail as an
// opaque DNS/HTTP error). Skipped when an explicit endpoint overrides the base.
func vertexValidate(conn *config.ProviderConnection) error {
	if strings.TrimSpace(conn.Endpoint) != "" {
		return nil
	}
	if strings.TrimSpace(conn.Project) == "" || strings.TrimSpace(conn.Location) == "" {
		return &ModelError{
			Type:    ErrorGeneric,
			Message: "vertex: project and location are required (set them on the connection, or supply an explicit endpoint)",
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// URL builders
// ---------------------------------------------------------------------------

// vertexAIHost returns the Vertex AI API host for a region: the regional host
// "{location}-aiplatform.googleapis.com", except the special "global" location,
// which uses the unprefixed host (the URL path still carries /locations/global/).
func vertexAIHost(location string) string {
	if location == "global" {
		return "aiplatform.googleapis.com"
	}
	return location + "-aiplatform.googleapis.com"
}

// vertexOpenAIBaseURL builds the Vertex OpenAI-compatible base URL (v1beta1) from
// a config's project/location. Location is lower-cased so the host/path are robust
// to a mixed-case region (and the "global" special case stays correct).
func vertexOpenAIBaseURL(c *config.ProviderConnection) string {
	loc := strings.ToLower(strings.TrimSpace(c.Location))
	return fmt.Sprintf("https://%s/v1beta1/projects/%s/locations/%s",
		vertexAIHost(loc), strings.TrimSpace(c.Project), loc)
}

// vertexNativeBaseURL builds the Vertex base URL for the native Gemini and Claude
// routes (v1, GA). The model name is interpolated into the path by the chat/stream
// URL builders, not the base.
func vertexNativeBaseURL(c *config.ProviderConnection) string {
	loc := strings.ToLower(strings.TrimSpace(c.Location))
	return fmt.Sprintf("https://%s/v1/projects/%s/locations/%s",
		vertexAIHost(loc), strings.TrimSpace(c.Project), loc)
}

// vertexNativeChatURL / vertexNativeStreamURL build the native Gemini routes
// (model in the path). Streaming targets :streamGenerateContent and appends
// ?alt=sse so Vertex frames the response as server-sent events.
func vertexNativeChatURL(base, model string) string {
	return fmt.Sprintf("%s/publishers/google/models/%s:generateContent",
		strings.TrimRight(strings.TrimSpace(base), "/"), strings.TrimSpace(model))
}

func vertexNativeStreamURL(base, model string) string {
	return fmt.Sprintf("%s/publishers/google/models/%s:streamGenerateContent?alt=sse",
		strings.TrimRight(strings.TrimSpace(base), "/"), strings.TrimSpace(model))
}

// vertexAnthropicChatURL / vertexAnthropicStreamURL build the Claude-on-Vertex
// routes (model in the path). :streamRawPredict already streams Anthropic SSE, so
// no ?alt=sse is needed; the request body still carries "stream":true.
func vertexAnthropicChatURL(base, model string) string {
	return fmt.Sprintf("%s/publishers/anthropic/models/%s:rawPredict",
		strings.TrimRight(strings.TrimSpace(base), "/"), strings.TrimSpace(model))
}

func vertexAnthropicStreamURL(base, model string) string {
	return fmt.Sprintf("%s/publishers/anthropic/models/%s:streamRawPredict",
		strings.TrimRight(strings.TrimSpace(base), "/"), strings.TrimSpace(model))
}

// ---------------------------------------------------------------------------
// Model Garden listing
// ---------------------------------------------------------------------------

// vertexPublisherLister discovers a publisher's models from the Vertex Model
// Garden — GET https://aiplatform.googleapis.com/v1beta1/publishers/{publisher}/models.
// This is a GLOBAL (location-independent) catalog; unlike the per-request routes
// it has no project in the URL path, so the consumer/quota project must be carried
// in the X-Goog-User-Project header (taken from the model's Project). Results are
// formatted into config-ready ids (format) and narrowed to the chat-model families
// the provider can actually use (keep), since the raw catalog also lists embedding,
// image, and video models that the chat endpoints can't serve.
type vertexPublisherLister struct {
	publisher string
	format    func(id string) string // catalog id -> config model id
	keep      func(id string) bool   // filter to usable chat models (nil = keep all)
}

func (l vertexPublisherLister) list(ctx context.Context, c *ModelConnection) ([]ModelInfo, error) {
	proj := ""
	gardenBase := vertexModelGardenBase
	if c.Conn != nil {
		proj = strings.TrimSpace(c.Conn.Project)
		// A connection may override the Model Garden host (private/proxied Vertex);
		// otherwise the global public catalog host is used.
		if d := strings.TrimSpace(c.Conn.DiscoveryEndpoint); d != "" {
			gardenBase = d
		}
	}
	if proj == "" {
		return nil, &ModelError{
			Type:    ErrorGeneric,
			Message: "vertex: project is required to list models (set it on the connection)",
		}
	}
	headers := http.Header{}
	headers.Set("X-Goog-User-Project", proj)
	listURL := strings.TrimRight(gardenBase, "/") + "/publishers/" + l.publisher + "/models"

	var out []ModelInfo
	seen := map[string]bool{}
	pageToken := ""
	// Bounded loop: the catalog is a few hundred entries; 20 pages of 200 is a far
	// upper bound that also guards against a server that never stops paginating.
	for page := 0; page < 20; page++ {
		u := listURL + "?pageSize=200"
		if pageToken != "" {
			u += "&pageToken=" + url.QueryEscape(pageToken)
		}
		body, err := c.doJSON(ctx, http.MethodGet, u, headers)
		if err != nil {
			return nil, err
		}
		var resp struct {
			PublisherModels []struct {
				Name string `json:"name"`
			} `json:"publisherModels"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, &ModelError{Type: ErrorGeneric, Message: fmt.Sprintf("failed to parse vertex model list: %v", err)}
		}
		for _, pm := range resp.PublisherModels {
			id := lastPathSegment(pm.Name) // publishers/google/models/gemini-2.5-flash -> gemini-2.5-flash
			if id == "" {
				continue
			}
			if l.keep != nil && !l.keep(id) {
				continue
			}
			mid := id
			if l.format != nil {
				mid = l.format(id)
			}
			if seen[mid] {
				continue
			}
			seen[mid] = true
			out = append(out, ModelInfo{ID: mid, OwnedBy: l.publisher})
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	if len(out) == 0 {
		return nil, &ModelError{Type: ErrorGeneric, Message: "no matching vertex models found"}
	}
	return out, nil
}

// lastPathSegment returns the final "/"-separated segment of s (the bare model id
// from a "publishers/<pub>/models/<id>" resource name).
func lastPathSegment(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// bareModelID is the identity formatter (native Gemini and Claude name the model
// bare). googlePublisherModelID prefixes the publisher for the OpenAI-compat shim,
// which addresses Gemini as "google/<model>".
func bareModelID(id string) string            { return id }
func googlePublisherModelID(id string) string { return "google/" + id }

// ensureGooglePublisher qualifies a bare model id for the Vertex OpenAI-compat shim,
// which addresses Gemini as "google/<model>". It is the request-build counterpart of the
// lister's googlePublisherModelID — the same rule the Model-Garden lister applies to
// discovered ids — but only when the id lacks a publisher, so an already-correct
// "google/gemini-…" (or another publisher) is left untouched. An empty id is left empty.
// Wired as the shim provider's normalizeModelID, it is the last-line guarantee that a bare
// name escaping ValidateModelConfig is never sent verbatim and 400s opaquely (issue #574).
func ensureGooglePublisher(id string) string {
	if id == "" || strings.Contains(id, "/") {
		return id
	}
	return googlePublisherModelID(id)
}

// hasPrefixAny returns a keep-filter matching ids that start with any prefix.
func hasPrefixAny(prefixes ...string) func(string) bool {
	return func(id string) bool {
		for _, p := range prefixes {
			if strings.HasPrefix(id, p) {
				return true
			}
		}
		return false
	}
}
