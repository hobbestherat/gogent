package tool

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gogent/internal/permission"
)

// allowNetwork returns a permission service that allows all network access.
func allowNetwork() *permission.Service {
	s := permission.New("")
	s.AddRule(permission.Rule{Action: string(permission.ActionNetwork), Resource: "*", Effect: "allow"})
	return s
}

func webFetchTool(t *testing.T, tr *ToolRegistry) *Tool {
	t.Helper()
	tr.RegisterWebFetchTool()
	tool := tr.Get("web_fetch")
	if tool == nil {
		t.Fatal("web_fetch not registered")
	}
	return tool
}

func TestWebFetchTool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<title>Doc</title><body><h1>Heading</h1><p>Hello world.</p></body>"))
	}))
	defer srv.Close()

	tr := NewToolRegistry()
	tr.Permission = allowNetwork()
	tool := webFetchTool(t, tr)

	res, err := tool.Execute(map[string]interface{}{"url": srv.URL}, ToolContext{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	out, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("result is %T, want map", res)
	}
	if out["title"] != "Doc" {
		t.Errorf("title = %v, want Doc", out["title"])
	}
	md, _ := out["markdown"].(string)
	if !strings.Contains(md, "# Heading") || !strings.Contains(md, "Hello world.") {
		t.Errorf("markdown missing expected content: %q", md)
	}
}

func TestWebFetchToolGatesPermission(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be reached when permission is denied")
	}))
	defer srv.Close()

	tr := NewToolRegistry()
	tr.Permission = permission.New("") // no prompter and no rule: network "ask" -> deny
	tool := webFetchTool(t, tr)

	if _, err := tool.Execute(map[string]interface{}{"url": srv.URL}, ToolContext{}); err == nil {
		t.Fatal("expected permission denial, got nil error")
	}
}

func TestWebFetchToolMaxLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<body><p>" + strings.Repeat("a", 500) + "</p></body>"))
	}))
	defer srv.Close()

	tr := NewToolRegistry()
	tr.Permission = allowNetwork()
	tool := webFetchTool(t, tr)

	res, err := tool.Execute(map[string]interface{}{"url": srv.URL, "max_length": float64(50)}, ToolContext{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	out := res.(map[string]interface{})
	if md := out["markdown"].(string); len(md) != 50 {
		t.Errorf("markdown length = %d, want 50", len(md))
	}
	if out["truncated"] != true {
		t.Errorf("truncated = %v, want true", out["truncated"])
	}
}

func TestWebFetchToolRejectsBadURL(t *testing.T) {
	tr := NewToolRegistry()
	tr.Permission = allowNetwork()
	tool := webFetchTool(t, tr)

	for _, bad := range []interface{}{"", "ftp://x", "not a url", "/local"} {
		if _, err := tool.Execute(map[string]interface{}{"url": bad}, ToolContext{}); err == nil {
			t.Errorf("Execute(url=%v) expected error, got nil", bad)
		}
	}
}
