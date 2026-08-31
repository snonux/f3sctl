package gogiosapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/gogios"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
)

// errFake is a stand-in backend error, the same "plug unreachable" message the
// composition root's own tests use, so error-propagating assertions read the
// same in both packages.
type errFake struct{}

func (errFake) Error() string { return "plug unreachable" }

// hasRel reports whether links carries a link whose first rel is rel.
func hasRel(links []contract.Link, rel string) bool {
	for _, l := range links {
		if len(l.Rel) > 0 && l.Rel[0] == rel {
			return true
		}
	}
	return false
}

// testSurface returns a Surface with no real collaborators: the read-side
// handlers under test here render only from state, and the table-declaration
// nil-safety means no Monitor or ActionsFor is needed to serve them.
func testSurface() *Surface {
	return New("test", contract.Hrefs(""), config.Default(), nil)
}

// gogiosSample is a small, representative Gogios report for handler tests:
// one unhandled CRITICAL, one stale WARNING (its lifecycle is stale, but its
// own severity stays WARNING), one suppressed UNKNOWN, and two OK checks.
// Mirrors the shape internal/gogios/gogios_test.go's own fixture describes.
func gogiosSample() *gogios.Report {
	return &gogios.Report{
		LastUpdated: "2026-08-27T08:58:18+02:00",
		Subject:     "GOGIOS Report [C:1 W:1 U:1 S:1 SU:1 OK:2]",
		Summary:     gogios.Summary{Critical: 1, Warning: 1, Unknown: 1, Stale: 1, Suppressed: 1, Ok: 2},
		Sections: gogios.Sections{
			Unhandled: []gogios.Check{
				{Name: "Check Ping6 r1.wg0.wan.buetow.org", Status: "CRITICAL", Output: "timed out", Epoch: 1},
			},
			Stale: []gogios.Check{
				{Name: "Check SWAP blowfish", Status: "WARNING", Output: "SWAP WARNING", Epoch: 2, LastCheckedAgeSeconds: 99999},
			},
			Suppressed: []gogios.Check{
				{Name: "Check Disk fishfinger", Status: "UNKNOWN", Output: "no data", Epoch: 3},
			},
			Ok: []gogios.Check{
				{Name: "Check Ping4 master.buetow.org", Status: "OK", Output: "PING OK", Epoch: 4},
				{Name: "Check HTTP IPv4 foo.zone", Status: "OK", Output: "HTTP OK", Epoch: 5},
			},
		},
	}
}

// TestHandleGogiosRendersTheOverview pins the happy path: the subject, the
// six summary counts, and a link to every drill-down category plus
// /monitoring (the separate mute concern).
func TestHandleGogiosRendersTheOverview(t *testing.T) {
	sf := testSurface()
	state := contract.State{Gogios: gogiosSample()}

	e, status, err := sf.handleOverview(context.Background(), state, contract.Request{})
	if err != nil {
		t.Fatalf("handleOverview: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if e.Properties["subject"] != state.Gogios.Subject {
		t.Errorf("subject = %v, want %v", e.Properties["subject"], state.Gogios.Subject)
	}

	summary, ok := e.Properties["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary property = %#v, want a map", e.Properties["summary"])
	}
	if summary["critical"] != 1 || summary["stale"] != 1 || summary["ok"] != 2 {
		t.Errorf("summary = %+v, want critical=1 stale=1 ok=2", summary)
	}

	for _, rel := range []string{"self", "up", "monitoring", "critical", "warning", "unknown", "stale", "suppressed", "ok"} {
		if !hasRel(e.Links, rel) {
			t.Errorf("overview links = %+v, missing rel %q", e.Links, rel)
		}
	}
}

// TestHandleGogiosReportsAFetchErrorAsAProperty pins the degraded path: a
// fetch failure is a property on a 200, the same convention
// handleMonitoring uses, not a non-2xx status -- see handleOverview's
// doc comment for why.
func TestHandleGogiosReportsAFetchErrorAsAProperty(t *testing.T) {
	sf := testSurface()
	state := contract.State{GogiosErr: errFake{}}

	e, status, err := sf.handleOverview(context.Background(), state, contract.Request{})
	if err != nil {
		t.Fatalf("handleOverview: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want %d", status, http.StatusOK)
	}
	if e.Properties["error"] != "plug unreachable" {
		t.Errorf("error property = %v, want the fetch error", e.Properties["error"])
	}
	if _, ok := e.Properties["subject"]; ok {
		t.Error("subject property present despite a fetch error")
	}
}

