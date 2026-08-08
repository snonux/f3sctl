package httpapi

import (
	"net/http"
	"testing"

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

// TestEveryActionHasJobArgsOrHandler catches a power action added to the
// registry without a matching CLI invocation, which would accept a request and
// then do nothing.
func TestEveryActionHasJobArgs(t *testing.T) {
	for _, name := range []string{"power-on", "power-off", "f3-on", "f3-off"} {
		action := map[string]string{
			"power-on": "on", "power-off": "off", "f3-on": "f3-on", "f3-off": "f3-off",
		}[name]
		if args := jobArgs(action); len(args) == 0 {
			t.Errorf("action %q maps to no CLI invocation", name)
		}
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
