package gogent

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxRepoMapBytes caps the ranked repo map injected into the system prompt so a
// large tree cannot blow the context budget. It is deliberately small: the map
// is a navigation aid (a symbol skeleton), not a substitute for reading files.
const maxRepoMapBytes = 16 * 1024

// maxRepoMapFiles bounds how many Go files the walker parses, so building the
// map stays cheap even on very large checkouts.
const maxRepoMapFiles = 2000

// repoSymbol is a single top-level declaration discovered in a Go source file.
type repoSymbol struct {
	file string // workspace-relative, slash-separated path
	line int    // 1-based source line, used to keep declarations in reading order
	name string // declared identifier, used for reference ranking
	sig  string // concise signature, e.g. "func NewGogent(homeDir string) *Gogent"
	rank int    // number of references to name across the workspace
}

// buildRepoMap walks the workspace, extracts top-level Go declarations, ranks
// them Aider-style by how often their name is referenced across the tree, and
// renders a size-capped skeleton grouped by file. It returns "" when no Go
// sources are found (e.g. a non-Go or empty workspace), so callers can treat the
// map as optional. Non-Go languages are out of scope for this slice; see the
// repomap follow-up note in the issue.
func buildRepoMap(root string) string {
	symbols, refs := collectGoSymbols(root)
	for i := range symbols {
		// A declaration's own name is counted once as a reference; subtract it so
		// an unused symbol ranks 0 rather than 1.
		if r := refs[symbols[i].name] - 1; r > 0 {
			symbols[i].rank = r
		}
	}
	return renderRepoMap(symbols)
}

// collectGoSymbols parses every Go file under root, returning the top-level
// declarations it finds and a global count of how often each identifier name is
// referenced (a rough inbound-reference signal for ranking).
func collectGoSymbols(root string) ([]repoSymbol, map[string]int) {
	var symbols []repoSymbol
	refs := make(map[string]int)
	parsed := 0

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if parsed >= maxRepoMapFiles || !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		fset := token.NewFileSet()
		// SkipObjectResolution: we only need the syntax tree, not scope info.
		f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			return nil // skip files that don't parse rather than failing the map
		}
		parsed++

		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)

		// Count every identifier occurrence as a reference. This over-counts local
		// variables that happen to share a name, but is a cheap, deterministic
		// approximation of inbound references that is good enough for ranking.
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				refs[id.Name]++
			}
			return true
		})

		for _, decl := range f.Decls {
			symbols = append(symbols, declSymbols(fset, rel, decl)...)
		}
		return nil
	})

	return symbols, refs
}

// declSymbols flattens a top-level declaration into one repoSymbol per declared
// name (a GenDecl may declare several types/consts/vars at once).
func declSymbols(fset *token.FileSet, file string, decl ast.Decl) []repoSymbol {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		return []repoSymbol{{
			file: file,
			line: fset.Position(d.Pos()).Line,
			name: d.Name.Name,
			sig:  funcSignature(fset, d),
		}}
	case *ast.GenDecl:
		var out []repoSymbol
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				out = append(out, repoSymbol{
					file: file,
					line: fset.Position(s.Pos()).Line,
					name: s.Name.Name,
					sig:  typeSignature(s),
				})
			case *ast.ValueSpec:
				for _, n := range s.Names {
					if n.Name == "_" {
						continue
					}
					out = append(out, repoSymbol{
						file: file,
						line: fset.Position(n.Pos()).Line,
						name: n.Name,
						sig:  d.Tok.String() + " " + n.Name, // "const X" / "var Y"
					})
				}
			}
		}
		return out
	}
	return nil
}

// funcSignature renders a function/method declaration without its body, on a
// single normalized line (e.g. "func (g *Gogent) buildSystemContext() string").
func funcSignature(fset *token.FileSet, fn *ast.FuncDecl) string {
	stripped := *fn
	stripped.Body = nil
	stripped.Doc = nil
	var b strings.Builder
	if err := printer.Fprint(&b, fset, &stripped); err != nil {
		return "func " + fn.Name.Name
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// typeSignature renders a type declaration's header, noting struct/interface so
// the skeleton conveys shape without dumping every field.
func typeSignature(s *ast.TypeSpec) string {
	switch s.Type.(type) {
	case *ast.StructType:
		return "type " + s.Name.Name + " struct"
	case *ast.InterfaceType:
		return "type " + s.Name.Name + " interface"
	default:
		return "type " + s.Name.Name
	}
}

// renderRepoMap groups symbols by file, orders files by their combined rank
// (most-referenced first), lists each file's declarations in source order, and
// stops once the byte cap is reached.
func renderRepoMap(symbols []repoSymbol) string {
	if len(symbols) == 0 {
		return ""
	}

	byFile := make(map[string][]repoSymbol)
	fileRank := make(map[string]int)
	for _, s := range symbols {
		byFile[s.file] = append(byFile[s.file], s)
		fileRank[s.file] += s.rank
	}

	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool {
		if fileRank[files[i]] != fileRank[files[j]] {
			return fileRank[files[i]] > fileRank[files[j]]
		}
		return files[i] < files[j]
	})

	var b strings.Builder
	b.WriteString("## Repo map\n")
	b.WriteString("Ranked skeleton of top-level Go declarations in the workspace (most-referenced files first). Use it to locate code; read a file for full detail.\n")

	truncated := false
	for _, f := range files {
		syms := byFile[f]
		sort.Slice(syms, func(i, j int) bool { return syms[i].line < syms[j].line })

		var sec strings.Builder
		sec.WriteString("\n### " + f + "\n")
		for _, s := range syms {
			sec.WriteString("- " + s.sig + "\n")
		}
		if b.Len()+sec.Len() > maxRepoMapBytes {
			truncated = true
			break
		}
		b.WriteString(sec.String())
	}
	if truncated {
		b.WriteString("\n[repo map truncated]\n")
	}

	return strings.TrimSpace(b.String())
}
