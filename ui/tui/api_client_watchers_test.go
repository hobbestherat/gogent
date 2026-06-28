package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gogent/internal/config"
)

// Issue #572: APIClient watcher methods. These assert the wire shape (method,
// path, :id escaping, body, auth) against the existing daemon /api/watchers
// surface, that the response decodes into WatcherDTO, and that non-2xx (404/409/
// 500) surface as errors so the handler can show them in the dialog/echoCommand.

func mustWatcherClient(t *testing.T, srv *httptest.Server) *APIClient {
	t.Helper()
	c, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	return c
}

func TestAPIClientListWatchersFree(t *testing.T) {
	var gotMethod, gotPath, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery, gotAuth = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]WatcherDTO{
			{ID: "w1", Name: "emailer", Kind: "free", Target: "free", Task: "send", Schedule: "every 5m", Enabled: true, Status: "idle"},
		})
	}))
	defer srv.Close()

	out, err := mustWatcherClient(t, srv).ListWatchers("")
	if err != nil {
		t.Fatalf("ListWatchers: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/api/watchers" {
		t.Errorf("request = %s %s, want GET /api/watchers", gotMethod, gotPath)
	}
	if gotQuery != "" {
		t.Errorf("free-list query = %q, want empty (no session_id)", gotQuery)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q, want Bearer tok", gotAuth)
	}
	if len(out) != 1 || out[0].ID != "w1" || out[0].Kind != "free" {
		t.Errorf("decoded DTO = %+v, want one free watcher w1", out)
	}
}

func TestAPIClientListWatchersScopedBySession(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]WatcherDTO{})
	}))
	defer srv.Close()

	if _, err := mustWatcherClient(t, srv).ListWatchers("sess-1"); err != nil {
		t.Fatalf("ListWatchers: %v", err)
	}
	// url.Values.Encode sorts keys; with one key the raw query is exactly this.
	if gotQuery != "session_id=sess-1" {
		t.Errorf("scoped query = %q, want session_id=sess-1", gotQuery)
	}
}

func TestAPIClientListWatchersEmptyArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	out, err := mustWatcherClient(t, srv).ListWatchers("")
	if err != nil {
		t.Fatalf("ListWatchers on []: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("len = %d, want 0", len(out))
	}
}

func TestAPIClientListWatchersServerNull(t *testing.T) {
	// A daemon with no watchers may serialise null; the decode must not error and
	// the caller must observe an empty (nil) slice, never a panic.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("null"))
	}))
	defer srv.Close()

	out, err := mustWatcherClient(t, srv).ListWatchers("")
	if err != nil {
		t.Fatalf("ListWatchers on null: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("len = %d, want 0", len(out))
	}
}

func TestAPIClientCreateWatcherBody(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(WatcherDTO{ID: "w9", Name: "nightly", Kind: "attached", Target: "sess-1"})
	}))
	defer srv.Close()

	target := "sess-1"
	dto, err := mustWatcherClient(t, srv).CreateWatcher(WatcherCreateDTO{
		Name:            "nightly",
		Task:            "summarise",
		Model:           "claude",
		Schedule:        config.ScheduleConfig{Every: "5m"},
		Enabled:         ptrBool(true),
		ReportToSession: &target,
	})
	if err != nil {
		t.Fatalf("CreateWatcher: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/watchers" {
		t.Errorf("request = %s %s, want POST /api/watchers", gotMethod, gotPath)
	}
	if gotBody["name"] != "nightly" || gotBody["task"] != "summarise" || gotBody["model"] != "claude" {
		t.Errorf("body scalar fields = %+v", gotBody)
	}
	if gotBody["enabled"] != true {
		t.Errorf("body enabled = %v, want true (sent explicitly to mirror embedded)", gotBody["enabled"])
	}
	if gotBody["report_to_session"] != "sess-1" {
		t.Errorf("body report_to_session = %v, want sess-1 (drives attached kind)", gotBody["report_to_session"])
	}
	sched, _ := gotBody["schedule"].(map[string]any)
	if sched == nil || sched["every"] != "5m" {
		t.Errorf("body schedule = %+v, want every=5m", gotBody["schedule"])
	}
	if _, dup := gotBody["daily_at"]; dup {
		t.Errorf("body should omit empty daily_at, got %+v", sched)
	}
	if dto.ID != "w9" || dto.Kind != "attached" {
		t.Errorf("decoded response = %+v, want w9 attached", dto)
	}
}

func TestAPIClientCreateWatcherFreeOmitsReportToSession(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(WatcherDTO{ID: "w", Kind: "free"})
	}))
	defer srv.Close()

	if _, err := mustWatcherClient(t, srv).CreateWatcher(WatcherCreateDTO{
		Name: "free-w", Task: "x", Schedule: config.ScheduleConfig{Every: "10m"}, Enabled: ptrBool(true),
	}); err != nil {
		t.Fatalf("CreateWatcher: %v", err)
	}
	if _, present := gotBody["report_to_session"]; present {
		t.Errorf("free create must omit report_to_session, body = %+v", gotBody)
	}
}

