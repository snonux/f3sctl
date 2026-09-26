package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/httpapi/gogiosapi"
	"github.com/snonux/f3sctl/internal/httpapi/powerapi"
	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power"
)

// The tests here hold the OpenAPI document to what the handlers actually
// answer: they drive requests through the real serve() pipeline and require
// every status that comes back to be one the document declares for that
// operation -- and, the other way round, that a synchronous action is not
// documented as a job, nor a job as synchronous.

// operation returns the Operation Object doc declares for method on path
// (a path as the document keys it, i.e. already under the router's base).
// It works on both a Build() result and a JSON-decoded /openapi.json.
func operation(t *testing.T, doc map[string]any, method, path string) map[string]any {
	t.Helper()
	paths, _ := doc["paths"].(map[string]any)
	entry, _ := paths[path].(map[string]any)
	op, ok := entry[lower(method)].(map[string]any)
	if !ok {
		t.Fatalf("the OpenAPI document has no %s %s operation", method, path)
	}
	return op
}

// documented reports whether doc declares status as a response of method on
// path.
func documented(t *testing.T, doc map[string]any, method, path string, status int) bool {
	t.Helper()
	responses, _ := operation(t, doc, method, path)["responses"].(map[string]any)
	_, ok := responses[strconv.Itoa(status)]
	return ok
}

// fakeJobs is a powerapi.Jobs that never spawns a child: Start records the
// job it was asked for and answers with it running, or with err when one is
// set.
type fakeJobs struct {
	err error
	// action and args are what the last Start was called with.
	action string
	args   []string
}

func (f *fakeJobs) Start(action string, args []string) (coordination.Job, error) {
	f.action, f.args = action, args
	if f.err != nil {
		return coordination.Job{}, f.err
	}
	return coordination.Job{
		ID: "j1", Action: action, State: coordination.JobRunning,
		Started: time.Now().UTC().Format(time.RFC3339), Node: "test",
	}, nil
}

func (*fakeJobs) StaleCeiling() time.Duration { return time.Minute }
func (*fakeJobs) Read() *coordination.Job     { return nil }

// fakePeers is a powerapi.Peers whose other node reports busy as set, and
// which counts the peer-job fetches made through it.
type fakePeers struct {
	busy    bool
	fetches int
}

func (p *fakePeers) Busy(context.Context, string, string) (bool, string) { return p.busy, "pi1" }

func (p *fakePeers) FetchJob(context.Context, string, string) *coordination.Job {
	p.fetches++
	return nil
}

// failingPlug is a plugRecorder whose plug writes fail, the way an
// unreachable Shelly does.
type failingPlug struct{ plugRecorder }

func (*failingPlug) FansSet(context.Context, bool) (power.FansState, error) {
	return power.FansState{}, errFake{}
}

func (*failingPlug) ACSet(context.Context, bool) (power.ACState, error) {
	return power.ACState{}, errFake{}
}

// fakeMonitor is a gogiosapi.Monitor whose mute writes succeed, or fail with
// err when one is set.
type fakeMonitor struct{ err error }

func (m fakeMonitor) MuteGogios(context.Context, io.Writer) error { return m.err }
func (m fakeMonitor) UnmuteNow(context.Context, io.Writer) error  { return m.err }
func (fakeMonitor) MonitoringStatus(context.Context) []power.GatewayMute {
	return []power.GatewayMute{{Name: "gw"}}
}

// docOpts shapes a docServer: what the fleet, the plugs and the gateways
// read as, and the collaborators behind the two surfaces. A nil collaborator
// is a well-behaved fake (writes succeed, no job, idle peer).
type docOpts struct {
	hostsUp, plugsOn, muted bool

	eng     powerapi.Engine
	jobs    powerapi.Jobs
	peers   powerapi.Peers
	monitor gogiosapi.Monitor
}

