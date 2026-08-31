package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/httpapi/powerapi"
	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power"
)

// probeCounter counts calls to the two reads jy0 made lazy: the fleet probe
// (Engine.ProbeAll) and the Shelly plug read (Engine.FansStatus).
type probeCounter struct {
	probes, fanReads int
}

// countingServer returns a Server driven entirely through serve(), whose
// probeHosts and fansStatus seams are counters rather than a real
// *power.Engine -- see Server.probeHosts/fansStatus and probeHostsFn/
// fansStatusFn. No *power.Engine is built at all: every route exercised below
// (job, openapi.json, status) never reaches s.engine, so leaving it nil is
// safe and keeps the test off the network entirely.
//
// jobs and peers point at an empty temp dir and an empty peer set, so
// Manager.Read returns nil (no job has ever run) and PeerSet.Busy returns
// false without a network call (there are no peers to ask) -- both cheap and
// deterministic, matching what a real idle node would report.
func countingServer(t *testing.T) (*Server, *probeCounter) {
	t.Helper()

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "apikey")
	if err := os.WriteFile(keyFile, []byte("sekrit\n"), 0o600); err != nil {
		t.Fatalf("writing the API key file: %v", err)
	}

	pc := &probeCounter{}
	srv := (&Server{
		cfg:   config.Default(),
		jobs:  coordination.NewManager(dir, config.Default().UnmuteTimeout.D(), power.ShutdownWorstCase(config.Default())),
		peers: coordination.NewPeerSet(nil, ""),
		auth:  NewAuthenticator(keyFile),
		siren: NewSirenRenderer(),
		node:  "test",
		probeHosts: func(context.Context) []power.HostStatus {
			pc.probes++
			return nil
		},
		fansStatus: func(context.Context) (power.FansState, error) {
			pc.fanReads++
			return power.FansState{}, nil
		},
	}).assemble(inventory.Default(), testPowerSurface(inventory.Default()), testGogiosSurface(), "")
	return srv, pc
}

// getRequest builds a GET request against path, authenticated the way
// countingServer's Authenticator expects.
func getRequest(path string) contract.Request {
	return contract.Request{
		Method: http.MethodGet,
		Path:   path,
		APIKey: "sekrit",
		Query:  url.Values{},
		Form:   url.Values{},
	}
}

// TestResolvePeerJobPathDerivesFromSCRIPTNAME is the regression test for uy0:
// with cfg.PeerJobPath left at its empty default, the peer job path must
// track this node's own mount point (router's base, built from SCRIPT_NAME)
// rather than a hardcoded literal, so remounting the CGI script needs one
// change, not a config edit on top of it.
func TestResolvePeerJobPathDerivesFromSCRIPTNAME(t *testing.T) {
	base := "/cgi-bin/f3sctl" // SCRIPT_NAME minus the trailing slash
	cfg := config.Default()
	cfg.PeerJobPath = ""

	want := base + powerapi.JobPath
	if got := resolvePeerJobPath(cfg, base); got != want {
		t.Errorf("resolvePeerJobPath(empty override) = %q, want %q", got, want)
	}
}

// TestResolvePeerJobPathHonoursExplicitOverride covers the escape hatch: two
// peers that are *not* mounted the same way still need a way to point at the
// other node's real path, so an explicit config value must win over the
// SCRIPT_NAME-derived default.
func TestResolvePeerJobPathHonoursExplicitOverride(t *testing.T) {
	base := "/cgi-bin/f3sctl"
	cfg := config.Default()
	cfg.PeerJobPath = "/somewhere/else/job"

	if got := resolvePeerJobPath(cfg, base); got != cfg.PeerJobPath {
		t.Errorf("resolvePeerJobPath(explicit override) = %q, want %q", got, cfg.PeerJobPath)
	}
}

