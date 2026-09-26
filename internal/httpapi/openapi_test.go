package httpapi

import (
	"context"
	"errors"
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

// fakeJobs is a powerapi.Jobs that never spawns a child: Start answers with
// a running job, or with err when one is set.
type fakeJobs struct{ err error }

func (f fakeJobs) Start(action string, _ []string) (coordination.Job, error) {
	if f.err != nil {
		return coordination.Job{}, f.err
	}
	return coordination.Job{
		ID: "j1", Action: action, State: coordination.JobRunning,
		Started: time.Now().UTC().Format(time.RFC3339), Node: "test",
	}, nil
}

func (fakeJobs) StaleCeiling() time.Duration { return time.Minute }
func (fakeJobs) Read() *coordination.Job     { return nil }

// failingPlug is a plugRecorder whose plug writes fail, the way an
// unreachable Shelly does.
type failingPlug struct{ plugRecorder }

func (*failingPlug) FansSet(context.Context, bool) (power.FansState, error) {
	return power.FansState{}, errFake{}
}

func (*failingPlug) ACSet(context.Context, bool) (power.ACState, error) {
	return power.ACState{}, errFake{}
}

// docServer is a Server over a cold fleet (every f-host down, so the power-on
// family is available) whose fan plug reads fansOn, with eng and jobs behind
// the power surface. Jobs never spawn a real child.
func docServer(t *testing.T, eng powerapi.Engine, jobs powerapi.Jobs, fansOn bool) *Server {
	t.Helper()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "apikey")
	if err := os.WriteFile(keyFile, []byte("sekrit\n"), 0o600); err != nil {
		t.Fatalf("writing the API key file: %v", err)
	}
	var hosts []power.HostStatus
	for _, name := range []string{"f0", "f1", "f2", "f3"} {
		hosts = append(hosts, power.HostStatus{Name: name, Role: "f", PingKnown: true})
	}

	cfg := config.Default()
	peers := coordination.NewPeerSet(nil, "")
	inv := inventory.Default()
	surface := powerapi.New("test", contract.Hrefs(""), inv, eng, jobs, peers)
	return (&Server{
		cfg: cfg, jobs: coordination.NewManager(dir, cfg.UnmuteTimeout.D(), 0), peers: peers,
		auth: NewAuthenticator(keyFile), siren: NewSirenRenderer(), node: "test",
		probeHosts: func(context.Context) []power.HostStatus { return hosts },
		fansStatus: func(context.Context) (power.FansState, error) {
			return power.FansState{On: fansOn}, nil
		},
		acStatus: func(context.Context) (power.ACState, error) { return power.ACState{}, nil },
	}).assemble(inv, surface, testGogiosSurface(), "")
}

// TestOpenAPIDocumentsTheStatusesHandlersReturn drives a synchronous action
// (fans-on) and a job action (power-on) through the real pipeline, on their
// success and their failure paths, and requires every status served to be
// documented for that operation. The success rows are the audit's finding:
// fans-on used to be documented as a 202 job while it answers 200.
func TestOpenAPIDocumentsTheStatusesHandlersReturn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		srv    *Server
		path   string
		apiKey string
		want   int
	}{
		{name: "sync action performed", srv: docServer(t, &plugRecorder{}, fakeJobs{}, false),
			path: "/fans/on", want: http.StatusOK},
		{name: "sync action plug write fails", srv: docServer(t, &failingPlug{}, fakeJobs{}, false),
			path: "/fans/on", want: http.StatusBadGateway},
		{name: "sync action not available", srv: docServer(t, &plugRecorder{}, fakeJobs{}, true),
			path: "/fans/on", want: http.StatusConflict},
		{name: "job action accepted", srv: docServer(t, &plugRecorder{}, fakeJobs{}, false),
			path: "/power/on", want: http.StatusAccepted},
		{name: "job action lock held", srv: docServer(t, &plugRecorder{}, fakeJobs{err: coordination.ErrJobRunning}, false),
			path: "/power/on", want: http.StatusConflict},
		{name: "job action spawn fails", srv: docServer(t, &plugRecorder{}, fakeJobs{err: errors.New("fork failed")}, false),
			path: "/power/on", want: http.StatusInternalServerError},
		{name: "bad API key", srv: docServer(t, &plugRecorder{}, fakeJobs{}, false),
			path: "/power/on", apiKey: "wrong", want: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := tc.apiKey
			if key == "" {
				key = "sekrit"
			}
			got := serveStatus(t, tc.srv, http.MethodPost, tc.path, key)
			if got != tc.want {
				t.Fatalf("POST %s = %d, want %d (the precondition this row exists to document)", tc.path, got, tc.want)
			}
			if !documented(t, tc.srv.openapi.Build(), http.MethodPost, tc.path, got) {
				t.Errorf("POST %s answered %d, which the OpenAPI document does not declare for it", tc.path, got)
			}
		})
	}
}

// serveStatus serves one request through the real pipeline and returns the
// CGI status code it wrote.
func serveStatus(t *testing.T, srv *Server, method, path, apiKey string) int {
	t.Helper()
	var out strings.Builder
	req := contract.Request{Method: method, Path: path, APIKey: apiKey, Query: url.Values{}, Form: url.Values{}}
	if err := srv.serve(&out, req); err != nil {
		t.Fatalf("serve(%s %s): %v", method, path, err)
	}
	status, _ := splitGogiosE2EResponse(t, out.String())
	return status
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
