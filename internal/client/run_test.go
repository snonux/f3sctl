package client

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
)

// TestJobWaitTimeoutAtLeastUnmuteTimeoutPlusBuffer is the regression test for
// the 2026-08-09 bug: waitForJob's poll deadline (jobWaitTimeout) must exceed
// the server's worst-case job runtime, which for `power on` is anchored on
// cfg.UnmuteTimeout (see jobWaitTimeout's doc comment and
// internal/gogios/monitor.go waitForCluster). A deadline that is only equal to
// UnmuteTimeout, with no buffer for the wake prelude and gateway SSH round
// trips, is exactly what let the client report "gave up waiting for the job"
// moments before the job actually finished.
func TestJobWaitTimeoutAtLeastUnmuteTimeoutPlusBuffer(t *testing.T) {
	cfg := config.Default()
	c := &Client{cfg: cfg}

	got := c.jobWaitTimeout()
	min := cfg.UnmuteTimeout.D() + jobWaitBuffer

	if got < min {
		t.Fatalf("jobWaitTimeout() = %s, want at least UnmuteTimeout + buffer = %s", got, min)
	}
}

// TestJobWaitTimeoutTracksConfiguredUnmuteTimeout is the negative case: a
// non-default UnmuteTimeout (an operator budgeting for a slower gateway, as
// happened 2026-08-09 when UnmuteTimeout itself was raised 600s -> 1200s)
// must actually change jobWaitTimeout's result. Falling back to a hardcoded
// value here would silently reintroduce the drift the fix exists to close,
// just against a different, config-invisible number.
func TestJobWaitTimeoutTracksConfiguredUnmuteTimeout(t *testing.T) {
	cfg := config.Default()
	cfg.UnmuteTimeout = config.Duration(37 * time.Minute)
	c := &Client{cfg: cfg}

	got := c.jobWaitTimeout()
	want := 37*time.Minute + jobWaitBuffer

	if got != want {
		t.Fatalf("jobWaitTimeout() with UnmuteTimeout=37m = %s, want %s", got, want)
	}

	defaultTimeout := (&Client{cfg: config.Default()}).jobWaitTimeout()
	if got == defaultTimeout {
		t.Fatalf("jobWaitTimeout() = %s did not change when UnmuteTimeout was configured away from its default", got)
	}
}

// TestParseHostReadsPingKnown is the regression test for hz0 at the point the
// bug actually lived: the old showStatus built its table row inline and read
// only ping/ssh off the entity, never pingKnown, so a host the probe never
// reached was indistinguishable here from one it measured and found silent.
// parseHost is what feeds presenter.Describe now, so this pins that the
// pingKnown property actually reaches the power.HostStatus it builds.
func TestParseHostReadsPingKnown(t *testing.T) {
	measured, ok := parseHost(Entity{Properties: map[string]any{
		"name": "f3", "ip": "192.168.1.13", "ping": false, "pingKnown": true, "ssh": false,
	}})
	if !ok || measured.PingKnown != true {
		t.Errorf("parseHost(pingKnown=true) = %+v, ok=%v, want PingKnown true", measured, ok)
	}

	unmeasured, ok := parseHost(Entity{Properties: map[string]any{
		"name": "f3", "ip": "192.168.1.13", "ping": false, "pingKnown": false, "ssh": false,
	}})
	if !ok || unmeasured.PingKnown != false {
		t.Errorf("parseHost(pingKnown=false) = %+v, ok=%v, want PingKnown false", unmeasured, ok)
	}
}

// TestParseHostDefaultsPingKnownWhenAbsent pins the back-compat rule
// docs/client-reference.js documents for the same field: a server that
// predates pingKnown omits the property entirely, and its absence means the
// probe ran (not that it didn't) -- so the zero value of a missing bool
// property, which is false, must not be read as PingKnown=false here.
func TestParseHostDefaultsPingKnownWhenAbsent(t *testing.T) {
	st, ok := parseHost(Entity{Properties: map[string]any{
		"name": "f3", "ip": "192.168.1.13", "ping": false, "ssh": false,
	}})
	if !ok || !st.PingKnown {
		t.Errorf("parseHost with pingKnown absent = %+v, ok=%v, want PingKnown true (older-server default)", st, ok)
	}
}

// TestParseFansReportsAnErrorPropertyAsAnError pins that a "fans" entity
// carrying an error property (the plug was unreachable) turns into a Go
// error, not a FansState claiming the plug is off -- see parseFans and
// presenter.Status.
func TestParseFansReportsAnErrorPropertyAsAnError(t *testing.T) {
	_, err := parseFans(Entity{Properties: map[string]any{"error": "dial tcp: timeout"}})
	if err == nil || err.Error() != "dial tcp: timeout" {
		t.Errorf("parseFans with an error property = %v, want it surfaced as an error", err)
	}

	fans, err := parseFans(Entity{Properties: map[string]any{"on": true, "ip": "192.168.1.99"}})
	if err != nil {
		t.Fatalf("parseFans with no error property: %v", err)
	}
	if !fans.On || fans.IP != "192.168.1.99" {
		t.Errorf("parseFans(on=true) = %+v, want On=true IP=192.168.1.99", fans)
	}
}

