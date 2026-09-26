package httpapi

import (
	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/httpapi/gogiosapi"
	"github.com/snonux/f3sctl/internal/httpapi/powerapi"
	"github.com/snonux/f3sctl/internal/inventory"
)

// Testsupport helpers for the composition-root tests: the route table here is
// assembled from the two domain surfaces bound to inert collaborators (nil
// engine, nil jobs, nil peers, nil monitor), which is enough to *declare* the
// table -- names, paths, methods, availability predicates -- and to serve the
// routes whose handlers never dereference those collaborators. A test that
// needs a served power or mute handler builds a real Surface itself (see
// powerapi/gogiosapi's own tests) or uses the fully-wired newServer / ServeCGI
// paths.

// testPowerSurface returns a factory for the power surface with inert
// collaborators, for Server.build (via assemble) to bind to its renderer.
func testPowerSurface(inv inventory.Inventory) powerSurfaceFunc {
	return func(actions contract.ActionRenderer) *powerapi.Surface {
		return powerapi.New("test", contract.Hrefs(""), inv, nil, nil, nil, actions)
	}
}

// testGogiosSurface returns a factory for the Gogios surface with inert
// collaborators (see testPowerSurface).
func testGogiosSurface() gogiosSurfaceFunc {
	return func(actions contract.ActionRenderer) *gogiosapi.Surface {
		return gogiosapi.New("test", contract.Hrefs(""), config.Default(), nil, actions)
	}
}

// testRouter builds a Server for inv mounted at base, through Server.build
// exactly as newServer does, and returns its Router -- the very Router the
// table's handlers render their actions through, so hrefs a test builds with
// it and hrefs a handler renders always share one base and one table.
func testRouter(inv inventory.Inventory, base string) *Router {
	return (&Server{}).assemble(inv, testPowerSurface(inv), testGogiosSurface(), base).router
}

// testRoutes is the route table of testRouter(inv, "") -- the same table
// newServer would build from inv, over inert surfaces. A test that needs a
// Router, or a non-empty base, uses testRouter instead, never a second Router
// over this table.
func testRoutes(inv inventory.Inventory) []contract.Route {
	return testRouter(inv, "").routes
}

// declaredRoutes builds inv's route table without a Router at all, for the
// one test that needs a table NewRouter would refuse (an ambiguous one). Its
// handlers render through a Server that is never built, so serving any of
// them panics rather than rendering some other Router's actions.
func declaredRoutes(inv inventory.Inventory) []contract.Route {
	srv := &Server{}
	actions := srv.actionRenderer()
	return srv.buildRoutes(inv, testPowerSurface(inv)(actions), testGogiosSurface()(actions))
}

// assemble is Server.build for tests, whose route tables are known to be
// unambiguous: it panics instead of returning the error, so a Server literal
// can be wired in one expression.
func (s *Server) assemble(inv inventory.Inventory, pw powerSurfaceFunc, gg gogiosSurfaceFunc, base string) *Server {
	srv, err := s.build(inv, pw, gg, base)
	if err != nil {
		panic(err)
	}
	return srv
}

// testServer returns a Server with no collaborators at all, for building the
// route table (which needs a Server only to bind the root-resource handlers).
func testServer() *Server {
	return (&Server{}).assemble(inventory.Default(), testPowerSurface(inventory.Default()), testGogiosSurface(), "")
}

// routeByName finds a route by its stable client-facing name, the way the
// availability/field tests below look routes up rather than hardcoding their
// position in the table.
func routeByName(name string) (contract.Route, bool) {
	for _, r := range testRoutes(inventory.Default()) {
		if r.Name == name {
			return r, true
		}
	}
	return contract.Route{}, false
}

// lower is openapi.go's method-name lowercasing, used by the OpenAPI coverage
// test.
func lower(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'A' && r <= 'Z' {
			out[i] = r + 32
		}
	}
	return string(out)
}

// errFake is a stand-in backend error, reused across the root tests that
// assert an error is *reported* (as a property, or a 502) rather than what
// its message says.
type errFake struct{}

func (errFake) Error() string { return "plug unreachable" }