// TestResolvePeerJobPathFallsBackToDefaultCGIMountWhenBaseIsEmpty is the
// regression test for the HIGH-severity finding on uy0: this node's own
// SCRIPT_NAME going missing (a bozohttpd quirk, a proxy stripping the
// header, ServeCGI invoked outside its normal CGI harness) must not be
// silently read as "the API is mounted at the root". Deriving anyway would
// hand PeerSet a bare "/job", which almost certainly 404s on the peer --
// fetchPeerJob errors, and PeerSet.Busy reads that as "peer idle", the
// unreachable-peer-reads-as-idle failure mode this whole feature exists to
// prevent. With an empty router base and no explicit override,
// resolvePeerJobPath must fall back to defaultCGIMount instead.
func TestResolvePeerJobPathFallsBackToDefaultCGIMountWhenBaseIsEmpty(t *testing.T) {
	base := "" // SCRIPT_NAME empty or unset
	cfg := config.Default()
	cfg.PeerJobPath = ""

	want := defaultCGIMount + powerapi.JobPath
	if got := resolvePeerJobPath(cfg, base); got != want {
		t.Errorf("resolvePeerJobPath(empty base) = %q, want %q (the safe fallback, not a bare %q)",
			got, want, powerapi.JobPath)
	}
}

// TestSnapshotSkipsTheProbeForRoutesThatNeverReadIt is the regression test
// for jy0: serve() used to call s.snapshot(ctx) unconditionally, which ran
// Engine.ProbeAll (7 concurrent ping+TCP probes, ~3s) and Engine.FansStatus
// (a Shelly HTTP call, up to 5s) on every request -- including /job, polled
// every 10s through a multi-minute shutdown, and /openapi.json, a static
// document. Neither handler renders state.Hosts or state.Fans (see powerapi's
// handleJob and the root's handleOpenAPI), so both reads were pure waste on
// these two routes. This pins that they are no longer made at all.
func TestSnapshotSkipsTheProbeForRoutesThatNeverReadIt(t *testing.T) {
	for _, path := range []string{powerapi.JobPath, openAPIPath, "/"} {
		t.Run(path, func(t *testing.T) {
			srv, pc := countingServer(t)
			var out bytes.Buffer
			if err := srv.serve(&out, getRequest(path)); err != nil {
				t.Fatalf("serve(%s): %v", path, err)
			}
			if pc.probes != 0 {
				t.Errorf("ProbeAll called %d times serving %s, want 0", pc.probes, path)
			}
			if pc.fanReads != 0 {
				t.Errorf("FansStatus called %d times serving %s, want 0", pc.fanReads, path)
			}
		})
	}
}

// TestSnapshotStillProbesRoutesThatNeedIt is the control for the test above:
// /status renders state.Hosts and state.Fans directly (handleStatus), so the
// laziness in snapshot() must not have turned into "never probes anything" --
// it has to still pay for exactly one probe and one fan read here.
func TestSnapshotStillProbesRoutesThatNeedIt(t *testing.T) {
	srv, pc := countingServer(t)
	var out bytes.Buffer
	if err := srv.serve(&out, getRequest("/status")); err != nil {
		t.Fatalf("serve(/status): %v", err)
	}
	if pc.probes != 1 {
		t.Errorf("ProbeAll called %d times serving /status, want exactly 1", pc.probes)
	}
	if pc.fanReads != 1 {
		t.Errorf("FansStatus called %d times serving /status, want exactly 1", pc.fanReads)
	}
}

// TestSkipsProbeCoversTheMonitoringFamily pins that skipsProbe treats every
// /monitoring path -- the resource itself and its mute/unmute actions -- the
// same way, matching the "/monitoring" prefix check enrichState already uses
// to decide whether to fetch the Gogios mute. Every handler in the mute
// family -- Gogios surface's handleMonitoring, handleMute, handleUnmute --
// renders only state.Monitoring; see gogiosapi/handlers.go.
//
// The root belongs on the SkipsProbe side since the section folders took
// over its actions list: handleRoot renders properties and links only, no
// state-derived data, so pointing a browser's menu at the entry point costs
// no probe at all. Everything that still renders state-derived actions
// (/status, the fans pair, /power's folder list) must keep probing.
func TestSkipsProbeCoversTheMonitoringFamily(t *testing.T) {
	for _, path := range []string{"/", "/monitoring", "/monitoring/mute", "/monitoring/unmute"} {
		if !skipsProbe(testRoutes(inventory.Default()), path) {
			t.Errorf("skipsProbe(%q) = false, want true: this handler renders no probe-derived state", path)
		}
	}
	for _, path := range []string{"/power", "/status", "/fans", "/fans/on", "/fans/off", "/power/off"} {
		if skipsProbe(testRoutes(inventory.Default()), path) {
			t.Errorf("skipsProbe(%q) = true, want false: this route's handler or Available predicates do read the probe", path)
		}
	}
}

