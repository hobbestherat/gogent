package gogent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildRepoMapExtractsDeclarations(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "a.go", `package p

const Answer = 42

var Greeting = "hi"

type Widget struct{ W int }

type Stringer interface{ String() string }

type Alias = int

func Foo(x int) string { return "" }

func (w Widget) Area() int { return w.W }
`)

	out := buildRepoMap(ws)

	for _, want := range []string{
		"## Repo map",
		"### a.go",
		"const Answer",
		"var Greeting",
		"type Widget struct",
		"type Stringer interface",
		"type Alias",
		"func Foo(x int) string",
		"func (w Widget) Area() int",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("repo map missing %q\n---\n%s", want, out)
		}
	}
}

func TestBuildRepoMapRanksReferencedFilesFirst(t *testing.T) {
	ws := t.TempDir()
	// core.go declares a symbol referenced many times from app.go.
	writeFile(t, ws, "core.go", "package p\n\nfunc Core() {}\n")
	// lonely.go declares a symbol that is never referenced.
	writeFile(t, ws, "lonely.go", "package p\n\nfunc Lonely() {}\n")
	writeFile(t, ws, "app.go", "package p\n\nfunc App() { Core(); Core(); Core() }\n")

	out := buildRepoMap(ws)

	core := strings.Index(out, "### core.go")
	lonely := strings.Index(out, "### lonely.go")
	if core == -1 || lonely == -1 {
		t.Fatalf("expected both files in map:\n%s", out)
	}
	if core > lonely {
		t.Fatalf("expected referenced core.go to rank before lonely.go:\n%s", out)
	}
}

func TestBuildRepoMapSkipsHiddenAndVendorDirs(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "main.go", "package p\n\nfunc Main() {}\n")
	writeFile(t, ws, "vendor/lib.go", "package lib\n\nfunc Vendored() {}\n")
	writeFile(t, ws, ".hidden/secret.go", "package h\n\nfunc Secret() {}\n")

	out := buildRepoMap(ws)
	if !strings.Contains(out, "func Main()") {
		t.Fatalf("expected workspace symbol, got:\n%s", out)
	}
	if strings.Contains(out, "Vendored") || strings.Contains(out, "Secret") {
		t.Fatalf("vendor/hidden dirs should be skipped:\n%s", out)
	}
}

func TestBuildRepoMapEmptyWorkspace(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "readme.txt", "no go here")
	if got := buildRepoMap(ws); got != "" {
		t.Fatalf("expected empty map for non-Go workspace, got %q", got)
	}
}

func TestRenderRepoMapTruncates(t *testing.T) {
	var syms []repoSymbol
	// Build more declarations than fit in the byte cap.
	long := strings.Repeat("X", 200)
	for i := 0; i < 500; i++ {
		syms = append(syms, repoSymbol{
			file: "f" + strings.Repeat("0", 1) + string(rune('a'+i%26)) + ".go",
			line: i,
			name: "Sym",
			sig:  "func Sym" + long + "()",
			rank: 1,
		})
	}
	out := renderRepoMap(syms)
	if len(out) > maxRepoMapBytes+200 {
		t.Fatalf("rendered map exceeded cap: %d bytes", len(out))
	}
	if !strings.Contains(out, "[repo map truncated]") {
		t.Fatalf("expected truncation marker:\n%s", out)
	}
}

func TestBuildSystemContextIncludesRepoMap(t *testing.T) {
	g := &Gogent{repoMap: "## Repo map\n### a.go\n- func Foo()"}
	ctx, _ := g.buildSystemContext("")
	if !strings.Contains(ctx, "## Repo map") || !strings.Contains(ctx, "func Foo()") {
		t.Fatalf("system context missing repo map: %q", ctx)
	}
}