func TestAPIClientSetWatcherEnabled(t *testing.T) {
	for _, on := range []bool{true, false} {
		var gotMethod, gotPath string
		var gotBody map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(http.StatusOK)
		}))
		c := mustWatcherClient(t, srv)
		if err := c.SetWatcherEnabled("emailer", on); err != nil {
			t.Fatalf("SetWatcherEnabled(%v): %v", on, err)
		}
		srv.Close()
		if gotMethod != http.MethodPut || gotPath != "/api/watchers/emailer/enabled" {
			t.Errorf("request = %s %s, want PUT /api/watchers/emailer/enabled", gotMethod, gotPath)
		}
		if gotBody["enabled"] != on {
			t.Errorf("body enabled = %v, want %v", gotBody["enabled"], on)
		}
	}
}

func TestAPIClientRunStopWatcher(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(c *APIClient) error
		suf  string
	}{
		{"Run", func(c *APIClient) error { return c.RunWatcher("emailer") }, "/run"},
		{"Stop", func(c *APIClient) error { return c.StopWatcher("emailer") }, "/stop"},
	} {
		var gotMethod, gotPath string
		var hadBody bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			hadBody = r.ContentLength > 0
			w.WriteHeader(http.StatusOK)
		}))
		c := mustWatcherClient(t, srv)
		if err := tc.fn(c); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		srv.Close()
		if gotMethod != http.MethodPost || gotPath != "/api/watchers/emailer"+tc.suf {
			t.Errorf("%s request = %s %s, want POST /api/watchers/emailer%s", tc.name, gotMethod, gotPath, tc.suf)
		}
		if hadBody {
			t.Errorf("%s should send no body", tc.name)
		}
	}
}

func TestAPIClientDeleteWatcher(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := mustWatcherClient(t, srv).DeleteWatcher("emailer"); err != nil {
		t.Fatalf("DeleteWatcher: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/watchers/emailer" {
		t.Errorf("request = %s %s, want DELETE /api/watchers/emailer", gotMethod, gotPath)
	}
}

// TestAPIClientWatcherNameWithPathCharsEscapes asserts :id segments with spaces
// (and other reserved chars) are PathEscape-d so the URL is not malformed and the
// daemon receives the intended name. The handler accepts id-or-name, so a name
// like "my watcher" must survive the round trip.
func TestAPIClientWatcherNameWithPathCharsEscapes(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := mustWatcherClient(t, srv).SetWatcherEnabled("my watcher", true); err != nil {
		t.Fatalf("SetWatcherEnabled: %v", err)
	}
	// The decoded path must contain the literal space (the escape round-trips);
	// an unescaped space would have corrupted the request line.
	if gotPath != "/api/watchers/my watcher/enabled" {
		t.Errorf("path = %q, want /api/watchers/my watcher/enabled", gotPath)
	}
}

func TestAPIClientWatcherNotFoundSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()
	if err := mustWatcherClient(t, srv).SetWatcherEnabled("ghost", true); err == nil {
		t.Fatal("SetWatcherEnabled on 404 = nil, want error")
	}
}

func TestAPIClientWatcherAmbiguousSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "ambiguous name", http.StatusConflict)
	}))
	defer srv.Close()
	if err := mustWatcherClient(t, srv).RunWatcher("dup"); err == nil {
		t.Fatal("RunWatcher on 409 = nil, want error")
	}
}

func TestAPIClientWatcherFeatureFlagOffSurfacesError(t *testing.T) {
	// The daemon returns 404 when Experimental.Watchers is off (per-handler gate);
	// the client must surface it so ListWatchers can degrade to "no watchers".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "watchers are not enabled", http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := mustWatcherClient(t, srv).ListWatchers(""); err == nil {
		t.Fatal("ListWatchers on 404 (flag off) = nil, want error")
	}
}

func TestAPIClientWatcherServerErrorSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if err := mustWatcherClient(t, srv).DeleteWatcher("w"); err == nil {
		t.Fatal("DeleteWatcher on 500 = nil, want error")
	}
}

// ptrBool is a tiny helper for the *bool Enabled field of WatcherCreateDTO.
func ptrBool(b bool) *bool { return &b }