// cgiEnvForJob sets the CGI environment variables ServeCGI reads (see
// parseCGIRequest) for a GET request against the job path, authenticated with
// apiKey. powerapi.JobPath is used by both tests below because it is the one route
// that needs no live probe, no peer network call and no Gogios SSH round
// trip (see skipsProbe and enrichState) -- so the only things standing
// between "bad key" and "200 with the job entity" are the pieces qz0 is
// about: auth, routing and siren rendering, not network flakiness.
func cgiEnvForJob(t *testing.T, apiKey string) {
	t.Helper()
	t.Setenv("REQUEST_METHOD", http.MethodGet)
	t.Setenv("PATH_INFO", powerapi.JobPath)
	t.Setenv("QUERY_STRING", "")
	t.Setenv("HTTP_X_API_KEY", apiKey)
	// Empty SCRIPT_NAME gives the Router an empty base, so hrefs below are
	// predictable (the job path's own href is the bare path) without
	// depending on whatever the test process's own environment happens to
	// have set.
	t.Setenv("SCRIPT_NAME", "")
	t.Setenv("CONTENT_LENGTH", "")
}

// TestAssembleInjectsRouterActionRenderingIntoThePowerSurface pins the wiring
// assemble exists for: a power-surface handler that renders a resource-scoped
// actions list (powerapi's handleFans) gets it from the composition root's
// Router, not
// from anything local to the surface. Without this injection the fans
// resource would render zero actions -- every client would then see a plug it
// may read but never switch -- and no availability test would catch it, since
// predicates live on the routes this wiring advertises.
func TestAssembleInjectsRouterActionRenderingIntoThePowerSurface(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "apikey")
	if err := os.WriteFile(keyFile, []byte("sekrit\n"), 0o600); err != nil {
		t.Fatalf("writing the API key file: %v", err)
	}

	srv := (&Server{
		cfg:   config.Default(),
		jobs:  coordination.NewManager(t.TempDir(), config.Default().UnmuteTimeout.D(), power.ShutdownWorstCase(config.Default())),
		peers: coordination.NewPeerSet(nil, ""),
		auth:  NewAuthenticator(keyFile),
		siren: NewSirenRenderer(),
		node:  "test",
		probeHosts: func(context.Context) []power.HostStatus {
			// No fleet probe in this test; the fans snapshot is what the fan
			// actions' availability reads.
			return nil
		},
		fansStatus: func(context.Context) (power.FansState, error) {
			return power.FansState{On: true}, nil
		},
	}).assemble(inventory.Default(), testPowerSurface(inventory.Default()), testGogiosSurface(), "")

	var out bytes.Buffer
	if err := srv.serve(&out, getRequest("/fans")); err != nil {
		t.Fatalf("serve(/fans): %v", err)
	}
	_, body := splitCGIResponse(t, out.String())
	var got contract.Entity
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", err, body)
	}
	if len(got.Actions) != 1 || got.Actions[0].Name != "fans-off" {
		t.Fatalf("/fans actions = %+v, want exactly [fans-off] (the plug is on)", got.Actions)
	}
	if got.Actions[0].CLIVerb != "fans off" {
		t.Errorf("fans-off cliVerb = %q, want \"fans off\" (rendered by the Router, from the route declaration)", got.Actions[0].CLIVerb)
	}
}