// docServer is a Server driven entirely by fakes -- no job child is ever
// spawned, no plug, gateway or Gogios endpoint is reached -- so a test can
// put any route into any outcome and read back the status it serves. The
// server's own peer set is always idle: a busy opts.peers is seen only by the
// power surface's handlers, not by serve()'s availability check.
func docServer(t *testing.T, o docOpts) *Server {
	t.Helper()
	o = o.withDefaults()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "apikey")
	if err := os.WriteFile(keyFile, []byte("sekrit\n"), 0o600); err != nil {
		t.Fatalf("writing the API key file: %v", err)
	}
	var hosts []power.HostStatus
	for _, name := range []string{"f0", "f1", "f2", "f3"} {
		hosts = append(hosts, power.HostStatus{Name: name, Role: "f", PingKnown: true, Ping: o.hostsUp, SSH: o.hostsUp})
	}

	cfg := config.Default()
	cfg.StateDir = dir
	cfg.GogiosURL = "http://127.0.0.1:1" // refused instantly: no network in tests
	cfg.GogiosFetchTimeout = config.Duration(time.Second)
	inv := inventory.Default()
	pw := powerapi.New("test", contract.Hrefs(""), inv, o.eng, o.jobs, o.peers)
	gg := gogiosapi.New("test", contract.Hrefs(""), cfg, o.monitor)
	return (&Server{
		cfg: cfg, jobs: coordination.NewManager(dir, cfg.UnmuteTimeout.D(), 0),
		peers: coordination.NewPeerSet(nil, ""),
		auth:  NewAuthenticator(keyFile), siren: NewSirenRenderer(), node: "test",
		probeHosts: func(context.Context) []power.HostStatus { return hosts },
		fansStatus: func(context.Context) (power.FansState, error) { return power.FansState{On: o.plugsOn}, nil },
		acStatus:   func(context.Context) (power.ACState, error) { return power.ACState{On: o.plugsOn}, nil },
		monitorStatus: func(context.Context) []power.GatewayMute {
			return []power.GatewayMute{{Name: "gw", Muted: o.muted}}
		},
	}).assemble(inv, pw, gg, "")
}

// withDefaults fills every nil collaborator with its well-behaved fake.
func (o docOpts) withDefaults() docOpts {
	if o.eng == nil {
		o.eng = &plugRecorder{}
	}
	if o.jobs == nil {
		o.jobs = &fakeJobs{}
	}
	if o.peers == nil {
		o.peers = &fakePeers{}
	}
	if o.monitor == nil {
		o.monitor = fakeMonitor{}
	}
	return o
}

// TestOpenAPIDocumentsTheStatusesHandlersReturn drives synchronous actions
// (the plugs, the mute pair) and a job action (power-on) through the real
// pipeline, on their success and their failure paths, and requires every
// status served to be documented for that operation. The first row is the
// audit's finding: fans-on used to be documented as a 202 job while it
// answers 200.
func TestOpenAPIDocumentsTheStatusesHandlersReturn(t *testing.T) {
	cold, hot := docOpts{}, docOpts{hostsUp: true, plugsOn: true, muted: true}
	busyPeer := docOpts{plugsOn: true, peers: &fakePeers{busy: true}}
	for _, tc := range []struct {
		name   string
		opts   docOpts
		path   string
		apiKey string
		want   int
	}{
		{name: "sync action performed", opts: cold, path: "/fans/on", want: http.StatusOK},
		{name: "sync action plug write fails", opts: docOpts{eng: &failingPlug{}},
			path: "/fans/on", want: http.StatusBadGateway},
		{name: "sync action not available", opts: hot, path: "/fans/on", want: http.StatusConflict},
		{name: "ac-off without force while hosts run", opts: hot, path: "/ac/off", want: http.StatusConflict},
		{name: "ac-off while a job started meanwhile", opts: busyPeer, path: "/ac/off", want: http.StatusConflict},
		{name: "mute gateway write fails", opts: docOpts{monitor: fakeMonitor{err: errFake{}}},
			path: "/monitoring/mute", want: http.StatusBadGateway},
		{name: "unmute gateway write fails", opts: docOpts{muted: true, monitor: fakeMonitor{err: errFake{}}},
			path: "/monitoring/unmute", want: http.StatusBadGateway},
		{name: "job action accepted", opts: cold, path: "/power/on", want: http.StatusAccepted},
		{name: "job action peer busy", opts: docOpts{peers: &fakePeers{busy: true}},
			path: "/power/on", want: http.StatusConflict},
		{name: "job action lock held", opts: docOpts{jobs: &fakeJobs{err: coordination.ErrJobRunning}},
			path: "/power/on", want: http.StatusConflict},
		{name: "job action spawn fails", opts: docOpts{jobs: &fakeJobs{err: errors.New("fork failed")}},
			path: "/power/on", want: http.StatusInternalServerError},
		{name: "bad API key", opts: cold, path: "/power/on", apiKey: "wrong", want: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := docServer(t, tc.opts)
			req := postRequest(tc.path, nil)
			if tc.apiKey != "" {
				req.APIKey = tc.apiKey
			}
			got := serveStatus(t, srv, req)
			if got != tc.want {
				t.Fatalf("POST %s = %d, want %d (the precondition this row exists to document)", tc.path, got, tc.want)
			}
			if !documented(t, srv.openapi.Build(), http.MethodPost, tc.path, got) {
				t.Errorf("POST %s answered %d, which the OpenAPI document does not declare for it", tc.path, got)
			}
		})
	}
}

