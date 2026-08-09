package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power"
)

// TestOpenAPICoversEveryRoute is the guard that keeps the two halves of
// "self-describing" honest: the Siren actions a client sees at runtime and the
// OpenAPI document a generator reads must both come from routes(), with
// neither inventing nor omitting an endpoint.
func TestOpenAPICoversEveryRoute(t *testing.T) {
	s := &Server{base: "/cgi-bin/f3sctl"}
	doc := s.openAPIDoc()

	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatal("openapi document has no paths object")
	}

	for _, r := range routes() {
		if r.Path == openAPIPath {
			continue
		}
		entry, ok := paths[s.href(r.Path)].(map[string]any)
		if !ok {
			t.Errorf("route %q (%s %s) is missing from the OpenAPI document", r.Name, r.Method, r.Path)
			continue
		}
		if _, ok := entry[lower(r.Method)]; !ok {
			t.Errorf("route %q is in the document but not under method %s", r.Name, r.Method)
		}
	}

	// And nothing in the document that is not a real route.
	for path := range paths {
		found := false
		for _, r := range routes() {
			if s.href(r.Path) == path {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("OpenAPI document describes %q, which no route serves", path)
		}
	}
}

// TestRoutesAreUnique catches two routes claiming the same method and path,
// which would make dispatch depend on declaration order.
func TestRoutesAreUnique(t *testing.T) {
	seen := map[string]string{}
	for _, r := range routes() {
		key := r.Method + " " + r.Path
		if prev, dup := seen[key]; dup {
			t.Errorf("%s is served by both %q and %q", key, prev, r.Name)
		}
		seen[key] = r.Name
	}
}

// TestActionAvailability pins the state-dependent behaviour that clients rely
// on instead of hard-coding rules. If any of these flip, every client silently
// changes behaviour, so they are worth asserting explicitly.
func TestActionAvailability(t *testing.T) {
	fHost := func(name string, ping bool) power.HostStatus {
		return power.HostStatus{Name: name, Role: "f", Ping: ping, SSH: ping}
	}
	// A host that answers ICMP but has no sshd yet: mid-boot, or mid-shutdown.
	booting := func(name string) power.HostStatus {
		return power.HostStatus{Name: name, Role: "f", Ping: true, SSH: false}
	}

	allUp := State{
		Hosts: []power.HostStatus{fHost("f0", true), fHost("f1", true), fHost("f2", true), fHost("f3", true)},
		Fans:  power.FansState{On: true},
	}
	allDown := State{
		Hosts: []power.HostStatus{fHost("f0", false), fHost("f1", false), fHost("f2", false), fHost("f3", false)},
		Fans:  power.FansState{On: false},
	}
	partial := State{
		Hosts: []power.HostStatus{fHost("f0", true), fHost("f1", false), fHost("f2", true), fHost("f3", false)},
		Fans:  power.FansState{On: true},
	}
	busy := State{
		Hosts: allUp.Hosts,
		Fans:  power.FansState{On: true},
		Job:   &Job{State: JobRunning, Action: "off"},
	}

	cases := []struct {
		name   string
		state  State
		action string
		want   bool
		why    string
	}{
		{"all up", allUp, "power-on", false, "nothing to wake"},
		{"all up", allUp, "power-off", true, "there is something to stop"},
		{"all down", allDown, "power-on", true, "there is something to wake"},
		{"all down", allDown, "power-off", false, "nothing to stop"},
		{"partial", partial, "power-on", true, "f1 is still down"},
		{"partial", partial, "power-off", true, "f0 and f2 are still up"},
		{"all up", allUp, "f3-on", false, "f3 already answers"},
		{"all up", allUp, "f3-off", true, "f3 answers"},
		{"all down", allDown, "f3-on", true, "f3 is off"},
		{"all down", allDown, "f3-off", false, "f3 is already off"},

		// A running job withholds every power action. This is what lets a
		// client grey out buttons without knowing anything about 409.
		{"job running", busy, "power-on", false, "a job is running"},
		{"job running", busy, "power-off", false, "a job is running"},
		{"job running", busy, "f3-on", false, "a job is running"},
		{"job running", busy, "f3-off", false, "a job is running"},

		// Fan actions are not gated on jobs: switching the plug is
		// instantaneous and independent of an in-flight power sequence.
		{"fans on", allUp, "fans-on", false, "already on"},
		{"fans on", allUp, "fans-off", true, "on, so it can be switched off"},
		{"fans off", allDown, "fans-on", true, "off, so it can be switched on"},
		{"fans off", allDown, "fans-off", false, "already off"},
	}

	// A host that is only mid-boot must not be offered a shutdown: the whole
	// operation runs over SSH, so the job could only fail. This was a real
	// failure on 2026-08-08, when f3 was offered f3-off 48 seconds after
	// waking and the zusb pre-flight got "connection refused".
	midBoot := State{
		Hosts: []power.HostStatus{booting("f0"), booting("f1"), booting("f2"), booting("f3")},
		Fans:  power.FansState{On: true},
	}
	cases = append(cases,
		struct {
			name   string
			state  State
			action string
			want   bool
			why    string
		}{"mid-boot", midBoot, "power-off", false, "no sshd yet, so a shutdown could only fail"},
		struct {
			name   string
			state  State
			action string
			want   bool
			why    string
		}{"mid-boot", midBoot, "f3-off", false, "no sshd yet, so a shutdown could only fail"},
	)

	for _, tc := range cases {
		r, ok := routeByName(tc.action)
		if !ok {
			t.Fatalf("no route named %q", tc.action)
		}
		if got := r.available(tc.state); got != tc.want {
			t.Errorf("%s: %s available = %v, want %v (%s)", tc.name, tc.action, got, tc.want, tc.why)
		}
	}
}

