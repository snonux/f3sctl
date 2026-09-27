package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/gogios"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/httpapi/gogiosapi"
	"github.com/snonux/f3sctl/internal/inventory"
)

// This file pins that the Gogios report reaches the API through exactly one
// injected gogiosapi.ReportSource (task da): enrichState's NeedReport fetch
// and the cache clear's own clear-and-re-read both go through the source the
// Gogios surface was built with, never through internal/gogios directly --
// so a fake source is all a test needs, and no request can read one report
// and render another.

// reportsServer serves the real pipeline with reports as the Gogios surface's
// report source and every other collaborator a fake.
func reportsServer(t *testing.T, reports *fakeReports) *Server {
	t.Helper()
	cfg := serverTestConfig(t, "sekrit")
	inv := inventory.Default()
	return (&Server{
		cfg:   cfg,
		jobs:  coordination.NewManager(t.TempDir(), cfg.UnmuteTimeout.D(), 0),
		peers: coordination.NewPeerSet(nil, ""),
		auth:  NewAuthenticator(cfg.APIKeyFile),
		siren: NewSirenRenderer(),
		node:  "test",
	}).assemble(inv, testPowerSurface(inv, ""), gogiosSurfaceOver("", reports, mutesRead(func(context.Context) []gogios.GatewayMute {
		return []gogios.GatewayMute{{Name: "blowfish"}}
	})), "")
}

// serveReportRoute serves one authenticated request and returns its status
// and raw body.
func serveReportRoute(t *testing.T, srv *Server, method, path string, query url.Values) (int, string) {
	t.Helper()
	req := getRequest(path)
	req.Method = method
	if query != nil {
		req.Query = query
	}
	var out strings.Builder
	if err := srv.serve(&out, req); err != nil {
		t.Fatalf("serve(%s %s): %v", method, path, err)
	}
	return splitGogiosE2EResponse(t, out.String())
}

// TestReportRoutesReadTheInjectedSource pins that enrichState's NeedReport
// fetch is the injected source's Fetch: each report route reads it exactly
// once and renders what it returned, and none clears it.
func TestReportRoutesReadTheInjectedSource(t *testing.T) {
	for _, tc := range []struct {
		path  string
		query url.Values
		want  string // a string only the fake's report can have put in the body
	}{
		{"/gogios", nil, needsReport.Subject},
		{"/gogios/critical", nil, needsCheckName},
		{"/gogios/check", url.Values{"name": {needsCheckName}}, needsCheckName},
	} {
		t.Run(tc.path, func(t *testing.T) {
			reports := &fakeReports{report: needsReport}
			status, body := serveReportRoute(t, reportsServer(t, reports), http.MethodGet, tc.path, tc.query)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want %d: %s", status, http.StatusOK, body)
			}
			if !strings.Contains(body, tc.want) {
				t.Errorf("body does not carry %q from the injected report: %s", tc.want, body)
			}
			if got, clears := reports.fetches.Load(), reports.clears.Load(); got != 1 || clears != 0 {
				t.Errorf("source fetches, clears = %d, %d; want 1, 0", got, clears)
			}
		})
	}
}

// TestNonReportRoutesLeaveTheSourceAlone is the negative half: a route that
// does not declare NeedReport never touches the report source.
func TestNonReportRoutesLeaveTheSourceAlone(t *testing.T) {
	for _, path := range []string{"/", "/monitoring", "/job"} {
		reports := &fakeReports{report: needsReport}
		if status, body := serveReportRoute(t, reportsServer(t, reports), http.MethodGet, path, nil); status != http.StatusOK {
			t.Fatalf("GET %s = %d, want %d: %s", path, status, http.StatusOK, body)
		}
		if got, clears := reports.fetches.Load(), reports.clears.Load(); got != 0 || clears != 0 {
			t.Errorf("GET %s: source fetches, clears = %d, %d; want 0, 0", path, got, clears)
		}
	}
}

// TestCacheClearGoesThroughTheInjectedSource pins the cache clear end to end:
// one Clear, then one Fetch -- the handler's re-read, through the same source
// enrichState reads -- and the re-rendered folder shows that report.
func TestCacheClearGoesThroughTheInjectedSource(t *testing.T) {
	reports := &fakeReports{report: needsReport}
	status, body := serveReportRoute(t, reportsServer(t, reports), http.MethodPost, "/gogios/cache/clear", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", status, http.StatusOK, body)
	}
	if got, clears := reports.fetches.Load(), reports.clears.Load(); got != 1 || clears != 1 {
		t.Errorf("source fetches, clears = %d, %d; want 1, 1", got, clears)
	}
	if !strings.Contains(body, needsReport.Subject) {
		t.Errorf("re-rendered folder does not carry the re-read report's subject: %s", body)
	}
}

