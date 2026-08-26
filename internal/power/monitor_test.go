package power

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/inventory"
)

// This file is the first direct coverage of the Gogios monitoring concern,
// which before o51 lived untested on the Engine. The extraction onto Monitor
// gives the concern its own faking points -- the gatewayVerb transport and a
// probe func -- so the behaviour can be pinned without an Engine, an SSH key,
// or a real network: a fake verb records what was asked, and a fake probe
// says whether the k3s nodes are up.

// fakeGatewayVerb is the gatewayVerb stand-in: it records every
// gogios-mute/gogios-unmute/gogios-status call (verb:host, in order) and
// returns a scripted stdout or error per (verb, host).
type fakeGatewayVerb struct {
	mu    sync.Mutex
	calls []string

	out map[string]string // "verb:host" -> stdout
	err map[string]error  // "verb:host" -> error
}

func (f *fakeGatewayVerb) AgentVerb(_ context.Context, h inventory.Host, verb string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, verb+":"+h.Name)
	key := verb + ":" + h.Name
	return f.out[key], f.err[key]
}

func (f *fakeGatewayVerb) callsList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// newTestMonitor builds a Monitor over named gateways and (for UnmuteGogios's
// wait) named cluster nodes, with the given verb and probe and a negligible
// un-mute budget. Named hosts only -- no real inventory -- so the test says
// exactly what the Monitor reaches.
func newTestMonitor(t *testing.T, verb *fakeGatewayVerb, probe func(context.Context, []inventory.Host) []HostStatus, gateways, nodes []string, unmute time.Duration) *Monitor {
	t.Helper()
	toHosts := func(names []string) []inventory.Host {
		var hs []inventory.Host
		for _, n := range names {
			hs = append(hs, inventory.Host{Name: n, IP: "192.0.2.1", SSHPort: 22, SSHUser: "u"})
		}
		return hs
	}
	return &Monitor{
		gateways: toHosts(gateways),
		nodes:    toHosts(nodes),
		unmute:   unmute,
		verb:     verb,
		probe:    probe,
	}
}

// TestMonitorMuteRunsTheVerbOnEveryGateway pins eachGateway's contract for
// the mute path: the verb runs once per gateway, in order, and the log says
// it muted each -- so an operator can see which gateway answered.
func TestMonitorMuteRunsTheVerbOnEveryGateway(t *testing.T) {
	verb := &fakeGatewayVerb{out: map[string]string{}, err: map[string]error{}}
	m := newTestMonitor(t, verb, nil, []string{"blowfish", "sunfish"}, nil, time.Minute)

	var log bytes.Buffer
	if err := m.Mute(context.Background(), &log); err != nil {
		t.Fatalf("Mute: %v", err)
	}
	if got := verb.callsList(); len(got) != 2 || got[0] != "gogios-mute:blowfish" || got[1] != "gogios-mute:sunfish" {
		t.Errorf("gogios-mute calls = %v, want [gogios-mute:blowfish gogios-mute:sunfish] in order", got)
	}
	if !strings.Contains(log.String(), "Gogios muted on blowfish") || !strings.Contains(log.String(), "Gogios muted on sunfish") {
		t.Errorf("log = %q, want it to report both gateways muted", log.String())
	}
}

// TestMonitorMuteContinuesAfterAFailedGatewayAndReportsIt pins eachGateway's
// "both are tried even if the first fails" rule: the two gateways are
// independent Gogios installs, and muting one of two is strictly better than
// muting neither. A run with one failing must still reach the other and must
// surface the failure rather than report success.
func TestMonitorMuteContinuesAfterAFailedGatewayAndReportsIt(t *testing.T) {
	verb := &fakeGatewayVerb{
		out: map[string]string{},
		err: map[string]error{"gogios-mute:blowfish": errors.New("ssh: connect timed out")},
	}
	m := newTestMonitor(t, verb, nil, []string{"blowfish", "sunfish"}, nil, time.Minute)

	var log bytes.Buffer
	err := m.Mute(context.Background(), &log)
	if err == nil {
		t.Fatal("Mute with a failing gateway succeeded, want an error naming it")
	}
	if !strings.Contains(err.Error(), "blowfish") {
		t.Errorf("error = %v, want it to name the failed gateway", err)
	}
	if got := verb.callsList(); len(got) != 2 {
		t.Errorf("gogios-mute calls = %v, want both gateways tried even after the first failed", got)
	}
	if !strings.Contains(log.String(), "Gogios muted on sunfish") {
		t.Errorf("log = %q, want it to report the second gateway still succeeded", log.String())
	}
}

