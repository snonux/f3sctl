package powerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/snonux/f3sctl/internal/httpapi/contract"
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

// testSurface returns a Surface whose fan plug is the fake one and whose
// confirming rack probe is confirm, so no ICMP leaves the box.
func testSurface(t *testing.T, plug *fakePlug, confirm func(context.Context) power.RackActivity) *Surface {
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
	sf := New("test", contract.Hrefs(""), cfg.Inventory, eng, nil, nil)
	sf.RackConfirm = confirm
	return sf
}

// fState builds one f-host's probe result. known=false is the probe that never
// happened, which is the case the whole guard turns on.
func fState(name string, ping, known bool) power.HostStatus {
	return power.HostStatus{Name: name, Role: string(inventory.RoleF), Ping: ping, PingKnown: known}
}

// coldSnapshot is a rack every host of which was probed and found silent.
func coldSnapshot() contract.State {
	return contract.State{
		Hosts: []power.HostStatus{
			fState("f0", false, true), fState("f1", false, true),
			fState("f2", false, true), fState("f3", false, true),
		},
		Fans: power.FansState{On: true},
	}
}

// forced is a POST /fans/off carrying the confirmation checkbox.
func forced() contract.Request {
	return contract.Request{Form: url.Values{"force": []string{"true"}}}
}

