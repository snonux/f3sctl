package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/client"
	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power"
)

// This file drives the Gogios alert-browse surface (v51's internal/gogios,
// w51's internal/httpapi routes, x51's internal/cli/internal/client) as one
// genuine full stack: a fake Gogios JSON upstream over real HTTP, the real
// *Server this package's own routes()/handlers dispatch through, exposed
// over a real net/http.Server, driven by the real internal/client.Client --
// the same client `f3sctl --remote` uses.
//
// This complements, rather than replaces, the rest of this package's tests
// (which call handlers directly with a hand-built State) and
// internal/client's own fakeAPI-based tests (which deliberately do NOT use
// this package's real Siren rendering -- see that file's doc comment, on
// keeping client and server tests independent so a wire-format drift between
// them cannot pass by construction). Here, for once, both halves are real:
// the point is to prove the two have not actually drifted, not just to
// exercise either one in isolation.

// gogiosE2EReportJSON is a fixture built specifically to prove the
// union-across-sections and name-with-spaces-and-dots behavior other tests
// only exercise with Go literals: a CRITICAL in "unhandled" and ANOTHER
// CRITICAL in "stale" (so /gogios/critical must return both), a WARNING, and
// one OK. The stale CRITICAL's name carries both a space and a dot, so it
// doubles as the "detail by name" fixture (scope item 3).
const gogiosE2EReportJSON = `{
  "subject": "GOGIOS Report [C:2 W:1 U:0 S:1 SU:0 OK:1]",
  "lastUpdated": "2026-08-27T08:58:18+02:00",
  "summary": {"critical":2,"warning":1,"unknown":0,"stale":1,"suppressed":0,"ok":1},
  "sections": {
    "unhandled": [
      {"name":"Check Ping6 r1.wg0.wan.buetow.org","status":"CRITICAL","output":"timed out","epoch":1724744298},
      {"name":"Check HTTP IPv4 foo.zone","status":"WARNING","output":"slow response","epoch":1724744299}
    ],
    "stale": [
      {"name":"Check Disk r0.internal server.local","status":"CRITICAL","output":"disk full","epoch":1724000000,"lastCheckedAgeSeconds":99999}
    ],
    "ok": [
      {"name":"Check Ping4 master.buetow.org","status":"OK","output":"PING OK","epoch":1724744300}
    ]
  }
}`

