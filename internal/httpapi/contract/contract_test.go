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