// TestEveryActionAnswersItsDeclaredSuccessStatus drives every action route
// to success -- on a cold fleet or a hot one, whichever it is offered on,
// with force=true so the off switches skip their confirming probe -- and
// requires the status it answers to be exactly its Response.Status(). This is
// what ties Response to the handler that actually serves the route. For a
// job route it also checks the job the handler started: its own JobAction,
// with the argv JobArgsFrom derives for it -- the detached child runs
// nothing otherwise.
func TestEveryActionAnswersItsDeclaredSuccessStatus(t *testing.T) {
	routes := testRoutes(inventory.Default())
	jobs := []*fakeJobs{{}, {}}
	servers := []*Server{
		docServer(t, docOpts{jobs: jobs[0]}),
		docServer(t, docOpts{hostsUp: true, plugsOn: true, muted: true, jobs: jobs[1]}),
	}
	force := url.Values{"force": {"true"}}
	for _, r := range routes {
		if !r.Action {
			continue
		}
		var got []int
		for i, srv := range servers {
			status := serveStatus(t, srv, postRequest(r.Path, force))
			if status/100 != 2 {
				got = append(got, status)
				continue
			}
			if status != r.Response.Status() {
				t.Errorf("route %q answered %d, but declares Response status %d", r.Name, status, r.Response.Status())
			}
			if r.Response == contract.ResponseJob {
				checkJobStarted(t, routes, r, jobs[i])
			}
			got = nil
			break
		}
		if got != nil {
			t.Errorf("route %q never succeeded (answered %v): no fixture offers it", r.Name, got)
		}
	}
}

// checkJobStarted requires the job r's handler just started through jobs to
// be r's own JobAction, run with the non-empty argv JobArgsFrom derives for
// that action from routes.
func checkJobStarted(t *testing.T, routes []contract.Route, r contract.Route, jobs *fakeJobs) {
	t.Helper()
	if jobs.action != r.JobAction() {
		t.Errorf("route %q started job %q, want its JobAction %q", r.Name, jobs.action, r.JobAction())
	}
	want := powerapi.JobArgsFrom(routes, r.JobAction())
	if len(want) == 0 {
		t.Errorf("route %q: JobArgsFrom(%q) is empty, so the job child would run nothing", r.Name, r.JobAction())
	}
	if strings.Join(jobs.args, " ") != strings.Join(want, " ") {
		t.Errorf("route %q started its job with argv %q, want %q", r.Name, jobs.args, want)
	}
}

// postRequest is an authenticated POST to path carrying form as its body.
func postRequest(path string, form url.Values) contract.Request {
	if form == nil {
		form = url.Values{}
	}
	return contract.Request{Method: http.MethodPost, Path: path, APIKey: "sekrit", Query: url.Values{}, Form: form}
}

// serveStatus serves one request through the real pipeline and returns the
// CGI status code it wrote.
func serveStatus(t *testing.T, srv *Server, req contract.Request) int {
	t.Helper()
	var out strings.Builder
	if err := srv.serve(&out, req); err != nil {
		t.Fatalf("serve(%s %s): %v", req.Method, req.Path, err)
	}
	status, _ := splitGogiosE2EResponse(t, out.String())
	return status
}

