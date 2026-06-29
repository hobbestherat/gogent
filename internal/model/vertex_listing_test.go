package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"gogent/internal/config"
)

// fakeModelGarden serves the Vertex Model Garden publisherModels listing for a
// publisher, optionally across two pages, and records the requests it sees.
func fakeModelGarden(t *testing.T, publisher string, pages [][]string) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r)
		wantPath := "/publishers/" + publisher + "/models"
		if r.URL.Path != wantPath {
			http.Error(w, "not found: "+r.URL.Path, http.StatusNotFound)
			return
		}
		page := 0
		if tok := r.URL.Query().Get("pageToken"); tok != "" {
			// token is "page-<n>"
			page = int(tok[len(tok)-1] - '0')
		}
		type pm struct {
			Name string `json:"name"`
		}
		var out struct {
			PublisherModels []pm   `json:"publisherModels"`
			NextPageToken   string `json:"nextPageToken"`
		}
		for _, id := range pages[page] {
			out.PublisherModels = append(out.PublisherModels, pm{Name: "publishers/" + publisher + "/models/" + id})
		}
		if page+1 < len(pages) {
			out.NextPageToken = "page-1"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func withModelGardenBase(t *testing.T, base string) {
	t.Helper()
	orig := vertexModelGardenBase
	vertexModelGardenBase = base
	t.Cleanup(func() { vertexModelGardenBase = orig })
}

// TestVertexScanGoogleCompat exercises the Scan button for the vertex (OpenAI
// compat) provider end-to-end: ADC bearer auth, the X-Goog-User-Project quota
// header, publisherModels paging, the gemini/gemma filter, and google/ id format.
func TestVertexScanGoogleCompat(t *testing.T) {
	withFakeADCTokenSource(t, func(ctx context.Context, scopes ...string) (oauth2.TokenSource, error) {
		return &staticTokenSource{token: "scan-token"}, nil
	})
	srv, seen := fakeModelGarden(t, "google", [][]string{
		{"gemini-2.5-pro", "text-embedding-005", "imagen-3.0"}, // page 0 (mixed catalog)
		{"gemini-2.5-flash", "gemma-3-27b-it", "veo-2.0"},      // page 1
	})
	withModelGardenBase(t, srv.URL)

	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType:  "vertex",
		Project:  "my-proj",
		Location: "us-central1",
		Model:    "google/gemini-2.5-flash",
	})
	models, err := conn.ListModels()
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}

	got := map[string]bool{}
	for _, m := range models {
		got[m.ID] = true
	}
	// gemini/gemma kept and prefixed with google/; embedding/imagen/veo filtered out.
	for _, want := range []string{"google/gemini-2.5-pro", "google/gemini-2.5-flash", "google/gemma-3-27b-it"} {
		if !got[want] {
			t.Errorf("missing expected model %q in %v", want, models)
		}
	}
	for _, unwanted := range []string{"text-embedding-005", "google/text-embedding-005", "imagen-3.0", "veo-2.0"} {
		if got[unwanted] {
			t.Errorf("non-chat model %q should have been filtered out", unwanted)
		}
	}
	// Both pages were fetched, and the quota-project header + ADC bearer were sent.
	if len(*seen) != 2 {
		t.Errorf("requests = %d, want 2 (paged)", len(*seen))
	}
	for _, r := range *seen {
		if r.Header.Get("X-Goog-User-Project") != "my-proj" {
			t.Errorf("X-Goog-User-Project = %q, want my-proj", r.Header.Get("X-Goog-User-Project"))
		}
		if r.Header.Get("Authorization") != "Bearer scan-token" {
			t.Errorf("Authorization = %q, want ADC bearer", r.Header.Get("Authorization"))
		}
	}
}

// TestVertexScanNativeBareIDs checks the native Gemini provider lists the same
// catalog but with bare ids (no google/ prefix), since the native route names the
// model bare in the URL path.
func TestVertexScanNativeBareIDs(t *testing.T) {
	withFakeADCTokenSource(t, func(ctx context.Context, scopes ...string) (oauth2.TokenSource, error) {
		return &staticTokenSource{token: "t"}, nil
	})
	srv, _ := fakeModelGarden(t, "google", [][]string{{"gemini-2.5-flash", "embedding-001"}})
	withModelGardenBase(t, srv.URL)

	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType: "vertex-native", Project: "p", Location: "global", Model: "gemini-2.5-flash",
	})
	models, err := conn.ListModels()
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "gemini-2.5-flash" {
		t.Errorf("models = %+v, want bare [gemini-2.5-flash]", models)
	}
}

// TestVertexScanAnthropicPublisher checks Claude-on-Vertex scans the anthropic
// publisher and keeps claude* ids bare.
func TestVertexScanAnthropicPublisher(t *testing.T) {
	withFakeADCTokenSource(t, func(ctx context.Context, scopes ...string) (oauth2.TokenSource, error) {
		return &staticTokenSource{token: "t"}, nil
	})
	srv, _ := fakeModelGarden(t, "anthropic", [][]string{{"claude-opus-4-8", "claude-sonnet-4-6"}})
	withModelGardenBase(t, srv.URL)

	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType: "vertex-anthropic", Project: "p", Location: "global", Model: "claude-opus-4-8",
	})
	models, err := conn.ListModels()
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 || models[0].ID != "claude-opus-4-8" {
		t.Errorf("models = %+v, want [claude-opus-4-8 claude-sonnet-4-6]", models)
	}
}

// TestVertexScanRequiresProject verifies a clear error when the model has no
// project (the quota header can't be set, so listing can't run).
func TestVertexScanRequiresProject(t *testing.T) {
	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType: "vertex", Endpoint: "https://example.test", Model: "google/gemini-2.5-flash",
	})
	_, err := conn.ListModels()
	if err == nil || !strings.Contains(err.Error(), "project is required") {
		t.Fatalf("err = %v, want a 'project is required' error", err)
	}
}
