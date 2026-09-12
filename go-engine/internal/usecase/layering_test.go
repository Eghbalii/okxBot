package usecase

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUsecaseImportsNoExchangeAdapter enforces the layering rule CLAUDE.md §10 states in prose:
// use-cases depend on internal/port and internal/domain, never on a concrete exchange adapter.
//
// This exists because the rule was already broken and nothing noticed. AffordabilityService held an
// okx.SymbolMap directly, so the package that is supposed to be exchange-agnostic could not compile
// without OKX — which would have forced a second exchange to either fabricate a redundant symbol
// map or fork the service. A design doc cannot catch that; a test that fails on the import can.
//
// Scanned at the AST level rather than by grepping, so a comment mentioning an adapter (this file
// has several) is never mistaken for a dependency on one.
func TestUsecaseImportsNoExchangeAdapter(t *testing.T) {
	// Adapters implement ports; a use-case importing one inverts the dependency this architecture
	// is built on. Add new adapters here as they are written.
	banned := []string{
		"internal/okx",
		"internal/mexc",
		"internal/postgres",
		"internal/kafkastream",
		"internal/gatewayclient",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, b := range banned {
				if strings.Contains(path, b) {
					t.Errorf("%s imports %s — a use-case must depend on internal/port, not on an "+
						"adapter (CLAUDE.md §10). If you need behaviour from it, add a port "+
						"interface and let the adapter satisfy it.",
						filepath.Base(name), path)
				}
			}
		}
	}

	// Guard against the test silently passing because it scanned nothing — the vacuity failure
	// CLAUDE.md §30.1 records three real bugs behind.
	if checked == 0 {
		t.Fatal("scanned no source files — the check is vacuous")
	}
	t.Logf("checked %d files in internal/usecase for adapter imports", checked)
}
