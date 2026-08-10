package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power"
)

// fakePlug stands in for the rack-fan Shelly plug, recording every Switch.Set.
//
// The tests below ask exactly one question -- "did the plug get switched off?"
// -- so recording the calls is the whole fixture. Digest auth is not offered:
// the engine only performs the challenge-response after a 401.
type fakePlug struct {
	srv *httptest.Server

	mu   sync.Mutex
	on   bool
	sets []bool
}

func newFakePlug(t *testing.T) *fakePlug {
	t.Helper()
	p := &fakePlug{on: true}
	p.srv = httptest.NewServer(http.HandlerFunc(p.handle))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakePlug) handle(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch r.URL.Path {
	case "/rpc/Switch.Set":
		on := r.URL.Query().Get("on") == "true"
		p.sets = append(p.sets, on)
		p.on = on
		_ = json.NewEncoder(w).Encode(map[string]bool{"was_on": !on})
	case "/rpc/Switch.GetStatus":
		_ = json.NewEncoder(w).Encode(map[string]bool{"output": p.on})
	default:
		http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
	}
}

func (p *fakePlug) setCalls() []bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bool(nil), p.sets...)
}

// testServer returns a Server whose fan plug is the fake one and whose
// confirming rack probe is confirm, so no ICMP leaves the box.
func testServer(t *testing.T, plug *fakePlug, confirm func(context.Context) power.RackActivity) *Server {
	t.Helper()

	pwFile := filepath.Join(t.TempDir(), "shelly_plug")
	if err := os.WriteFile(pwFile, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatalf("writing the Shelly password file: %v", err)
	}

	cfg := config.Default()
	cfg.ShellyPasswordFile = []string{pwFile}
	cfg.Inventory.ShellyIP = strings.TrimPrefix(plug.srv.URL, "http://")

	eng, err := power.New(cfg)
	if err != nil {
		t.Fatalf("power.New: %v", err)
	}
	return &Server{cfg: cfg, engine: eng, node: "test", router: NewRouter(""), rackConfirm: confirm}
}

// fState builds one f-host's probe result. known=false is the probe that never
// happened, which is the case the whole guard turns on.
func fState(name string, ping, known bool) power.HostStatus {
	return power.HostStatus{Name: name, Role: string(inventory.RoleF), Ping: ping, PingKnown: known}
}

// coldSnapshot is a rack every host of which was probed and found silent.
func coldSnapshot() State {
	return State{
		Hosts: []power.HostStatus{
			fState("f0", false, true), fState("f1", false, true),
			fState("f2", false, true), fState("f3", false, true),
		},
		Fans: power.FansState{On: true},
	}
}

// forced is a POST /fans/off carrying the confirmation checkbox.
func forced() request {
	return request{Form: url.Values{"force": []string{"true"}}}
}

// TestJobEntityAdvertisesStaleAfterSecondsFromTheManager pins lz0's half of
// the fix: the job entity's wire properties must carry this node's own
// staleness ceiling (s.jobs.StaleCeiling(), UnmuteTimeout-derived as of kz0)
// under "staleAfterSeconds", not a value made up here -- a remote client (see
// docs/client-reference.js's waitForJob) has no other way to learn it.
//
// Two different UnmuteTimeouts are checked to prove this reads the live
// ceiling rather than a compile-time constant that happens to match one case.
func TestJobEntityAdvertisesStaleAfterSecondsFromTheManager(t *testing.T) {
	for _, unmute := range []time.Duration{20 * time.Minute, 37 * time.Minute} {
		t.Run(unmute.String(), func(t *testing.T) {
			srv := &Server{
				jobs:   coordination.NewManager(t.TempDir(), unmute, power.ShutdownWorstCase(config.Default())),
				router: NewRouter(""),
			}
			want := int(srv.jobs.StaleCeiling().Seconds())

			e := srv.jobEntity(coordination.Job{
				ID: "x", State: coordination.JobRunning,
				Started: time.Now().UTC().Format(time.RFC3339),
			})

			got, ok := e.Properties["staleAfterSeconds"].(int)
			if !ok {
				t.Fatalf("Properties[%q] = %#v (%T), want an int", "staleAfterSeconds", e.Properties["staleAfterSeconds"], e.Properties["staleAfterSeconds"])
			}
			if got != want {
				t.Errorf("staleAfterSeconds = %d, want %d (StaleCeiling() for UnmuteTimeout=%s)", got, want, unmute)
			}
		})
	}
}

