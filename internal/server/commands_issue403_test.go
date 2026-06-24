package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func postCommandIssue403(t *testing.T, srv *Server, body string) (commandView, int, string) {
	t.Helper()
	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/commands", strings.NewReader(body)))
	var got commandView
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("invalid command JSON: %v", err)
		}
	}
	return got, rec.Code, rec.Body.String()
}

func getCommandIssue403(t *testing.T, srv *Server, name string) (commandView, int, string) {
	t.Helper()
	rec := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/commands/"+name, nil))
	var got commandView
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("invalid command JSON: %v", err)
		}
	}
	return got, rec.Code, rec.Body.String()
}

func TestIssue403CommandsAPICreateListUpdateHistoryRestoreDelete(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})

	created, code, body := postCommandIssue403(t, srv, `{
		"name": "review-change",
		"description": "Review one target",
		"parameters": [
			{"name": "target", "description": "Thing to review", "required": true},
			{"name": "depth", "default": "quick"}
		],
		"template": "Review $target with $depth detail",
		"model": "fast",
		"agent": "reviewer",
		"subtask": true
	}`)
	if code != http.StatusOK {
		t.Fatalf("create status = %d, want 200; body=%s", code, body)
	}
	if created.Name != "review-change" || created.Version != 1 || len(created.Versions) != 1 {
		t.Fatalf("created command = %+v, want name review-change version 1 with one snapshot", created)
	}
	if created.Versions[0].Template != created.Template || created.Versions[0].Model != "fast" ||
		created.Versions[0].Agent != "reviewer" || !created.Versions[0].Subtask {
		t.Fatalf("create snapshot does not mirror top-level fields: %+v", created.Versions[0])
	}
	if _, err := time.Parse(time.RFC3339, created.Versions[0].SavedAt); err != nil {
		t.Fatalf("saved_at is not RFC3339: %q: %v", created.Versions[0].SavedAt, err)
	}

	rec := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/commands", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var listed []commandView
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("invalid command list JSON: %v", err)
	}
	if len(listed) != 1 || listed[0].Name != "review-change" {
		t.Fatalf("listed commands = %+v, want only review-change", listed)
	}

	got, code, body := getCommandIssue403(t, srv, "review-change")
	if code != http.StatusOK {
		t.Fatalf("get status = %d, want 200; body=%s", code, body)
	}
	if got.Template != created.Template || got.Parameters[0].Name != "target" {
		t.Fatalf("get command = %+v, want created content", got)
	}

	updateBody := strings.NewReader(`{
		"name": "smuggled-rename",
		"description": "Updated",
		"parameters": [{"name": "target", "required": true}],
		"template": "Deep review $target",
		"model": "slow"
	}`)
	rec = serveOne(t, srv, loopbackReq(http.MethodPut, "/api/commands/review-change", updateBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var updated commandView
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("invalid updated command JSON: %v", err)
	}
	if updated.Name != "review-change" {
		t.Fatalf("path name must be authoritative, got renamed command %+v", updated)
	}
	if updated.Version != 2 || len(updated.Versions) != 2 || updated.Template != "Deep review $target" {
		t.Fatalf("updated command = %+v, want version 2 with new template", updated)
	}

	rec = serveOne(t, srv, loopbackReq(http.MethodGet, "/api/commands/review-change/history", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("history status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var history []commandVersionView
	if err := json.Unmarshal(rec.Body.Bytes(), &history); err != nil {
		t.Fatalf("invalid history JSON: %v", err)
	}
	if len(history) != 2 || history[0].Version != 1 || history[1].Version != 2 {
		t.Fatalf("history = %+v, want versions 1 and 2", history)
	}

	rec = serveOne(t, srv, loopbackReq(http.MethodPost, "/api/commands/review-change/restore", strings.NewReader(`{"version":1}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("restore status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var restored commandView
	if err := json.Unmarshal(rec.Body.Bytes(), &restored); err != nil {
		t.Fatalf("invalid restored command JSON: %v", err)
	}
	if restored.Version != 3 || len(restored.Versions) != 3 || restored.Template != created.Template ||
		restored.Model != "fast" || restored.Agent != "reviewer" || !restored.Subtask {
		t.Fatalf("restored command = %+v, want version 3 with version 1 content", restored)
	}

	rec = serveOne(t, srv, loopbackReq(http.MethodDelete, "/api/commands/review-change", nil))
	if rec.Code < 200 || rec.Code >= 300 {
		t.Fatalf("delete status = %d, want 2xx; body=%s", rec.Code, rec.Body.String())
	}
	_, code, _ = getCommandIssue403(t, srv, "review-change")
	if code != http.StatusNotFound {
		t.Fatalf("get deleted status = %d, want 404", code)
	}
}

func TestIssue403CommandsAPIRejectsCollisionsAndBadRequests(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	if _, code, body := postCommandIssue403(t, srv, `{"name":"help","template":"shadow"}`); code != http.StatusBadRequest {
		t.Fatalf("built-in collision status = %d, want 400; body=%s", code, body)
	}
	if _, code, body := postCommandIssue403(t, srv, `{"name":"review","template":"x"}`); code != http.StatusOK {
		t.Fatalf("seed create status = %d, want 200; body=%s", code, body)
	}
	if _, code, body := postCommandIssue403(t, srv, `{"name":"review","template":"y"}`); code != http.StatusConflict {
		t.Fatalf("duplicate custom status = %d, want 409; body=%s", code, body)
	}
	if _, code, body := postCommandIssue403(t, srv, `{"name":"BadName","template":"x"}`); code != http.StatusBadRequest {
		t.Fatalf("invalid name status = %d, want 400; body=%s", code, body)
	}
	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/commands/review/restore", strings.NewReader(`{"version":99}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("restore missing version status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
