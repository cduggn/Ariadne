package decide

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// allowedImports is the decide package's stdlib allowlist. This test keeps
// the gateway's decision core pure through the import graph, not through
// review.
var allowedImports = map[string]bool{
	"encoding/json": true,
	"errors":        true,
	"strconv":       true,
	"strings":       true,
	"time":          true,
	"math":          true,
	"bytes":         true,
	"slices":        true,
	"sort":          true,
}

func TestPackageImportsAreRestricted(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++

		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if !allowedImports[path] {
				t.Errorf("%s: import %q is outside the decide package's stdlib allowlist", name, path)
			}
		}
	}

	if checked == 0 {
		t.Fatal("no non-test .go files found to check")
	}
}
