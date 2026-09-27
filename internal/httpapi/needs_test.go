package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/gogios"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/httpapi/gogiosapi"
	"github.com/snonux/f3sctl/internal/httpapi/powerapi"
	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power"
)

// This file pins contract.Route.Needs, the per-route declaration of the
// request-scoped state enrichState fetches (task ca). It replaced path
// predicates (IsMonitorPath, IsFolderPath, IsReportPath, and the /job and
// /status exclusions) that ignored the HTTP method and had to be kept in step
// with the routes by hand -- POST /gogios/cache/clear once re-rendered the
// Gogios folder without the mute pair, and /status still rendered the whole
// actions list without the mute state its mute pair is judged on.

// needCase is one contract.Need and how to take its state away again: drop
// turns a state in which the need was fetched into exactly what enrichState
// leaves behind when a route does not declare it.
type needCase struct {
	need *contract.Need
	name string
	drop func(contract.State) contract.State
}

var needCases = []needCase{
	{contract.NeedPeerBusy, "NeedPeerBusy", func(s contract.State) contract.State {
		s.PeerBusy = false
		return s
	}},
	{gogiosapi.NeedMonitoring, "NeedMonitoring", func(s contract.State) contract.State {
		return gogiosapi.WithMonitoring(s, nil)
	}},
	{gogiosapi.NeedReport, "NeedReport", func(s contract.State) contract.State {
		return gogiosapi.WithReport(s, nil, nil)
	}},
}

// needsCheckName is a check in needsReport, so gogios-check has something to
// look up (it answers 404 for an unknown name whether or not it has a report).
const needsCheckName = "Check Ping6 r1.wg0.wan.buetow.org"

var needsReport = &gogios.Report{
	Subject:     "GOGIOS Report [C:1 W:0 U:0 S:0 SU:0 OK:1]",
	LastUpdated: "2026-09-27T08:00:00+02:00",
	Summary:     gogios.Summary{Critical: 1, Ok: 1},
	Sections: gogios.Sections{
		Unhandled: []gogios.Check{{Name: needsCheckName, Status: "CRITICAL", Output: "timed out"}},
		Ok:        []gogios.Check{{Name: "Check Ping4 master.buetow.org", Status: "OK", Output: "PING OK"}},
	},
}

// needsBaseStates are states with every Need's state fetched: a partial mute
// (so both mute actions are offered) and a report, which dropping them
// changes, on two fleets (all up and all down) with the peer busy and idle.
// The busy peer is what dropping NeedPeerBusy changes; the idle one is what
// offers the power and plug actions at all -- between the four states every
// action is offered in at least one (TestNeedsBaseStatesOfferEveryAction), so
// the guard never judges an action only in states where it is withheld anyway.
func needsBaseStates() []contract.State {
	fHosts := func(up bool) []power.HostStatus {
		var out []power.HostStatus
		for _, h := range inventory.Default().ByRole(inventory.RoleF) {
			out = append(out, power.HostStatus{Name: h.Name, Role: "f", Ping: up, PingKnown: true, SSH: up})
		}
		return out
	}
	enriched := func(up, peerBusy bool) contract.State {
		s := powerapi.WithSnapshot(contract.State{PeerBusy: peerBusy}, powerapi.Snapshot{
			Hosts: fHosts(up),
			Fans:  power.FansState{On: up},
			AC:    power.ACState{On: up},
		})
		s = gogiosapi.WithMonitoring(s, []gogios.GatewayMute{{Name: "blowfish", Muted: true}, {Name: "fishfinger"}})
		return gogiosapi.WithReport(s, needsReport, nil)
	}
	return []contract.State{enriched(true, true), enriched(false, true), enriched(true, false), enriched(false, false)}
}

// needsJobs is a powerapi.Jobs that never spawns a child and answers every
// Start with the same fixed job, so a job route's Handle can be observed, and
// compared, like any other.
type needsJobs struct{}

