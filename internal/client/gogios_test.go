package client

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestPrintGogiosOverviewRendersTheSummary pins the happy path: the subject
// headline, lastUpdated, and every one of the six summary counts as an int
// (not the raw float64 encoding/json decodes JSON numbers into).
func TestPrintGogiosOverviewRendersTheSummary(t *testing.T) {
	var buf bytes.Buffer
	printGogiosOverview(&buf, Entity{Properties: map[string]any{
		"subject":     "GOGIOS Report [C:1 W:0 U:0 S:0 SU:0 OK:42]",
		"lastUpdated": "2026-08-27T08:58:18+02:00",
		"summary": map[string]any{
			"critical": float64(1), "warning": float64(0), "unknown": float64(0),
			"stale": float64(0), "suppressed": float64(0), "ok": float64(42),
		},
	}})

	out := buf.String()
	if !strings.Contains(out, "GOGIOS Report [C:1 W:0 U:0 S:0 SU:0 OK:42]") {
		t.Errorf("output = %q, want the subject line", out)
	}
	if !strings.Contains(out, "2026-08-27T08:58:18+02:00") {
		t.Errorf("output = %q, want lastUpdated", out)
	}
	if !strings.Contains(out, "critical=1") || !strings.Contains(out, "ok=42") {
		t.Errorf("output = %q, want the summary counts as plain integers", out)
	}
}

// TestPrintGogiosOverviewReportsAnErrorProperty pins the degraded path,
// mirroring parseFans's "error property, not a bare state" convention: an
// unreachable report must not render as an empty-but-successful overview.
func TestPrintGogiosOverviewReportsAnErrorProperty(t *testing.T) {
	var buf bytes.Buffer
	printGogiosOverview(&buf, Entity{Properties: map[string]any{"error": "dial tcp: timeout"}})

	out := buf.String()
	if !strings.Contains(out, "unknown") || !strings.Contains(out, "dial tcp: timeout") {
		t.Errorf("output = %q, want it to say unknown and name the error", out)
	}
	if strings.Contains(out, "GOGIOS") {
		t.Errorf("output = %q, want no report content rendered", out)
	}
}

// TestPrintGogiosChecksListsEachCheck pins the drill-down rendering, and that
// an empty category says so rather than printing nothing.
func TestPrintGogiosChecksListsEachCheck(t *testing.T) {
	var buf bytes.Buffer
	printGogiosChecks(&buf, "critical", Entity{Entities: []Entity{
		{Properties: map[string]any{"name": "Check Ping6 r1", "status": "CRITICAL", "output": "timed out"}},
	}})
	if out := buf.String(); !strings.Contains(out, "CRITICAL: Check Ping6 r1 - timed out") {
		t.Errorf("output = %q, want the one check rendered", out)
	}

	buf.Reset()
	printGogiosChecks(&buf, "ok", Entity{})
	if out := buf.String(); !strings.Contains(out, "no ok checks") {
		t.Errorf("output = %q, want it to say there are none", out)
	}
}

// TestPrintGogiosCheckShowsOnlyThePresentOptionalFields pins that
// prevStatus/federatedFrom/lastCheckedAgeSeconds are shown only when the
// server actually sent them -- matching gogiosCheckEntity's own conditional
// encoding (internal/httpapi/handlers.go) rather than printing an empty
// "prev:" line for every check.
func TestPrintGogiosCheckShowsOnlyThePresentOptionalFields(t *testing.T) {
	var buf bytes.Buffer
	printGogiosCheck(&buf, Entity{Properties: map[string]any{
		"name": "Check Ping6 r1", "status": "CRITICAL", "output": "timed out",
	}})
	out := buf.String()
	if strings.Contains(out, "prev:") || strings.Contains(out, "from:") || strings.Contains(out, "age:") {
		t.Errorf("output = %q, want no optional fields when absent", out)
	}

	buf.Reset()
	printGogiosCheck(&buf, Entity{Properties: map[string]any{
		"name": "Check Ping6 r1", "status": "CRITICAL", "output": "timed out",
		"prevStatus": "OK", "federatedFrom": "fishfinger", "lastCheckedAgeSeconds": float64(187),
	}})
	out = buf.String()
	for _, want := range []string{"prev:   OK", "from:   fishfinger", "age:    187s"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to contain %q", out, want)
		}
	}
}

// fakeGogiosAPI is a minimal Siren-over-HTTP fixture covering the "gogios"
// entity and its drill-downs/action, enough to drive runGogios end to end
// without a real internal/httpapi server.
type fakeGogiosAPI struct {
	srv *httptest.Server

	mu          sync.Mutex
	cacheClears int
}