// TestHostEntityReportsWhetherTheProbeRan pins the wire contract the fan guard
// makes necessary.
//
// The server refuses fans-off for a host it could not probe, so a client given
// only ping=false sees a confirmation appear over what looks to it like a cold
// rack -- and would render that host as "off" while the server treats it as
// possibly running. docs/client-reference.js checks the guard against exactly
// this field.
func TestHostEntityReportsWhetherTheProbeRan(t *testing.T) {
	for _, tc := range []struct {
		name  string
		host  power.HostStatus
		known bool
	}{
		{name: "probed", host: fState("f0", false, true), known: true},
		{name: "not probed", host: fState("f0", false, false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := hostEntity(tc.host).Properties["pingKnown"].(bool)
			if !ok {
				t.Fatal("the host entity carries no pingKnown property")
			}
			if got != tc.known {
				t.Errorf("pingKnown = %v, want %v", got, tc.known)
			}
		})
	}
}

// TestFansOffRefusedWhenTheRackCouldNotBeProbed is the regression test for the
// third fan guard, the one that stayed open after the other two were fixed.
//
// The API judged "is anything running" on HostStatus.Ping, which had already
// thrown away whether the probe ran at all. In the environment this API
// actually serves from -- a CGI under bozohttpd, whose PATH lacks /sbin and so
// finds no ping(8) -- every host reported ping=false, the `force` field was
// therefore not advertised, and POST /fans/off cut the cooling to a fully
// running rack without so much as a confirmation. Meanwhile `f3sctl fans off`
// typed on the same Pi refused. Unknown must count as running here too.
func TestFansOffRefusedWhenTheRackCouldNotBeProbed(t *testing.T) {
	plug := newFakePlug(t)
	confirmed := false
	srv := testServer(t, plug, func(context.Context) power.RackActivity {
		confirmed = true
		return power.RackActivity{}
	})

	unprobed := State{
		Hosts: []power.HostStatus{
			fState("f0", false, false), fState("f1", false, false),
			fState("f2", false, false), fState("f3", false, false),
		},
		Fans: power.FansState{On: true},
	}

	_, status, err := srv.handleFansOff(context.Background(), unprobed, request{})
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want %d: nothing is known about the rack", status, http.StatusConflict)
	}
	if err == nil || !strings.Contains(err.Error(), "could not be probed") {
		t.Errorf("error = %v, want it to say the hosts could not be probed", err)
	}
	if got := plug.setCalls(); len(got) != 0 {
		t.Fatalf("Switch.Set calls = %v, want none", got)
	}
	if confirmed {
		t.Error("the confirming probe ran even though the snapshot already said the rack was busy")
	}
}

// TestFansOffRefusedWhileAHostAnswers is the ordinary case, and pins that the
// snapshot alone is enough to refuse: re-probing to confirm what a host has
// already answered would cost a request several seconds for nothing.
func TestFansOffRefusedWhileAHostAnswers(t *testing.T) {
	plug := newFakePlug(t)
	confirmed := false
	srv := testServer(t, plug, func(context.Context) power.RackActivity {
		confirmed = true
		return power.RackActivity{}
	})

	hot := coldSnapshot()
	hot.Hosts[3] = fState("f3", true, true)

	_, status, err := srv.handleFansOff(context.Background(), hot, request{})
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want %d while f3 answers", status, http.StatusConflict)
	}
	if err == nil || !strings.Contains(err.Error(), "f3") {
		t.Errorf("error = %v, want it to name f3", err)
	}
	if got := plug.setCalls(); len(got) != 0 {
		t.Fatalf("Switch.Set calls = %v, want none", got)
	}
	if confirmed {
		t.Error("the confirming probe ran even though the snapshot already said the rack was busy")
	}
}

