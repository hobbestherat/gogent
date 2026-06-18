package web

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// roundTripFunc lets a test stand in for an *http.Client transport.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newResponse(status int, contentType, body string) *http.Response {
	h := make(http.Header)
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestFetchExtractsMarkdown(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if got := r.Header.Get("User-Agent"); got != userAgent {
			t.Errorf("User-Agent = %q, want %q", got, userAgent)
		}
		return newResponse(200, "text/html", "<title>Docs</title><body><h1>Hi</h1><p>Body text.</p></body>"), nil
	})}
	f := NewFetcher(Config{Client: client})

	res, err := f.Fetch("https://example.com/page")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if res.Title != "Docs" {
		t.Errorf("Title = %q, want Docs", res.Title)
	}
	if res.Markdown != "# Hi\n\nBody text." {
		t.Errorf("Markdown = %q", res.Markdown)
	}
	if res.FromCache {
		t.Error("first fetch should not be from cache")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestFetchCachesWithinTTL(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return newResponse(200, "text/html", "<p>cached</p>"), nil
	})}
	f := NewFetcher(Config{Client: client, TTL: time.Hour})

	first, err := f.Fetch("https://example.com/")
	if err != nil {
		t.Fatalf("first Fetch() error = %v", err)
	}
	if first.FromCache {
		t.Error("first fetch should not be cached")
	}

	second, err := f.Fetch("https://example.com/")
	if err != nil {
		t.Fatalf("second Fetch() error = %v", err)
	}
	if !second.FromCache {
		t.Error("second fetch within TTL should be served from cache")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (second served from cache)", calls)
	}
}

func TestFetchCacheExpires(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return newResponse(200, "text/html", "<p>fresh</p>"), nil
	})}
	f := NewFetcher(Config{Client: client, TTL: time.Minute})
	current := time.Unix(1000, 0)
	f.now = func() time.Time { return current }

	if _, err := f.Fetch("https://example.com/"); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	// Advance past the TTL: the next fetch must hit the network again.
	current = current.Add(2 * time.Minute)
	res, err := f.Fetch("https://example.com/")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if res.FromCache {
		t.Error("fetch after TTL should not be from cache")
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestFetchTruncatesLargeBody(t *testing.T) {
	big := "<body>" + strings.Repeat("x", 10000) + "</body>"
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return newResponse(200, "text/html", big), nil
	})}
	f := NewFetcher(Config{Client: client, MaxBytes: 1000})

	res, err := f.Fetch("https://example.com/")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if !res.Truncated {
		t.Error("expected Truncated to be true")
	}
	if len(res.Markdown) > 1000 {
		t.Errorf("markdown length = %d, expected <= 1000", len(res.Markdown))
	}
}

func TestFetchNonHTMLPassthrough(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return newResponse(200, "text/plain", "plain <b>not bold</b> text"), nil
	})}
	f := NewFetcher(Config{Client: client})

	res, err := f.Fetch("https://example.com/raw.txt")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if res.Markdown != "plain <b>not bold</b> text" {
		t.Errorf("non-HTML body should pass through verbatim, got %q", res.Markdown)
	}
}

func TestFetchRejectsBadScheme(t *testing.T) {
	f := NewFetcher(Config{})
	for _, u := range []string{"ftp://example.com", "file:///etc/passwd", "/relative/path", "not a url"} {
		if _, err := f.Fetch(u); err == nil {
			t.Errorf("Fetch(%q) expected error, got nil", u)
		}
	}
}

func TestFetchHTTPError(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return newResponse(404, "text/html", "nope"), nil
	})}
	f := NewFetcher(Config{Client: client})
	if _, err := f.Fetch("https://example.com/missing"); err == nil {
		t.Error("expected error on HTTP 404")
	}
}

func TestTruncateChars(t *testing.T) {
	tests := []struct {
		in      string
		max     int
		want    string
		wantCut bool
	}{
		{"hello", 10, "hello", false},
		{"hello", 3, "hel", true},
		{"héllo", 3, "hél", true}, // rune-safe, not byte-safe
		{"x", 0, "x", false},
		{"", 5, "", false},
	}
	for _, tt := range tests {
		got, cut := TruncateChars(tt.in, tt.max)
		if got != tt.want || cut != tt.wantCut {
			t.Errorf("TruncateChars(%q, %d) = (%q, %v), want (%q, %v)", tt.in, tt.max, got, cut, tt.want, tt.wantCut)
		}
	}
}
