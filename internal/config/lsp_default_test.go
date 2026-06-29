package config

import "testing"

// TestGetDefaultConfigIncludesGopls pins the headline "zero-config Go support when
// gopls is on PATH" promise (LSP design §2, §13): a freshly generated default
// config carries a gopls entry routing .go files, launched as `gopls`, rooted by
// go.work/go.mod.
func TestGetDefaultConfigIncludesGopls(t *testing.T) {
	servers := GetDefaultConfig().LSPServers
	var gopls *LSPServerConfig
	for i := range servers {
		if servers[i].Name == "gopls" {
			gopls = &servers[i]
			break
		}
	}
	if gopls == nil {
		t.Fatalf("default config has no gopls LSP server: %+v", servers)
	}
	if gopls.Command != "gopls" {
		t.Errorf("gopls command = %q, want %q", gopls.Command, "gopls")
	}
	if !contains(gopls.Extensions, ".go") {
		t.Errorf("gopls extensions = %v, want to include .go", gopls.Extensions)
	}
	for _, marker := range []string{"go.work", "go.mod"} {
		if !contains(gopls.RootMarkers, marker) {
			t.Errorf("gopls root markers = %v, want to include %q", gopls.RootMarkers, marker)
		}
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
