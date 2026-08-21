package sdk

import (
	"bytes"
	"flag"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite sdk/testdata/api-v0.golden")

// TestAPISurfaceIsPinned renders every exported declaration of package sdk and
// compares it with the committed golden file. Any change to the public surface
// must be deliberate: run `go test ./sdk -update` and review the diff under
// docs/sdk-stability.md rules (v0: additive changes only without a note).
func TestAPISurfaceIsPinned(t *testing.T) {
	got := renderAPI(t)
	const golden = "testdata/api-v0.golden"
	if *updateGolden {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("missing golden (run with -update): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("public SDK surface changed; review and run `go test ./sdk -update`.\n--- got ---\n%s", got)
	}
}

func renderAPI(t *testing.T) []byte {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	var lines []string
	{
		for _, f := range files {
			for _, d := range f.Decls {
				switch decl := d.(type) {
				case *ast.FuncDecl:
					if !decl.Name.IsExported() {
						continue
					}
					if decl.Recv != nil {
						recv := exprString(fset, decl.Recv.List[0].Type)
						if !ast.IsExported(strings.TrimPrefix(recv, "*")) {
							continue
						}
					}
					decl.Body = nil
					decl.Doc = nil
					lines = append(lines, exprString(fset, decl))
				case *ast.GenDecl:
					for _, s := range decl.Specs {
						switch sp := s.(type) {
						case *ast.TypeSpec:
							if sp.Name.IsExported() {
								sp.Doc, sp.Comment = nil, nil
								stripComments(sp.Type)
								lines = append(lines, "type "+exprString(fset, sp))
							}
						case *ast.ValueSpec:
							var names []string
							for _, n := range sp.Names {
								if n.IsExported() {
									names = append(names, n.Name)
								}
							}
							if len(names) == 0 {
								continue
							}
							kw := "var"
							if decl.Tok == token.CONST {
								kw = "const"
							}
							typ := ""
							if sp.Type != nil {
								typ = " " + exprString(fset, sp.Type)
							}
							vals := ""
							if len(sp.Values) > 0 && decl.Tok == token.CONST {
								var vs []string
								for _, v := range sp.Values {
									vs = append(vs, exprString(fset, v))
								}
								vals = " = " + strings.Join(vs, ", ")
							}
							lines = append(lines, kw+" "+strings.Join(names, ", ")+typ+vals)
						}
					}
				}
			}
		}
	}
	sort.Strings(lines)
	return []byte(strings.Join(lines, "\n") + "\n")
}

func stripComments(n ast.Node) {
	ast.Inspect(n, func(x ast.Node) bool {
		if f, ok := x.(*ast.Field); ok {
			f.Doc, f.Comment = nil, nil
		}
		return true
	})
}

func exprString(fset *token.FileSet, n any) string {
	var buf bytes.Buffer
	cfg := printer.Config{Mode: printer.RawFormat}
	_ = cfg.Fprint(&buf, fset, n)
	s := buf.String()
	// Collapse whitespace so formatting changes do not count as API changes.
	return strings.Join(strings.Fields(s), " ")
}
