package gogiosapi

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

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

// testSurface returns a Surface with no real collaborators beyond the echo
// renderer: the read-side handlers under test here render only from state, so
// the fake Monitor New requires is never reached.
func testSurface() *Surface {
	return reportSurface(&fakeReports{})
}

// gogiosSample is a small, representative Gogios report for handler tests:
// one unhandled CRITICAL, one stale WARNING (its lifecycle is stale, but its
// own severity stays WARNING), a suppressed UNKNOWN and a suppressed
// CRITICAL, and two OK checks. The summary counts leave the suppressed checks
// out, exactly as Gogios's countBy does. The unhandled CRITICAL and one OK
// changed since the last notification, so -- exactly as Gogios writes it --
// each is also listed in StatusChanged with its PrevStatus. Mirrors the shape
// internal/gogios/gogios_test.go's own fixture describes.
func gogiosSample() *gogios.Report {
	return &gogios.Report{
		LastUpdated: "2026-08-27T08:58:18+02:00",
		Subject:     "GOGIOS Report [C:1 W:1 U:0 S:1 SU:2 OK:2]",
		Summary:     gogios.Summary{Critical: 1, Warning: 1, Unknown: 0, Stale: 1, Suppressed: 2, Ok: 2},
		Sections: gogios.Sections{
			StatusChanged: []gogios.Check{
				{Name: "Check Ping6 r1.wg0.wan.buetow.org", Status: "CRITICAL", PrevStatus: "OK", Output: "timed out", Epoch: 1},
				{Name: "Check HTTP IPv4 foo.zone", Status: "OK", PrevStatus: "WARNING", Output: "HTTP OK", Epoch: 5},
			},
			Unhandled: []gogios.Check{
				{Name: "Check Ping6 r1.wg0.wan.buetow.org", Status: "CRITICAL", Output: "timed out", Epoch: 1},
			},
			Stale: []gogios.Check{
				{Name: "Check SWAP blowfish", Status: "WARNING", Output: "SWAP WARNING", Epoch: 2, LastCheckedAgeSeconds: 99999},
			},
			Suppressed: []gogios.Check{
				{Name: "Check Disk fishfinger", Status: "UNKNOWN", Output: "no data", Epoch: 3},
				{Name: "Check Load r2.wg0.wan.buetow.org", Status: "CRITICAL", Output: "load 42", Epoch: 6},
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
	state := WithReport(contract.State{}, gogiosSample(), nil)

	e, status, err := sf.handleOverview(context.Background(), state, contract.Request{})
	if err != nil {
		t.Fatalf("handleOverview: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if report, _ := Report(state); e.Properties["subject"] != report.Subject {
		t.Errorf("subject = %v, want %v", e.Properties["subject"], report.Subject)
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
	state := WithReport(contract.State{}, nil, errFake{})

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
// each is the union, across Unhandled, Stale and Ok (the sections behind
// Gogios's summary counts), of checks with that Status -- see
// gogios.Report.ChecksFor. A check also listed in StatusChanged must appear
// once, not twice, and a suppressed check not at all.
func TestHandleGogiosStatusFiltersBySeverity(t *testing.T) {
	sf := testSurface()
	state := WithReport(contract.State{}, gogiosSample(), nil)

	for _, tc := range []struct {
		status string
		want   []string
	}{
		{"critical", []string{"Check Ping6 r1.wg0.wan.buetow.org"}},
		{"warning", []string{"Check SWAP blowfish"}},
		{"unknown", nil}, // the only UNKNOWN is suppressed
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

// TestHandleGogiosCriticalListsAChangedCheckOnce pins the /gogios/critical
// regression: a CRITICAL listed in both StatusChanged and Unhandled is one
// entity (matching summary.critical), and it keeps the PrevStatus only the
// StatusChanged copy carries.
func TestHandleGogiosCriticalListsAChangedCheckOnce(t *testing.T) {
	sf := testSurface()
	state := WithReport(contract.State{}, gogiosSample(), nil)

	e, _, err := sf.statusHandle("critical")(context.Background(), state, contract.Request{})
	if err != nil {
		t.Fatalf("statusHandle(critical): %v", err)
	}
	if report, _ := Report(state); len(e.Entities) != report.Summary.Critical {
		t.Fatalf("critical entities = %d, want %d (summary.critical): %+v",
			len(e.Entities), report.Summary.Critical, e.Entities)
	}
	if got := e.Entities[0].Properties["prevStatus"]; got != "OK" {
		t.Errorf("critical[0].prevStatus = %v, want OK", got)
	}
}

// TestHandleGogiosSeverityDrillDownsMatchTheSummary pins that each severity
// drill-down lists exactly as many checks as Gogios's summary counts for it:
// suppressed checks (here a suppressed CRITICAL) are in neither, while the
// per-check lookup still finds the suppressed CRITICAL by name.
func TestHandleGogiosSeverityDrillDownsMatchTheSummary(t *testing.T) {
	sf := testSurface()
	state := WithReport(contract.State{}, gogiosSample(), nil)
	report, _ := Report(state)
	sum := report.Summary

	for status, want := range map[string]int{
		"critical": sum.Critical, "warning": sum.Warning, "unknown": sum.Unknown, "ok": sum.Ok,
	} {
		e, _, err := sf.statusHandle(status)(context.Background(), state, contract.Request{})
		if err != nil {
			t.Fatalf("statusHandle(%q): %v", status, err)
		}
		if len(e.Entities) != want {
			t.Errorf("%s entities = %d, want %d (the summary count): %+v", status, len(e.Entities), want, e.Entities)
		}
	}

	name := "Check Load r2.wg0.wan.buetow.org"
	e, status, err := sf.handleCheck(context.Background(), state, contract.Request{Query: url.Values{"name": {name}}})
	if err != nil || status != http.StatusOK {
		t.Fatalf("handleCheck(suppressed) = %d, %v; want 200", status, err)
	}
	if e.Properties["name"] != name || e.Properties["status"] != "CRITICAL" {
		t.Errorf("suppressed check properties = %+v, want the suppressed CRITICAL", e.Properties)
	}
}

// TestHandleGogiosStatusLifecycleGroupings pins the other two categories:
// "stale" and "suppressed" read Sections.Stale/Suppressed directly rather
// than filtering by Status, because a stale or suppressed check keeps
// whatever severity it already had (here, WARNING, and UNKNOWN plus CRITICAL,
// none of which is the literal string "stale"/"suppressed").
func TestHandleGogiosStatusLifecycleGroupings(t *testing.T) {
	sf := testSurface()
	state := WithReport(contract.State{}, gogiosSample(), nil)

	for _, tc := range []struct {
		status string
		want   []string
	}{
		{"stale", []string{"Check SWAP blowfish"}},
		{"suppressed", []string{"Check Disk fishfinger", "Check Load r2.wg0.wan.buetow.org"}},
	} {
		t.Run(tc.status, func(t *testing.T) {
			e, _, err := sf.statusHandle(tc.status)(context.Background(), state, contract.Request{})
			if err != nil {
				t.Fatalf("statusHandle: %v", err)
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

// TestHandleGogiosStatusReportsAFetchError is the drill-down's half of
// TestHandleGogiosReportsAFetchErrorAsAProperty.
func TestHandleGogiosStatusReportsAFetchError(t *testing.T) {
	sf := testSurface()
	state := WithReport(contract.State{}, nil, errFake{})

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
	state := WithReport(contract.State{}, gogiosSample(), nil)
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
	state := WithReport(contract.State{}, gogiosSample(), nil)

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
	state := WithReport(contract.State{}, nil, errFake{})

	_, status, err := sf.handleCheck(context.Background(), state, contract.Request{Query: url.Values{"name": {"anything"}}})
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", status, http.StatusBadGateway)
	}
	if err == nil {
		t.Fatal("expected an error")
	}
}

// fakeReports is a ReportSource serving a fixed report (or a fixed error)
// that records its calls in order, so a test can pin what a handler asked
// its source for, and when, without real HTTP or an on-disk cache.
type fakeReports struct {
	report   *gogios.Report
	err      error // returned by Fetch instead of report, when set
	clearErr error // returned by Clear, when set
	calls    []string
}

func (f *fakeReports) Fetch(context.Context) (*gogios.Report, error) {
	f.calls = append(f.calls, "fetch")
	if f.err != nil {
		return nil, f.err
	}
	return f.report, nil
}

func (f *fakeReports) Clear() error {
	f.calls = append(f.calls, "clear")
	return f.clearErr
}

// reportSurface returns a Surface reading its report from reports (and its
// mute from fakeMonitor, which New requires).
func reportSurface(reports ReportSource) *Surface {
	return New("test", contract.Hrefs(""), reports, fakeMonitor{}, echoActions{})
}

// TestHandleGogiosClearCacheClearsAndRefetches pins the whole point of the
// action: it clears the source's cache and then re-reads the report through
// the same source -- in that order, so the re-read cannot be served from the
// cache just cleared -- and renders what that re-read returned.
func TestHandleGogiosClearCacheClearsAndRefetches(t *testing.T) {
	reports := &fakeReports{report: gogiosSample()}
	sf := reportSurface(reports)

	e, status, err := sf.handleClearCache(context.Background(), contract.State{}, contract.Request{})
	if err != nil {
		t.Fatalf("handleClearCache: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if want := []string{"clear", "fetch"}; !equalLists(reports.calls, want) {
		t.Errorf("source calls = %v, want %v", reports.calls, want)
	}
	if e.Properties["subject"] != gogiosSample().Subject {
		t.Errorf("subject = %v, want the re-read report's %q", e.Properties["subject"], gogiosSample().Subject)
	}
}

// TestHandleGogiosClearCacheSurfacesAFetchErrorAfterClearing is the negative
// case: clearing the cache can succeed while the immediate re-fetch fails
// (the upstream is down). That must still render as a 200 with an "error"
// property -- handleClearCache delegates to handleOverview for
// rendering, so it inherits that convention rather than needing its own.
func TestHandleGogiosClearCacheSurfacesAFetchErrorAfterClearing(t *testing.T) {
	sf := reportSurface(&fakeReports{err: errFake{}})

	e, status, err := sf.handleClearCache(context.Background(), contract.State{}, contract.Request{})
	if err != nil {
		t.Fatalf("handleClearCache: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d (clearing succeeded; only the re-fetch failed)", status, http.StatusOK)
	}
	if e.Properties["error"] != (errFake{}).Error() {
		t.Errorf("error property = %v, want %q", e.Properties["error"], errFake{}.Error())
	}
}

// TestHandleGogiosClearCacheFailsOnAClearError pins the other negative case:
// a cache that cannot be cleared is a server fault (500), and the report is
// not re-read -- it would come from the very cache the clear failed to drop,
// presented as if the clear had worked.
func TestHandleGogiosClearCacheFailsOnAClearError(t *testing.T) {
	reports := &fakeReports{report: gogiosSample(), clearErr: errFake{}}
	sf := reportSurface(reports)

	_, status, err := sf.handleClearCache(context.Background(), contract.State{}, contract.Request{})
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", status, http.StatusInternalServerError)
	}
	if err == nil || !strings.Contains(err.Error(), (errFake{}).Error()) {
		t.Errorf("err = %v, want it to carry the clear error", err)
	}
	if want := []string{"clear"}; !equalLists(reports.calls, want) {
		t.Errorf("source calls = %v, want %v (no re-read after a failed clear)", reports.calls, want)
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

// TestDrillDownsFollowGogiosStatuses pins that the API's drill-down surface
// is built from gogios.Statuses, the list the local CLI and remote client
// also parse against: one /gogios/<status> route per category and one
// overview link per category, both in Statuses order, so no consumer can
// offer a category the others do not know.
func TestDrillDownsFollowGogiosStatuses(t *testing.T) {
	sf := testSurface()
	want := gogios.Statuses()

	// A drill-down is a GET /gogios/<status> named after its category; the
	// per-check lookup (/gogios/check) also matches that shape but takes a
	// ?name= query, which no drill-down does.
	var routes []string
	for _, r := range sf.Routes() {
		if status, ok := strings.CutPrefix(r.Path, "/gogios/"); ok && r.Name == "gogios-"+status && len(r.Query) == 0 {
			routes = append(routes, status)
		}
	}
	if !equalLists(routes, want) {
		t.Errorf("drill-down routes = %v, want %v", routes, want)
	}

	e, _, err := sf.handleOverview(context.Background(), WithReport(contract.State{}, gogiosSample(), nil), contract.Request{})
	if err != nil {
		t.Fatalf("handleOverview: %v", err)
	}
	var links []string
	for _, l := range e.Links {
		if status, ok := strings.CutPrefix(l.Href, "/gogios/"); ok && len(l.Rel) == 1 && l.Rel[0] == status {
			links = append(links, status)
		}
	}
	if !equalLists(links, want) {
		t.Errorf("overview drill-down links = %v, want %v", links, want)
	}
}