// TestMonitorStatusMapsTheVerbOutputToMuted pins the wire format the
// gogios-status verb speaks: "muted" means suppressed, anything else means
// alerting, and a verb error is reported as Err rather than a guessed state.
func TestMonitorStatusMapsTheVerbOutputToMuted(t *testing.T) {
	verb := &fakeGatewayVerb{
		out: map[string]string{
			"gogios-status:blowfish": "muted",
			"gogios-status:sunfish":  "alerting",
		},
		err: map[string]error{"gogios-status:goldfish": errors.New("ssh: refused")},
	}
	m := newTestMonitor(t, verb, nil, []string{"blowfish", "sunfish", "goldfish"}, nil, time.Minute)

	states := m.Status(context.Background())
	want := map[string]struct {
		muted bool
		err   bool
	}{
		"blowfish": {muted: true, err: false},
		"sunfish":  {muted: false, err: false},
		"goldfish": {muted: false, err: true},
	}
	if len(states) != len(want) {
		t.Fatalf("Status = %d entries, want %d", len(states), len(want))
	}
	for _, st := range states {
		w, ok := want[st.Name]
		if !ok {
			t.Errorf("unexpected gateway %q in Status", st.Name)
			continue
		}
		if st.Muted != w.muted {
			t.Errorf("%s: Muted = %v, want %v", st.Name, st.Muted, w.muted)
		}
		if w.err && st.Err == nil {
			t.Errorf("%s: Err = nil, want one", st.Name)
		}
		if !w.err && st.Err != nil {
			t.Errorf("%s: Err = %v, want nil", st.Name, st.Err)
		}
	}
}

// TestMonitorUnmuteDoesNotWaitForTheCluster pins the difference between
// Unmute (the operator's escape hatch) and UnmuteGogios (the wake path's
// wait-then-clear): Unmute runs gogios-unmute on every gateway straight away,
// never probing the k3s nodes.
func TestMonitorUnmuteDoesNotWaitForTheCluster(t *testing.T) {
	probeCalled := false
	probe := func(context.Context, []inventory.Host) []HostStatus {
		probeCalled = true
		return nil
	}
	verb := &fakeGatewayVerb{out: map[string]string{}, err: map[string]error{}}
	m := newTestMonitor(t, verb, probe, []string{"blowfish", "sunfish"}, []string{"r0", "r1", "r2"}, time.Minute)

	var log bytes.Buffer
	if err := m.Unmute(context.Background(), &log); err != nil {
		t.Fatalf("Unmute: %v", err)
	}
	if probeCalled {
		t.Error("Unmute probed the cluster, want it to clear the marker without waiting")
	}
	if got := verb.callsList(); len(got) != 2 || got[0] != "gogios-unmute:blowfish" || got[1] != "gogios-unmute:sunfish" {
		t.Errorf("gogios-unmute calls = %v, want one per gateway", got)
	}
}

