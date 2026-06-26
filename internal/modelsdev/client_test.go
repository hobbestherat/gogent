package modelsdev

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeClock is a mutable clock so tests can advance time between Catalog calls.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// fetchResult is one Fetch outcome. A queueFetcher returns them in order,
// repeating the last for any extra calls.
type fetchResult struct {
	data        []byte
	etag        string
	lastMod     string
	notModified bool
	err         error
}

type queueFetcher struct {
	results     []fetchResult
	calls       int
	gotETags    []string
	gotLastMods []string
}

func (f *queueFetcher) Fetch(_ context.Context, etag, lastMod string) ([]byte, string, string, bool, error) {
	i := f.calls
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	f.calls++
	f.gotETags = append(f.gotETags, etag)
	f.gotLastMods = append(f.gotLastMods, lastMod)
	r := f.results[i]
	return r.data, r.etag, r.lastMod, r.notModified, r.err
}

func okResult(etag, lastMod string) fetchResult {
	return fetchResult{
		data: []byte(`{"groq":{"id":"groq","name":"Groq","models":{` +
			`"llama-3.3-70b":{"id":"llama-3.3-70b","name":"Llama 3.3 70B",` +
			`"limit":{"context":131072,"output":32768}}}}}`),
		etag:    etag,
		lastMod: lastMod,
	}
}

// alternateResult carries a different provider so refresh-replaces is observable.
func alternateResult() fetchResult {
	return fetchResult{data: []byte(`{"deepseek":{"id":"deepseek","name":"DeepSeek","models":{}}}`)}
}

func newTestClient(t *testing.T, cachePath string, f Fetcher, ttl time.Duration, clk *fakeClock) *Client {
	t.Helper()
	return &Client{
		cachePath: cachePath,
		ttl:       ttl,
		fetcher:   f,
		now:       clk.now,
	}
}

func readCacheFile(t *testing.T, path string) *cacheFile {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cache %s: %v", path, err)
	}
	var cf cacheFile
	if err := json.Unmarshal(b, &cf); err != nil {
		t.Fatalf("decode cache: %v", err)
	}
	return &cf
}

func TestCatalogColdFetch(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	f := &queueFetcher{results: []fetchResult{okResult("e1", "lm1")}}
	c := newTestClient(t, filepath.Join(t.TempDir(), "cache.json"), f, 24*time.Hour, clk)

	cat, err := c.Catalog(context.Background(), false)
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if f.calls != 1 {
		t.Errorf("fetcher calls = %d, want 1 on cold cache", f.calls)
	}
	if _, ok := cat["groq"]; !ok {
		t.Fatalf("catalog = %+v, want a groq provider", cat)
	}
	// The cache file is written for the next open.
	if _, err := os.Stat(c.cachePath); err != nil {
		t.Fatalf("cache file not written: %v", err)
	}
}

func TestCatalogTTLShortCircuitDoesNotFetch(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	f := &queueFetcher{results: []fetchResult{okResult("e1", "lm1")}}
	c := newTestClient(t, filepath.Join(t.TempDir(), "cache.json"), f, 24*time.Hour, clk)

	if _, err := c.Catalog(context.Background(), false); err != nil {
		t.Fatalf("cold fetch: %v", err)
	}
	// Second call within the TTL must NOT hit the network.
	cat, err := c.Catalog(context.Background(), false)
	if err != nil {
		t.Fatalf("warm fetch: %v", err)
	}
	if f.calls != 1 {
		t.Errorf("fetcher calls = %d after warm read, want 1 (TTL should short-circuit)", f.calls)
	}
	if _, ok := cat["groq"]; !ok {
		t.Errorf("warm catalog missing groq")
	}
}