// TestFansOffForceField pins the guard that keeps the rack from being left
// without cooling: the confirmation field appears only while a host is up.
func TestFansOffForceField(t *testing.T) {
	r, ok := routeByName("fans-off")
	if !ok {
		t.Fatal("no fans-off route")
	}

	hot := State{Hosts: []power.HostStatus{{Name: "f0", Role: "f", Ping: true}}, Fans: power.FansState{On: true}}
	cold := State{Hosts: []power.HostStatus{{Name: "f0", Role: "f", Ping: false}}, Fans: power.FansState{On: true}}

	fields := r.fields(hot)
	if len(fields) != 1 || fields[0].Name != "force" {
		t.Fatalf("expected a single 'force' field while a host is up, got %+v", fields)
	}
	if !fields[0].Required {
		t.Error("the force field must be required, or a client may omit it and be surprised by a 409")
	}
	if fields[0].Title == "" {
		t.Error("the force field needs a title: clients are told to show it rather than invent their own wording")
	}

	if got := r.fields(cold); len(got) != 0 {
		t.Errorf("expected no fields when the rack is cold, got %+v", got)
	}
}

// TestFansUnavailableWhenPlugUnreadable checks that an unreachable plug hides
// both fan actions. Offering a switch whose result cannot be read back would
// mean reporting success without evidence.
func TestFansUnavailableWhenPlugUnreadable(t *testing.T) {
	broken := State{FansErr: errFake{}}
	for _, name := range []string{"fans-on", "fans-off"} {
		r, _ := routeByName(name)
		if r.available(broken) {
			t.Errorf("%s should not be offered when the plug cannot be read", name)
		}
	}
}

// TestGETRoutesAreLinksNotActions ensures read-only routes never appear as
// actions, which clients treat as state changes.
func TestGETRoutesAreLinksNotActions(t *testing.T) {
	for _, r := range routes() {
		if r.Method == http.MethodGet && r.Action {
			t.Errorf("route %q is a GET but marked as an action", r.Name)
		}
		if r.Method == http.MethodPost && !r.Action {
			t.Errorf("route %q is a POST but not marked as an action", r.Name)
		}
	}
}