func newFakeGogiosAPI(t *testing.T) *fakeGogiosAPI {
	t.Helper()
	f := &fakeGogiosAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGogiosAPI) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/" && r.Method == http.MethodGet:
		writeEntity(w, Entity{
			Properties: map[string]any{"apiVersion": float64(SupportedAPIVersion)},
			Links:      []Link{{Rel: []string{"gogios"}, Href: "/gogios"}},
		})
	case r.URL.Path == "/gogios" && r.Method == http.MethodGet:
		f.mu.Lock()
		cleared := f.cacheClears
		f.mu.Unlock()
		writeEntity(w, Entity{
			Class: []string{"gogios"},
			Properties: map[string]any{
				"subject": "GOGIOS Report [C:1 W:0 U:0 S:0 SU:0 OK:1]", "lastUpdated": "2026-08-27T08:58:18+02:00",
				"summary": map[string]any{"critical": float64(1), "warning": float64(0), "unknown": float64(0),
					"stale": float64(0), "suppressed": float64(0), "ok": float64(1)},
				"clears": float64(cleared), // lets a test prove this GET followed a real re-fetch
			},
			Links: []Link{
				{Rel: []string{"critical"}, Href: "/gogios/critical"},
				{Rel: []string{"warning"}, Href: "/gogios/warning"},
				{Rel: []string{"unknown"}, Href: "/gogios/unknown"},
				{Rel: []string{"stale"}, Href: "/gogios/stale"},
				{Rel: []string{"suppressed"}, Href: "/gogios/suppressed"},
				{Rel: []string{"ok"}, Href: "/gogios/ok"},
			},
			Actions: []Action{{
				Name: "gogios-cache-clear", Title: "Clear the cached Gogios report",
				Method: http.MethodPost, Href: "/gogios/cache/clear", CLIVerb: "gogios cache clear",
			}},
		})
	case r.URL.Path == "/gogios/critical" && r.Method == http.MethodGet:
		writeEntity(w, Entity{Entities: []Entity{
			{Properties: map[string]any{"name": "Check Ping6 r1", "status": "CRITICAL", "output": "timed out"}},
		}})
	case strings.HasPrefix(r.URL.Path, "/gogios/") && r.URL.Path != "/gogios/critical" &&
		r.URL.Path != "/gogios/cache/clear" && r.Method == http.MethodGet:
		writeEntity(w, Entity{}) // every other drill-down category is empty
	case r.URL.Path == "/gogios/cache/clear" && r.Method == http.MethodPost:
		f.mu.Lock()
		f.cacheClears++
		f.mu.Unlock()
		writeEntity(w, Entity{Properties: map[string]any{"subject": "fresh"}})
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

// TestRunGogiosShowsTheOverview pins the bare/"status" path end to end.
func TestRunGogiosShowsTheOverview(t *testing.T) {
	api := newFakeGogiosAPI(t)
	c := newTestClient(t, api.srv.URL, "k")
	var out bytes.Buffer
	c.stdout = &out

	if err := c.runGogios(context.Background(), nil, false); err != nil {
		t.Fatalf("runGogios(nil): %v", err)
	}
	if !strings.Contains(out.String(), "GOGIOS Report") {
		t.Errorf("output = %q, want the overview", out.String())
	}
}

// TestRunGogiosDrillsDownByCategory pins a status argument following the
// gogios entity's own link.
func TestRunGogiosDrillsDownByCategory(t *testing.T) {
	api := newFakeGogiosAPI(t)
	c := newTestClient(t, api.srv.URL, "k")
	var out bytes.Buffer
	c.stdout = &out

	if err := c.runGogios(context.Background(), []string{"critical"}, false); err != nil {
		t.Fatalf("runGogios(critical): %v", err)
	}
	if !strings.Contains(out.String(), "Check Ping6 r1") {
		t.Errorf("output = %q, want the critical check listed", out.String())
	}
}

// TestRunGogiosDetailFindsACheckAcrossCategories pins showGogiosCheck's
// search: the check lives under "critical", and "detail" is not told which
// category to look in.
func TestRunGogiosDetailFindsACheckAcrossCategories(t *testing.T) {
	api := newFakeGogiosAPI(t)
	c := newTestClient(t, api.srv.URL, "k")
	var out bytes.Buffer
	c.stdout = &out

	if err := c.runGogios(context.Background(), []string{"detail", "Check", "Ping6", "r1"}, false); err != nil {
		t.Fatalf("runGogios(detail): %v", err)
	}
	if !strings.Contains(out.String(), "name:   Check Ping6 r1") {
		t.Errorf("output = %q, want the check's detail", out.String())
	}
}

// TestRunGogiosDetailReportsAnUnknownName is the negative case: a name
// matching nothing in any category is an error, not a silent empty render.
func TestRunGogiosDetailReportsAnUnknownName(t *testing.T) {
	api := newFakeGogiosAPI(t)
	c := newTestClient(t, api.srv.URL, "k")

	err := c.runGogios(context.Background(), []string{"detail", "no", "such", "check"}, false)
	if err == nil || !strings.Contains(err.Error(), "no such Gogios check") {
		t.Errorf("err = %v, want it to say no such check", err)
	}
}

// TestRunGogiosCacheClearInvokesTheActionAndReFetches pins that "cache
// clear" performs the POST action and then genuinely re-follows "gogios"
// (via showGogios) rather than trusting a stale render -- the fake's
// "clears" counter, only incremented by the real POST handler, proves the
// GET that follows saw the post-clear state.
func TestRunGogiosCacheClearInvokesTheActionAndReFetches(t *testing.T) {
	api := newFakeGogiosAPI(t)
	c := newTestClient(t, api.srv.URL, "k")
	var out bytes.Buffer
	c.stdout = &out

	if err := c.runGogios(context.Background(), []string{"cache", "clear"}, false); err != nil {
		t.Fatalf("runGogios(cache clear): %v", err)
	}
	api.mu.Lock()
	clears := api.cacheClears
	api.mu.Unlock()
	if clears != 1 {
		t.Fatalf("POST /gogios/cache/clear calls = %d, want 1", clears)
	}
	if !strings.Contains(out.String(), "GOGIOS Report") {
		t.Errorf("output = %q, want the re-fetched overview", out.String())
	}
}

// TestRunGogiosRejectsAnUnknownCommand pins the negative dispatch case: a
// spelling matching none of the documented verbs is an error, not a silent
// no-op or a request to some route that does not exist.
func TestRunGogiosRejectsAnUnknownCommand(t *testing.T) {
	api := newFakeGogiosAPI(t)
	c := newTestClient(t, api.srv.URL, "k")

	err := c.runGogios(context.Background(), []string{"bogus"}, false)
	if err == nil || !strings.Contains(err.Error(), `unknown gogios command "gogios bogus"`) {
		t.Errorf("err = %v, want it to name the unknown command", err)
	}
}
