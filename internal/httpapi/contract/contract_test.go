package contract

import "testing"

// TestHrefBuildsUnderBase pins the one wire rule both surfaces and the Router
// share: every href is base + path, with the single exception of the root,
// whose href carries the trailing slash a client resolves every other href
// against (rather than a doubled slash), and the empty-base mount case.
func TestHrefBuildsUnderBase(t *testing.T) {
	for _, tc := range []struct {
		base, path, want string
	}{
		{base: "/cgi-bin/f3sctl", path: "/status", want: "/cgi-bin/f3sctl/status"},
		{base: "/cgi-bin/f3sctl", path: "/", want: "/cgi-bin/f3sctl/"},
		{base: "", path: "/status", want: "/status"},
		{base: "", path: "/", want: "/"},
	} {
		if got := Href(tc.base, tc.path); got != tc.want {
			t.Errorf("Href(%q, %q) = %q, want %q", tc.base, tc.path, got, tc.want)
		}
	}
}

// TestHrefsBoundBuilder pins the request-time shape the surfaces carry: the
// bound builder produces the same hrefs Href itself does.
func TestHrefsBoundBuilder(t *testing.T) {
	h := Hrefs("/cgi-bin/f3sctl")
	if got := h("/gogios"); got != "/cgi-bin/f3sctl/gogios" {
		t.Errorf("Hrefs(\"/cgi-bin/f3sctl\")(\"/gogios\") = %q, want /cgi-bin/f3sctl/gogios", got)
	}
}

// TestJobActionFallsBackToName pins the one override (power-on/power-off keep
// their pre-registry job names "on"/"off") and the default: a route that
// declares no JobActionName is matched by its own Name.
func TestJobActionFallsBackToName(t *testing.T) {
	if got := (Route{Name: "fans-on"}).JobAction(); got != "fans-on" {
		t.Errorf("JobAction() = %q, want the route's Name", got)
	}
	if got := (Route{Name: "power-on", JobActionName: "on"}).JobAction(); got != "on" {
		t.Errorf("JobAction() = %q, want the declared override", got)
	}
}

// TestResponseKindStatus pins the success status each ResponseKind stands
// for, and that the zero value -- what every route not declaring Response
// gets -- is the synchronous 200, not the job 202.
func TestResponseKindStatus(t *testing.T) {
	for _, tc := range []struct {
		kind ResponseKind
		want int
	}{
		{kind: ResponseSync, want: 200},
		{kind: ResponseJob, want: 202},
		{kind: Route{}.Response, want: 200},
	} {
		if got := tc.kind.Status(); got != tc.want {
			t.Errorf("ResponseKind(%d).Status() = %d, want %d", tc.kind, got, tc.want)
		}
	}
}

// TestNeedHas pins Need as a set: Has is true only when every need asked
// about is declared, and the zero value -- a route declaring nothing --
// has none of them.
func TestNeedHas(t *testing.T) {
	both := NeedMonitoring | NeedReport
	for _, tc := range []struct {
		set, x Need
		want   bool
	}{
		{set: both, x: NeedMonitoring, want: true},
		{set: both, x: NeedReport, want: true},
		{set: both, x: both, want: true},
		{set: both, x: NeedPeerBusy, want: false},
		{set: NeedMonitoring, x: both, want: false},
		{set: Route{}.Needs, x: NeedPeerBusy, want: false},
	} {
		if got := tc.set.Has(tc.x); got != tc.want {
			t.Errorf("Need(%#x).Has(%#x) = %v, want %v", tc.set, tc.x, got, tc.want)
		}
	}
}