func TestCatalog304RevalidationBumpsTimestamp(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	f := &queueFetcher{results: []fetchResult{
		okResult("e1", "lm1"),
		{notModified: true, etag: "e1", lastMod: "lm1"},
	}}
	c := newTestClient(t, filepath.Join(t.TempDir(), "cache.json"), f, 24*time.Hour, clk)

	if _, err := c.Catalog(context.Background(), false); err != nil {
		t.Fatalf("cold fetch: %v", err)
	}

	// Advance beyond the TTL so the cached copy must revalidate.
	clk.t = clk.t.Add(48 * time.Hour)
	cat, err := c.Catalog(context.Background(), false)
	if err != nil {
		t.Fatalf("revalidate: %v", err)
	}
	if f.calls != 2 {
		t.Errorf("fetcher calls = %d, want 2 (one cold + one revalidation)", f.calls)
	}
	// A 304 must still serve the cached catalog unchanged.
	if _, ok := cat["groq"]; !ok {
		t.Errorf("304 returned catalog without groq")
	}
	// ...and bump the cache's FetchedAt so the TTL clock restarts.
	cf := readCacheFile(t, c.cachePath)
	if !cf.FetchedAt.Equal(clk.t) {
		t.Errorf("FetchedAt = %v, want %v (304 should refresh the timestamp)", cf.FetchedAt, clk.t)
	}
}

func TestCatalog200RefreshReplacesData(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	f := &queueFetcher{results: []fetchResult{
		okResult("e1", "lm1"),
		alternateResult(),
	}}
	c := newTestClient(t, filepath.Join(t.TempDir(), "cache.json"), f, 24*time.Hour, clk)

	if _, err := c.Catalog(context.Background(), false); err != nil {
		t.Fatalf("cold fetch: %v", err)
	}
	clk.t = clk.t.Add(48 * time.Hour)
	cat, err := c.Catalog(context.Background(), false)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, ok := cat["groq"]; ok {
		t.Error("stale groq provider served after a 200 refresh")
	}
	if _, ok := cat["deepseek"]; !ok {
		t.Error("refreshed catalog missing deepseek")
	}
	// The replaced data is persisted.
	cf := readCacheFile(t, c.cachePath)
	if _, ok := cf.Data["deepseek"]; !ok {
		t.Error("cache file not updated with refreshed data")
	}
}

func TestCatalogNetworkErrorServesStaleCache(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	f := &queueFetcher{results: []fetchResult{
		okResult("e1", "lm1"),
		{err: errors.New("dial tcp: unreachable")},
	}}
	c := newTestClient(t, filepath.Join(t.TempDir(), "cache.json"), f, 24*time.Hour, clk)

	if _, err := c.Catalog(context.Background(), false); err != nil {
		t.Fatalf("cold fetch: %v", err)
	}
	clk.t = clk.t.Add(48 * time.Hour)
	cat, err := c.Catalog(context.Background(), false)
	if err != nil {
		t.Fatalf("offline read should fall back to cache, got %v", err)
	}
	if _, ok := cat["groq"]; !ok {
		t.Error("offline fallback did not serve the cached groq provider")
	}
}

func TestCatalogNoCacheNetworkErrorIsError(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	f := &queueFetcher{results: []fetchResult{{err: errors.New("offline")}}}
	c := newTestClient(t, filepath.Join(t.TempDir(), "cache.json"), f, 24*time.Hour, clk)

	cat, err := c.Catalog(context.Background(), false)
	if err == nil {
		t.Fatal("Catalog with no cache + network error = nil, want error")
	}
	if cat != nil {
		t.Errorf("catalog = %+v, want nil on hard failure", cat)
	}
}

func TestCatalogDecodeErrorServesStaleCache(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	f := &queueFetcher{results: []fetchResult{
		okResult("e1", "lm1"),
		{data: []byte("not-json{{")}, // a 200 with an undecodable body
	}}
	c := newTestClient(t, filepath.Join(t.TempDir(), "cache.json"), f, 24*time.Hour, clk)

	if _, err := c.Catalog(context.Background(), false); err != nil {
		t.Fatalf("cold fetch: %v", err)
	}
	clk.t = clk.t.Add(48 * time.Hour)
	cat, err := c.Catalog(context.Background(), false)
	if err != nil {
		t.Fatalf("decode error should fall back to cache, got %v", err)
	}
	if _, ok := cat["groq"]; !ok {
		t.Error("decode-error fallback did not serve cached groq")
	}
}