// TestEveryActionHasJobArgs catches a power action added to the registry
// without a matching CLI invocation, which would accept a request and then do
// nothing at all.
func TestEveryActionHasJobArgs(t *testing.T) {
	for _, r := range routes() {
		if !r.Action || !strings.HasPrefix(r.Path, "/power/") {
			continue
		}
		// The cluster-wide pair use bare verbs; per-host actions are
		// "<host>-on"/"<host>-off" and the action name is the route name.
		action := r.Name
		switch r.Name {
		case "power-on":
			action = "on"
		case "power-off":
			action = "off"
		}
		if args := jobArgs(action); len(args) == 0 {
			t.Errorf("action %q maps to no CLI invocation", r.Name)
		}
	}
}

// TestEveryFHostIsIndividuallyControllable pins that each f-host can be
// powered on its own, not only as part of the cluster-wide group.
func TestEveryFHostIsIndividuallyControllable(t *testing.T) {
	for _, h := range inventory.Default().ByRole(inventory.RoleF) {
		for _, verb := range []string{"on", "off"} {
			name := h.Name + "-" + verb
			if _, ok := routeByName(name); !ok {
				t.Errorf("no %q action; %s cannot be powered individually", name, h.Name)
			}
			if _, ok := lookup("POST", "/power/"+h.Name+"/"+verb); !ok {
				t.Errorf("no route serving POST /power/%s/%s", h.Name, verb)
			}
		}
	}
}

// TestMonitoringUnmuteIsReachableWithTheFleetUp is the regression test for the
// hole this resource was added to close.
//
// Un-muting used to happen only inside power-on, and power-on is withheld once
// every host answers. So the exact situation a stranded mute leaves behind --
// fleet up, alerting still suppressed -- was the one situation in which the API
// offered no way to clear it, and Gogios stayed blind until somebody SSHed to
// both gateways by hand.
func TestMonitoringUnmuteIsReachableWithTheFleetUp(t *testing.T) {
	allUp := State{
		Hosts: []power.HostStatus{
			{Name: "f0", Role: "f", Ping: true, SSH: true},
			{Name: "f1", Role: "f", Ping: true, SSH: true},
			{Name: "f2", Role: "f", Ping: true, SSH: true},
		},
		Monitoring: []power.GatewayMute{
			{Name: "blowfish", Muted: true},
			{Name: "fishfinger", Muted: true},
		},
	}

	if _, ok := routeByName("power-on"); !ok {
		t.Fatal("power-on route missing")
	}
	if r, _ := routeByName("power-on"); r.available(allUp) {
		t.Fatal("precondition failed: power-on should be withheld with the fleet up")
	}

	r, ok := routeByName("monitoring-unmute")
	if !ok {
		t.Fatal("no monitoring-unmute action")
	}
	if !r.available(allUp) {
		t.Error("monitoring-unmute withheld while the fleet is up and muted; " +
			"a stranded mute would be unclearable through the API")
	}
}

// TestMonitoringActionsAreMutuallyExclusive pins that exactly one of mute and
// un-mute is offered, so a client never renders both.
func TestMonitoringActionsAreMutuallyExclusive(t *testing.T) {
	for _, tc := range []struct {
		name  string
		muted bool
		want  string
	}{
		{"muted", true, "monitoring-unmute"},
		{"alerting", false, "monitoring-mute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := State{Monitoring: []power.GatewayMute{{Name: "blowfish", Muted: tc.muted}}}
			for _, name := range []string{"monitoring-mute", "monitoring-unmute"} {
				r, ok := routeByName(name)
				if !ok {
					t.Fatalf("no %q action", name)
				}
				if got := r.available(s); got != (name == tc.want) {
					t.Errorf("%s available=%v, want %v", name, got, name == tc.want)
				}
			}
		})
	}
}

// TestMonitoringActionsWithheldWhenUnknown pins that neither action is offered
// when the gateway state was not collected.
//
// Most routes never read the mute, because it costs an SSH round trip per
// gateway. Offering "mute" off the back of a zero value would mean offering it
// on every response that skipped the lookup.
func TestMonitoringActionsWithheldWhenUnknown(t *testing.T) {
	var unknown State // Monitoring is nil
	for _, name := range []string{"monitoring-mute", "monitoring-unmute"} {
		r, ok := routeByName(name)
		if !ok {
			t.Fatalf("no %q action", name)
		}
		if r.available(unknown) {
			t.Errorf("%s offered without having read the gateways", name)
		}
	}
}

