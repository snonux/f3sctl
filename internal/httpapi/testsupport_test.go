package httpapi

import (
	"context"
	"errors"
	"io"
	"sync/atomic"

	"github.com/snonux/f3sctl/internal/gogios"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/httpapi/gogiosapi"
	"github.com/snonux/f3sctl/internal/httpapi/powerapi"
	"github.com/snonux/f3sctl/internal/inventory"
)

// Testsupport helpers for the composition-root tests: the route table here is
// assembled from the two domain surfaces bound to inert collaborators (nil
// engine, nil jobs and nil peers on the power side; noGateways and
// unreachableReports() on the Gogios side, whose New rejects nil ones), which
// is enough to *declare* the table -- names, paths, methods, availability
// predicates -- and to serve the routes whose handlers never dereference those
// collaborators. A test that needs a served power or mute handler builds a
// real Surface itself (see powerapi/gogiosapi's own tests) or uses the
// fully-wired newServer / ServeCGI paths.

// testPowerSurface returns a factory for the power surface with inert
// collaborators, building its links under base, for Server.build (via
// assemble) to bind to its renderer and its Prober -- the Server's own, so a
// test's probeHosts/fansStatus/acStatus hooks are what the surface's snapshot
// reads. base must be the one the Server is assembled with, as newServer
// shares one base between the two.
func testPowerSurface(inv inventory.Inventory, base string) powerSurfaceFunc {
	return func(actions contract.ActionRenderer, p powerapi.Prober) *powerapi.Surface {
		return powerapi.New("test", contract.Hrefs(base), inv, nil, p, nil, nil, actions)
	}
}

// testGogiosSurface returns a factory for the Gogios surface with inert
// collaborators (see testPowerSurface): noGateways as its Monitor, and
// unreachableReports as its report source, so no test serving a Gogios route
// through it can reach a real gateway or the real Gogios.
func testGogiosSurface(base string) gogiosSurfaceFunc {
	return gogiosSurfaceOver(base, unreachableReports(), noGateways)
}

// gogiosSurfaceOver returns a factory for the Gogios surface reading its
// report from reports and its gateway mute from monitor -- the collaborators
// its NeedReport and NeedMonitoring Providers fetch through, so a test
// serving a route that declares either wires them here.
func gogiosSurfaceOver(base string, reports gogiosapi.ReportSource, monitor gogiosapi.Monitor) gogiosSurfaceFunc {
	return func(actions contract.ActionRenderer) *gogiosapi.Surface {
		return gogiosapi.New("test", contract.Hrefs(base), reports, monitor, actions)
	}
}

// mutesRead is a gogiosapi.Monitor that only reads the gateway mute, for the
// tests that serve routes judged on the mute but never change it: a mute or
// un-mute through it fails.
type mutesRead func(context.Context) []gogios.GatewayMute

func (m mutesRead) MonitoringStatus(ctx context.Context) []gogios.GatewayMute { return m(ctx) }

func (mutesRead) MuteGogios(context.Context, io.Writer) error { return errMuteReadOnly }

func (mutesRead) UnmuteNow(context.Context, io.Writer) error { return errMuteReadOnly }

// noGateways is a Monitor with no gateways at all: the mute reads as nothing
// (neither mute action is offered) and a mute or un-mute fails.
var noGateways = mutesRead(func(context.Context) []gogios.GatewayMute { return nil })

// errMuteReadOnly is what a mutesRead answers a mute or un-mute with.
var errMuteReadOnly = errors.New("mutesRead: this test's gateways are read-only")

// fakeReports is a gogiosapi.ReportSource that serves a fixed report (or a
// fixed error) and counts its calls, so a test can tell which reads and
// clears one request paid for without real HTTP or an on-disk cache.
type fakeReports struct {
	report   *gogios.Report
	err      error // returned by Fetch instead of report, when set
	clearErr error // returned by Clear, when set

	fetches, clears atomic.Int32
}

func (f *fakeReports) Fetch(context.Context) (*gogios.Report, error) {
	f.fetches.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.report, nil
}

func (f *fakeReports) Clear() error {
	f.clears.Add(1)
	return f.clearErr
}

// errNoReport is the fetch error unreachableReports answers with.
var errNoReport = errors.New("gogios report unavailable in tests")

// unreachableReports is a report source whose every fetch fails, standing in
// for a Gogios upstream that cannot be reached.
func unreachableReports() *fakeReports { return &fakeReports{err: errNoReport} }

// testRouter builds a Server for inv mounted at base, through Server.build
// exactly as newServer does, and returns its Router -- the very Router the
// table's handlers render their actions through, so hrefs a test builds with
// it and hrefs a handler renders -- links and actions alike -- always share
// one base and one table.
func testRouter(inv inventory.Inventory, base string) *Router {
	return (&Server{}).assemble(inv, testPowerSurface(inv, base), testGogiosSurface(base), base).router
}

// testRoutes is the route table of testRouter(inv, "") -- the same table
// newServer would build from inv, over inert surfaces. A test that needs a
// Router, or a non-empty base, uses testRouter instead, never a second Router
// over this table.
func testRoutes(inv inventory.Inventory) []contract.Route {
	return testRouter(inv, "").routes
}

// declaredRoutes builds inv's route table without a Router at all, for the
// tests that check a table's ambiguity themselves -- where NewRouter, which
// refuses an ambiguous table, must not get there first. Its handlers render
// through a Server that is never built, so serving any of them panics rather
// than rendering some other Router's actions.
func declaredRoutes(inv inventory.Inventory) []contract.Route {
	srv := &Server{}
	actions := srv.actionRenderer()
	return srv.buildRoutes(inv, testPowerSurface(inv, "")(actions, nil), testGogiosSurface("")(actions))
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
	return (&Server{}).assemble(inventory.Default(), testPowerSurface(inventory.Default(), ""), testGogiosSurface(""), "")
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