// serverTestConfig returns a config.Default() pointed at throwaway,
// test-local files: an API key file under a fresh temp dir (also used as
// StateDir, so Manager.Read() sees no job ever having run) and no peers to
// contact. It is deliberately built with config.Default() plus overrides --
// the same shape newServer's caller (cmd/f3sctl/main.go) uses -- rather than
// a hand-picked subset of fields, since the whole point of the two tests
// below is to construct a Server the way production does.
func serverTestConfig(t *testing.T, apiKey string) config.Config {
	t.Helper()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "apikey")
	if err := os.WriteFile(keyFile, []byte(apiKey+"\n"), 0o600); err != nil {
		t.Fatalf("writing the API key file: %v", err)
	}

	cfg := config.Default()
	cfg.APIKeyFile = keyFile
	cfg.StateDir = dir
	cfg.PeerNodes = nil
	return cfg
}

// TestServeCGIEndToEndRejectsBadAPIKeyBeforeTouchingTheHandler is the
// regression test for qz0: every Server built in this package's tests before
// it was a literal carrying only the fields one handler needed (see
// countingServer above and handlers_test.go's testServer, both of which
// leave auth, siren, openapi, peers nil) and called a handler function
// directly -- so nothing ever exercised the fully-wired production pipeline
// serve() actually runs: auth check -> route lookup -> peer-busy check ->
// handler -> siren render. A Server missing its Authenticator would panic
// the instant serve() reached s.auth.Check (nil pointer dereference), which
// this test would catch immediately, not silently pass.
//
// This drives the request through ServeCGI -- the exact function
// cmd/f3sctl/main.go calls in CGI mode -- reading the request from real CGI
// environment variables and stdin the way parseCGIRequest expects, so the
// Server under test is the one newServer actually builds, and the assertion
// is against the real response bytes, not a handler's return value taken as
// a shortcut.
func TestServeCGIEndToEndRejectsBadAPIKeyBeforeTouchingTheHandler(t *testing.T) {
	cfg := serverTestConfig(t, "correct-key")
	cgiEnvForJob(t, "wrong-key")

	var out bytes.Buffer
	if err := ServeCGI(cfg, &out); err != nil {
		t.Fatalf("ServeCGI: %v", err)
	}

	headers, body := splitCGIResponse(t, out.String())
	if headers["Status"] != "401 Unauthorized" {
		t.Fatalf("Status header = %q, want %q: a fully-wired Server must reject a bad key before reaching any handler\nbody: %s",
			headers["Status"], "401 Unauthorized", body)
	}

	var got struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", err, body)
	}
	if msg, _ := got.Properties["message"].(string); msg != "unauthorized" {
		t.Errorf("properties.message = %q, want %q", msg, "unauthorized")
	}
}

// TestServeCGIEndToEndRendersTheJobEntityThroughTheRealPipeline is qz0's
// positive case, and the true differentiator from calling the power surface's
// handleJob directly: the job entity's "self"/"up" links come out of the
// injected href builder, and a Server assembled with a half-wired surface
// (exactly the gap in hand-built Server literals -- nil jobs or peers would
// have panicked here long before the body ever rendered) produces a real,
// fully-linked response only when the whole production wiring ran. It also
// proves the response actually went through SirenRenderer (the CGI header
// block, the Siren media type) rather than just checking the Entity the
// power surface's handleJob returns.
func TestServeCGIEndToEndRendersTheJobEntityThroughTheRealPipeline(t *testing.T) {
	cfg := serverTestConfig(t, "correct-key")
	cgiEnvForJob(t, "correct-key")

	var out bytes.Buffer
	if err := ServeCGI(cfg, &out); err != nil {
		t.Fatalf("ServeCGI: %v", err)
	}

	headers, body := splitCGIResponse(t, out.String())
	if headers["Status"] != "200 OK" {
		t.Fatalf("Status header = %q, want %q\nbody: %s", headers["Status"], "200 OK", body)
	}
	if headers["Content-Type"] != sirenMediaType {
		t.Errorf("Content-Type = %q, want the Siren media type %q", headers["Content-Type"], sirenMediaType)
	}

	var got contract.Entity
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", err, body)
	}
	if len(got.Class) != 1 || got.Class[0] != "job" {
		t.Errorf("class = %v, want [job]", got.Class)
	}
	if state, _ := got.Properties["state"].(string); state != "none" {
		t.Errorf("properties.state = %q, want %q: no job has ever run in this fresh StateDir", state, "none")
	}

	var self string
	for _, l := range got.Links {
		for _, rel := range l.Rel {
			if rel == "self" {
				self = l.Href
			}
		}
	}
	if self != powerapi.JobPath {
		t.Errorf("self link = %q, want %q (router.Href(%q) with an empty SCRIPT_NAME base): "+
			"a nil Router would have panicked before this response was ever produced", self, powerapi.JobPath, powerapi.JobPath)
	}
}

