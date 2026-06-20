// Package web provides an HTTP fetcher behind the web_fetch tool: it downloads a
// URL, extracts the main readable content as Markdown, caps the response size,
// and caches results for a short TTL. It depends only on the standard library —
// the Markdown extraction is a lightweight, hand-rolled HTML reducer (see
// html.go) rather than a third-party readability/parser dependency.
package web

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Defaults applied by NewFetcher when a Config field is left zero.
const (
	DefaultTimeout  = 30 * time.Second
	DefaultMaxBytes = 5 << 20 // source bytes downloaded before truncation
	DefaultMaxChars = 100_000 // characters of extracted Markdown returned
	DefaultTTL      = 5 * time.Minute
	userAgent       = "gogent-web-fetch/1.0"
)

// Config tunes a Fetcher. Zero fields fall back to the Default* constants.
type Config struct {
	Timeout  time.Duration
	MaxBytes int64
	MaxChars int
	TTL      time.Duration
	// Client overrides the HTTP client (tests inject a stub). nil builds a client
	// from Timeout.
	Client *http.Client
}

// Result is the outcome of a fetch.
type Result struct {
	URL       string `json:"url"`
	Title     string `json:"title,omitempty"`
	Markdown  string `json:"markdown"`
	Truncated bool   `json:"truncated,omitempty"`
	FromCache bool   `json:"from_cache,omitempty"`
}

type cacheEntry struct {
	result  Result
	expires time.Time
}

// Fetcher downloads and extracts pages, caching results for a short TTL. It is
// safe for concurrent use.
type Fetcher struct {
	client   *http.Client
	maxBytes int64
	maxChars int
	ttl      time.Duration
	now      func() time.Time // injectable clock for tests

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// NewFetcher builds a Fetcher, applying built-in defaults for any zero Config
// field.
func NewFetcher(cfg Config) *Fetcher {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.MaxChars <= 0 {
		cfg.MaxChars = DefaultMaxChars
	}
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultTTL
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	return &Fetcher{
		client:   client,
		maxBytes: cfg.MaxBytes,
		maxChars: cfg.MaxChars,
		ttl:      cfg.TTL,
		now:      time.Now,
		cache:    make(map[string]cacheEntry),
	}
}

// Fetch downloads rawURL and returns its main content as Markdown. Fresh results
// are served from the TTL cache. Only http and https URLs are accepted.
func (f *Fetcher) Fetch(rawURL string) (Result, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return Result{}, fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Result{}, fmt.Errorf("unsupported url scheme %q (only http and https are allowed)", u.Scheme)
	}
	if u.Host == "" {
		return Result{}, fmt.Errorf("url has no host")
	}

	key := u.String()
	if r, ok := f.fromCache(key); ok {
		return r, nil
	}

	req, err := http.NewRequest(http.MethodGet, key, nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,*/*")

	resp, err := f.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{}, fmt.Errorf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	// Read at most maxBytes+1 so we can detect (and flag) truncation.
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("reading body: %w", err)
	}
	truncated := int64(len(body)) > f.maxBytes
	if truncated {
		body = body[:f.maxBytes]
	}

	result := Result{URL: key}
	if isHTML(resp.Header.Get("Content-Type"), body) {
		result.Title, result.Markdown = HTMLToMarkdown(string(body))
	} else {
		result.Markdown = string(body)
	}

	md, cut := TruncateChars(result.Markdown, f.maxChars)
	result.Markdown = md
	result.Truncated = truncated || cut

	f.store(key, result)
	return result, nil
}

func (f *Fetcher) fromCache(key string) (Result, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.cache[key]
	if !ok || f.now().After(e.expires) {
		return Result{}, false
	}
	r := e.result
	r.FromCache = true
	return r, true
}

func (f *Fetcher) store(key string, r Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cache[key] = cacheEntry{result: r, expires: f.now().Add(f.ttl)}
}

// TruncateChars caps s to at most max characters (runes), reporting whether it
// cut anything. A non-positive max leaves s unchanged.
func TruncateChars(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		// len in bytes <= max implies rune count <= max, so no decode needed.
		return s, false
	}
	r := []rune(s)
	if len(r) <= max {
		return s, false
	}
	return string(r[:max]), true
}

// isHTML decides whether body should be run through the HTML extractor, using
// the Content-Type when present and a small sniff otherwise.
func isHTML(contentType string, body []byte) bool {
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "html") || strings.Contains(ct, "xml") {
		return true
	}
	if ct != "" {
		return false // a declared non-HTML type (text/plain, json, …) is used as-is
	}
	head := strings.ToLower(string(body[:min(len(body), 512)]))
	return strings.Contains(head, "<html") || strings.Contains(head, "<!doctype html") ||
		strings.Contains(head, "<body") || strings.Contains(head, "<head")
}