// TestJobPeerParameterIsDeclaredAndHonoured pins the /job route's one query
// parameter from both sides: the document declares it (optional), and the
// handler really reads it -- a GET carrying it skips the peer-job fetch a
// plain GET makes -- with both answers documented.
func TestJobPeerParameterIsDeclaredAndHonoured(t *testing.T) {
	peers := &fakePeers{}
	srv := docServer(t, docOpts{peers: peers})
	doc := srv.openapi.Build()

	params, _ := operation(t, doc, http.MethodGet, powerapi.JobPath)["parameters"].([]any)
	if len(params) != 1 {
		t.Fatalf("job declares %d parameters, want exactly 1 (%s)", len(params), coordination.PeerQueryParam)
	}
	if p, _ := params[0].(map[string]any); p["name"] != coordination.PeerQueryParam || p["in"] != "query" || p["required"] != false {
		t.Errorf("job parameter = %v, want name=%s, in=query, required=false", p, coordination.PeerQueryParam)
	}

	for _, tc := range []struct {
		query       url.Values
		wantFetches int
	}{
		{query: url.Values{}, wantFetches: 1},
		{query: url.Values{coordination.PeerQueryParam: {"1"}}, wantFetches: 0},
	} {
		peers.fetches = 0
		req := getRequest(powerapi.JobPath)
		req.Query = tc.query
		got := serveStatus(t, srv, req)
		if !documented(t, doc, http.MethodGet, powerapi.JobPath, got) {
			t.Errorf("GET /job?%s answered %d, which is not documented", tc.query.Encode(), got)
		}
		if peers.fetches != tc.wantFetches {
			t.Errorf("GET /job?%s made %d peer-job fetches, want %d", tc.query.Encode(), peers.fetches, tc.wantFetches)
		}
	}
}

// TestOpenAPISuccessStatusFollowsResponseKind is the negative half, over the
// whole table: a job route documents 202 and never 200, every other route
// 200 and never 202. Documenting both would let a generated client accept
// either and hide exactly the sync/job mix-up this guards against.
func TestOpenAPISuccessStatusFollowsResponseKind(t *testing.T) {
	doc := testServer().openapi.Build()
	jobs := 0
	for _, r := range testRoutes(inventory.Default()) {
		if r.Path == openAPIPath {
			continue
		}
		want, notWant := http.StatusOK, http.StatusAccepted
		if r.Response == contract.ResponseJob {
			jobs++
			want, notWant = http.StatusAccepted, http.StatusOK
		}
		if !documented(t, doc, r.Method, r.Path, want) {
			t.Errorf("route %q does not document its success status %d", r.Name, want)
		}
		if documented(t, doc, r.Method, r.Path, notWant) {
			t.Errorf("route %q documents %d, which it never answers", r.Name, notWant)
		}
	}
	if jobs == 0 {
		t.Error("no route declares contract.ResponseJob: the power operations must")
	}
}

// TestOpenAPIDocumentsOnlyReachableErrors is the reverse check over the
// whole table: a generated error status must be one the route can reach. No
// 409 without an Available predicate or a job behind the route (the cache
// clear is always offered and starts no job), no 400 on anything but a POST.
// A route's own Errors may add either back, so those are exempted.
func TestOpenAPIDocumentsOnlyReachableErrors(t *testing.T) {
	doc := testServer().openapi.Build()
	for _, r := range testRoutes(inventory.Default()) {
		if r.Path == openAPIPath {
			continue
		}
		mayConflict := r.Available != nil || r.Response == contract.ResponseJob || declares(r, http.StatusConflict)
		if got := documented(t, doc, r.Method, r.Path, http.StatusConflict); got != mayConflict {
			t.Errorf("route %q documents 409 = %v, want %v", r.Name, got, mayConflict)
		}
		mayBadRequest := r.Method == http.MethodPost || declares(r, http.StatusBadRequest)
		if got := documented(t, doc, r.Method, r.Path, http.StatusBadRequest); got != mayBadRequest {
			t.Errorf("route %q documents 400 = %v, want %v", r.Name, got, mayBadRequest)
		}
	}

	if documented(t, doc, http.MethodPost, "/gogios/cache/clear", http.StatusConflict) {
		t.Error("gogios-cache-clear documents a 409, but it has no Available predicate and starts no job")
	}
	desc, _ := operation(t, doc, http.MethodPost, "/gogios/cache/clear")["description"].(string)
	if strings.Contains(desc, "409") {
		t.Errorf("gogios-cache-clear's description %q mentions a 409 it can never answer", desc)
	}
}

// declares reports whether r's own Errors list status.
func declares(r contract.Route, status int) bool {
	for _, e := range r.Errors {
		if e.Status == status {
			return true
		}
	}
	return false
}

// TestJobRoutesAreExactlyThePowerOperations pins which routes declare
// ResponseJob: exactly the power operations under /power/, whose handler
// starts a detached job -- and no plug, mute or cache action, which all
// answer synchronously.
func TestJobRoutesAreExactlyThePowerOperations(t *testing.T) {
	for _, r := range testRoutes(inventory.Default()) {
		isPowerOp := r.Action && strings.HasPrefix(r.Path, "/power/")
		if got := r.Response == contract.ResponseJob; got != isPowerOp {
			t.Errorf("route %q (%s) declares ResponseJob = %v, want %v", r.Name, r.Path, got, isPowerOp)
		}
	}
}

