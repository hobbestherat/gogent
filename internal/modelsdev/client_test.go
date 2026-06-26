package modelsdev

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// --- fixes-round-1 pins: context propagation, empty-catalog not cached, concurrency ---

// ctxCaptureFetcher records the context Catalog handed it, then returns a fixed
// result. Used to prove Catalog threads its ctx through to the Fetcher.
type ctxCaptureFetcher struct {
	gotCtx context.Context
	result fetchResult
}

func (f *ctxCaptureFetcher) Fetch(ctx context.Context, _, _ string) ([]byte, string, string, bool, error) {
	f.gotCtx = ctx
	return f.result.data, f.result.etag, f.result.lastMod, f.result.notModified, f.result.err
}

func TestCatalogPassesContextToFetcher(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	f := &ctxCaptureFetcher{result: okResult("e1", "lm1")}
	c := newTestClient(t, filepath.Join(t.TempDir(), "cache.json"), f, 24*time.Hour, clk)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := c.Catalog(ctx, false); err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	// The exact ctx instance must reach the fetcher so a caller's cancellation
	// (the dialog's Cancel/Escape) actually aborts the in-flight GET.
	if f.gotCtx != ctx {
		t.Errorf("fetcher received a different context than the one passed to Catalog")
	}
}

// ctxAwareFetcher honors cancellation: it surfaces ctx.Err() when the context is
// already done, otherwise returns a fixed result.
type ctxAwareFetcher struct {
	calls  int
	result fetchResult
}

func (f *ctxAwareFetcher) Fetch(ctx context.Context, _, _ string) ([]byte, string, string, bool, error) {
	f.calls++
	if err := ctx.Err(); err != nil {
		return nil, "", "", false, err
	}
	return f.result.data, f.result.etag, f.result.lastMod, f.result.notModified, f.result.err
}

func TestCatalogHonorsCancelledContextNoCache(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	f := &ctxAwareFetcher{result: okResult("e1", "lm1")}
	c := newTestClient(t, filepath.Join(t.TempDir(), "cache.json"), f, 24*time.Hour, clk)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled: the fetch must abort and, with no cache, surface an error
	_, err := c.Catalog(ctx, false)
	if err == nil {
		t.Fatal("Catalog with cancelled ctx + no cache = nil, want error")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("error = %q, want it to mention context canceled", err.Error())
	}
}

func TestCatalogHonorsCancelledContextServesStale(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	f := &ctxAwareFetcher{result: okResult("e1", "lm1")}
	c := newTestClient(t, filepath.Join(t.TempDir(), "cache.json"), f, 24*time.Hour, clk)

	if _, err := c.Catalog(context.Background(), false); err != nil { // cold: populates cache
		t.Fatalf("cold fetch: %v", err)
	}
	clk.t = clk.t.Add(48 * time.Hour) // stale → must revalidate

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cat, err := c.Catalog(ctx, false)
	if err != nil {
		t.Fatalf("cancelled revalidation with a cache should serve stale, got %v", err)
	}
	if _, ok := cat["groq"]; !ok {
		t.Error("stale fallback on cancelled revalidation did not serve cached groq")
	}
}

// A FRESH cache must short-circuit and ignore a cancelled context (there's no
// work to abort).
func TestCatalogFreshCacheIgnoresCancelledContext(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	f := &ctxAwareFetcher{result: okResult("e1", "lm1")}
	c := newTestClient(t, filepath.Join(t.TempDir(), "cache.json"), f, 24*time.Hour, clk)

	if _, err := c.Catalog(context.Background(), false); err != nil { // cold (call 1)
		t.Fatalf("cold fetch: %v", err)
	}
	before := f.calls

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cat, err := c.Catalog(ctx, false) // within TTL → short-circuit, ctx unused
	if err != nil {
		t.Fatalf("fresh-cache read with cancelled ctx errored: %v", err)
	}
	if f.calls != before {
		t.Errorf("fetcher calls = %d, want %d (a fresh cache must not fetch even with a cancelled ctx)", f.calls, before)
	}
	if _, ok := cat["groq"]; !ok {
		t.Error("fresh-cache read missing groq")
	}
}