// TestServerStaleCeilingReadsTheJobProperty pins how the accepted job's
// advertised staleness ceiling is read: a decoded JSON number (float64) in
// seconds, and zero -- "no opinion" -- when the property is absent, so an
// older server leaves waitForJob on its own jobWaitTimeout.
func TestServerStaleCeilingReadsTheJobProperty(t *testing.T) {
	job := Entity{Properties: map[string]any{"staleAfterSeconds": float64(2880)}}
	if got, want := serverStaleCeiling(job), 48*time.Minute; got != want {
		t.Errorf("serverStaleCeiling = %s, want %s", got, want)
	}
	if got := serverStaleCeiling(Entity{}); got != 0 {
		t.Errorf("serverStaleCeiling without the property = %s, want 0", got)
	}
}

// newCapturingClient is newTestClient with stdout captured, for tests that
// assert on what the remote path prints.
func newCapturingClient(t *testing.T, base, key string) (*Client, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	c, err := New(base, key, config.Default(), &out)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return c, &out
}

// statusFixtureActions is what the fake /status advertises: two actions
// possible right now, in the order the line must list them by CLI verb.
var statusFixtureActions = []Action{
	{Name: "power-on", Method: http.MethodPost, Href: "/power/on", CLIVerb: "power on"},
	{Name: "fans-off", Method: http.MethodPost, Href: "/fans/off", CLIVerb: "fans off"},
}

// TestRunStatusListsTheStatusEntitysActions is the regression test for ma:
// since the section folders the root renders no actions, so a showStatus
// reading root.Actions never printed the "available now" line. The fake root
// carries a decoy action to pin that the line comes from /status, not root.
func TestRunStatusListsTheStatusEntitysActions(t *testing.T) {
	for _, cmd := range [][]string{{"power", "status"}, {"fans", "status"}, {"ac", "status"}} {
		t.Run(strings.Join(cmd, " "), func(t *testing.T) {
			api := newFakeAPI(t, "key")
			api.rootActions = []Action{{Name: "root-decoy", Method: http.MethodPost, Href: "/decoy"}}
			api.statusActions = statusFixtureActions
			c, out := newCapturingClient(t, api.srv.URL, "key")

			if err := Run(context.Background(), c, cmd, false); err != nil {
				t.Fatalf("Run(%v): %v", cmd, err)
			}
			if want := "available now: power on, fans off\n"; !strings.Contains(out.String(), want) {
				t.Errorf("output = %q, want it to contain %q", out.String(), want)
			}
			if strings.Contains(out.String(), "root-decoy") {
				t.Errorf("output = %q lists the root's action; the line must come from /status", out.String())
			}
		})
	}
}

// TestRunRefusedActionListsWhatIsAvailable pins the refused-action path of
// runAction: a verb its holder does not advertise is reported as
// unavailable, and the status it was judged against -- including what IS
// possible -- follows.
//
// The holder is the /power section folder, reached through the root's
// "power" link as on the real server, and it withholds power off. The root
// carries a decoy power-off that would be performed (a POST the fake 404s)
// if runAction fell back to the root instead of reading the folder.
func TestRunRefusedActionListsWhatIsAvailable(t *testing.T) {
	api := newFakeAPI(t, "key")
	api.rootActions = []Action{{Name: "power-off", Method: http.MethodPost, Href: "/power/off", CLIVerb: "power off"}}
	api.powerActions = statusFixtureActions[:1] // power on only: power off is withheld
	api.statusActions = statusFixtureActions
	c, out := newCapturingClient(t, api.srv.URL, "key")

	if err := Run(context.Background(), c, []string{"power", "off"}, false); err != nil {
		t.Fatalf("Run(power off): %v", err)
	}
	if !slices.Contains(api.getPaths(), "/power") {
		t.Errorf("GETs = %v, want the /power section folder resolved as the holder", api.getPaths())
	}
	got := out.String()
	if !strings.Contains(got, `"power off" is not available right now.`) {
		t.Errorf("output = %q, want the refusal", got)
	}
	if !strings.Contains(got, "available now: power on, fans off\n") {
		t.Errorf("output = %q, want the actions /status advertises", got)
	}
}

// TestRunStatusWithNoActionsPrintsNoAvailableLine is the negative case: a
// status entity advertising nothing (everything withheld) must print no
// "available now" line at all, not an empty one.
func TestRunStatusWithNoActionsPrintsNoAvailableLine(t *testing.T) {
	api := newFakeAPI(t, "key")
	c, out := newCapturingClient(t, api.srv.URL, "key")

	if err := Run(context.Background(), c, []string{"power", "status"}, false); err != nil {
		t.Fatalf("Run(power status): %v", err)
	}
	if strings.Contains(out.String(), "available now") {
		t.Errorf("output = %q, want no available-now line when /status advertises nothing", out.String())
	}
}