func (needsJobs) Start(action string, _ []string) (coordination.Job, error) {
	return coordination.Job{ID: "j1", Action: action, State: coordination.JobRunning,
		Started: "2026-09-27T08:00:00Z", Node: "test"}, nil
}
func (needsJobs) StaleCeiling() time.Duration { return time.Minute }
func (needsJobs) Read() *coordination.Job     { return nil }

// needsServer is a Server whose every collaborator a handler can reach is a
// fake -- the plug engine, the gateway mute, an empty peer set, a job starter
// that spawns nothing, a report source with no report -- so every route's
// Handle can be called directly, side effects included, without touching the
// network, a real gateway or a real job.
func needsServer(t *testing.T) *Server {
	t.Helper()

	cfg := config.Default()
	jobs := coordination.NewManager(t.TempDir(), cfg.UnmuteTimeout.D(), 0)
	peers := coordination.NewPeerSet(nil, "")
	gw := &gatewayRecorder{gws: []gogios.GatewayMute{{Name: "blowfish", Muted: true}, {Name: "fishfinger"}}}
	inv := inventory.Default()

	href := contract.Hrefs("")
	pw := func(a contract.ActionRenderer, p powerapi.Prober) *powerapi.Surface {
		return powerapi.New("test", href, inv, &plugRecorder{}, p, needsJobs{}, peers, a)
	}
	return (&Server{
		cfg: cfg, jobs: jobs, peers: peers, siren: NewSirenRenderer(), node: "test",
	}).assemble(inv, pw, gogiosSurfaceOver("", unreachableReports(), gw), "")
}

// observeRoute is everything about route name that the state enrichState
// fetched could change, rendered as one comparable string: whether the route
// is available and with which fields (what serve() judges before any handler
// runs), and what its own Handle answers. A panicking handler (one
// dereferencing a report that was never fetched) is an answer too. Job routes
// are observed through needsJobs, so a job handler that started reading State
// would be held to its declarations like every other handler.
//
// Each observation gets a fresh Server, so a handler's side effects (a mute,
// a cleared cache) cannot leak from one observation into the next.
func observeRoute(t *testing.T, name string, state contract.State) string {
	t.Helper()
	srv := needsServer(t)
	r, ok := routeNamed(srv, name)
	if !ok {
		t.Fatalf("route %q vanished from a freshly built table", name)
	}

	return fmt.Sprintf("available=%v fields=%+v handle=%s", r.IsAvailable(state), r.FieldsFor(state), handleOutput(r, state))
}

func routeNamed(srv *Server, name string) (contract.Route, bool) {
	for _, r := range srv.router.routes {
		if r.Name == name {
			return r, true
		}
	}
	return contract.Route{}, false
}