// gogiosE2EUpstream stands in for the real gogios.buetow.org, serving body
// for every request and counting hits so cache hit/miss/clear scenarios can
// assert on the exact number of real HTTP fetches that occurred.
func gogiosE2EUpstream(t *testing.T, body string, status int) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// gogiosE2EServer stands up a real *Server -- the same struct ServeCGI
// dispatches through, built the same way countingServer (server_test.go)
// builds one for other end-to-end tests -- with cfg.GogiosURL pointed at
// upstream and cfg.StateDir at a fresh temp dir, exposed over a real
// net/http.Server. A real internal/client.Client driving this exercises the
// actual route table, handlers, and on-disk cache end to end.
//
// It bypasses ServeCGI/parseCGIRequest, which reads the CGI environment via
// os.Getenv -- process-global state unsafe to share across the concurrent
// net/http requests a real httptest.Server may serve within one test
// binary. Instead it builds the request struct directly from the incoming
// *http.Request and calls srv.serve, the exact function ServeCGI calls once
// its own parsing is done, so everything downstream of request parsing is
// the real, unmodified pipeline.
//
// No engine, and peers is an empty PeerSet: every /gogios* route is
// SkipsProbe:true and enrichState's PeerBusy check (server.go) loops over
// zero peer nodes and returns immediately, so nothing here ever needs a
// fleet probe, a Shelly read, or a real peer to ask -- see countingServer's
// own doc comment for the same reasoning.
func gogiosE2EServer(t *testing.T, upstream *httptest.Server) (*httptest.Server, config.Config, string) {
	t.Helper()

	const apiKey = "e2e-key"
	keyFile := filepath.Join(t.TempDir(), "apikey")
	if err := os.WriteFile(keyFile, []byte(apiKey+"\n"), 0o600); err != nil {
		t.Fatalf("writing the API key file: %v", err)
	}

	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.GogiosURL = upstream.URL
	cfg.GogiosFetchTimeout = config.Duration(5 * time.Second)
	cfg.GogiosCacheTTL = config.Duration(60 * time.Second)

	router := NewRouter("", inventory.Default())
	srv := &Server{
		cfg:     cfg,
		jobs:    coordination.NewManager(t.TempDir(), cfg.UnmuteTimeout.D(), 0),
		peers:   coordination.NewPeerSet(nil, ""),
		auth:    NewAuthenticator(keyFile),
		router:  router,
		openapi: NewOpenAPIBuilder(router),
		siren:   NewSirenRenderer(),
		node:    "e2e",
		// Root ("/") is not SkipsProbe, so snapshot() calls these; a nil
		// engine (there is none here -- see this function's doc comment)
		// would otherwise panic the moment client.Root fetches it. Neither
		// stub is ever exercised by a /gogios* route itself.
		probeHosts: func(context.Context) []power.HostStatus { return nil },
		fansStatus: func(context.Context) (power.FansState, error) { return power.FansState{}, nil },
	}

	e2e := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req := request{
			Method: r.Method,
			Path:   normalisePath(r.URL.Path),
			Query:  r.URL.Query(),
			Form:   r.PostForm,
			APIKey: r.Header.Get("X-API-Key"),
		}
		var buf bytes.Buffer
		if err := srv.serve(&buf, req); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		status, body := splitGogiosE2EResponse(t, buf.String())
		w.Header().Set("Content-Type", "application/vnd.siren+json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(e2e.Close)
	return e2e, cfg, apiKey
}

// splitGogiosE2EResponse extracts the HTTP status and body from a
// CGI-formatted response, built on this package's own splitCGIResponse
// (siren_test.go) rather than duplicating its header/body split: that
// helper's "Status" header value is "NNN Text" (e.g. "200 OK"), so only the
// leading status code needs pulling out here.
func splitGogiosE2EResponse(t *testing.T, raw string) (int, string) {
	t.Helper()
	headers, body := splitCGIResponse(t, raw)
	code, _, _ := strings.Cut(headers["Status"], " ")
	status, err := strconv.Atoi(code)
	if err != nil {
		t.Fatalf("parsing CGI status %q: %v", headers["Status"], err)
	}
	return status, body
}

// newGogiosE2EClient builds a real internal/client.Client against base --
// through client.New, the same constructor runRemote (internal/cli) uses --
// not a &client.Client{} literal.
func newGogiosE2EClient(t *testing.T, base, apiKey string, cfg config.Config, stdout io.Writer) *client.Client {
	t.Helper()
	c, err := client.New(base, apiKey, cfg, stdout)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return c
}

// e2eGetRaw makes an authenticated GET directly (bypassing hypermedia
// discovery), for the one scenario -- a check name that does not exist --
// that has no link to follow at all.
func e2eGetRaw(t *testing.T, base, apiKey, path string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("X-API-Key", apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}
	return resp.StatusCode, body
}

// TestGogiosE2EOverview pins scope item 1: the overview's headline
// properties and its links to every drill-down category plus /monitoring.
func TestGogiosE2EOverview(t *testing.T) {
	upstream, hits := gogiosE2EUpstream(t, gogiosE2EReportJSON, http.StatusOK)
	e2e, cfg, apiKey := gogiosE2EServer(t, upstream)
	c := newGogiosE2EClient(t, e2e.URL, apiKey, cfg, io.Discard)
	ctx := context.Background()

	root, err := c.Root(ctx)
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	overview, err := c.Follow(ctx, root, "gogios")
	if err != nil {
		t.Fatalf("Follow(gogios): %v", err)
	}

	if overview.Properties["subject"] != "GOGIOS Report [C:2 W:1 U:0 S:1 SU:0 OK:1]" {
		t.Errorf("subject = %v", overview.Properties["subject"])
	}
	if overview.Properties["lastUpdated"] != "2026-08-27T08:58:18+02:00" {
		t.Errorf("lastUpdated = %v", overview.Properties["lastUpdated"])
	}
	summary, ok := overview.Properties["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary property = %#v, want a map", overview.Properties["summary"])
	}
	if summary["critical"] != float64(2) || summary["ok"] != float64(1) {
		t.Errorf("summary = %+v, want critical=2 ok=1", summary)
	}

	for _, rel := range []string{"critical", "warning", "unknown", "stale", "suppressed", "ok", "monitoring"} {
		if _, ok := overview.Link(rel); !ok {
			t.Errorf("gogios overview carries no %q link", rel)
		}
	}

	if got := atomic.LoadInt32(hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1", got)
	}
}