// TestPrintAvailableFallsBackToTheActionName pins the legacy half of the
// line: an action without a CLIVerb (a server predating the field) is shown
// by its name, while one carrying a CLIVerb is shown as the command to type.
func TestPrintAvailableFallsBackToTheActionName(t *testing.T) {
	var out bytes.Buffer
	c := &Client{stdout: &out}
	c.printAvailable([]Action{
		{Name: "power-on", CLIVerb: "power on"},
		{Name: "fans-off"},
	})
	if got, want := out.String(), "\navailable now: power on, fans-off\n"; got != want {
		t.Errorf("printAvailable = %q, want %q", got, want)
	}
}

// TestRunWithNoArgsIsAnError is the negative case for Run's routing: with no
// noun to route on it must return errNoCommand rather than index args[0]
// (a panic) or reach the network. The client has no API behind it, so any
// request it attempted would fail differently.
func TestRunWithNoArgsIsAnError(t *testing.T) {
	for _, args := range [][]string{nil, {}} {
		var out bytes.Buffer
		c := &Client{stdout: &out}

		err := Run(context.Background(), c, args, false)
		if !errors.Is(err, errNoCommand) {
			t.Errorf("Run(%#v) = %v, want errNoCommand", args, err)
		}
		if out.Len() != 0 {
			t.Errorf("Run(%#v) printed %q, want nothing", args, out.String())
		}
	}
}

// TestRunUnknownNounFallsBackToTheRackFollowUp is the negative case for the
// follow-up table: a noun neither nounHolderPath nor nounFollowUp lists is
// looked up on the root, reported unavailable, and followed by the rack
// status -- the table's default -- with nothing performed.
func TestRunUnknownNounFallsBackToTheRackFollowUp(t *testing.T) {
	api := newFakeAPI(t, "key")
	api.statusActions = statusFixtureActions
	c, out := newCapturingClient(t, api.srv.URL, "key")

	if err := Run(context.Background(), c, []string{"bogus", "zap"}, false); err != nil {
		t.Fatalf("Run(bogus zap): %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `"bogus zap" is not available right now.`) {
		t.Errorf("output = %q, want the refusal", got)
	}
	if !strings.Contains(got, "available now: power on, fans off\n") {
		t.Errorf("output = %q, want the rack status's actions", got)
	}
	if !slices.Contains(api.getPaths(), "/status") {
		t.Errorf("GETs = %v, want /status rendered as the follow-up", api.getPaths())
	}
}

// TestNounFollowUpTable pins the follow-up each noun's actions get: which
// view re-renders the state they changed, whether a synchronous success is
// announced as "<action>: done", and the hint an interrupted request gives.
// An unknown noun gets the rack's, the same fallback resolveHolder makes.
func TestNounFollowUpTable(t *testing.T) {
	tests := []struct {
		noun string
		want followUp
	}{
		{"power", followUp{view: viewRack, sayDone: true, checkHint: checkRackHint}},
		{"fans", followUp{view: viewRack, sayDone: true, checkHint: checkRackHint}},
		{"ac", followUp{view: viewRack, sayDone: true, checkHint: checkRackHint}},
		{"monitoring", followUp{view: viewMonitoring, sayDone: false, checkHint: checkMonitoringHint}},
		{"gogios", followUp{view: viewGogios, sayDone: false, checkHint: checkMonitoringHint}},
		{"bogus", followUp{view: viewRack, sayDone: true, checkHint: checkRackHint}},
		{"", followUp{view: viewRack, sayDone: true, checkHint: checkRackHint}},
	}
	for _, tc := range tests {
		if got := followUpFor(tc.noun); got != tc.want {
			t.Errorf("followUpFor(%q) = %+v, want %+v", tc.noun, got, tc.want)
		}
	}
}

// TestNounFollowUpCoversEveryHolderNoun keeps the two per-noun tables in
// step: a noun that gains a holder path must also say how its actions are
// followed up, rather than silently getting the rack's default.
func TestNounFollowUpCoversEveryHolderNoun(t *testing.T) {
	for noun := range nounHolderPath {
		if _, ok := nounFollowUp[noun]; !ok {
			t.Errorf("nounHolderPath lists %q but nounFollowUp does not", noun)
		}
	}
	for noun := range nounFollowUp {
		if _, ok := nounHolderPath[noun]; !ok {
			t.Errorf("nounFollowUp lists %q but nounHolderPath does not", noun)
		}
	}
}

// TestInterruptedInFlightHintFollowsTheTable pins interruptedInFlight to
// nounFollowUp: the hint it prints for each noun is that noun's checkHint,
// so the Ctrl-C advice and the rendered follow-up cannot disagree.
func TestInterruptedInFlightHintFollowsTheTable(t *testing.T) {
	for _, noun := range []string{"power", "fans", "ac", "monitoring", "gogios", "bogus"} {
		var out bytes.Buffer
		c := &Client{stdout: &out}

		err := c.interruptedInFlight("some-action", noun, context.Canceled)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s: interruptedInFlight = %v, want a wrapped context.Canceled", noun, err)
		}
		if want := followUpFor(noun).checkHint + "\n"; !strings.HasSuffix(out.String(), want) {
			t.Errorf("%s: output = %q, want it to end with %q", noun, out.String(), want)
		}
	}
}