// TestFansOffConfirmsASnapshotThatLooksCold pins the half that makes the API
// guard as strong as the CLI's rather than merely as strong as one ping.
//
// The snapshot is a single probe per host, taken to render a response. One
// dropped echo reply in it reads as a powered-off host -- and the interface
// really does flap mid-shutdown, which is exactly when someone reaches for the
// fans. So the answer that would actually cut cooling is re-checked against the
// same consecutive-silence evidence `f3sctl fans off` demands locally.
func TestFansOffConfirmsASnapshotThatLooksCold(t *testing.T) {
	plug := newFakePlug(t)
	confirmed := false
	srv := testServer(t, plug, func(context.Context) power.RackActivity {
		confirmed = true
		// The confirming probe hears f3 answer on a later round.
		return power.RackActivityFrom([]power.HostStatus{fState("f3", true, true)})
	})

	_, status, err := srv.handleFansOff(context.Background(), coldSnapshot(), request{})
	if !confirmed {
		t.Fatal("a snapshot showing a cold rack was accepted without confirmation")
	}
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want %d: the confirming probe heard f3", status, http.StatusConflict)
	}
	if err == nil || !strings.Contains(err.Error(), "f3") {
		t.Errorf("error = %v, want it to name f3", err)
	}
	if got := plug.setCalls(); len(got) != 0 {
		t.Fatalf("Switch.Set calls = %v, want none", got)
	}
}

// TestFansOffSwitchesThePlugOnceTheRackIsConfirmedIdle is the other half: a
// guard that only ever refused would leave the fans running for good.
func TestFansOffSwitchesThePlugOnceTheRackIsConfirmedIdle(t *testing.T) {
	plug := newFakePlug(t)
	srv := testServer(t, plug, func(context.Context) power.RackActivity {
		return power.RackActivity{}
	})

	_, status, err := srv.handleFansOff(context.Background(), coldSnapshot(), request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got := plug.setCalls(); len(got) != 1 || got[0] {
		t.Fatalf("Switch.Set calls = %v, want exactly one with on=false", got)
	}
}

// TestFansOffWithForceSkipsTheGuardEntirely pins the escape hatch, and that it
// costs nothing: an operator who has said "I mean it" must not also wait out a
// confirmation probe they have already overridden.
func TestFansOffWithForceSkipsTheGuardEntirely(t *testing.T) {
	plug := newFakePlug(t)
	confirmed := false
	srv := testServer(t, plug, func(context.Context) power.RackActivity {
		confirmed = true
		return power.RackActivity{}
	})

	hot := coldSnapshot()
	hot.Hosts[3] = fState("f3", true, true)

	if _, status, err := srv.handleFansOff(context.Background(), hot, forced()); err != nil || status != http.StatusOK {
		t.Fatalf("forced fans off: status = %d, err = %v", status, err)
	}
	if got := plug.setCalls(); len(got) != 1 || got[0] {
		t.Fatalf("Switch.Set calls = %v, want exactly one with on=false", got)
	}
	if confirmed {
		t.Error("the confirming probe ran despite force=true")
	}
}

// TestSetFansRespectsTheRequestContext is the regression test for ky0: setFans
// used to reach for its own context.Background() rather than the context
// serve() built for the request, so a client's deadline or an abandoned
// request never reached the Shelly call.
//
// A context cancelled before the handler runs is used rather than a deadline,
// so the test does not race a timer: if the plug's HTTP client actually reads
// the context handed to it, the very first request built from it fails
// immediately with ctx.Err(), and the fake plug never sees a request at all.
// Under the old context.Background() this call would instead have gone out
// over the wire and succeeded.
func TestSetFansRespectsTheRequestContext(t *testing.T) {
	plug := newFakePlug(t)
	srv := testServer(t, plug, func(context.Context) power.RackActivity {
		return power.RackActivity{}
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, status, err := srv.handleFansOn(ctx, State{}, request{})
	if err == nil {
		t.Fatal("expected an error from a cancelled context, got none")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", status, http.StatusBadGateway)
	}
	if got := plug.setCalls(); len(got) != 0 {
		t.Fatalf("Switch.Set calls = %v, want none: the cancelled context should have stopped the request before it was sent", got)
	}
}