// decodedEntity is a served Siren entity, decoded loosely: tests below
// assert on the JSON's own shape (which keys exist at all), which contract.Entity
// would hide with its omitempty zero values.
type servedEntity struct {
	Class      []string          `json:"class"`
	Title      string            `json:"title"`
	Links      []contract.Link   `json:"links"`
	Actions    []contract.Action `json:"actions"`
	Properties map[string]any    `json:"properties"`
}

// getEntity serves one authenticated GET request through the real pipeline
// and decodes the Siren body (serve writes CGI headers first -- split them).
func getEntity(t *testing.T, srv *Server, path string) servedEntity {
	t.Helper()
	var out bytes.Buffer
	if err := srv.serve(&out, getRequest(path)); err != nil {
		t.Fatalf("serve(%s): %v", path, err)
	}
	headers, body, ok := bytes.Cut(out.Bytes(), []byte("\r\n\r\n"))
	if !ok {
		t.Fatalf("serve(%s) wrote no CGI header block:\n%s", path, out.String())
	}
	var e servedEntity
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("body of %s is not valid JSON: %v\nheaders: %s\nbody: %s", path, err, headers, body)
	}
	return e
}

// hasRel returns whether links carries rel (first rel element only, the way
// every link in this API is declared).
func hasServedRel(links []contract.Link, rel string) bool {
	for _, l := range links {
		if len(l.Rel) > 0 && l.Rel[0] == rel {
			return true
		}
	}
	return false
}

// actionNames lists a served entity's action names.
func actionNames(e servedEntity) []string {
	var out []string
	for _, a := range e.Actions {
		out = append(out, a.Name)
	}
	return out
}

// hasAction returns whether the entity offers an action by name.
func hasAction(e servedEntity, name string) bool {
	for _, n := range actionNames(e) {
		if n == name {
			return true
		}
	}
	return false
}

// TestRootIsAFolderIndex pins the folder-shaped overview: the root carries no
// actions of its own -- every operation lives in its section folder -- and
// its links are the two section folders plus the read-only resources. The
// drill-downs and the mute resource must NOT be linked from the root: they
// are what the folders are for. See handleRoot's folder comment and
// enrichState's folder routes.
func TestRootIsAFolderIndex(t *testing.T) {
	srv, pc := countingServer(t)
	e := getEntity(t, srv, "/")

	if len(e.Actions) != 0 {
		t.Errorf("root actions = %v, want none: the operations live in the section folders", actionNames(e))
	}
	for _, rel := range []string{"self", "describedby", "power", "status", "job", "fans", "gogios"} {
		if !hasServedRel(e.Links, rel) {
			t.Errorf("root links = %+v, missing rel %q", e.Links, rel)
		}
	}
	for _, rel := range []string{"monitoring", "gogios-critical"} {
		if hasServedRel(e.Links, rel) {
			t.Errorf("root links = %+v carry rel %q: that belongs in its section folder", e.Links, rel)
		}
	}
	if pc.probes != 0 || pc.fanReads != 0 {
		t.Errorf("root fetch paid %d probes and %d fan reads, want neither: the overview is a folder index now (SkipsProbe)", pc.probes, pc.fanReads)
	}
}