// TestCacheClearFailureIsA500 pins a Clear error through the whole pipeline:
// a 500 carrying the cause, and no re-read of the cache that could not be
// dropped.
func TestCacheClearFailureIsA500(t *testing.T) {
	reports := &fakeReports{report: needsReport, clearErr: errFake{}}
	status, body := serveReportRoute(t, reportsServer(t, reports), http.MethodPost, "/gogios/cache/clear", nil)
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d: %s", status, http.StatusInternalServerError, body)
	}
	if !strings.Contains(body, errFake{}.Error()) {
		t.Errorf("body does not carry the clear error: %s", body)
	}
	if got := reports.fetches.Load(); got != 0 {
		t.Errorf("source fetches = %d, want 0 after a failed clear", got)
	}
}

// TestReportFetchFailureRendersAsToday pins a Fetch error through the whole
// pipeline, unchanged by the injection: the list resources render it as an
// "error" property on a 200, the single-check lookup answers 502.
func TestReportFetchFailureRendersAsToday(t *testing.T) {
	for _, tc := range []struct {
		path   string
		query  url.Values
		status int
	}{
		{"/gogios", nil, http.StatusOK},
		{"/gogios/critical", nil, http.StatusOK},
		{"/gogios/check", url.Values{"name": {needsCheckName}}, http.StatusBadGateway},
	} {
		t.Run(tc.path, func(t *testing.T) {
			reports := unreachableReports()
			status, body := serveReportRoute(t, reportsServer(t, reports), http.MethodGet, tc.path, tc.query)
			if status != tc.status {
				t.Errorf("status = %d, want %d: %s", status, tc.status, body)
			}
			if !strings.Contains(body, errNoReport.Error()) {
				t.Errorf("body does not carry the fetch error: %s", body)
			}
			if got := reports.fetches.Load(); got != 1 {
				t.Errorf("source fetches = %d, want 1", got)
			}
		})
	}
}

// TestReportNeedIsFilledThroughTheSurfacesSource pins the ownership seam
// build wires: the Fetch the Server runs for gogiosapi.NeedReport is the
// Gogios surface's own, reading the very source that surface was built with
// -- the one its cache clear clears -- rather than one the Server keeps.
func TestReportNeedIsFilledThroughTheSurfacesSource(t *testing.T) {
	reports := &fakeReports{report: needsReport}
	inv := inventory.Default()
	srv := (&Server{}).assemble(inv, testPowerSurface(inv, ""), gogiosSurfaceOver("", reports, nil), "")

	fetch, ok := srv.fetchers[gogiosapi.NeedReport]
	if !ok {
		t.Fatal("build installed no Fetch for gogiosapi.NeedReport")
	}
	report, err := gogiosapi.Report(fetch(context.Background(), contract.State{}, contract.Request{}))
	if report != needsReport || err != nil {
		t.Errorf("the NeedReport Fetch returned %p, %v; want the surface source's %p", report, err, needsReport)
	}
	if got := reports.fetches.Load(); got != 1 {
		t.Errorf("surface source fetches = %d, want 1", got)
	}
}

// TestProductionGogiosSurfaceOwnsARealSource pins the production wiring:
// the factory newServer hands build gives the Gogios surface a real report
// source, over the dedicated-client gogios.Source rather than any stand-in,
// and the same one on every call -- so the surface's reads and its cache
// clear always meet at one cache.
func TestProductionGogiosSurfaceOwnsARealSource(t *testing.T) {
	factory := productionGogiosSurface(serverTestConfig(t, "sekrit"), "test", contract.Hrefs(""), nil)
	actions := (&Server{}).actionRenderer()

	first := factory(actions).Reports()
	if src, ok := first.(*gogios.Source); !ok || src == nil {
		t.Errorf("the Gogios surface's report source = %#v, want a non-nil *gogios.Source", first)
	}
	if again := factory(actions).Reports(); again != first {
		t.Error("two surfaces from one factory read from two different sources")
	}
}
