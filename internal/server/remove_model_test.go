package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Issue #509: DELETE /api/models/:name removes a configured model. The policy is
// enforced in core (gogent.RemoveModel); the server (modelsSvc.Delete) only
// translates the sentinel errors to HTTP status — 404 for unknown, 409 for the
// blocked cases (default-while-others, in-use). These mirror models_create_test.go
// (POST /models): human-scoped, status-mapped, and the existing GET/POST/PUT/scan
// routes must keep working alongside the new DELETE.

// removeServerIssue509 stands up a loopback (human-scoped) /api server over a fresh
// core seeded with the built-in model list (default "local-lan"). The model
// endpoints only read/mutate config + the in-memory session map, so no live model
// provider is exercised. srv.g is the live core for direct setup/assertion.
func removeServerIssue509(t *testing.T) *Server {
	t.Helper()
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	return srv
}

// removePickModels returns the default model name and a non-default model name from
// the live core, so the tests don't hard-code the seeded set.
func removePickModels(t *testing.T, srv *Server) (def, other string) {
	t.Helper()
	def = srv.g.DefaultModelName()
	for _, m := range srv.g.Models() {
		if m.Name != def {
			return def, m.Name
		}
	}
	t.Fatalf("need a non-default model alongside the default %q to test the block", def)
	return
}

func TestDeleteModelSuccess(t *testing.T) {
	srv := removeServerIssue509(t)
	_, other := removePickModels(t, srv)

	rec := serveOne(t, srv, loopbackReq(http.MethodDelete, "/api/models/"+other, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE %q status = %d, want 200; body=%s", other, rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode delete response: %v (body=%s)", err, rec.Body.String())
	}
	if resp["removed"] != other {
		t.Errorf("response removed = %v, want %q", resp["removed"], other)
	}
	// The model is gone from the core and from GET /models.
	if modelExists(t, srv, other) {
		t.Fatal("deleted model still listed by GET /models")
	}
}

func TestDeleteModelUnknownReturns404(t *testing.T) {
	srv := removeServerIssue509(t)
	rec := serveOne(t, srv, loopbackReq(http.MethodDelete, "/api/models/never-existed", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE unknown status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestDeleteModelDefaultWhileOthersReturns409(t *testing.T) {
	srv := removeServerIssue509(t)
	def, _ := removePickModels(t, srv)
	before := srv.g.Models()

	rec := serveOne(t, srv, loopbackReq(http.MethodDelete, "/api/models/"+def, nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("DELETE default status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	// The 409 body carries the reason so the TUI can show it ("set another default first").
	if rec.Body.Len() == 0 {
		t.Error("409 body is empty; the block reason should be passed through")
	}
	// Nothing changed.
	if got := srv.g.DefaultModelName(); got != def {
		t.Fatalf("default changed to %q on a blocked delete; want %q", got, def)
	}
	if len(srv.g.Models()) != len(before) {
		t.Fatal("a blocked default delete must not change the model count")
	}
}

func TestDeleteModelInUseReturns409(t *testing.T) {
	srv := removeServerIssue509(t)
	_, other := removePickModels(t, srv)
	// A live session mid-turn on "other" (a non-default, so the 409 is attributable
	// to in-use, not to the default).
	us := srv.g.NewSession("sess-1")
	us.SetPrimaryModel(other)

	rec := serveOne(t, srv, loopbackReq(http.MethodDelete, "/api/models/"+other, nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("DELETE in-use status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !modelExists(t, srv, other) {
		t.Fatal("an in-use model must not be deleted")
	}
}

func TestDeleteModelLastSucceedsAndClearsDefault(t *testing.T) {
	srv := removeServerIssue509(t)
	def := srv.g.DefaultModelName()
	// Tear down every non-default model via the core until only the default remains.
	for _, m := range srv.g.Models() {
		if m.Name != def {
			if err := srv.g.RemoveModel(m.Name); err != nil {
				t.Fatalf("setup RemoveModel(%q): %v", m.Name, err)
			}
		}
	}
	if len(srv.g.Models()) != 1 {
		t.Fatalf("setup left %d models, want exactly the default", len(srv.g.Models()))
	}

	// DELETE the last (default) model over HTTP — allowed; default clears; empty list.
	rec := serveOne(t, srv, loopbackReq(http.MethodDelete, "/api/models/"+def, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE last default status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(srv.g.Models()) != 0 {
		t.Fatalf("models after deleting the last = %v, want empty", srv.g.Models())
	}
	if got := srv.g.DefaultModelName(); got != "" {
		t.Fatalf("default after deleting the last = %q, want empty", got)
	}
}

// TestDeleteModelRequiresHuman mirrors TestCreateModelRequiresHuman: DELETE /models
// is gated on the human scope, so an anonymous remote caller is rejected and the
// entry is not removed.
func TestDeleteModelRequiresHuman(t *testing.T) {
	srv := removeServerIssue509(t)
	_, other := removePickModels(t, srv)

	r := httptest.NewRequest(http.MethodDelete, "/api/models/"+other, nil)
	r.RemoteAddr = "10.0.0.5:1" // non-loopback, no credential
	rec := serveOne(t, srv, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous DELETE status = %d, want 401", rec.Code)
	}
	if !modelExists(t, srv, other) {
		t.Fatal("an anonymous delete must not remove the model")
	}
}

// TestDeleteModelLeavesOtherModelRoutesIntact is the no-regression guard: adding
// the DELETE route must not disturb GET/POST/PUT on /models. A quick round-trip
// proves the sibling handlers still resolve alongside the new DELETE.
func TestDeleteModelLeavesOtherModelRoutesIntact(t *testing.T) {
	srv := removeServerIssue509(t)

	// GET still works.
	if rec := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/models", nil)); rec.Code != http.StatusOK {
		t.Fatalf("GET /models status = %d after adding DELETE", rec.Code)
	}
	// POST still works (Create).
	body := modelCreateBody(t, "routecheck")
	if rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/models", bytes.NewReader(body))); rec.Code != http.StatusOK {
		t.Fatalf("POST /models status = %d; body=%s", rec.Code, rec.Body.String())
	}
	// PUT on the created model still works (UpdateModel matches on the path name).
	if rec := serveOne(t, srv, loopbackReq(http.MethodPut, "/api/models/routecheck", bytes.NewReader(body))); rec.Code != http.StatusOK {
		t.Fatalf("PUT /models/routecheck status = %d; body=%s", rec.Code, rec.Body.String())
	}
}

// modelExists reports whether GET /models lists a model with the given name.
func modelExists(t *testing.T, srv *Server, name string) bool {
	rec := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/models", nil))
	return rec.Code == http.StatusOK && bytes.Contains(rec.Body.Bytes(), []byte(`"`+name+`"`))
}
