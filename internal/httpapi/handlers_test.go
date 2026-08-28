package httpapi

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/gogios"
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
	return &Server{cfg: cfg, engine: eng, node: "test", router: NewRouter("", inventory.Default()), rackConfirm: confirm}
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
				router: NewRouter("", inventory.Default()),
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

// TestHandleJobMergesLocalAndPeerJobs pins currentJob's purpose end to end:
// GET /job answers with whichever of the local and the peer's job is
// authoritative (coordination.NewestJob) -- here, the peer's running job over
// a finished local one -- so a client sees the same job regardless of which
// of pi0/pi1 it asked, closing the gap reported against the "all-on" job that
// prompted this.
func TestHandleJobMergesLocalAndPeerJobs(t *testing.T) {
	peerSrv, _ := fakePeerJobServer(t, peerRunningJobBody)
	srv := &Server{
		router: NewRouter("", inventory.Default()),
		node:   "test",
		jobs:   coordination.NewManager(t.TempDir(), config.Default().UnmuteTimeout.D(), power.ShutdownWorstCase(config.Default())),
		peers:  &coordination.PeerSet{Nodes: []string{peerSrv.Listener.Addr().String()}, JobPath: "/job"},
	}
	local := &coordination.Job{ID: "local-job", State: coordination.JobDone, Started: "2026-08-16T09:00:00Z"}

	e, status, err := srv.handleJob(context.Background(), State{Job: local}, request{APIKey: "k"})
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
	srv := &Server{
		router: NewRouter("", inventory.Default()),
		node:   "test",
		jobs:   coordination.NewManager(t.TempDir(), config.Default().UnmuteTimeout.D(), power.ShutdownWorstCase(config.Default())),
		peers:  &coordination.PeerSet{Nodes: []string{peerSrv.Listener.Addr().String()}, JobPath: "/job"},
	}
	local := &coordination.Job{ID: "local-job", State: coordination.JobDone, Started: "2026-08-16T09:00:00Z"}
	req := request{APIKey: "k", Query: url.Values{coordination.PeerQueryParam: []string{"1"}}}

	e, _, err := srv.handleJob(context.Background(), State{Job: local}, req)
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
	srv := &Server{
		router: NewRouter("", inventory.Default()),
		node:   "test",
		jobs:   coordination.NewManager(t.TempDir(), config.Default().UnmuteTimeout.D(), power.ShutdownWorstCase(config.Default())),
		peers:  &coordination.PeerSet{Nodes: []string{peerSrv.Listener.Addr().String()}, JobPath: "/job"},
	}

	e, _, err := srv.handleStatus(context.Background(), State{}, request{APIKey: "k"})
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

// gogiosSample is a small, representative Gogios report for handler tests:
// one unhandled CRITICAL, one stale WARNING (its lifecycle is stale, but its
// own severity stays WARNING), one suppressed UNKNOWN, and two OK checks.
// Mirrors the shape internal/gogios/gogios_test.go's own fixture describes.
func gogiosSample() *gogios.Report {
	return &gogios.Report{
		LastUpdated: "2026-08-27T08:58:18+02:00",
		Subject:     "GOGIOS Report [C:1 W:1 U:1 S:1 SU:1 OK:2]",
		Summary:     gogios.Summary{Critical: 1, Warning: 1, Unknown: 1, Stale: 1, Suppressed: 1, Ok: 2},
		Sections: gogios.Sections{
			Unhandled: []gogios.Check{
				{Name: "Check Ping6 r1.wg0.wan.buetow.org", Status: "CRITICAL", Output: "timed out", Epoch: 1},
			},
			Stale: []gogios.Check{
				{Name: "Check SWAP blowfish", Status: "WARNING", Output: "SWAP WARNING", Epoch: 2, LastCheckedAgeSeconds: 99999},
			},
			Suppressed: []gogios.Check{
				{Name: "Check Disk fishfinger", Status: "UNKNOWN", Output: "no data", Epoch: 3},
			},
			Ok: []gogios.Check{
				{Name: "Check Ping4 master.buetow.org", Status: "OK", Output: "PING OK", Epoch: 4},
				{Name: "Check HTTP IPv4 foo.zone", Status: "OK", Output: "HTTP OK", Epoch: 5},
			},
		},
	}
}

// hasRel reports whether links carries a link whose first rel is rel.
func hasRel(links []Link, rel string) bool {
	for _, l := range links {
		if len(l.Rel) > 0 && l.Rel[0] == rel {
			return true
		}
	}
	return false
}

// TestHandleGogiosRendersTheOverview pins the happy path: the subject, the
// six summary counts, and a link to every drill-down category plus
// /monitoring (the separate mute concern).
func TestHandleGogiosRendersTheOverview(t *testing.T) {
	srv := &Server{router: NewRouter("", inventory.Default()), node: "test"}
	state := State{Gogios: gogiosSample()}

	e, status, err := srv.handleGogios(context.Background(), state, request{})
	if err != nil {
		t.Fatalf("handleGogios: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if e.Properties["subject"] != state.Gogios.Subject {
		t.Errorf("subject = %v, want %v", e.Properties["subject"], state.Gogios.Subject)
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
// handleFans/handleMonitoring use, not a non-2xx status -- see handleGogios's
// doc comment for why.
func TestHandleGogiosReportsAFetchErrorAsAProperty(t *testing.T) {
	srv := &Server{router: NewRouter("", inventory.Default()), node: "test"}
	state := State{GogiosErr: errFake{}}

	e, status, err := srv.handleGogios(context.Background(), state, request{})
	if err != nil {
		t.Fatalf("handleGogios: %v", err)
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
// each is the union, across every lifecycle section, of checks with that
// Status -- see gogiosChecksForStatus.
func TestHandleGogiosStatusFiltersBySeverity(t *testing.T) {
	srv := &Server{router: NewRouter("", inventory.Default()), node: "test"}
	state := State{Gogios: gogiosSample()}

	for _, tc := range []struct {
		status string
		want   []string
	}{
		{"critical", []string{"Check Ping6 r1.wg0.wan.buetow.org"}},
		{"warning", []string{"Check SWAP blowfish"}},
		{"unknown", []string{"Check Disk fishfinger"}},
		{"ok", []string{"Check Ping4 master.buetow.org", "Check HTTP IPv4 foo.zone"}},
	} {
		t.Run(tc.status, func(t *testing.T) {
			e, status, err := handleGogiosStatus(tc.status)(srv, context.Background(), state, request{})
			if err != nil {
				t.Fatalf("handleGogiosStatus(%q): %v", tc.status, err)
			}
			if status != http.StatusOK {
				t.Fatalf("status = %d, want %d", status, http.StatusOK)
			}
			var got []string
			for _, sub := range e.Entities {
				got = append(got, sub.Properties["name"].(string))
			}
			if !equalArgs(got, tc.want) {
				t.Errorf("%s checks = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

// TestHandleGogiosStatusLifecycleGroupings pins the other two categories:
// "stale" and "suppressed" read Sections.Stale/Suppressed directly rather
// than filtering by Status, because a stale or suppressed check keeps
// whatever severity it already had (here, WARNING and UNKNOWN respectively,
// neither of which is the literal string "stale"/"suppressed").
func TestHandleGogiosStatusLifecycleGroupings(t *testing.T) {
	srv := &Server{router: NewRouter("", inventory.Default()), node: "test"}
	state := State{Gogios: gogiosSample()}

	for _, tc := range []struct{ status, want string }{
		{"stale", "Check SWAP blowfish"},
		{"suppressed", "Check Disk fishfinger"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			e, _, err := handleGogiosStatus(tc.status)(srv, context.Background(), state, request{})
			if err != nil {
				t.Fatalf("handleGogiosStatus(%q): %v", tc.status, err)
			}
			if len(e.Entities) != 1 || e.Entities[0].Properties["name"] != tc.want {
				t.Errorf("%s checks = %+v, want exactly [%s]", tc.status, e.Entities, tc.want)
			}
		})
	}
}

// TestHandleGogiosStatusReportsAFetchError is the drill-down's half of
// TestHandleGogiosReportsAFetchErrorAsAProperty.
func TestHandleGogiosStatusReportsAFetchError(t *testing.T) {
	srv := &Server{router: NewRouter("", inventory.Default()), node: "test"}
	state := State{GogiosErr: errFake{}}

	e, status, err := handleGogiosStatus("critical")(srv, context.Background(), state, request{})
	if err != nil {
		t.Fatalf("handleGogiosStatus: %v", err)
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
	srv := &Server{router: NewRouter("", inventory.Default()), node: "test"}
	state := State{Gogios: gogiosSample()}
	name := "Check Ping6 r1.wg0.wan.buetow.org"

	e, status, err := srv.handleGogiosCheck(context.Background(), state, request{Query: url.Values{"name": {name}}})
	if err != nil {
		t.Fatalf("handleGogiosCheck: %v", err)
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
	srv := &Server{router: NewRouter("", inventory.Default()), node: "test"}
	state := State{Gogios: gogiosSample()}

	_, status, err := srv.handleGogiosCheck(context.Background(), state, request{Query: url.Values{"name": {"no such check"}}})
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
	srv := &Server{router: NewRouter("", inventory.Default()), node: "test"}
	state := State{GogiosErr: errFake{}}

	_, status, err := srv.handleGogiosCheck(context.Background(), state, request{Query: url.Values{"name": {"anything"}}})
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", status, http.StatusBadGateway)
	}
	if err == nil {
		t.Fatal("expected an error")
	}
}

// gogiosCacheTestServer returns a Server whose cfg points at an httptest
// server serving body, with the cache in a fresh temp dir -- the same
// hermetic setup internal/gogios/gogios_test.go uses. handleGogiosClearCache
// calls gogios.ClearCache/gogios.Fetch directly against s.cfg (there is no
// mockable seam for them, unlike probeHosts/fansStatus -- see enrichState's
// doc comment in server.go), so exercising it for real is the only way to
// pin its behaviour.
func gogiosCacheTestServer(t *testing.T, body string) (*Server, *int32) {
	t.Helper()
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.GogiosURL = upstream.URL
	cfg.GogiosFetchTimeout = config.Duration(5 * time.Second)
	cfg.GogiosCacheTTL = config.Duration(60 * time.Second)

	return &Server{cfg: cfg, router: NewRouter("", inventory.Default()), node: "test"}, &hits
}

// gogiosReportJSON is a minimal, valid Gogios report body for
// gogiosCacheTestServer.
const gogiosReportJSON = `{"subject":"GOGIOS Report [C:0 W:0 U:0 S:0 SU:0 OK:1]",` +
	`"lastUpdated":"2026-08-27T08:58:18+02:00","summary":{"critical":0,"warning":0,"unknown":0,"stale":0,"suppressed":0,"ok":1},` +
	`"sections":{"ok":[{"name":"Check Ping4 master.buetow.org","status":"OK","output":"PING OK","epoch":1}]}}`

// TestHandleGogiosClearCacheClearsAndRefetches pins the whole point of the
// action: after it runs, even a cache well within its TTL must not be served
// -- the very next read has to see a real fetch.
func TestHandleGogiosClearCacheClearsAndRefetches(t *testing.T) {
	srv, hits := gogiosCacheTestServer(t, gogiosReportJSON)

	// Prime the cache so ClearCache has something to remove.
	if _, err := gogios.Fetch(context.Background(), srv.cfg); err != nil {
		t.Fatalf("priming the cache: %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("hits after priming = %d, want 1", got)
	}

	e, status, err := srv.handleGogiosClearCache(context.Background(), State{}, request{})
	if err != nil {
		t.Fatalf("handleGogiosClearCache: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Errorf("hits after clear+refetch = %d, want 2 (the cache must have been cleared, forcing a re-fetch)", got)
	}
	if e.Properties["subject"] == "" {
		t.Error("the re-fetched overview has an empty subject")
	}
	if _, err := os.Stat(filepath.Join(srv.cfg.StateDir, "gogios-report.json")); err != nil {
		t.Errorf("no cache file after clear+refetch: %v", err)
	}
}

// TestHandleGogiosClearCacheSurfacesAFetchErrorAfterClearing is the negative
// case: clearing the cache can succeed while the immediate re-fetch fails
// (the upstream is down). That must still render as a 200 with an "error"
// property -- handleGogiosClearCache delegates to handleGogios for
// rendering, so it inherits that convention rather than needing its own.
func TestHandleGogiosClearCacheSurfacesAFetchErrorAfterClearing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.GogiosURL = upstream.URL
	cfg.GogiosFetchTimeout = config.Duration(5 * time.Second)
	cfg.GogiosCacheTTL = config.Duration(60 * time.Second)
	srv := &Server{cfg: cfg, router: NewRouter("", inventory.Default()), node: "test"}

	e, status, err := srv.handleGogiosClearCache(context.Background(), State{}, request{})
	if err != nil {
		t.Fatalf("handleGogiosClearCache: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d (clearing succeeded; only the re-fetch failed)", status, http.StatusOK)
	}
	if e.Properties["error"] == nil {
		t.Error("no error property despite the re-fetch failing")
	}
}