func TestCatalogNoCacheDecodeErrorIsError(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	f := &queueFetcher{results: []fetchResult{{data: []byte("not-json")}}}
	c := newTestClient(t, filepath.Join(t.TempDir(), "cache.json"), f, 24*time.Hour, clk)

	if _, err := c.Catalog(context.Background(), false); err == nil {
		t.Fatal("Catalog with no cache + decode error = nil, want error")
	}
}

func TestCatalogForceBypassesTTL(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	f := &queueFetcher{results: []fetchResult{
		okResult("e1", "lm1"),
		alternateResult(),
	}}
	c := newTestClient(t, filepath.Join(t.TempDir(), "cache.json"), f, 24*time.Hour, clk)

	if _, err := c.Catalog(context.Background(), false); err != nil {
		t.Fatalf("cold fetch: %v", err)
	}
	// Within the TTL, but force=true must re-fetch anyway.
	cat, err := c.Catalog(context.Background(), true)
	if err != nil {
		t.Fatalf("force refresh: %v", err)
	}
	if f.calls != 2 {
		t.Errorf("fetcher calls = %d, want 2 (force must bypass the TTL short-circuit)", f.calls)
	}
	if _, ok := cat["deepseek"]; !ok {
		t.Error("force refresh did not return the refreshed data")
	}
}

func TestCatalogPersistsAcrossClientInstances(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "cache.json")
	clk := &fakeClock{t: time.Unix(1000, 0)}

	c1 := newTestClient(t, cachePath, &queueFetcher{results: []fetchResult{okResult("e1", "lm1")}}, 24*time.Hour, clk)
	if _, err := c1.Catalog(context.Background(), false); err != nil {
		t.Fatalf("cold fetch: %v", err)
	}

	// A brand-new Client over the same cache file must serve the persisted copy
	// without any network call (warm read).
	fresh := &queueFetcher{results: []fetchResult{{err: errors.New("must not be called")}}}
	c2 := newTestClient(t, cachePath, fresh, 24*time.Hour, clk)
	cat, err := c2.Catalog(context.Background(), false)
	if err != nil {
		t.Fatalf("warm read across instances: %v", err)
	}
	if fresh.calls != 0 {
		t.Errorf("second client fetched (calls=%d); the cache should have been served", fresh.calls)
	}
	if _, ok := cat["groq"]; !ok {
		t.Error("persisted catalog missing groq")
	}
}

func TestCatalogRevalidationSendsCachedValidators(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	f := &queueFetcher{results: []fetchResult{
		okResult(`"abc"`, "Wed, 01 Jan 2025 00:00:00 GMT"),
		{notModified: true, etag: `"abc"`, lastMod: "Wed, 01 Jan 2025 00:00:00 GMT"},
	}}
	c := newTestClient(t, filepath.Join(t.TempDir(), "cache.json"), f, 24*time.Hour, clk)

	if _, err := c.Catalog(context.Background(), false); err != nil {
		t.Fatalf("cold fetch: %v", err)
	}
	clk.t = clk.t.Add(48 * time.Hour)
	if _, err := c.Catalog(context.Background(), false); err != nil {
		t.Fatalf("revalidate: %v", err)
	}
	// The revalidation fetch must carry the cached ETag/Last-Modified.
	if got := f.gotETags[1]; got != `"abc"` {
		t.Errorf("revalidation ETag = %q, want %q", got, `"abc"`)
	}
	if got := f.gotLastMods[1]; got != "Wed, 01 Jan 2025 00:00:00 GMT" {
		t.Errorf("revalidation Last-Modified = %q, want the cached value", got)
	}
}