// TestHandleGogiosStatusFiltersBySeverity pins the four severity categories:
// each is the union, across every lifecycle section, of checks with that
// Status -- see checksForStatus.
func TestHandleGogiosStatusFiltersBySeverity(t *testing.T) {
	sf := testSurface()
	state := contract.State{Gogios: gogiosSample()}

	for _, tc := range []struct {
		status string
		want   []string
	}{
		{"critical", []string{"Check Ping6 r1.wg0.wan.buetow.org"}},
		{"warning", []string{"Check SWAP blowfish"}},
		{"unknown", []string{"Check Disk fishfinger"}},
		{"ok", []string{"Check Ping4 master.buetow.org", "Check HTTP IPv4 foo.zone"}},
	} {
		t.Run(tc.status, func(t *testing.T) {
			e, status, err := sf.statusHandle(tc.status)(context.Background(), state, contract.Request{})
			if err != nil {
				t.Fatalf("statusHandle(%q): %v", tc.status, err)
			}
			if status != http.StatusOK {
				t.Fatalf("status = %d, want %d", status, http.StatusOK)
			}
			var got []string
			for _, sub := range e.Entities {
				got = append(got, sub.Properties["name"].(string))
			}
			if !equalLists(got, tc.want) {
				t.Errorf("%s checks = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

// TestHandleGogiosStatusLifecycleGroupings pins the other two categories:
// "stale" and "suppressed" read Sections.Stale/Suppressed directly rather
// than filtering by Status, because a stale or suppressed check keeps
// whatever severity it already had (here, WARNING and UNKNOWN respectively,
// neither of which is the literal string "stale"/"suppressed").
func TestHandleGogiosStatusLifecycleGroupings(t *testing.T) {
	sf := testSurface()
	state := contract.State{Gogios: gogiosSample()}

	for _, tc := range []struct{ status, want string }{
		{"stale", "Check SWAP blowfish"},
		{"suppressed", "Check Disk fishfinger"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			e, _, err := sf.statusHandle(tc.status)(context.Background(), state, contract.Request{})
			if err != nil {
				t.Fatalf("statusHandle: %v", err)
			}
			if len(e.Entities) != 1 || e.Entities[0].Properties["name"] != tc.want {
				t.Errorf("%s checks = %+v, want exactly [%s]", tc.status, e.Entities, tc.want)
			}
		})
	}
}

// TestHandleGogiosStatusReportsAFetchError is the drill-down's half of
// TestHandleGogiosReportsAFetchErrorAsAProperty.
func TestHandleGogiosStatusReportsAFetchError(t *testing.T) {
	sf := testSurface()
	state := contract.State{GogiosErr: errFake{}}

	e, status, err := sf.statusHandle("critical")(context.Background(), state, contract.Request{})
	if err != nil {
		t.Fatalf("statusHandle: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want %d", status, http.StatusOK)
	}
	if e.Properties["error"] != "plug unreachable" {
		t.Errorf("error property = %v, want the fetch error", e.Properties["error"])
	}
	if len(e.Entities) != 0 {
		t.Errorf("entities = %+v, want none on a fetch error", e.Entities)
	}
}

// TestHandleGogiosCheckFindsByName pins the by-name lookup, including a name
// containing spaces -- Gogios check names mirror the monitored command (e.g.
// "Check Ping6 r1.wg0.wan.buetow.org"), which is exactly why /gogios/check
// takes the name as a query parameter rather than a path segment.
func TestHandleGogiosCheckFindsByName(t *testing.T) {
	sf := testSurface()
	state := contract.State{Gogios: gogiosSample()}
	name := "Check Ping6 r1.wg0.wan.buetow.org"

	e, status, err := sf.handleCheck(context.Background(), state, contract.Request{Query: url.Values{"name": {name}}})
	if err != nil {
		t.Fatalf("handleCheck: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if e.Properties["name"] != name || e.Properties["status"] != "CRITICAL" || e.Properties["output"] != "timed out" {
		t.Errorf("check entity properties = %+v, want the CRITICAL check", e.Properties)
	}
	if e.Rel != nil {
		t.Error("a standalone check entity must not carry rel (only an embedded one does)")
	}
}

// TestHandleGogiosCheckNotFound is the negative case: a name matching no
// check is a 404, not an empty 200 or a silently-ignored lookup.
func TestHandleGogiosCheckNotFound(t *testing.T) {
	sf := testSurface()
	state := contract.State{Gogios: gogiosSample()}

	_, status, err := sf.handleCheck(context.Background(), state, contract.Request{Query: url.Values{"name": {"no such check"}}})
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", status, http.StatusNotFound)
	}
	if err == nil || !strings.Contains(err.Error(), "no such Gogios check") {
		t.Errorf("error = %v, want it to say no such check", err)
	}
}

// TestHandleGogiosCheckFailsHardOnAFetchError pins the one place a Gogios
// fetch failure is a real error rather than a property: a single-entity
// lookup cannot answer "does this check exist" at all without the report.
func TestHandleGogiosCheckFailsHardOnAFetchError(t *testing.T) {
	sf := testSurface()
	state := contract.State{GogiosErr: errFake{}}

	_, status, err := sf.handleCheck(context.Background(), state, contract.Request{Query: url.Values{"name": {"anything"}}})
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", status, http.StatusBadGateway)
	}
	if err == nil {
		t.Fatal("expected an error")
	}
}

// gogiosCacheTestSurface returns a Surface whose config points at an
// httptest server serving body, with the cache in a fresh temp dir -- the
// same hermetic setup internal/gogios/gogios_test.go uses. handleClearCache
// calls gogios.ClearCache/gogios.Fetch directly against sf.Config (there is
// no mockable seam for them, unlike the composition root's probe seams), so
// exercising it for real is the only way to pin its behaviour.
func gogiosCacheTestSurface(t *testing.T, body string) (*Surface, *int32) {
	t.Helper()
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.GogiosURL = upstream.URL
	cfg.GogiosFetchTimeout = config.Duration(5 * time.Second)
	cfg.GogiosCacheTTL = config.Duration(60 * time.Second)

	return New("test", contract.Hrefs(""), cfg, nil), &hits
}

// gogiosReportJSON is a minimal, valid Gogios report body for
// gogiosCacheTestSurface.
const gogiosReportJSON = `{"subject":"GOGIOS Report [C:0 W:0 U:0 S:0 SU:0 OK:1]",` +
	`"lastUpdated":"2026-08-27T08:58:18+02:00","summary":{"critical":0,"warning":0,"unknown":0,"stale":0,"suppressed":0,"ok":1},` +
	`"sections":{"ok":[{"name":"Check Ping4 master.buetow.org","status":"OK","output":"PING OK","epoch":1}]}}`

// TestHandleGogiosClearCacheClearsAndRefetches pins the whole point of the
// action: after it runs, even a cache well within its TTL must not be served
// -- the very next read has to see a real fetch.
func TestHandleGogiosClearCacheClearsAndRefetches(t *testing.T) {
	sf, hits := gogiosCacheTestSurface(t, gogiosReportJSON)

	// Prime the cache so ClearCache has something to remove.
	if _, err := gogios.Fetch(context.Background(), sf.Config); err != nil {
		t.Fatalf("priming the cache: %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("hits after priming = %d, want 1", got)
	}

	e, status, err := sf.handleClearCache(context.Background(), contract.State{}, contract.Request{})
	if err != nil {
		t.Fatalf("handleClearCache: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Errorf("hits after clear+refetch = %d, want 2 (the cache must have been cleared, forcing a re-fetch)", got)
	}
	if e.Properties["subject"] == "" {
		t.Error("the re-fetched overview has an empty subject")
	}
	if _, err := os.Stat(filepath.Join(sf.Config.StateDir, "gogios-report.json")); err != nil {
		t.Errorf("no cache file after clear+refetch: %v", err)
	}
}

// TestHandleGogiosClearCacheSurfacesAFetchErrorAfterClearing is the negative
// case: clearing the cache can succeed while the immediate re-fetch fails
// (the upstream is down). That must still render as a 200 with an "error"
// property -- handleClearCache delegates to handleOverview for
// rendering, so it inherits that convention rather than needing its own.
func TestHandleGogiosClearCacheSurfacesAFetchErrorAfterClearing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.GogiosURL = upstream.URL
	cfg.GogiosFetchTimeout = config.Duration(5 * time.Second)
	cfg.GogiosCacheTTL = config.Duration(60 * time.Second)
	sf := New("test", contract.Hrefs(""), cfg, nil)

	e, status, err := sf.handleClearCache(context.Background(), contract.State{}, contract.Request{})
	if err != nil {
		t.Fatalf("handleClearCache: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d (clearing succeeded; only the re-fetch failed)", status, http.StatusOK)
	}
	if e.Properties["error"] == nil {
		t.Error("no error property despite the re-fetch failing")
	}
}

// equalLists reports whether two string slices carry the same elements in the
// same order.
func equalLists(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