// TestGogiosE2ECriticalDrillDownUnionsAcrossSections pins scope item 2: a
// CRITICAL check that is unhandled and a different CRITICAL check that is
// stale both appear under /gogios/critical, in the same order
// Report.ByStatus unions its sections (Unhandled before Stale).
func TestGogiosE2ECriticalDrillDownUnionsAcrossSections(t *testing.T) {
	upstream, _ := gogiosE2EUpstream(t, gogiosE2EReportJSON, http.StatusOK)
	e2e, cfg, apiKey := gogiosE2EServer(t, upstream)
	c := newGogiosE2EClient(t, e2e.URL, apiKey, cfg, io.Discard)
	ctx := context.Background()

	root, _ := c.Root(ctx)
	overview, err := c.Follow(ctx, root, "gogios")
	if err != nil {
		t.Fatalf("Follow(gogios): %v", err)
	}
	critical, err := c.Follow(ctx, overview, "critical")
	if err != nil {
		t.Fatalf("Follow(critical): %v", err)
	}

	want := []string{"Check Ping6 r1.wg0.wan.buetow.org", "Check Disk r0.internal server.local"}
	if len(critical.Entities) != len(want) {
		t.Fatalf("critical checks = %+v, want %d entries", critical.Entities, len(want))
	}
	for i, e := range critical.Entities {
		if got, _ := e.Properties["name"].(string); got != want[i] {
			t.Errorf("critical[%d].name = %q, want %q", i, got, want[i])
		}
		if got, _ := e.Properties["status"].(string); got != "CRITICAL" {
			t.Errorf("critical[%d].status = %q, want CRITICAL", i, got)
		}
	}
}

// TestGogiosE2ECheckDetailByName pins scope item 3's happy path: following a
// check's own self link -- whose name contains both a space and a dot --
// resolves to its full detail, including the conditional fields.
func TestGogiosE2ECheckDetailByName(t *testing.T) {
	upstream, _ := gogiosE2EUpstream(t, gogiosE2EReportJSON, http.StatusOK)
	e2e, cfg, apiKey := gogiosE2EServer(t, upstream)
	c := newGogiosE2EClient(t, e2e.URL, apiKey, cfg, io.Discard)
	ctx := context.Background()

	root, _ := c.Root(ctx)
	overview, _ := c.Follow(ctx, root, "gogios")
	stale, err := c.Follow(ctx, overview, "stale")
	if err != nil {
		t.Fatalf("Follow(stale): %v", err)
	}
	if len(stale.Entities) != 1 {
		t.Fatalf("stale checks = %+v, want exactly one", stale.Entities)
	}

	check, err := c.Follow(ctx, stale.Entities[0], "self")
	if err != nil {
		t.Fatalf("Follow(check self, name has spaces and a dot): %v", err)
	}
	if got, _ := check.Properties["name"].(string); got != "Check Disk r0.internal server.local" {
		t.Errorf("name = %q", got)
	}
	if got, _ := check.Properties["output"].(string); got != "disk full" {
		t.Errorf("output = %q", got)
	}
	if got, _ := check.Properties["lastCheckedAgeSeconds"].(float64); got != 99999 {
		t.Errorf("lastCheckedAgeSeconds = %v, want 99999", check.Properties["lastCheckedAgeSeconds"])
	}
}