// A valid-but-empty `{}` response must NOT be cached (it would poison the cache
// for a full TTL). Cold path: surface an error.
func TestCatalogEmptyResponseNotCachedCold(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	f := &queueFetcher{results: []fetchResult{{data: []byte("{}")}}}
	c := newTestClient(t, filepath.Join(t.TempDir(), "cache.json"), f, 24*time.Hour, clk)

	_, err := c.Catalog(context.Background(), false)
	if err == nil {
		t.Fatal("empty catalog with no cache = nil, want error")
	}
	// Nothing was persisted, so the next open is still a cold fetch (not a 24h
	// wait on an empty cache).
	if _, statErr := os.Stat(c.cachePath); !os.IsNotExist(statErr) {
		t.Errorf("empty response should not be written to the cache; stat = %v", statErr)
	}
}

// And when a good cache exists, an empty response must NOT overwrite it.
func TestCatalogEmptyResponseServesStaleAndDoesNotPoison(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	f := &queueFetcher{results: []fetchResult{
		okResult("e1", "lm1"),
		{data: []byte("{}")}, // a transient empty 200
	}}
	c := newTestClient(t, filepath.Join(t.TempDir(), "cache.json"), f, 24*time.Hour, clk)

	if _, err := c.Catalog(context.Background(), false); err != nil { // cold groq
		t.Fatalf("cold fetch: %v", err)
	}
	clk.t = clk.t.Add(48 * time.Hour)
	cat, err := c.Catalog(context.Background(), false)
	if err != nil {
		t.Fatalf("empty response should fall back to cache, got %v", err)
	}
	if _, ok := cat["groq"]; !ok {
		t.Error("empty-response fallback did not serve cached groq")
	}
	// The good cache survives the empty response (not poisoned).
	cf := readCacheFile(t, c.cachePath)
	if _, ok := cf.Data["groq"]; !ok {
		t.Fatal("cache poisoned: groq replaced by the empty response")
	}
	if len(cf.Data) != 1 {
		t.Errorf("cache has %d providers, want 1 (groq only)", len(cf.Data))
	}
}

// slowSafeFetcher is concurrency-safe (no shared mutable state), sleeps to widen
// the race window, and honors cancellation.
type slowSafeFetcher struct{ d time.Duration }

func (f *slowSafeFetcher) Fetch(ctx context.Context, _, _ string) ([]byte, string, string, bool, error) {
	select {
	case <-time.After(f.d):
		r := okResult("e1", "lm1")
		return r.data, r.etag, r.lastMod, false, nil
	case <-ctx.Done():
		return nil, "", "", false, ctx.Err()
	}
}

// Concurrent Catalog calls (force=true → every call fetches + writes) must not
// panic or leave a torn/corrupt cache file. The Client.mu serializes the writes.
// (Best-effort without -race on Pi5: it proves no panic/corruption, not the
// absence of the data race itself.)
func TestCatalogConcurrentCallsDoNotCorrupt(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	f := &slowSafeFetcher{d: 3 * time.Millisecond}
	c := newTestClient(t, filepath.Join(t.TempDir(), "cache.json"), f, 24*time.Hour, clk)

	const N = 8
	var wg sync.WaitGroup
	errs := make([]error, N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start                                            // release together to maximize overlap
			_, errs[i] = c.Catalog(context.Background(), true) // force → fetch + save
		}(i)
	}
	close(start)
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("goroutine %d errored: %v", i, e)
		}
	}
	cf := readCacheFile(t, c.cachePath) // would fail to decode if writes had interleaved/torn
	if _, ok := cf.Data["groq"]; !ok {
		t.Fatal("cache file corrupted after concurrent writes (no groq)")
	}
}
