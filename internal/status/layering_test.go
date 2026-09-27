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
// It walks the module's own imports with go/build (non-test files) rather
// than shelling out to `go list`, so it needs nothing beyond the source tree.
// The walk is repeated for every platform f3sctl ships on (see targets), so a
// file built only for one of them cannot hide an import from the others.
func TestConsumersDoNotImportTheEngine(t *testing.T) {
	const forbidden = modulePath + "/internal/power"
	for _, ctxt := range targets() {
		for _, pkg := range []string{
			"internal/status",
			"internal/presenter",
			"internal/client",
			"internal/httpapi/contract",
			"internal/httpapi/gogiosapi",
		} {
			t.Run(ctxt.GOOS+"-"+ctxt.GOARCH+"/"+pkg, func(t *testing.T) {
				if path := importPath(t, ctxt, modulePath+"/"+pkg, forbidden); path != nil {
					t.Errorf("%s depends on %s: %s", pkg, forbidden, strings.Join(path, " -> "))
				}
			})
		}
	}
}

// TestStatusIsALeaf pins that this package imports nothing from the module,
// so every producer and consumer can depend on it without a cycle.
func TestStatusIsALeaf(t *testing.T) {
	for _, ctxt := range targets() {
		for _, imp := range moduleImports(t, ctxt, modulePath+"/internal/status") {
			t.Errorf("%s/%s: internal/status imports %s; it must stay a leaf",
				ctxt.GOOS, ctxt.GOARCH, imp)
		}
	}
}

// targets returns a build context per platform f3sctl runs on: linux (earth,
// the CI host), NetBSD on the arm64 Pis (the CLI and CGI) and FreeBSD on the
// amd64 f-hosts (the agent). Each is a copy of build.Default with GOOS and
// GOARCH replaced, so per-OS files (foo_netbsd.go, //go:build freebsd) are
// selected exactly as the compiler would select them for that target.
func targets() []build.Context {
	var out []build.Context
	for _, t := range []struct{ goos, goarch string }{
		{"linux", "amd64"},
		{"linux", "arm64"},
		{"netbsd", "arm64"},
		{"freebsd", "amd64"},
	} {
		ctxt := build.Default
		ctxt.GOOS, ctxt.GOARCH = t.goos, t.goarch
		out = append(out, ctxt)
	}
	return out
}

// importPath returns the chain of module packages by which from reaches
// target, or nil when it does not.
func importPath(t *testing.T, ctxt build.Context, from, target string) []string {
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
		for _, imp := range moduleImports(t, ctxt, pkg) {
			if _, seen := parent[imp]; !seen {
				parent[imp] = pkg
				queue = append(queue, imp)
			}
		}
	}
	return nil
}

// moduleImports returns pkg's non-test imports that belong to this module,
// as ctxt selects its files.
// The package's directory is derived from the module root, two levels up
// from this test's working directory (internal/status).
func moduleImports(t *testing.T, ctxt build.Context, pkg string) []string {
	t.Helper()
	rel := strings.TrimPrefix(strings.TrimPrefix(pkg, modulePath), "/")
	dir := filepath.Join("..", "..", filepath.FromSlash(rel))
	bp, err := ctxt.ImportDir(dir, 0)
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