func handleOutput(r contract.Route, state contract.State) (out string) {
	defer func() {
		if p := recover(); p != nil {
			out = fmt.Sprintf("panic: %v", p)
		}
	}()
	req := contract.Request{
		Method: r.Method, Path: r.Path, APIKey: "sekrit",
		Query: url.Values{"name": {needsCheckName}}, Form: url.Values{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	e, status, err := r.Handle(ctx, state, req)
	raw, jerr := json.Marshal(e)
	if jerr != nil {
		return "unencodable entity: " + jerr.Error()
	}
	return fmt.Sprintf("%d %v %s", status, err, raw)
}

// TestRoutesDeclareTheStateTheyRead is the table-level guard behind
// contract.Route.Needs. It does not trust a declaration on its author's word:
// for every route and every Need, it observes the route (availability, fields
// and its own handler's answer, rendered actions included) once with that
// need's state fetched and once without it, and requires the two to differ
// exactly when the route declares the need.
//
// An undeclared need that changes the answer is the bug path predicates let
// through: a response that silently renders without state it depends on (the
// cache clear's folder without its mute pair, /status's actions without
// theirs). A declared need that never changes the answer is a round trip paid
// for nothing -- the reason /job must not declare NeedPeerBusy, and the cache
// clear, whose handler re-reads the report after clearing it, NeedReport.
func TestRoutesDeclareTheStateTheyRead(t *testing.T) {
	var covered contract.Needs
	for _, c := range needCases {
		covered = append(covered, c.need)
	}

	for _, r := range needsServer(t).router.routes {
		for _, n := range r.Needs {
			if !covered.Has(n) {
				t.Errorf("route %q declares Need %v, which this guard has no needCase for: add one", r.Name, n)
			}
		}
		for _, c := range needCases {
			reads := false
			for _, base := range needsBaseStates() {
				if observeRoute(t, r.Name, base) != observeRoute(t, r.Name, c.drop(base)) {
					reads = true
					break
				}
			}
			switch declared := r.Needs.Has(c.need); {
			case reads && !declared:
				t.Errorf("route %q (%s %s) reads the state of %s but does not declare it: "+
					"served, it would render as if that state were absent", r.Name, r.Method, r.Path, c.name)
			case declared && !reads:
				t.Errorf("route %q (%s %s) declares %s but never reads it: every request pays for a fetch it discards",
					r.Name, r.Method, r.Path, c.name)
			}
		}
	}
}

// TestNeedsBaseStatesOfferEveryAction keeps the guard from passing vacuously
// for an action: one withheld in every base state would compare equal with
// and without a need it does read -- "unavailable" both times -- and so
// escape TestRoutesDeclareTheStateTheyRead unnoticed.
func TestNeedsBaseStatesOfferEveryAction(t *testing.T) {
	for _, r := range needsServer(t).router.routes {
		if !r.Action {
			continue
		}
		offered := false
		for _, base := range needsBaseStates() {
			offered = offered || r.IsAvailable(base)
		}
		if !offered {
			t.Errorf("action %q is available in none of needsBaseStates: add a base state that offers it, "+
				"or the Needs guard cannot see what it reads", r.Name)
		}
	}
}

// TestNeedsGuardSeesEachNeed is the guard's own negative test: each needCase
// must actually change something observable on a route known to read it, or
// TestRoutesDeclareTheStateTheyRead would pass vacuously -- flagging every
// declaration as unused, and missing every omission.
func TestNeedsGuardSeesEachNeed(t *testing.T) {
	readers := map[*contract.Need]string{
		contract.NeedPeerBusy:    "power",
		gogiosapi.NeedMonitoring: "monitoring",
		gogiosapi.NeedReport:     "gogios-critical",
	}
	for _, c := range needCases {
		base := needsBaseStates()[0]
		if observeRoute(t, readers[c.need], base) == observeRoute(t, readers[c.need], c.drop(base)) {
			t.Errorf("dropping %s changes nothing on route %q: the guard cannot see this need", c.name, readers[c.need])
		}
	}
}

// fetchCounts are the three round trips enrichState can make, counted at the
// fakes standing in for the machines they would reach.
type fetchCounts struct {
	peer, mute int32
	reports    *fakeReports // the report source, counting its own fetches
}

// counts is fc's totals so far: peer job fetches, gateway mute reads, and
// Gogios report fetches.
func (fc *fetchCounts) counts() [3]int32 {
	return [3]int32{atomic.LoadInt32(&fc.peer), atomic.LoadInt32(&fc.mute), fc.reports.fetches.Load()}
}

// countingMonitor is a gatewayRecorder that counts its gateway reads into
// reads -- both enrichState's (through the Gogios surface's NeedMonitoring
// Provider) and a mute handler's own re-read -- which in production are the
// same engine, and here the same Monitor.
type countingMonitor struct {
	gatewayRecorder
	reads *int32
}

func (m *countingMonitor) MonitoringStatus(ctx context.Context) []gogios.GatewayMute {
	atomic.AddInt32(m.reads, 1)
	return m.gatewayRecorder.MonitoringStatus(ctx)
}

// fetchCountingServer serves the real pipeline against a fake peer node over
// real HTTP, a counting report source and a counting gateway mute read -- so
// a test can tell exactly which round trips one request paid for, including
// the ones a handler makes itself (the /job and /status peer merge, the cache
// clear's re-fetch). The report source has no cache, so every report read
// counts.
func fetchCountingServer(t *testing.T) (*Server, *fetchCounts) {
	t.Helper()
	fc := &fetchCounts{}
	// One alerting gateway, the plugs off: the mute and fans-on are both
	// available, so serving them reaches their handlers.
	gw := &countingMonitor{gatewayRecorder: gatewayRecorder{gws: []gogios.GatewayMute{{Name: "blowfish"}}}, reads: &fc.mute}

	idle := coordination.Job{ID: "j0", Action: "on", State: coordination.JobDone, Node: "pi1"}
	peerBody, err := json.Marshal(map[string]any{"class": []string{"job"}, "properties": idle})
	if err != nil {
		t.Fatalf("encoding peer job: %v", err)
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&fc.peer, 1)
		_, _ = w.Write(peerBody)
	}))
	t.Cleanup(peer.Close)
	fc.reports = &fakeReports{report: needsReport}

	keyFile := filepath.Join(t.TempDir(), "apikey")
	if err := os.WriteFile(keyFile, []byte("sekrit\n"), 0o600); err != nil {
		t.Fatalf("writing the API key file: %v", err)
	}
	cfg := config.Default()
	jobs := coordination.NewManager(t.TempDir(), cfg.UnmuteTimeout.D(), 0)
	peers := coordination.NewPeerSet([]string{strings.TrimPrefix(peer.URL, "http://")}, "/job")
	inv := inventory.Default()
	href := contract.Hrefs("")
	return (&Server{
		cfg: cfg, jobs: jobs, peers: peers,
		auth: NewAuthenticator(keyFile), siren: NewSirenRenderer(), node: "test",
		probeHosts: func(context.Context) []power.HostStatus { return nil },
		fansStatus: func(context.Context) (power.FansState, error) { return power.FansState{}, nil },
		acStatus:   func(context.Context) (power.ACState, error) { return power.ACState{}, nil },
	}).assemble(inv, func(a contract.ActionRenderer, p powerapi.Prober) *powerapi.Surface {
		return powerapi.New("test", href, inv, &plugRecorder{}, p, jobs, peers, a)
	}, gogiosSurfaceOver("", fc.reports, gw), ""), fc
}