// folderServer is countingServer with a chosen fleet: the named hosts are all
// ping+SSH-up, the fan plug reads as on, and the gateway mute read returns
// the given state. It exists so the folder tests can judge availability.
func folderServer(t *testing.T, hosts []power.HostStatus, monitor func(context.Context) []power.GatewayMute) *Server {
	t.Helper()

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "apikey")
	if err := os.WriteFile(keyFile, []byte("sekrit\n"), 0o600); err != nil {
		t.Fatalf("writing the API key file: %v", err)
	}
	cfg := config.Default()
	cfg.GogiosURL = "http://127.0.0.1:1" // refused instantly: no network in tests
	cfg.GogiosFetchTimeout = config.Duration(time.Second)
	cfg.GogiosCacheTTL = config.Duration(time.Minute)
	return (&Server{
		cfg:   cfg,
		jobs:  coordination.NewManager(t.TempDir(), cfg.UnmuteTimeout.D(), 0),
		peers: coordination.NewPeerSet(nil, ""),
		auth:  NewAuthenticator(keyFile),
		siren: NewSirenRenderer(),
		node:  "test",
		probeHosts: func(context.Context) []power.HostStatus {
			return hosts
		},
		fansStatus: func(context.Context) (power.FansState, error) {
			return power.FansState{On: true}, nil
		},
		monitorStatus: monitor,
	}).assemble(inventory.Default(), testPowerSurface(inventory.Default()), testGogiosSurface(), "")
}

// TestPowerFolderOffersThePowerActions pins that /power is the one overview
// of the domain: with a fleet up and cooling, the folder advertises the
// shutdowns it can actually perform and the fans-off confirmation, withholds
// what cannot work (waking an up host), and links its own resources. This is
// the list the ROOT used to carry -- see handlePowerFolder and handleRoot.
func TestPowerFolderOffersThePowerActions(t *testing.T) {
	hosts := []power.HostStatus{
		{Name: "f0", Role: "f", Ping: true, PingKnown: true, SSH: true},
		{Name: "f1", Role: "f", Ping: true, PingKnown: true, SSH: true},
		{Name: "f2", Role: "f", Ping: true, PingKnown: true, SSH: true},
	}
	srv := folderServer(t, hosts, nil)
	e := getEntity(t, srv, "/power")

	if e.Title != "Power control" {
		t.Errorf("title = %q, want %q", e.Title, "Power control")
	}
	for _, name := range []string{"power-off", "all-off", "f0-off", "fans-off"} {
		if !hasAction(e, name) {
			t.Errorf("power folder actions = %v, want %s offered (the fleet is up and the plug is on)", actionNames(e), name)
		}
	}
	for _, name := range []string{"power-on", "all-on", "f0-on", "fans-on"} {
		if hasAction(e, name) {
			t.Errorf("power folder actions = %v, want %s withheld: only possible actions are ever advertised", actionNames(e), name)
		}
	}
	for _, rel := range []string{"self", "up", "status", "job", "fans"} {
		if !hasServedRel(e.Links, rel) {
			t.Errorf("power folder links = %+v, missing rel %q", e.Links, rel)
		}
	}
}

// TestGogiosFolderCarriesTheMutePair pins that the /gogios folder offers the
// whole family's controls, not just the report browser's: the cache clear
// always, and exactly one of mute/unmute judged on the same gateway mute
// state /monitoring renders. This is what makes /gogios a folder rather than
// only a report -- see handleOverview and enrichState's IsFolderPath fetch.
func TestGogiosFolderCarriesTheMutePair(t *testing.T) {
	tests := []struct {
		name string
		mute []power.GatewayMute
		want string // the mute action that must be offered
	}{
		{"muted", []power.GatewayMute{{Name: "blowfish", Muted: true}}, "monitoring-unmute"},
		{"alerting", []power.GatewayMute{{Name: "blowfish", Muted: false}}, "monitoring-mute"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := folderServer(t, nil, func(context.Context) []power.GatewayMute { return tt.mute })
			e := getEntity(t, srv, "/gogios")

			if !hasAction(e, tt.want) {
				t.Errorf("gogios folder actions = %v, want %s offered while %s", actionNames(e), tt.want, tt.name)
			}
			if !hasAction(e, "gogios-cache-clear") {
				t.Errorf("gogios folder actions = %v, want gogios-cache-clear offered (always available)", actionNames(e))
			}
		})
	}
}
