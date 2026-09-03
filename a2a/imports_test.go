package a2a

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// a2a reaches the host through injected seams. An import of engine, plugin,
// store or orchestration is an import cycle waiting to happen, and it means
// the seams stopped being the boundary. Widening the allowlist is not the
// fix; moving whatever leaked back behind a seam is.
func TestPackageImportsNothingButCortexAndID(t *testing.T) {
	const module = "github.com/xraph/cortex"
	allowed := map[string]bool{
		module:         true,
		module + "/id": true,
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if parseErr != nil {
			t.Fatalf("ParseFile %s: %v", name, parseErr)
		}
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(path, module) {
				continue // standard library and third party are fine
			}
			if !allowed[path] {
				t.Errorf("%s imports %s, which breaks the leaf-package rule", name, path)
			}
		}
	}
}