// TestOpenAPIDeclaresGogiosCheckNameParam pins the query parameter a
// generated client needs to call gogios-check at all: ?name=, required. And,
// as the control, that a route reading no query string declares none.
func TestOpenAPIDeclaresGogiosCheckNameParam(t *testing.T) {
	doc := testServer().openapi.Build()

	params, _ := operation(t, doc, http.MethodGet, "/gogios/check")["parameters"].([]any)
	if len(params) != 1 {
		t.Fatalf("gogios-check declares %d parameters, want exactly 1 (name)", len(params))
	}
	p, _ := params[0].(map[string]any)
	if p["name"] != "name" || p["in"] != "query" || p["required"] != true {
		t.Errorf("gogios-check parameter = %v, want name=name, in=query, required=true", p)
	}
	if schema, _ := p["schema"].(map[string]any); schema["type"] != "string" {
		t.Errorf("gogios-check name parameter schema = %v, want type string", p["schema"])
	}

	if _, ok := operation(t, doc, http.MethodGet, powerapi.StatusPath)["parameters"]; ok {
		t.Error("status declares parameters, but its handler reads no query string")
	}
}

// TestOpenAPIJoinsGenericAndRouteSpecificReasons pins that a status both the
// pipeline and a route's own handler can produce keeps both reasons: fans-off
// 409s on serve()'s availability backstop and on its own force guard.
func TestOpenAPIJoinsGenericAndRouteSpecificReasons(t *testing.T) {
	doc := testServer().openapi.Build()
	responses, _ := operation(t, doc, http.MethodPost, "/fans/off")["responses"].(map[string]any)
	conflict, _ := responses["409"].(map[string]any)
	desc, _ := conflict["description"].(string)
	for _, want := range []string{"read its actions", "force=true"} {
		if !strings.Contains(desc, want) {
			t.Errorf("fans-off 409 description = %q, want it to mention %q", desc, want)
		}
	}
}

// TestOpenAPIDocumentsGogiosCheckStatusesOverTheWire serves gogios-check and
// the cache-clear action through a real HTTP front end, reads the document
// itself from /openapi.json on the same server, and requires each status
// served -- found, not found, upstream down, cache cleared -- to be declared.
func TestOpenAPIDocumentsGogiosCheckStatusesOverTheWire(t *testing.T) {
	check := "/gogios/check?name="
	for _, tc := range []struct {
		name     string
		upstream int
		method   string
		path     string
		want     int
	}{
		{name: "found", upstream: http.StatusOK, method: http.MethodGet,
			path: check + url.QueryEscape("Check Ping4 master.buetow.org"), want: http.StatusOK},
		{name: "not found", upstream: http.StatusOK, method: http.MethodGet,
			path: check + url.QueryEscape("no such check"), want: http.StatusNotFound},
		{name: "upstream down", upstream: http.StatusInternalServerError, method: http.MethodGet,
			path: check + "anything", want: http.StatusBadGateway},
		{name: "cache cleared", upstream: http.StatusOK, method: http.MethodPost,
			path: "/gogios/cache/clear", want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, _ := gogiosE2EUpstream(t, gogiosE2EReportJSON, tc.upstream)
			e2e, _, apiKey := gogiosE2EServer(t, upstream)

			got := e2eStatus(t, e2e.URL, apiKey, tc.method, tc.path)
			if got != tc.want {
				t.Fatalf("%s %s = %d, want %d", tc.method, tc.path, got, tc.want)
			}
			status, doc := e2eGetRaw(t, e2e.URL, apiKey, openAPIPath)
			if status != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200", openAPIPath, status)
			}
			path, _, _ := strings.Cut(tc.path, "?")
			if !documented(t, doc, tc.method, path, got) {
				t.Errorf("%s %s answered %d, which /openapi.json does not declare for it", tc.method, path, got)
			}
		})
	}
}

// e2eStatus makes one authenticated request against the e2e front end and
// returns only its status code.
func e2eStatus(t *testing.T, base, apiKey, method, path string) int {
	t.Helper()
	req, err := http.NewRequest(method, base+path, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("X-API-Key", apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
