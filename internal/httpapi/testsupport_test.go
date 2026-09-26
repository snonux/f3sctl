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

// testPowerSurface returns the power surface with inert collaborators,
// rendering its actions through actions -- the actionRenderer of the Server
// it is about to be assembled into, exactly as newServer wires it.
func testPowerSurface(inv inventory.Inventory, actions contract.ActionRenderer) *powerapi.Surface {
	return powerapi.New("test", contract.Hrefs(""), inv, nil, nil, nil, actions)
}

// testGogiosSurface returns the Gogios surface with inert collaborators,
// rendering its actions through actions (see testPowerSurface).
func testGogiosSurface(actions contract.ActionRenderer) *gogiosapi.Surface {
	return gogiosapi.New("test", contract.Hrefs(""), config.Default(), nil, actions)
}

// testRoutes builds the same table newServer would, from the given inventory
// and inert surfaces -- the pure-declaration subset of production wiring.
func testRoutes(inv inventory.Inventory) []contract.Route {
	srv := testServer()
	return srv.buildRoutes(inv, testPowerSurface(inv, srv.actionRenderer()), testGogiosSurface(srv.actionRenderer()))
}

// assemble is Server.build for tests, whose route tables are known to be
// unambiguous: it panics instead of returning the error, so a Server literal
// can be wired in one expression.
func (s *Server) assemble(inv inventory.Inventory, pw *powerapi.Surface, gg *gogiosapi.Surface, base string) *Server {
	srv, err := s.build(inv, pw, gg, base)
	if err != nil {
		panic(err)
	}
	return srv
}

// mustRouter is NewRouter for tests whose route tables are known to be
// unambiguous.
func mustRouter(base string, rs []contract.Route) *Router {
	rt, err := NewRouter(base, rs)
	if err != nil {
		panic(err)
	}
	return rt
}

// testServer returns a Server with no collaborators at all, for building the
// route table (which needs a Server only to bind the root-resource handlers).
func testServer() *Server {
	srv := &Server{}
	return srv.assemble(inventory.Default(), testPowerSurface(inventory.Default(), srv.actionRenderer()), testGogiosSurface(srv.actionRenderer()), "")
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
