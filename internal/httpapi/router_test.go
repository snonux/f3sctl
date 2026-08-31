package httpapi

import (
	"net/http"
	"testing"

	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power"
)

// TestRouterHrefBuildsUnderBase pins that every href is base + path, with the
// one special case of the root ("/") not becoming a doubled slash.
func TestRouterHrefBuildsUnderBase(t *testing.T) {
	rt := NewRouter("/cgi-bin/f3sctl", testRoutes(inventory.Default()))
	if got := rt.Href("/status"); got != "/cgi-bin/f3sctl/status" {
		t.Errorf("Href(/status) = %q", got)
	}
	if got := rt.Href("/"); got != "/cgi-bin/f3sctl/" {
		t.Errorf("Href(/) = %q, want the base with a trailing slash, not a doubled path", got)
	}
}

// TestRouterHrefWithEmptyBase covers a server mounted at the CGI root, where
// SCRIPT_NAME is empty.
func TestRouterHrefWithEmptyBase(t *testing.T) {
	rt := NewRouter("", testRoutes(inventory.Default()))
	if got := rt.Href("/status"); got != "/status" {
		t.Errorf("Href(/status) = %q, want /status", got)
	}
}

// TestRouterLookupFindsAnExactMethodAndPath pins ordinary dispatch.
func TestRouterLookupFindsAnExactMethodAndPath(t *testing.T) {
	rt := NewRouter("", testRoutes(inventory.Default()))
	r, ok := rt.Lookup(http.MethodGet, "/status")
	if !ok {
		t.Fatal("expected GET /status to resolve")
	}
	if r.Name != "status" {
		t.Errorf("resolved route name = %q, want status", r.Name)
	}
}

// TestRouterLookupMissesAWrongMethod is what separates a 404 from a 405: the
// path exists, but not for this method.
func TestRouterLookupMissesAWrongMethod(t *testing.T) {
	rt := NewRouter("", testRoutes(inventory.Default()))
	if _, ok := rt.Lookup(http.MethodPost, "/status"); ok {
		t.Error("POST /status should not resolve; status is a GET-only resource")
	}
	if !rt.PathExists("/status") {
		t.Error("PathExists(/status) = false, want true: the path is real even though POST is not")
	}
}

// TestRouterLookupMissesAnUnknownPath is the plain 404 case.
func TestRouterLookupMissesAnUnknownPath(t *testing.T) {
	rt := NewRouter("", testRoutes(inventory.Default()))
	if _, ok := rt.Lookup(http.MethodGet, "/no-such-resource"); ok {
		t.Error("expected no route to resolve for an unknown path")
	}
	if rt.PathExists("/no-such-resource") {
		t.Error("PathExists(/no-such-resource) = true, want false")
	}
}

// TestRouterLinksOmitsActions pins that only GET resources are rendered as
// links; state-changing routes belong in Actions instead, and never both.
func TestRouterLinksOmitsActions(t *testing.T) {
	rt := NewRouter("/cgi-bin/f3sctl", testRoutes(inventory.Default()))
	links := rt.Links()
	if len(links) == 0 {
		t.Fatal("expected at least one link")
	}
	for _, l := range links {
		for _, r := range testRoutes(inventory.Default()) {
			if r.Path == l.Href[len("/cgi-bin/f3sctl"):] && r.Action {
				t.Errorf("action route %q rendered as a link", r.Name)
			}
		}
	}
}

// TestRouterLinksOmitsNoRootLinkRoutes is the regression test for the
// gogios-check root-link bug: a GET resource whose href is meaningless
// without a query string (?name=...) must not appear in Router.Links(),
// since a client following docs/CLIENT.md's "use the href exactly as given"
// rule would otherwise 404 on every root fetch. Every other GET resource
// still must appear -- this pins the exclusion as an exception, not a
// regression that silently drops links wholesale.
func TestRouterLinksOmitsNoRootLinkRoutes(t *testing.T) {
	rt := NewRouter("", testRoutes(inventory.Default()))
	links := rt.Links()

	for _, l := range links {
		if len(l.Rel) > 0 && l.Rel[0] == "gogios-check" {
			t.Errorf("gogios-check rendered as a root link (%+v); its href is meaningless without ?name=", l)
		}
	}

	var sawGogios bool
	for _, l := range links {
		if len(l.Rel) > 0 && l.Rel[0] == "gogios" {
			sawGogios = true
		}
	}
	if !sawGogios {
		t.Error("the gogios overview route was not rendered as a root link, want it present")
	}
}

// TestRouterActionsForNarrowsToTheNamedRoutes pins that a resource-scoped
// actions list never leaks an action belonging to a different resource.
func TestRouterActionsForNarrowsToTheNamedRoutes(t *testing.T) {
	rt := NewRouter("", testRoutes(inventory.Default()))
	// The fan plug readably on, so fans-off (and only fans-off, of the pair)
	// is available; every other action stays withheld by its own Available
	// predicate regardless of the name filter.
	state := contract.State{Fans: power.FansState{On: true}}
	got := rt.actionsFor(state, "fans-on", "fans-off")
	if len(got) != 1 || got[0].Name != "fans-off" {
		t.Fatalf("ActionsFor(fans-on, fans-off) = %v, want just [fans-off]", names(got))
	}
	for _, a := range got {
		if a.Name != "fans-on" && a.Name != "fans-off" {
			t.Errorf("ActionsFor(fans-on, fans-off) included %q", a.Name)
		}
	}
}

func names(actions []contract.Action) []string {
	out := make([]string, len(actions))
	for i, a := range actions {
		out[i] = a.Name
	}
	return out
}