// TestJobEntityAdvertisesStaleAfterSecondsFromTheManager pins lz0's half of
// the fix: the job entity's wire properties must carry this node's own
// staleness ceiling (Jobs.StaleCeiling(), UnmuteTimeout-derived as of kz0)
// under "staleAfterSeconds", not a value made up here -- a remote client (see
// docs/client-reference.js's waitForJob) has no other way to learn it.
//
// Two different UnmuteTimeouts are checked to prove this reads the live
// ceiling rather than a compile-time constant that happens to match one case.
func TestJobEntityAdvertisesStaleAfterSecondsFromTheManager(t *testing.T) {
	for _, unmute := range []time.Duration{20 * time.Minute, 37 * time.Minute} {
		t.Run(unmute.String(), func(t *testing.T) {
			sf := &Surface{
				Node: "test",
				Href: contract.Hrefs(""),
				Jobs: coordination.NewManager(t.TempDir(), unmute, power.ShutdownWorstCase(config.Default())),
			}
			want := int(sf.Jobs.StaleCeiling().Seconds())

			e := sf.jobEntity(coordination.Job{
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
	sf := testSurface(t, plug, func(context.Context) power.RackActivity {
		confirmed = true
		return power.RackActivity{}
	})

	unprobed := contract.State{
		Hosts: []power.HostStatus{
			fState("f0", false, false), fState("f1", false, false),
			fState("f2", false, false), fState("f3", false, false),
		},
		Fans: power.FansState{On: true},
	}

	_, status, err := sf.handleFansOff(context.Background(), unprobed, contract.Request{})
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
	sf := testSurface(t, plug, func(context.Context) power.RackActivity {
		confirmed = true
		return power.RackActivity{}
	})

	hot := coldSnapshot()
	hot.Hosts[1] = fState("f1", true, true)

	_, status, err := sf.handleFansOff(context.Background(), hot, contract.Request{})
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want %d while f1 answers", status, http.StatusConflict)
	}
	if err == nil || !strings.Contains(err.Error(), "f1") {
		t.Errorf("error = %v, want it to name f1", err)
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
	sf := testSurface(t, plug, func(context.Context) power.RackActivity {
		confirmed = true
		// The confirming probe hears f1 answer on a later round.
		return power.RackActivityFrom([]power.HostStatus{fState("f1", true, true)})
	})

	_, status, err := sf.handleFansOff(context.Background(), coldSnapshot(), contract.Request{})
	if !confirmed {
		t.Fatal("a snapshot showing a cold rack was accepted without confirmation")
	}
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want %d: the confirming probe heard f1", status, http.StatusConflict)
	}
	if err == nil || !strings.Contains(err.Error(), "f1") {
		t.Errorf("error = %v, want it to name f1", err)
	}
	if got := plug.setCalls(); len(got) != 0 {
		t.Fatalf("Switch.Set calls = %v, want none", got)
	}
}

// TestFansOffSwitchesThePlugOnceTheRackIsConfirmedIdle is the other half: a
// guard that only ever refused would leave the fans running for good.
func TestFansOffSwitchesThePlugOnceTheRackIsConfirmedIdle(t *testing.T) {
	plug := newFakePlug(t)
	sf := testSurface(t, plug, func(context.Context) power.RackActivity {
		return power.RackActivity{}
	})

	_, status, err := sf.handleFansOff(context.Background(), coldSnapshot(), contract.Request{})
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
	sf := testSurface(t, plug, func(context.Context) power.RackActivity {
		confirmed = true
		return power.RackActivity{}
	})

	hot := coldSnapshot()
	hot.Hosts[1] = fState("f1", true, true)

	if _, status, err := sf.handleFansOff(context.Background(), hot, forced()); err != nil || status != http.StatusOK {
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
	sf := testSurface(t, plug, func(context.Context) power.RackActivity {
		return power.RackActivity{}
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, status, err := sf.handleFansOn(ctx, contract.State{}, contract.Request{})
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

// fakePeerJobServer answers every GET with a canned /job document, recording
// whether any request carried coordination.PeerQueryParam. It stands in for
// the other API node in the handleJob/handleStatus merge tests below.
func fakePeerJobServer(t *testing.T, body string) (*httptest.Server, *bool) {
	t.Helper()
	asked := new(bool)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*asked = true
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, asked
}

const peerRunningJobBody = `{"properties":{"id":"peer-job","state":"running","node":"pi1","started":"2026-08-16T12:00:00Z"}}`

// testSurfaceWithPeer returns a Surface wired to a real coordination.Manager
// (fresh temp dir, no job has ever run) and a PeerSet whose only peer is a
// fake peer job server -- the shape the /job and /status merge tests need,
// with no engine at all (nothing they exercise reads one).
func testSurfaceWithPeer(t *testing.T, peer *httptest.Server) *Surface {
	t.Helper()
	return &Surface{
		Node:  "test",
		Href:  contract.Hrefs(""),
		Jobs:  coordination.NewManager(t.TempDir(), config.Default().UnmuteTimeout.D(), power.ShutdownWorstCase(config.Default())),
		Peers: &coordination.PeerSet{Nodes: []string{peer.Listener.Addr().String()}, JobPath: "/job"},
	}
}

// TestHandleJobMergesLocalAndPeerJobs pins currentJob's purpose end to end:
// GET /job answers with whichever of the local and the peer's job is
// authoritative (coordination.NewestJob) -- here, the peer's running job over
// a finished local one -- so a client sees the same job regardless of which
// of pi0/pi1 it asked, closing the gap reported against the "all-on" job that
// prompted this.
func TestHandleJobMergesLocalAndPeerJobs(t *testing.T) {
	peerSrv, _ := fakePeerJobServer(t, peerRunningJobBody)
	sf := testSurfaceWithPeer(t, peerSrv)
	local := &coordination.Job{ID: "local-job", State: coordination.JobDone, Started: "2026-08-16T09:00:00Z"}

	e, status, err := sf.handleJob(context.Background(), contract.State{Job: local}, contract.Request{APIKey: "k"})
	if err != nil || status != http.StatusOK {
		t.Fatalf("handleJob: status=%d err=%v", status, err)
	}
	if id, _ := e.Properties["id"].(string); id != "peer-job" {
		t.Errorf("properties.id = %q, want %q: a running peer job must win over a finished local one", id, "peer-job")
	}
}

// TestHandleJobDoesNotMergeAPeerOriginatedQuery pins the other half of
// PeerQueryParam's contract: a GET /job carrying that marker -- i.e. this
// node answering another node's own peer check -- must report its own job
// only, even with a peer configured that would otherwise be asked, or two
// nodes merging on every request would ask each other forever (see
// PeerQueryParam's doc comment).
func TestHandleJobDoesNotMergeAPeerOriginatedQuery(t *testing.T) {
	peerSrv, asked := fakePeerJobServer(t, peerRunningJobBody)
	sf := testSurfaceWithPeer(t, peerSrv)
	local := &coordination.Job{ID: "local-job", State: coordination.JobDone, Started: "2026-08-16T09:00:00Z"}
	req := contract.Request{APIKey: "k", Query: url.Values{coordination.PeerQueryParam: []string{"1"}}}

	e, _, err := sf.handleJob(context.Background(), contract.State{Job: local}, req)
	if err != nil {
		t.Fatalf("handleJob: %v", err)
	}
	if id, _ := e.Properties["id"].(string); id != "local-job" {
		t.Errorf("properties.id = %q, want %q: a peer-marked request must not merge", id, "local-job")
	}
	if *asked {
		t.Error("the peer was asked despite the request carrying PeerQueryParam")
	}
}

// TestHandleStatusEmbedsThePeersJobWhenLocalHasNone pins that /status, like
// /job, does not omit the job entity just because this node's own job.json is
// empty: the peer's job is worth surfacing there too, via the same currentJob
// merge -- /status is never itself asked as a peer query, so it always
// merges, unlike /job.
func TestHandleStatusEmbedsThePeersJobWhenLocalHasNone(t *testing.T) {
	peerSrv, _ := fakePeerJobServer(t, peerRunningJobBody)
	sf := testSurfaceWithPeer(t, peerSrv)

	e, _, err := sf.handleStatus(context.Background(), contract.State{}, contract.Request{APIKey: "k"})
	if err != nil {
		t.Fatalf("handleStatus: %v", err)
	}
	var found bool
	for _, sub := range e.Entities {
		if len(sub.Class) > 0 && sub.Class[0] == "job" {
			if id, _ := sub.Properties["id"].(string); id == "peer-job" {
				found = true
			}
		}
	}
	if !found {
		t.Error("handleStatus did not embed the peer's job when this node had none")
	}
}
