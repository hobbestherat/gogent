package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gogent/internal/gogent"
)

// Regression coverage for issue #507: the daemon's default-model name is exposed as a
// daemon-owned field on the existing /api/settings route (settingsView.DefaultModel),
// backed by g.DefaultModelName()/g.SetDefaultModel(), exactly as budget is.
//
// Design criteria under test:
//   (1) goal — default_model round-trips over HTTP against the live core; an invalid
//       name surfaces as a 400 (user-correctable), not a 500; an empty/omitted field
//       never clears the daemon's default (older clients stay backward-compatible).
//   (2) usability — a bad name fails BEFORE any other field is persisted (validate-first
//       ordering), so a full PUT carrying a changed budget + an invalid model leaves no
//       partial write; an idempotent same-value PUT is a clean success.
//   (3) no regressions — the /api/settings/notifications endpoint is independent of
//       default_model; adding the field is backward-compatible JSON.
//   (4) holistic — the change is confined to the server's settings Get/Set on the
//       existing route (no new route), reusing budget's surface.

// newDefaultModelServerIssue507 builds a loopback (human-scoped) /api server over a
// fresh core that uses the built-in default model list (local-lan/qemu-host/localhost/
// groq-free, default "local-lan"). The settings endpoint only reads/sets the
// default-model NAME — SetDefaultModel validates it against the configured list, not a
// live backend — so no model provider is needed. The returned server's .g is the live
// core, for direct assertion alongside the HTTP response.
func newDefaultModelServerIssue507(t *testing.T) *Server {
	t.Helper()
	g := gogent.NewGogent(t.TempDir())
	return NewServer(g, Options{})
}

func getSettingsIssue507(t *testing.T, srv *Server) (settingsView, int) {
	t.Helper()
	rec := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/settings", nil))
	var v settingsView
	if rec.Code == http.StatusOK {
		if err := json.NewDecoder(rec.Body).Decode(&v); err != nil {
			t.Fatalf("decode GET /api/settings: %v (body=%s)", err, rec.Body.String())
		}
	}
	return v, rec.Code
}

func putSettingsIssue507(t *testing.T, srv *Server, body string) (settingsView, int, string) {
	t.Helper()
	rec := serveOne(t, srv, loopbackReq(http.MethodPut, "/api/settings", strings.NewReader(body)))
	var v settingsView
	if rec.Code == http.StatusOK {
		_ = json.NewDecoder(rec.Body).Decode(&v)
	}
	return v, rec.Code, rec.Body.String()
}

// TestSettingsDefaultModelIssue507GetReflectsCoreDefault: GET /api/settings surfaces the
// core's default-model name under the exact JSON key "default_model".
func TestSettingsDefaultModelIssue507GetReflectsCoreDefault(t *testing.T) {
	srv := newDefaultModelServerIssue507(t)

	v, code := getSettingsIssue507(t, srv)
	if code != http.StatusOK {
		t.Fatalf("GET /api/settings status = %d, want 200", code)
	}
	if v.DefaultModel != srv.g.DefaultModelName() {
		t.Fatalf("GET default_model = %q, want core default %q", v.DefaultModel, srv.g.DefaultModelName())
	}
	if v.DefaultModel != "local-lan" {
		t.Fatalf("expected the built-in default %q, got %q", "local-lan", v.DefaultModel)
	}

	// The JSON key must be exactly "default_model" — a tag typo would silently break the
	// round-trip. Assert against the raw payload rather than the decoded struct.
	rec := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/settings", nil))
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal GET body: %v", err)
	}
	if _, ok := raw["default_model"]; !ok {
		t.Fatalf("response JSON has no default_model key (tag typo?); keys present: %v", keysIssue507(raw))
	}
}

func keysIssue507(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestSettingsDefaultModelIssue507PutValidUpdatesCore: a PUT with a known-valid model
// name updates the live core's default and echoes it back.
func TestSettingsDefaultModelIssue507PutValidUpdatesCore(t *testing.T) {
	srv := newDefaultModelServerIssue507(t)
	// "qemu-host" is a built-in model name, so it is a valid default.
	v, code, body := putSettingsIssue507(t, srv, `{"default_model":"qemu-host"}`)
	if code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", code, body)
	}
	if v.DefaultModel != "qemu-host" {
		t.Fatalf("PUT response default_model = %q, want qemu-host", v.DefaultModel)
	}
	if got := srv.g.DefaultModelName(); got != "qemu-host" {
		t.Fatalf("core DefaultModelName = %q, want qemu-host (PUT did not update the core)", got)
	}
}