// TestEachRouteFetchesExactlyWhatItDeclares serves representative routes
// through the real pipeline and counts every round trip each one paid for:
// [peer job fetches, gateway mute reads, Gogios upstream fetches].
//
// The zeros are the negative half: the root and /monitoring pay no peer
// round trip any more (they render no power action), the drill-downs no
// gateway SSH, /job only its own merge -- never a second, bouncing peer check
// -- and /status one peer round trip, not two, and no gateway SSH. POST /gogios/cache/clear
// fetches the report once, after clearing it, not also before.
func TestEachRouteFetchesExactlyWhatItDeclares(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         [3]int32
	}{
		{http.MethodGet, "/", [3]int32{0, 0, 0}},
		{http.MethodGet, openAPIPath, [3]int32{0, 0, 0}},
		{http.MethodGet, "/job", [3]int32{1, 0, 0}},
		{http.MethodGet, "/status", [3]int32{1, 0, 0}},
		{http.MethodGet, "/power", [3]int32{1, 0, 0}},
		{http.MethodGet, "/fans", [3]int32{1, 0, 0}},
		{http.MethodGet, "/monitoring", [3]int32{0, 1, 0}},
		{http.MethodGet, "/gogios", [3]int32{0, 1, 1}},
		{http.MethodGet, "/gogios/critical", [3]int32{0, 0, 1}},
		{http.MethodPost, "/gogios/cache/clear", [3]int32{0, 1, 1}},
		// The mute reads the gateways twice: once for serve()'s availability
		// check, once in the handler to report what the mute left behind.
		{http.MethodPost, "/monitoring/mute", [3]int32{0, 2, 0}},
		{http.MethodPost, "/fans/on", [3]int32{1, 0, 0}},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			srv, fc := fetchCountingServer(t)
			req := getRequest(tc.path)
			req.Method = tc.method
			if msg, failed := serveEntity(t, srv, req).Properties["message"]; failed {
				t.Errorf("%s %s answered an error (%v): its handler must run for the counts to mean anything",
					tc.method, tc.path, msg)
			}

			if got := fc.counts(); got != tc.want {
				t.Errorf("[peer, mute, report] fetches = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestStatusRendersOnlyActionsJudgedOnItsOwnState is the regression test for
// /status rendering the whole API's actions list without the gateway mute
// the mute pair is judged on: whatever the gateways said, both mute actions
// silently vanished, as if nothing were muted. /status must stay cheap enough
// for a watchface to poll (docs/CLIENT.md), so it does not read the mute;
// instead it renders exactly the Power and AC folders' actions -- the ones
// judged on the state it does gather -- and leaves the mute pair to /gogios
// and /monitoring. The gateway states vary to show the answer never depends
// on them, and no gateway is read at all.
func TestStatusRendersOnlyActionsJudgedOnItsOwnState(t *testing.T) {
	var up []power.HostStatus
	for _, h := range inventory.Default().ByRole(inventory.RoleF) {
		up = append(up, power.HostStatus{Name: h.Name, Role: "f", Ping: true, PingKnown: true, SSH: true})
	}
	for _, tc := range []struct {
		name string
		mute []gogios.GatewayMute
	}{
		{"muted", []gogios.GatewayMute{{Name: "blowfish", Muted: true}}},
		{"alerting", []gogios.GatewayMute{{Name: "blowfish"}}},
		{"partial", []gogios.GatewayMute{{Name: "blowfish", Muted: true}, {Name: "fishfinger"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads int
			srv := folderServer(t, up, func(context.Context) []gogios.GatewayMute {
				reads++
				return tc.mute
			})
			status := getEntity(t, srv, "/status")
			if reads != 0 {
				t.Errorf("gateway mute reads serving /status = %d, want 0", reads)
			}

			want := append(actionNames(getEntity(t, srv, "/power")), actionNames(getEntity(t, srv, "/ac-control"))...)
			if got := actionNames(status); !slices.Equal(got, want) {
				t.Errorf("/status actions = %v, want the Power and AC folders' %v", got, want)
			}
			if !hasAction(status, "power-off") || !hasAction(status, "fans-off") {
				t.Errorf("/status actions = %v, want power-off and fans-off offered with the fleet up", actionNames(status))
			}
			for _, name := range []string{"monitoring-mute", "monitoring-unmute", "gogios-cache-clear"} {
				if hasAction(status, name) {
					t.Errorf("/status actions = %v carry %s: Gogios actions live on /gogios", actionNames(status), name)
				}
			}
		})
	}
}

// TestEnrichStateFollowsTheMatchedRouteNotThePath pins that what is fetched
// belongs to the route serving the request -- method and path -- rather than
// to its path alone, which is all the old path predicates could see. Two
// routes share one path here; only the one declaring the Need may pay for
// its fetch.
//
// The Need, its state and its Provider are all declared right here, as a
// new domain would declare its own: nothing in contract or in enrichState
// knows about them, and the routes' Needs alone decide what runs.
func TestEnrichStateFollowsTheMatchedRouteNotThePath(t *testing.T) {
	need := contract.NewNeed("test-gateways")
	gateways := contract.NewSlot[int]("test gateways")
	var reads int
	render := func(_ context.Context, s contract.State, _ contract.Request) (contract.Entity, int, error) {
		return contract.Entity{Properties: map[string]any{"gateways": gateways.Get(s)}}, http.StatusOK, nil
	}
	routes := []contract.Route{
		{Name: "shared-get", Method: http.MethodGet, Path: "/shared", SkipsProbe: true,
			Needs: contract.Needs{need}, Handle: render},
		{Name: "shared-post", Method: http.MethodPost, Path: "/shared", Action: true, SkipsProbe: true,
			Handle: render},
	}
	fetchers, err := needFetchers(routes, []contract.Provider{{Need: need, Fetch: func(_ context.Context, s contract.State, _ contract.Request) contract.State {
		reads++
		return gateways.With(s, 1)
	}}})
	if err != nil {
		t.Fatalf("needFetchers: %v", err)
	}
	srv, _ := countingServer(t)
	srv.router, srv.fetchers = routerOver(t, routes), fetchers

	if got := postEntity(t, srv, "/shared").Properties["gateways"]; got != float64(0) || reads != 0 {
		t.Errorf("POST /shared: gateways = %v after %d reads, want 0 after 0: it declares no Need", got, reads)
	}
	if got := getEntity(t, srv, "/shared").Properties["gateways"]; got != float64(1) || reads != 1 {
		t.Errorf("GET /shared: gateways = %v after %d reads, want 1 after 1", got, reads)
	}
}

// routerOver is a Router over a hand-built route table, for the tests above
// (and in server_test.go) that serve two routes sharing one path through a
// real Server.
func routerOver(t *testing.T, rs []contract.Route) *Router {
	t.Helper()
	rt, err := NewRouter("", rs)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return rt
}

// TestNeedFetchersRefusesWhatEnrichStateCannotServe is the negative half of
// the Provider seam build relies on: a route table declaring a Need nobody
// provides would be served without the state it reads, and a Need with two
// Providers would get whichever happened to be indexed last -- both are
// refused when the Server is built, not discovered on a request.
func TestNeedFetchersRefusesWhatEnrichStateCannotServe(t *testing.T) {
	need, other := contract.NewNeed("need"), contract.NewNeed("other")
	fetch := func(_ context.Context, s contract.State, _ contract.Request) contract.State { return s }
	route := func(ns ...*contract.Need) []contract.Route {
		return []contract.Route{{Name: "r", Method: http.MethodGet, Path: "/r", Needs: ns}}
	}
	for _, tc := range []struct {
		name      string
		routes    []contract.Route
		providers []contract.Provider
		wantErr   string // "" means build must succeed
	}{
		{"declared and provided", route(need), []contract.Provider{{Need: need, Fetch: fetch}}, ""},
		{"provided but unused", route(), []contract.Provider{{Need: need, Fetch: fetch}}, ""},
		{"declared, not provided", route(need), []contract.Provider{{Need: other, Fetch: fetch}}, `route "r" (GET /r) declares Need need`},
		{"declared nil Need", route(nil), nil, "declares Need <nil Need>"},
		{"declared twice", route(need, need), []contract.Provider{{Need: need, Fetch: fetch}}, `route "r" (GET /r) declares Need need twice`},
		{"provided twice", route(need), []contract.Provider{{Need: need, Fetch: fetch}, {Need: need, Fetch: fetch}}, "more than one Provider fills Need need"},
		{"provider without a Need", route(), []contract.Provider{{Fetch: fetch}}, "declares no Need"},
		{"provider without a Fetch", route(need), []contract.Provider{{Need: need}}, "Need need has no Fetch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetchers, err := needFetchers(tc.routes, tc.providers)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("needFetchers: %v, want success", err)
			case tc.wantErr == "" && len(fetchers) != len(tc.providers):
				t.Errorf("needFetchers indexed %d Fetches, want %d", len(fetchers), len(tc.providers))
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("needFetchers error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestBuildWiresEachNeedToItsOwner pins who fills what once build has run:
// every Need a served route declares has a Fetch, NeedPeerBusy is the
// Server's own (the peer set is the composition root's), and the Gogios
// Needs are the Gogios surface's -- the state each surface owns is filled by
// that surface, not by the root.
func TestBuildWiresEachNeedToItsOwner(t *testing.T) {
	srv := testServer()
	for _, r := range srv.router.routes {
		for _, n := range r.Needs {
			if _, ok := srv.fetchers[n]; !ok {
				t.Errorf("route %q declares Need %v, which build left without a Fetch", r.Name, n)
			}
		}
	}

	owners := map[*contract.Need]string{contract.NeedPeerBusy: "server"}
	for _, p := range srv.providers() {
		if p.Need != contract.NeedPeerBusy {
			t.Errorf("the Server provides Need %v: only the shared NeedPeerBusy is the root's", p.Need)
		}
	}
	for _, p := range testGogiosSurface("")(srv.actionRenderer()).Providers() {
		owners[p.Need] = "gogiosapi"
	}
	for need, want := range map[*contract.Need]string{
		contract.NeedPeerBusy:    "server",
		gogiosapi.NeedMonitoring: "gogiosapi",
		gogiosapi.NeedReport:     "gogiosapi",
	} {
		if got := owners[need]; got != want {
			t.Errorf("Need %v is provided by %q, want %q", need, got, want)
		}
	}
	if len(srv.fetchers) != len(owners) {
		t.Errorf("build indexed %d Fetches, want exactly the %d the Server and the Gogios surface provide",
			len(srv.fetchers), len(owners))
	}
}

// TestEnrichStatePanicsOnAnUnprovidedNeed pins the backstop for a Server
// whose router was swapped in past build: serving a route declaring a Need
// with no Fetch is a wiring bug, and fails loudly (serveCGI turns the panic
// into a 500) instead of rendering the route as if its state were absent.
func TestEnrichStatePanicsOnAnUnprovidedNeed(t *testing.T) {
	srv, _ := countingServer(t)
	r := contract.Route{Name: "orphaned", Needs: contract.Needs{contract.NewNeed("orphan")}}
	defer func() {
		if p := recover(); p == nil || !strings.Contains(fmt.Sprint(p), "orphan") {
			t.Errorf("enrichState recovered %v, want a panic naming the unprovided Need", p)
		}
	}()
	srv.enrichState(context.Background(), contract.State{}, r, getRequest("/"))
}

// TestNeedsAreFetchedInDeclarationOrder pins the order Route.Needs promises:
// enrichState runs the Fetches one after another in the order the route
// declares its Needs -- and the Gogios folder declares the gateway mute
// before the report, the order it was fetched in before Needs were a set.
func TestNeedsAreFetchedInDeclarationOrder(t *testing.T) {
	first, second := contract.NewNeed("first"), contract.NewNeed("second")
	var order []string
	record := func(name string) contract.Fetch {
		return func(_ context.Context, s contract.State, _ contract.Request) contract.State {
			order = append(order, name)
			return s
		}
	}
	srv := &Server{fetchers: map[*contract.Need]contract.Fetch{first: record("first"), second: record("second")}}

	srv.enrichState(context.Background(), contract.State{}, contract.Route{Needs: contract.Needs{second, first}}, contract.Request{})
	if !slices.Equal(order, []string{"second", "first"}) {
		t.Errorf("fetch order = %v, want the declaration order [second first]", order)
	}

	r, ok := routeNamed(testServer(), "gogios")
	if !ok {
		t.Fatal("no gogios route")
	}
	if want := (contract.Needs{gogiosapi.NeedMonitoring, gogiosapi.NeedReport}); !slices.Equal(r.Needs, want) {
		t.Errorf("GET /gogios Needs = %v, want %v: the mute is fetched before the report", r.Needs, want)
	}
}