// TestPowerActionsWithheldWhileThePeerIsBusy pins that a job on the *other* API
// node withholds power actions here too.
//
// relayd load-balances pi0 and pi1, so the node answering a request is often
// not the node running the job. Judging availability on local job state alone
// made the idle node advertise power-off, f0-off, f1-off and f2-off during a
// live shutdown -- every one of which 409s on contact. Observed 2026-08-09:
// pi1 correctly offered only fans-off while pi0 offered five power actions.
//
// A client is entitled to trust what it was handed; that is the entire premise
// of a self-describing API.
func TestPowerActionsWithheldWhileThePeerIsBusy(t *testing.T) {
	busy := State{
		Hosts: []power.HostStatus{
			{Name: "f0", Role: "f", Ping: true, SSH: true},
			{Name: "f1", Role: "f", Ping: true, SSH: true},
			{Name: "f2", Role: "f", Ping: true, SSH: true},
		},
		Fans:     power.FansState{On: true},
		PeerBusy: true,
		// Job is nil: this node itself is idle, which is the whole point.
	}

	for _, r := range routes() {
		if !r.Action || !strings.HasPrefix(r.Path, "/power/") {
			continue
		}
		if r.available(busy) {
			t.Errorf("%q offered while the peer node is running a job", r.Name)
		}
	}

	// The fan plug is not part of a power job, so it stays usable.
	if r, ok := routeByName("fans-off"); ok && !r.available(busy) {
		t.Error("fans-off withheld because the peer is busy; the plug is independent of power jobs")
	}
}

// TestAllActionsAccountForF3 pins that the all-on/all-off pair is judged
// against f3 as well, not just the cluster three.
//
// With f0-f2 up and only f3 down, "power on" is correctly withheld -- the
// cluster is already up -- but "all-on" must still be offered, or the group
// that exists specifically to include f3 would ignore it.
func TestAllActionsAccountForF3(t *testing.T) {
	onlyF3Down := State{
		Hosts: []power.HostStatus{
			{Name: "f0", Role: "f", Ping: true, SSH: true},
			{Name: "f1", Role: "f", Ping: true, SSH: true},
			{Name: "f2", Role: "f", Ping: true, SSH: true},
			{Name: "f3", Role: "f", Ping: false, SSH: false},
		},
	}

	if r, ok := routeByName("power-on"); ok && r.available(onlyF3Down) {
		t.Error("power-on offered with the whole cluster up; it ignores f3 by design")
	}
	if r, ok := routeByName("all-on"); !ok {
		t.Fatal("no all-on action")
	} else if !r.available(onlyF3Down) {
		t.Error("all-on withheld while f3 is down; the group exists to include f3")
	}
}

// TestAllOffNeedsSSHNotPing mirrors power-off: the shutdown runs over SSH, so a
// rack that only answers ICMP cannot be shut down and must not be offered.
func TestAllOffNeedsSSHNotPing(t *testing.T) {
	midBoot := State{
		Hosts: []power.HostStatus{
			{Name: "f0", Role: "f", Ping: true, SSH: false},
			{Name: "f1", Role: "f", Ping: true, SSH: false},
			{Name: "f2", Role: "f", Ping: true, SSH: false},
			{Name: "f3", Role: "f", Ping: true, SSH: false},
		},
	}
	r, ok := routeByName("all-off")
	if !ok {
		t.Fatal("no all-off action")
	}
	if r.available(midBoot) {
		t.Error("all-off offered while no host answers SSH; the job could only fail")
	}
}

func routeByName(name string) (route, bool) {
	for _, r := range routes() {
		if r.Name == name {
			return r, true
		}
	}
	return route{}, false
}

func lower(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'A' && r <= 'Z' {
			out[i] = r + 32
		}
	}
	return string(out)
}

type errFake struct{}

func (errFake) Error() string { return "plug unreachable" }