// TestMonitorUnmuteGogiosWaitsForTheClusterThenClears pins the happy path:
// with the cluster up, UnmuteGogios waits (probe says all nodes answer) and
// then runs gogios-unmute on every gateway.
func TestMonitorUnmuteGogiosWaitsForTheClusterThenClears(t *testing.T) {
	allUp := func(_ context.Context, hosts []inventory.Host) []HostStatus {
		out := make([]HostStatus, len(hosts))
		for i, h := range hosts {
			out[i] = HostStatus{Name: h.Name, Ping: true}
		}
		return out
	}
	verb := &fakeGatewayVerb{out: map[string]string{}, err: map[string]error{}}
	m := newTestMonitor(t, verb, allUp, []string{"blowfish", "sunfish"}, []string{"r0", "r1", "r2"}, time.Minute)

	var log bytes.Buffer
	if err := m.UnmuteGogios(context.Background(), &log); err != nil {
		t.Fatalf("UnmuteGogios: %v", err)
	}
	if got := verb.callsList(); len(got) != 2 || got[0] != "gogios-unmute:blowfish" {
		t.Errorf("calls = %v, want gogios-unmute on each gateway after the cluster answered", got)
	}
}

// TestMonitorUnmuteGogiosLeavesTheMarkerWhenTheClusterNeverAnswers pins the
// failure mode UnmuteGogios's doc promises: when the wait expires, it does
// NOT clear the marker (clearing it while the nodes boot would fire the very
// alerts the mute suppresses), leaves it in place, prints how to clear it by
// hand, and returns the error.
func TestMonitorUnmuteGogiosLeavesTheMarkerWhenTheClusterNeverAnswers(t *testing.T) {
	oneDown := func(_ context.Context, hosts []inventory.Host) []HostStatus {
		out := make([]HostStatus, len(hosts))
		for i, h := range hosts {
			out[i] = HostStatus{Name: h.Name, Ping: i != 0} // r0 never answers
		}
		return out
	}
	verb := &fakeGatewayVerb{out: map[string]string{}, err: map[string]error{}}
	// A negligible budget so the wait expires on the first probe rather than
	// after a real 15s tick.
	// A negative budget makes the wait already expired on its first
	// check, so the test does not pay waitForCluster's real 15s poll gap:
	// the point is "wait fails -> marker left", not the wait itself.
	m := newTestMonitor(t, verb, oneDown, []string{"blowfish", "sunfish"}, []string{"r0", "r1", "r2"}, -time.Second)

	var log bytes.Buffer
	err := m.UnmuteGogios(context.Background(), &log)
	if err == nil {
		t.Fatal("UnmuteGogios with a never-answering cluster succeeded, want an error")
	}
	if got := verb.callsList(); len(got) != 0 {
		t.Errorf("gogios-unmute calls = %v, want none: the marker must stay in place when the wait fails", got)
	}
	if !strings.Contains(log.String(), "Leaving Gogios muted") {
		t.Errorf("log = %q, want it to say the marker is left in place", log.String())
	}
	if !strings.Contains(log.String(), "ssh -p 22 u@192.0.2.1 gogios-unmute") {
		t.Errorf("log = %q, want it to print how to clear the marker by hand", log.String())
	}
}

// TestAnyMutedKeysOnAnyRatherThanAll pins the "one of two muted is still a
// gap" rule: the state a failed un-mute leaves behind is one muted gateway,
// not two, and the offer to un-mute must appear for that case.
func TestAnyMutedKeysOnAnyRatherThanAll(t *testing.T) {
	if !AnyMuted([]GatewayMute{{Name: "a", Muted: true}, {Name: "b"}}) {
		t.Error("AnyMuted with one muted of two = false, want true")
	}
	if !AnyMuted([]GatewayMute{{Name: "a", Muted: true}, {Name: "b", Muted: true}}) {
		t.Error("AnyMuted with both muted = false, want true")
	}
	if AnyMuted([]GatewayMute{{Name: "a"}, {Name: "b"}}) {
		t.Error("AnyMuted with none muted = true, want false")
	}
	// A gateway whose status read errored is not "muted": an unknown state is
	// not evidence of suppression.
	if AnyMuted([]GatewayMute{{Name: "a", Err: errors.New("ssh: refused")}}) {
		t.Error("AnyMuted with an errored gateway = true, want false: an unknown state is not muted")
	}
}
