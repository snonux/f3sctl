package status

import (
	"go/build"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const modulePath = "github.com/snonux/f3sctl"

// TestConsumersDoNotImportTheEngine pins the reason this package exists: the
// packages that only show or transport a status must not reach
// internal/power, directly or through anything they import. The engine pulls
// in SSH, Wake-on-LAN, the Shelly RPC and the Gogios mute; a client that
// renders a table has no business depending on any of it.
//
// It walks the module's own imports with go/build (non-test files, for the
// GOOS/GOARCH the test runs on) rather than shelling out to `go list`, so it
// needs nothing beyond the source tree.
func TestConsumersDoNotImportTheEngine(t *testing.T) {
	const forbidden = modulePath + "/internal/power"
	for _, pkg := range []string{
		"internal/status",
		"internal/presenter",
		"internal/client",
		"internal/httpapi/contract",
	} {
		t.Run(pkg, func(t *testing.T) {
			if path := importPath(t, modulePath+"/"+pkg, forbidden); path != nil {
				t.Errorf("%s depends on %s: %s", pkg, forbidden, strings.Join(path, " -> "))
			}
		})
	}
}

// TestStatusIsALeaf pins that this package imports nothing from the module,
// so every producer and consumer can depend on it without a cycle.
func TestStatusIsALeaf(t *testing.T) {
	for _, imp := range moduleImports(t, modulePath+"/internal/status") {
		t.Errorf("internal/status imports %s; it must stay a leaf", imp)
	}
}

// importPath returns the chain of module packages by which from reaches
// target, or nil when it does not.
func importPath(t *testing.T, from, target string) []string {
	t.Helper()
	parent := map[string]string{from: ""}
	queue := []string{from}
	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]
		if pkg == target {
			var chain []string
			for p := pkg; p != ""; p = parent[p] {
				chain = append(chain, p)
			}
			slices.Reverse(chain)
			return chain
		}
		for _, imp := range moduleImports(t, pkg) {
			if _, seen := parent[imp]; !seen {
				parent[imp] = pkg
				queue = append(queue, imp)
			}
		}
	}
	return nil
}

// moduleImports returns pkg's non-test imports that belong to this module.
// The package's directory is derived from the module root, two levels up
// from this test's working directory (internal/status).
func moduleImports(t *testing.T, pkg string) []string {
	t.Helper()
	rel := strings.TrimPrefix(strings.TrimPrefix(pkg, modulePath), "/")
	dir := filepath.Join("..", "..", filepath.FromSlash(rel))
	bp, err := build.Default.ImportDir(dir, 0)
	if err != nil {
		t.Fatalf("reading %s: %v", pkg, err)
	}
	var out []string
	for _, imp := range bp.Imports {
		if imp == modulePath || strings.HasPrefix(imp, modulePath+"/") {
			out = append(out, imp)
		}
	}
	return out
}