// TestGogiosE2ECheckDetailNotFound pins scope item 3's negative case: a name
// matching no check 404s, with a message naming it.
func TestGogiosE2ECheckDetailNotFound(t *testing.T) {
	upstream, _ := gogiosE2EUpstream(t, gogiosE2EReportJSON, http.StatusOK)
	e2e, _, apiKey := gogiosE2EServer(t, upstream)

	status, body := e2eGetRaw(t, e2e.URL, apiKey, "/gogios/check?name="+url.QueryEscape("no such check"))
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", status, http.StatusNotFound)
	}
	props, _ := body["properties"].(map[string]any)
	if msg, _ := props["message"].(string); !strings.Contains(msg, "no such Gogios check") {
		t.Errorf("message = %q, want it to say no such check", msg)
	}
}

// TestGogiosE2ECacheServesWithinTTLThenRefetchesAfterExpiry pins scope item
// 4: a cold cache fetches once; a second read within GogiosCacheTTL is
// served from disk without a second fetch; once the cache is backdated past
// the TTL (the same instant, deterministic technique
// internal/gogios/gogios_test.go uses, rather than sleeping out a real 60s
// TTL), the next read fetches again.
func TestGogiosE2ECacheServesWithinTTLThenRefetchesAfterExpiry(t *testing.T) {
	upstream, hits := gogiosE2EUpstream(t, gogiosE2EReportJSON, http.StatusOK)
	e2e, cfg, apiKey := gogiosE2EServer(t, upstream)
	c := newGogiosE2EClient(t, e2e.URL, apiKey, cfg, io.Discard)
	ctx := context.Background()
	root, _ := c.Root(ctx)

	if _, err := c.Follow(ctx, root, "gogios"); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("hits after first read = %d, want 1", got)
	}

	if _, err := c.Follow(ctx, root, "gogios"); err != nil {
		t.Fatalf("second read: %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Errorf("hits after second read within TTL = %d, want still 1 (a cache hit)", got)
	}

	cachePath := filepath.Join(cfg.StateDir, "gogios-report.json")
	past := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(cachePath, past, past); err != nil {
		t.Fatalf("backdating the cache mtime: %v", err)
	}

	if _, err := c.Follow(ctx, root, "gogios"); err != nil {
		t.Fatalf("third read after TTL expiry: %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Errorf("hits after TTL expiry = %d, want 2 (a cache older than the TTL must re-fetch)", got)
	}
}

// TestGogiosE2ECacheClearForcesARefetchEvenWithinTTL pins scope item 5's
// direct-action half: gogios-cache-clear forces a fetch (its own re-render
// costs one), and the cache it leaves behind is fresh -- the very next read
// must not fetch again.
func TestGogiosE2ECacheClearForcesARefetchEvenWithinTTL(t *testing.T) {
	upstream, hits := gogiosE2EUpstream(t, gogiosE2EReportJSON, http.StatusOK)
	e2e, cfg, apiKey := gogiosE2EServer(t, upstream)
	c := newGogiosE2EClient(t, e2e.URL, apiKey, cfg, io.Discard)
	ctx := context.Background()
	root, _ := c.Root(ctx)

	overview, err := c.Follow(ctx, root, "gogios")
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("hits after first read = %d, want 1", got)
	}

	clear, ok := overview.Action("gogios-cache-clear")
	if !ok {
		t.Fatalf("gogios overview does not advertise gogios-cache-clear")
	}
	if _, err := c.Perform(ctx, clear, false); err != nil {
		t.Fatalf("Perform(gogios-cache-clear): %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Errorf("hits after cache-clear = %d, want 2 (clear, then re-fetch, happens server-side)", got)
	}

	if _, err := c.Follow(ctx, root, "gogios"); err != nil {
		t.Fatalf("read after clear: %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Errorf("hits after a read following clear = %d, want still 2 (the clear's own re-fetch primed a fresh cache)", got)
	}
}

// TestGogiosE2ERemoteCLIVerbsRenderExpectedText pins scope item 6: driving
// the same client.Run entry point `f3sctl --remote gogios ...` uses, for
// every documented verb, against the real server -- the overview, a
// drill-down, a per-check detail lookup, and the cache-clear action all
// render the text a real invocation would print.
func TestGogiosE2ERemoteCLIVerbsRenderExpectedText(t *testing.T) {
	upstream, _ := gogiosE2EUpstream(t, gogiosE2EReportJSON, http.StatusOK)
	e2e, cfg, apiKey := gogiosE2EServer(t, upstream)

	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"overview", []string{"gogios"}, []string{"GOGIOS Report", "critical=2"}},
		{"drill-down", []string{"gogios", "critical"},
			[]string{"Check Ping6 r1.wg0.wan.buetow.org", "Check Disk r0.internal server.local"}},
		{"detail", []string{"gogios", "detail", "Check", "Ping6", "r1.wg0.wan.buetow.org"},
			[]string{"name:   Check Ping6 r1.wg0.wan.buetow.org", "status: CRITICAL"}},
		{"cache clear", []string{"gogios", "cache", "clear"}, []string{"GOGIOS Report"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			c := newGogiosE2EClient(t, e2e.URL, apiKey, cfg, &out)
			if err := client.Run(context.Background(), c, tc.args, false); err != nil {
				t.Fatalf("client.Run(%v): %v", tc.args, err)
			}
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output = %q, want it to contain %q", out.String(), want)
				}
			}
		})
	}
}

