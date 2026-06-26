// Package modelsdev fetches the public models.dev catalog (provider + model
// metadata) and caches it on disk so the TUI's "Add model from catalog" flow can
// pre-populate a config.ModelConfig without the user hand-transcribing endpoints,
// limits and reasoning options.
//
// The catalog is fetched from https://models.dev/api.json (unauthenticated). It
// is ~hundreds of models, so Client caches it to <home>/.gogent/modelsdev-cache.json
// with a 24h TTL and ETag/If-Modified-Since revalidation, falling back to the
// cached copy on any network failure. The HTTP fetch is hidden behind the Fetcher
// interface so tests run without the network.
//
// Only stdlib is used (net/http + encoding/json); this package adds no Go-module
// dependency. The Provider+Model -> config.ModelConfig transform (transform.go) is
// pure and unit-testable without a Client.
package modelsdev

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// catalogURL is the richest unauthenticated models.dev endpoint (providers +
// models + pricing). models.json/catalog.json are alternatives we don't use.
const catalogURL = "https://models.dev/api.json"

const (
	// defaultTTL is how long a cached catalog is served without revalidation.
	defaultTTL = 24 * time.Hour
	// cacheFileName lives alongside config.json under ~/.gogent.
	cacheFileName = "modelsdev-cache.json"
	// fetchTimeout bounds a single live catalog GET.
	fetchTimeout = 30 * time.Second
)

// Catalog is the decoded api.json, keyed by provider id (e.g. "openrouter").
type Catalog map[string]Provider

// Provider is one backend in the models.dev catalog. Env names the credential
// environment variable(s) the user should have ready; API is the /v1 base URL;
// NPM is a client-library hint used for APIType derivation.
type Provider struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	Env    []string         `json:"env"`
	NPM    string           `json:"npm"`
	API    string           `json:"api"`
	Doc    string           `json:"doc"`
	Models map[string]Model `json:"models"`
}

// Model is one model offered by a Provider. Temperature reports only WHETHER a
// custom temperature is accepted, not a value. ReasoningOptions drives the
// effort selector and thinking toggle.
type Model struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Reasoning        bool              `json:"reasoning"`
	ToolCall         bool              `json:"tool_call"`
	Attachment       bool              `json:"attachment"`
	Temperature      bool              `json:"temperature"`
	Limit            Limit             `json:"limit"`
	Cost             Cost              `json:"cost"`
	ReasoningOptions []ReasoningOption `json:"reasoning_options"`
}

// Limit is a model's token budget: Context is the input window (drives
// compaction), Output the per-request response cap.
type Limit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

// Cost is per-million-token pricing; both zero marks a free model.
type Cost struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

// ReasoningOption is one reasoning control a model exposes: Type "effort" with
// Values like ["low","medium","high"], or Type "toggle" for an on/off switch.
type ReasoningOption struct {
	Type   string   `json:"type"`
	Values []string `json:"values"`
}

// Fetcher abstracts the catalog HTTP GET so tests inject a fake without the
// network. etag/lastMod carry the cached validators; a 304 response returns
// notModified=true with nil data.
type Fetcher interface {
	Fetch(ctx context.Context, etag, lastMod string) (data []byte, newETag, newLastMod string, notModified bool, err error)
}

// httpFetcher is the production Fetcher: a plain net/http GET with conditional
// headers.
type httpFetcher struct {
	url    string
	client *http.Client
}

func (f *httpFetcher) Fetch(ctx context.Context, etag, lastMod string) ([]byte, string, string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
	if err != nil {
		return nil, "", "", false, fmt.Errorf("build request: %w", err)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastMod != "" {
		req.Header.Set("If-Modified-Since", lastMod)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, "", "", false, fmt.Errorf("get %s: %w", f.url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified {
		return nil, etag, lastMod, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", false, fmt.Errorf("models.dev returned %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", "", false, fmt.Errorf("read body: %w", err)
	}
	return body, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), false, nil
}

// Client fetches and caches the models.dev catalog. Build one with NewClient.
// now is injected only so cache-TTL behaviour is deterministic in tests; in
// production it is time.Now.
type Client struct {
	cachePath string
	ttl       time.Duration
	fetcher   Fetcher
	now       func() time.Time
}

// NewClient returns a Client caching to <homeDir>/.gogent/modelsdev-cache.json
// with a 24h TTL and the default HTTP fetcher.
func NewClient(homeDir string) *Client {
	return &Client{
		cachePath: filepath.Join(homeDir, ".gogent", cacheFileName),
		ttl:       defaultTTL,
		fetcher:   &httpFetcher{url: catalogURL, client: &http.Client{Timeout: fetchTimeout}},
		now:       time.Now,
	}
}

// cacheFile is the on-disk shape of the cached catalog plus its revalidation
// metadata.
type cacheFile struct {
	FetchedAt    time.Time `json:"fetched_at"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	Data         Catalog   `json:"data"`
}

// Catalog returns the models.dev catalog. When force is false and the cache is
// younger than the TTL, the cached copy is returned without any network call.
// Otherwise the catalog is revalidated (If-None-Match/If-Modified-Since): a 304
// refreshes the cache timestamp and returns the cached copy; a 200 replaces it.
// On any network or decode error a present cache is returned (graceful offline
// fallback); only a failure with NO cache is surfaced as an error. force=true
// ("Refresh catalog") bypasses the TTL short-circuit but still sends validators.
func (c *Client) Catalog(ctx context.Context, force bool) (Catalog, error) {
	cached, hasCache := c.loadCache()

	if !force && hasCache && c.now().Sub(cached.FetchedAt) < c.ttl {
		return cached.Data, nil
	}

	etag, lastMod := "", ""
	if hasCache {
		etag, lastMod = cached.ETag, cached.LastModified
	}

	data, newETag, newLastMod, notModified, err := c.fetcher.Fetch(ctx, etag, lastMod)
	if err != nil {
		if hasCache {
			return cached.Data, nil // offline: serve stale rather than fail
		}
		return nil, fmt.Errorf("fetch models.dev catalog: %w", err)
	}

	if notModified && hasCache {
		cached.FetchedAt = c.now()
		c.saveCache(cached)
		return cached.Data, nil
	}

	var cat Catalog
	if err := json.Unmarshal(data, &cat); err != nil {
		if hasCache {
			return cached.Data, nil
		}
		return nil, fmt.Errorf("decode models.dev catalog: %w", err)
	}

	c.saveCache(&cacheFile{FetchedAt: c.now(), ETag: newETag, LastModified: newLastMod, Data: cat})
	return cat, nil
}

// loadCache reads the cache file. ok is false when it is absent or unreadable;
// callers then treat it as a cold cache.
func (c *Client) loadCache() (*cacheFile, bool) {
	data, err := os.ReadFile(c.cachePath) //nolint:gosec // cache path derived from the user's own home dir
	if err != nil {
		return nil, false
	}
	var cf cacheFile
	if err := json.Unmarshal(data, &cf); err != nil || cf.Data == nil {
		return nil, false
	}
	return &cf, true
}

// saveCache writes the cache file best-effort; a failure to persist is non-fatal
// (the next open simply re-fetches), so the error is intentionally swallowed.
func (c *Client) saveCache(cf *cacheFile) {
	dir := filepath.Dir(c.cachePath)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return
	}
	data, err := json.Marshal(cf)
	if err != nil {
		return
	}
	_ = os.WriteFile(c.cachePath, data, 0600)
}
