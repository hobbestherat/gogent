package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"gogent/internal/permission"
)

func newWatcherTestServer(t *testing.T) *Server {
	t.Helper()
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	srv.g.GetConfig().Experimental.Watchers = true
	srv.g.GetPermissionService().AddRule(permission.Rule{
		Action:   string(permission.ActionWatcher),
		Resource: "*",
		Effect:   string(permission.EffectAllow),
	})
	srv.g.StartWatchers()
	t.Cleanup(srv.g.StopWatchers)
	return srv
}

func postWatcher(t *testing.T, srv *Server, body string) watcherView {
	t.Helper()
	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/watchers", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("create watcher status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got watcherView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid watcher JSON: %v", err)
	}
	if got.ID == "" {
		t.Fatal("created watcher has no id")
	}
	return got
}

func getWatcher(t *testing.T, srv *Server, id string) (watcherView, int, string) {
	t.Helper()
	rec := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/watchers/"+id, nil))
	var got watcherView
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("invalid watcher JSON: %v", err)
		}
	}
	return got, rec.Code, rec.Body.String()
}

func TestWatchersAPIRequiresExperimentalGate(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	rec := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/watchers", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when watchers are disabled; body=%s", rec.Code, rec.Body.String())
	}
}

func TestWatchersAPICreateListGetUpdateAndDeleteFreeRunning(t *testing.T) {
	srv := newWatcherTestServer(t)

	created := postWatcher(t, srv, `{
		"name": "daily-brief",
		"task": "Summarize today's agenda.",
		"schedule": {"every": "1h"},
		"model": "test-model"
	}`)
	if created.Name != "daily-brief" || created.Kind != "free" || created.Target != "free" {
		t.Fatalf("created watcher = %+v, want free daily-brief target free", created)
	}
	if created.Task != "Summarize today's agenda." {
		t.Fatalf("task = %q", created.Task)
	}
	if created.Schedule == "" {
		t.Fatal("schedule should be populated")
	}
	if !created.Enabled {
		t.Fatal("created watcher should default to enabled")
	}
	if created.Status != "idle" {
		t.Fatalf("status = %q, want idle", created.Status)
	}
	if created.NextFire == "" {
		t.Fatal("next_fire should be populated for an enabled watcher")
	}

	rec := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/watchers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var listed []watcherView
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("invalid watcher list JSON: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("listed watchers = %+v, want only created watcher %q", listed, created.ID)
	}

	got, code, body := getWatcher(t, srv, created.ID)
	if code != http.StatusOK {
		t.Fatalf("get by id status = %d, want 200; body=%s", code, body)
	}
	if got.Name != created.Name {
		t.Fatalf("get by id name = %q, want %q", got.Name, created.Name)
	}
	gotByName, code, body := getWatcher(t, srv, created.Name)
	if code != http.StatusOK {
		t.Fatalf("get by name status = %d, want 200; body=%s", code, body)
	}
	if gotByName.ID != created.ID {
		t.Fatalf("get by name id = %q, want %q", gotByName.ID, created.ID)
	}

	update := strings.NewReader(`{
		"name": "daily-brief-renamed",
		"task": "Summarize calendar and blockers.",
		"schedule": {"every": "2h"}
	}`)
	rec = serveOne(t, srv, loopbackReq(http.MethodPatch, "/api/watchers/"+created.ID, update))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var updated watcherView
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("invalid updated watcher JSON: %v", err)
	}
	if updated.ID != created.ID || updated.Name != "daily-brief-renamed" {
		t.Fatalf("updated watcher = %+v", updated)
	}
	if updated.Task != "Summarize calendar and blockers." {
		t.Fatalf("updated task = %q", updated.Task)
	}
	if !strings.Contains(updated.Schedule, "2h") {
		t.Fatalf("updated schedule = %q, want it to mention 2h", updated.Schedule)
	}

	rec = serveOne(t, srv, loopbackReq(http.MethodDelete, "/api/watchers/"+created.ID, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	_, code, _ = getWatcher(t, srv, created.ID)
	if code != http.StatusNotFound {
		t.Fatalf("get after delete status = %d, want 404", code)
	}
}

func TestWatchersAPICreateAttachedGetAndScopedList(t *testing.T) {
	srv := newWatcherTestServer(t)

	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/sessions", strings.NewReader(`{"title":"owner","persisted":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("create session status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var sess sessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &sess); err != nil {
		t.Fatalf("invalid session JSON: %v", err)
	}

	created := postWatcher(t, srv, `{
		"name": "attached-poller",
		"task": "Check the project and report here.",
		"schedule": {"every": "30m"},
		"report_to_session": "`+sess.ID+`"
	}`)
	if created.Kind != "attached" || created.Target != sess.ID {
		t.Fatalf("created watcher = %+v, want attached target %q", created, sess.ID)
	}

	got, code, body := getWatcher(t, srv, created.ID)
	if code != http.StatusOK {
		t.Errorf("get attached watcher status = %d, want 200; body=%s", code, body)
	} else if got.ID != created.ID || got.Kind != "attached" || got.Target != sess.ID {
		t.Fatalf("get attached watcher = %+v, want id %q attached target %q", got, created.ID, sess.ID)
	}

	rec = serveOne(t, srv, loopbackReq(http.MethodGet, "/api/watchers?session_id="+sess.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scoped list status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var scoped []watcherView
	if err := json.Unmarshal(rec.Body.Bytes(), &scoped); err != nil {
		t.Fatalf("invalid scoped watcher list JSON: %v", err)
	}
	if len(scoped) != 1 || scoped[0].ID != created.ID || scoped[0].Target != sess.ID {
		t.Errorf("scoped watchers = %+v, want attached watcher %q for %q", scoped, created.ID, sess.ID)
	}
}

func TestWatchersAPIEnableDisableRunStopAndDelete(t *testing.T) {
	srv := newWatcherTestServer(t)
	created := postWatcher(t, srv, `{
		"name": "manual",
		"task": "Run once when requested.",
		"schedule": {"every": "24h"}
	}`)

	rec := serveOne(t, srv, loopbackReq(http.MethodPut, "/api/watchers/"+created.ID+"/enabled", strings.NewReader(`{"enabled":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got, code, body := getWatcher(t, srv, created.ID)
	if code != http.StatusOK {
		t.Fatalf("get disabled status = %d, want 200; body=%s", code, body)
	}
	if got.Enabled {
		t.Fatalf("enabled = true after explicit disable: %+v", got)
	}

	rec = serveOne(t, srv, loopbackReq(http.MethodPost, "/api/watchers/"+created.ID+"/toggle", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("toggle status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	got, code, body = getWatcher(t, srv, created.ID)
	if code != http.StatusOK {
		t.Fatalf("get toggled status = %d, want 200; body=%s", code, body)
	}
	if !got.Enabled {
		t.Fatalf("enabled = false after toggle from disabled: %+v", got)
	}

	rec = serveOne(t, srv, loopbackReq(http.MethodPost, "/api/watchers/"+created.ID+"/run", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("run status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, code, body = getWatcher(t, srv, created.ID)
		if code != http.StatusOK {
			t.Fatalf("get after run status = %d, want 200; body=%s", code, body)
		}
		if got.LastRun != "" || got.LastResult != "" || got.Status == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("watcher run did not become observable before timeout: %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}

	rec = serveOne(t, srv, loopbackReq(http.MethodPost, "/api/watchers/"+created.ID+"/stop", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("stop status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	rec = serveOne(t, srv, loopbackReq(http.MethodDelete, "/api/watchers/"+created.ID, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	rec = serveOne(t, srv, loopbackReq(http.MethodPost, "/api/watchers/"+created.ID+"/run", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("run deleted watcher status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestWatchersAPIBadInputAndUnknownWatcherErrors(t *testing.T) {
	srv := newWatcherTestServer(t)

	tests := []struct {
		name string
		body string
	}{
		{name: "missing name", body: `{"task":"t","schedule":{"every":"1h"}}`},
		{name: "missing task", body: `{"name":"w","schedule":{"every":"1h"}}`},
		{name: "invalid schedule", body: `{"name":"w","task":"t","schedule":{"every":"not-a-duration"}}`},
		{name: "unknown attached target", body: `{"name":"w","task":"t","schedule":{"every":"1h"},"report_to_session":"missing-session"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/watchers", strings.NewReader(tc.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
		})
	}

	rec := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/watchers/no-such-watcher", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get unknown status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	rec = serveOne(t, srv, loopbackReq(http.MethodDelete, "/api/watchers/no-such-watcher", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	rec = serveOne(t, srv, loopbackReq(http.MethodPatch, "/api/watchers/no-such-watcher", strings.NewReader(`{"task":"x"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("patch unknown status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