// TestGogiosE2EUnreachableUpstreamSurfacesAsAPropertyAndDoesNotBreakOtherRoutes
// pins scope item 7: an unreachable Gogios upstream renders as an "error"
// property (not a request failure, and not an empty successful report), and
// the server keeps serving every other route afterwards.
func TestGogiosE2EUnreachableUpstreamSurfacesAsAPropertyAndDoesNotBreakOtherRoutes(t *testing.T) {
	upstream, _ := gogiosE2EUpstream(t, "boom", http.StatusInternalServerError)
	e2e, cfg, apiKey := gogiosE2EServer(t, upstream)
	c := newGogiosE2EClient(t, e2e.URL, apiKey, cfg, io.Discard)
	ctx := context.Background()

	root, err := c.Root(ctx)
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	overview, err := c.Follow(ctx, root, "gogios")
	if err != nil {
		t.Fatalf("Follow(gogios) against an unreachable upstream: %v", err)
	}

	msg, _ := overview.Properties["error"].(string)
	if msg == "" {
		t.Fatal("gogios overview carries no error property against an unreachable upstream")
	}
	if _, ok := overview.Properties["subject"]; ok {
		t.Error("subject property present despite the upstream being unreachable")
	}

	if _, err := c.Root(ctx); err != nil {
		t.Fatalf("root became unreachable after a failed gogios fetch: %v", err)
	}
}

// TestGogiosE2EStaleLastUpdatedPassesThroughUnchanged pins scope item 8: an
// old lastUpdated is not rewritten or dropped anywhere in the pipeline, so
// an operator reading it can tell the report is stale on its own -- there is
// no separate "age" computation to also get right.
func TestGogiosE2EStaleLastUpdatedPassesThroughUnchanged(t *testing.T) {
	const weekOld = "2026-08-20T09:00:00+02:00"
	oldReport := strings.Replace(gogiosE2EReportJSON, "2026-08-27T08:58:18+02:00", weekOld, 1)

	upstream, _ := gogiosE2EUpstream(t, oldReport, http.StatusOK)
	e2e, cfg, apiKey := gogiosE2EServer(t, upstream)
	c := newGogiosE2EClient(t, e2e.URL, apiKey, cfg, io.Discard)
	ctx := context.Background()

	root, _ := c.Root(ctx)
	overview, err := c.Follow(ctx, root, "gogios")
	if err != nil {
		t.Fatalf("Follow(gogios): %v", err)
	}
	if got, _ := overview.Properties["lastUpdated"].(string); got != weekOld {
		t.Errorf("lastUpdated = %q, want the report's own stale timestamp %q preserved verbatim", got, weekOld)
	}
}