// TestSettingsDefaultModelIssue507PutUnknownReturns400AndLeavesDefault: an unknown model
// name is user-correctable input, so it must fail with a 400 (NOT the 500 webapi would
// assign to a bare error) and leave the default unchanged. This is the assertion that
// catches the bare-error→500 trap.
func TestSettingsDefaultModelIssue507PutUnknownReturns400AndLeavesDefault(t *testing.T) {
	srv := newDefaultModelServerIssue507(t)
	before := srv.g.DefaultModelName()

	_, code, body := putSettingsIssue507(t, srv, `{"default_model":"does-not-exist"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("PUT unknown model status = %d, want exactly 400 (a bare error would yield 500); body=%s", code, body)
	}
	if got := srv.g.DefaultModelName(); got != before {
		t.Fatalf("default changed to %q after a rejected PUT; want unchanged %q", got, before)
	}
}

// TestSettingsDefaultModelIssue507BadModelDoesNotPartialWriteBudget: the validate-first
// ordering. A full PUT carrying BOTH a changed budget AND an invalid default model must
// 400 AND leave the budget untouched — proving no partial write. (If default_model were
// applied after the other setters, the budget would already be persisted.)
func TestSettingsDefaultModelIssue507BadModelDoesNotPartialWriteBudget(t *testing.T) {
	srv := newDefaultModelServerIssue507(t)
	before, _ := getSettingsIssue507(t, srv)

	_, code, _ := putSettingsIssue507(t, srv, `{"default_model":"nope","budget":{"token_budget":99999}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (invalid model must fail the whole PUT before any setter runs)", code)
	}

	after, _ := getSettingsIssue507(t, srv)
	if after.Budget.TokenBudget == 99999 {
		t.Fatalf("budget was partially written (token_budget=99999) despite the 400; validate-first ordering is broken")
	}
	if after.Budget.TokenBudget != before.Budget.TokenBudget {
		t.Fatalf("budget changed from %d to %d on a rejected PUT; want unchanged", before.Budget.TokenBudget, after.Budget.TokenBudget)
	}
	if after.DefaultModel != before.DefaultModel {
		t.Fatalf("default_model changed on a rejected PUT; want unchanged")
	}
}

// TestSettingsDefaultModelIssue507EmptyInPutLeavesDefaultUnchanged: an older client that
// omits default_model (decoded as "") must never clear the daemon's default.
func TestSettingsDefaultModelIssue507EmptyInPutLeavesDefaultUnchanged(t *testing.T) {
	srv := newDefaultModelServerIssue507(t)
	before := srv.g.DefaultModelName()

	// An otherwise-meaningful PUT (changed budget) that omits default_model entirely.
	_, code, body := putSettingsIssue507(t, srv, `{"budget":{"token_budget":12345}}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	if got := srv.g.DefaultModelName(); got != before {
		t.Fatalf("empty/omitted default_model cleared the default to %q; want %q", got, before)
	}
}

// TestSettingsDefaultModelIssue507PutSameValueIsNoop: setting the default to the value
// it already holds is an idempotent success (the "!= current" guard must not error and
// must not needlessly re-validate/persist).
func TestSettingsDefaultModelIssue507PutSameValueIsNoop(t *testing.T) {
	srv := newDefaultModelServerIssue507(t)
	cur := srv.g.DefaultModelName() // "local-lan"

	_, code, body := putSettingsIssue507(t, srv, `{"default_model":"local-lan"}`)
	if code != http.StatusOK {
		t.Fatalf("PUT same-value status = %d, want 200; body=%s", code, body)
	}
	if got := srv.g.DefaultModelName(); got != cur {
		t.Fatalf("default changed to %q on an idempotent same-value PUT; want %q", got, cur)
	}
}

// TestSettingsDefaultModelIssue507RoundTripIsLossless: a GET→PUT round-trip preserves
// every field (default_model included), so the attached client's read-modify-write is
// lossless — it cannot accidentally clobber an unrelated setting.
func TestSettingsDefaultModelIssue507RoundTripIsLossless(t *testing.T) {
	srv := newDefaultModelServerIssue507(t)
	// Establish a non-default state worth round-tripping.
	if _, code, _ := putSettingsIssue507(t, srv, `{"default_model":"qemu-host","budget":{"token_budget":777}}`); code != http.StatusOK {
		t.Fatalf("seed PUT status = %d, want 200", code)
	}
	first, _ := getSettingsIssue507(t, srv)

	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, code, body := putSettingsIssue507(t, srv, string(raw)); code != http.StatusOK {
		t.Fatalf("round-trip PUT status = %d, want 200; body=%s", code, body)
	}

	second, _ := getSettingsIssue507(t, srv)
	if second.DefaultModel != first.DefaultModel {
		t.Fatalf("default_model not preserved by round-trip: %q → %q", first.DefaultModel, second.DefaultModel)
	}
	if second.Budget.TokenBudget != first.Budget.TokenBudget {
		t.Fatalf("budget not preserved by round-trip: %d → %d", first.Budget.TokenBudget, second.Budget.TokenBudget)
	}
	if second.ReviewEdits != first.ReviewEdits {
		t.Fatalf("review_edits not preserved by round-trip: %v → %v", first.ReviewEdits, second.ReviewEdits)
	}
}

// TestSettingsDefaultModelIssue507NotificationsEndpointIndependent: the
// /api/settings/notifications route (daemon-side fallback) and the default_model field
// on /api/settings must be fully independent — neither touches the other.
func TestSettingsDefaultModelIssue507NotificationsEndpointIndependent(t *testing.T) {
	srv := newDefaultModelServerIssue507(t)
	before := srv.g.DefaultModelName()

	// Change the daemon notify block over its OWN endpoint.
	rec := serveOne(t, srv, loopbackReq(http.MethodPut, "/api/settings/notifications",
		strings.NewReader(`{"enabled":true,"bell":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT notifications status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := srv.g.DefaultModelName(); got != before {
		t.Fatalf("default_model changed after a notifications PUT: %q → %q", before, got)
	}

	// And a default-model PUT must not clear the just-set notify block.
	if _, code, body := putSettingsIssue507(t, srv, `{"default_model":"qemu-host"}`); code != http.StatusOK {
		t.Fatalf("default PUT status = %d, want 200; body=%s", code, body)
	}
	if n := srv.g.Notifications(); !n.Enabled || !n.Bell {
		t.Fatalf("notify block was altered by a default-model PUT: %+v", n)
	}
}
