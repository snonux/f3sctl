package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/snonux/f3sctl/internal/client"
	"github.com/snonux/f3sctl/internal/config"
)

// TestParseGlobalFlags is the table test the wy0 annotation asked for: before
// this, TestParseGlobalFlagsConsumesForce (below, pre-existing) was the only
// case exercised, leaving --remote/-r, --local/-l and --verbose/-v -- and the
// "-v implies --remote" rule in particular -- with no coverage at all.
//
// globalFlags is a plain struct of bools, so it is compared with == rather
// than field by field.
func TestParseGlobalFlags(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantRest  []string
		wantFlags globalFlags
	}{
		{
			name:      "no flags",
			args:      []string{"power", "status"},
			wantRest:  []string{"power", "status"},
			wantFlags: globalFlags{},
		},
		{
			name:      "--remote",
			args:      []string{"--remote", "power", "status"},
			wantRest:  []string{"power", "status"},
			wantFlags: globalFlags{remote: true},
		},
		{
			name:      "-r shorthand",
			args:      []string{"-r", "power", "status"},
			wantRest:  []string{"power", "status"},
			wantFlags: globalFlags{remote: true},
		},
		{
			name:      "--local",
			args:      []string{"--local", "fans", "status"},
			wantRest:  []string{"fans", "status"},
			wantFlags: globalFlags{local: true},
		},
		{
			name:      "-l shorthand",
			args:      []string{"-l", "fans", "status"},
			wantRest:  []string{"fans", "status"},
			wantFlags: globalFlags{local: true},
		},
		{
			name:      "--force",
			args:      []string{"fans", "off", "--force"},
			wantRest:  []string{"fans", "off"},
			wantFlags: globalFlags{force: true},
		},
		{
			name:      "-f shorthand",
			args:      []string{"fans", "off", "-f"},
			wantRest:  []string{"fans", "off"},
			wantFlags: globalFlags{force: true},
		},
		{
			// The deliberately surprising rule at remote.go's parseGlobalFlags:
			// tracing only makes sense against the API, so --verbose implies
			// --remote rather than tracing a local run that never makes an HTTP
			// call.
			name:      "--verbose implies --remote",
			args:      []string{"--verbose", "power", "status"},
			wantRest:  []string{"power", "status"},
			wantFlags: globalFlags{verbose: true, remote: true},
		},
		{
			name:      "-v shorthand implies --remote",
			args:      []string{"-v", "power", "status"},
			wantRest:  []string{"power", "status"},
			wantFlags: globalFlags{verbose: true, remote: true},
		},
		{
			name:      "every flag combined",
			args:      []string{"-r", "-f", "-v", "-l", "power", "status"},
			wantRest:  []string{"power", "status"},
			wantFlags: globalFlags{remote: true, force: true, verbose: true, local: true},
		},
		{
			// parseGlobalFlags accepts global flags anywhere in the argument
			// list, not just before or after the command -- this pins that a
			// flag in the middle is still recognised and still removed.
			name:      "flags interspersed among the command",
			args:      []string{"power", "--force", "status", "-r"},
			wantRest:  []string{"power", "status"},
			wantFlags: globalFlags{remote: true, force: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rest, flags := parseGlobalFlags(tc.args)
			if !slices.Equal(rest, tc.wantRest) {
				t.Errorf("rest = %v, want %v", rest, tc.wantRest)
			}
			if flags != tc.wantFlags {
				t.Errorf("flags = %+v, want %+v", flags, tc.wantFlags)
			}
		})
	}
}

// TestGlobalFlagsUseAPI covers useAPI's three branches directly -- it was at
// 60% coverage, exercised only incidentally via globalFlags{}.useAPI in
// TestIsShutdownAgreesWithPowerActionFor. --local's override and --remote's
// unconditional "yes" had no test of their own.
func TestGlobalFlagsUseAPI(t *testing.T) {
	shutdown := []string{"power", "off"}
	readOnly := []string{"power", "status"}

	cases := []struct {
		name  string
		flags globalFlags
		args  []string
		want  bool
	}{
		{"local overrides a shutdown", globalFlags{local: true}, shutdown, false},
		{"local overrides remote too", globalFlags{local: true, remote: true}, shutdown, false},
		{"remote forces even a read-only command", globalFlags{remote: true}, readOnly, true},
		{"neither flag: a shutdown still routes to the API", globalFlags{}, shutdown, true},
		{"neither flag: a read-only power command stays local", globalFlags{}, readOnly, false},
		{"neither flag: monitoring always routes to the API", globalFlags{}, []string{"monitoring", "mute"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.flags.useAPI(tc.args); got != tc.want {
				t.Errorf("%+v.useAPI(%v) = %v, want %v", tc.flags, tc.args, got, tc.want)
			}
		})
	}
}

// TestIsMonitoringAgreesWithParseMonitoringArgs is the routing-level half of
// the iz0 regression, mirroring TestIsShutdownAgreesWithPowerActionFor for
// monitoring: isMonitoring used to check only that args held exactly two
// tokens starting with "monitoring", not that the second was a real verb. A
// malformed verb of the right arity -- "monitoring xyz" -- was therefore
// still routed to the API, which reports "not available right now" with a
// nil error (an exit-0 no-op) instead of the local "unknown monitoring
// command" every other bad spelling gets. isMonitoring must agree with
// parseMonitoringArgs on every case: routable if and only if the args after
// "monitoring" parse to a real verb.
func TestIsMonitoringAgreesWithParseMonitoringArgs(t *testing.T) {
	for _, args := range [][]string{
		{"monitoring", "status"},
		{"monitoring", "mute"},
		{"monitoring", "unmute"},
		{"monitoring", "xyz"}, // the misclassified spelling
		{"monitoring", "mute", "junk"},
		{"monitoring"},
		{"power", "status"},
		{},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var want bool
			if len(args) > 0 && args[0] == "monitoring" {
				_, ok := parseMonitoringArgs(args[1:])
				want = ok
			}
			if got := isMonitoring(args); got != want {
				t.Errorf("isMonitoring(%v) = %v, want %v", args, got, want)
			}
		})
	}

	// The malformed spelling the reviewer verified concretely: a real verb
	// with the wrong word must not reach the API's confusing no-op.
	if isMonitoring([]string{"monitoring", "xyz"}) {
		t.Error(`isMonitoring([]string{"monitoring", "xyz"}) = true, want false: not a real monitoring verb`)
	}
	if (globalFlags{}).useAPI([]string{"monitoring", "xyz"}) {
		t.Error(`globalFlags{}.useAPI on "monitoring xyz" = true, want false: must stay local and hit runMonitoring's validation`)
	}
}

// TestIsGogiosAgreesWithParseGogiosArgs is the gogios sibling of
// TestIsMonitoringAgreesWithParseMonitoringArgs: isGogios must agree with
// parseGogiosArgs on every case, or a malformed verb of otherwise-plausible
// shape (e.g. a real word in the wrong slot) would be routed to the API's
// confusing no-op instead of reaching runGogios's own local validation.
func TestIsGogiosAgreesWithParseGogiosArgs(t *testing.T) {
	for _, args := range [][]string{
		{"gogios"},
		{"gogios", "status"},
		{"gogios", "critical"},
		{"gogios", "stale"},
		{"gogios", "detail", "x"},
		{"gogios", "detail"},
		{"gogios", "cache", "clear"},
		{"gogios", "cache"},
		{"gogios", "xyz"}, // the misclassified spelling
		{"monitoring", "status"},
		{},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var want bool
			if len(args) > 0 && args[0] == "gogios" {
				_, ok := parseGogiosArgs(args[1:])
				want = ok
			}
			if got := isGogios(args); got != want {
				t.Errorf("isGogios(%v) = %v, want %v", args, got, want)
			}
		})
	}

	if isGogios([]string{"gogios", "xyz"}) {
		t.Error(`isGogios([]string{"gogios", "xyz"}) = true, want false: not a real gogios verb`)
	}
	if (globalFlags{}).useAPI([]string{"gogios", "xyz"}) {
		t.Error(`globalFlags{}.useAPI on "gogios xyz" = true, want false: must stay local and hit runGogios's validation`)
	}
	if !(globalFlags{}).useAPI([]string{"gogios", "status"}) {
		t.Error(`globalFlags{}.useAPI on "gogios status" = false, want true: gogios defaults through the API, like monitoring`)
	}
}

// fakeRemoteAPI is a minimal Siren-over-HTTP fixture for driving runRemote
// end to end: GET / (root, linking to "fans" and "status"), GET /fans
// (advertising fans-off with its required force checkbox, exactly as
// internal/httpapi/registry.go does), POST /fans/off, and GET /status (so
// runAction's post-action showStatus has somewhere to land). It exists
// because internal/client's own fake (internal/client/remote_test.go) is
// unexported and this test wants to pin the *cli-side* wiring -- runRemote
// itself, and parseGlobalFlags/useAPI feeding it -- not just the client
// package in isolation.
type fakeRemoteAPI struct {
	srv     *httptest.Server
	wantKey string

	mu        sync.Mutex
	forceSent []string
}

func newFakeRemoteAPI(t *testing.T, wantKey string) *fakeRemoteAPI {
	t.Helper()
	f := &fakeRemoteAPI{wantKey: wantKey}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRemoteAPI) handle(w http.ResponseWriter, r *http.Request) {
	// Every route, root included, requires the key: internal/httpapi/server.go's
	// Server.serve calls s.auth.Check(req.APIKey) unconditionally before
	// dispatching to any handler, and docs/CLIENT.md says a client MUST send
	// X-API-Key on every request with no root exception. Client.do
	// (internal/client/client.go) always sets the header regardless of route,
	// so this gate never sees an unauthenticated request in the correct-key
	// tests above.
	if r.Header.Get("X-API-Key") != f.wantKey {
		w.WriteHeader(http.StatusUnauthorized)
		writeRemoteEntity(w, client.Entity{Properties: map[string]any{"message": "bad API key"}})
		return
	}

	switch {
	case r.URL.Path == "/" && r.Method == http.MethodGet:
		f.handleRoot(w)
	case r.URL.Path == "/fans" && r.Method == http.MethodGet:
		f.handleFans(w)
	case r.URL.Path == "/fans/off" && r.Method == http.MethodPost:
		f.handleFansOff(w, r)
	case r.URL.Path == "/status" && r.Method == http.MethodGet:
		writeRemoteEntity(w, client.Entity{})
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

// handleRoot answers GET / with the two links runAction needs: "fans", to
// find the fans-off action, and "status", for the post-action showStatus.
func (f *fakeRemoteAPI) handleRoot(w http.ResponseWriter) {
	writeRemoteEntity(w, client.Entity{
		Properties: map[string]any{"apiVersion": float64(client.SupportedAPIVersion)},
		Links: []client.Link{
			{Rel: []string{"fans"}, Href: "/fans"},
			{Rel: []string{"status"}, Href: "/status"},
		},
	})
}

// handleFans answers GET /fans, advertising fans-off with a required force
// checkbox -- the same shape internal/httpapi/registry.go's real advertising
// takes, which is what makes Client.Perform decide whether to fill the field.
func (f *fakeRemoteAPI) handleFans(w http.ResponseWriter) {
	writeRemoteEntity(w, client.Entity{
		Class: []string{"fans"},
		Actions: []client.Action{{
			Name:    "fans-off",
			Title:   "Switch the rack fans off",
			Method:  http.MethodPost,
			Href:    "/fans/off",
			CLIVerb: "fans off",
			Fields: []client.Field{
				{Name: "force", Type: "checkbox", Title: "hosts may still be running", Required: true},
			},
		}},
	})
}

// handleFansOff answers POST /fans/off, recording the "force" form value the
// tests assert on and reporting a synchronous state change.
func (f *fakeRemoteAPI) handleFansOff(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.forceSent = append(f.forceSent, r.PostForm.Get("force"))
	f.mu.Unlock()
	writeRemoteEntity(w, client.Entity{Properties: map[string]any{"on": false}})
}

func (f *fakeRemoteAPI) forceValues() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.forceSent...)
}

func writeRemoteEntity(w http.ResponseWriter, e client.Entity) {
	w.Header().Set("Content-Type", "application/vnd.siren+json")
	w.Header().Set("X-F3sctl-Node", "test-node")
	_ = json.NewEncoder(w).Encode(e)
}

// remoteConfig points ResolveAPIURL/ResolveAPIKey (internal/config) at api,
// via the environment variables those methods check first -- the same knobs
// F3SCTL_URL/F3SCTL_KEY document for pointing the real binary at a test
// instance.
func remoteConfig(t *testing.T, api *fakeRemoteAPI, key string) config.Config {
	t.Helper()
	t.Setenv("F3SCTL_URL", api.srv.URL)
	t.Setenv("F3SCTL_KEY", key)
	return config.Default()
}

// TestRunRemoteForceFlagReachesTheServer is the end-to-end coverage the wy0
// annotation asked for: internal/cli/remote.go's runRemote (0% coverage
// before this) driven from run() with a real --remote/--force invocation,
// through client.New, client.Run, Client.Perform and Client.do, against a
// real HTTP server. The assertion that gives this its teeth is that the fake
// server actually received force=true -- confirming the flag survives the
// whole trip from argv to the wire, not merely that the command returned no
// error.
func TestRunRemoteForceFlagReachesTheServer(t *testing.T) {
	api := newFakeRemoteAPI(t, "correct-key")
	cfg := remoteConfig(t, api, "correct-key")

	// fansOff's local thermal guard (runFans/fansOff in cli.go) never gets a
	// look-in here: "fans off" alone is not classified a shutdown by
	// isShutdown, so only the explicit --remote flag sends this through the
	// API rather than the local engine.
	out, _, err := runCLI(t, cfg, hostsUp(), "--remote", "fans", "off", "--force")
	if err != nil {
		t.Fatalf("--remote fans off --force: %v", err)
	}

	if got := api.forceValues(); len(got) != 1 || got[0] != "true" {
		t.Fatalf("force values sent to POST /fans/off = %v, want exactly one \"true\"", got)
	}
	if !strings.Contains(out, "fans-off: done") {
		t.Errorf("output = %q, want it to report the action done", out)
	}
}

// TestRunRemoteWithoutForceIsRefusedBeforeAnyRequestIsSent pins the negative
// half at the cli layer: without --force, Client.Perform refuses locally
// (the required checkbox is unconfirmed), so POST /fans/off must never be
// called at all.
func TestRunRemoteWithoutForceIsRefusedBeforeAnyRequestIsSent(t *testing.T) {
	api := newFakeRemoteAPI(t, "correct-key")
	cfg := remoteConfig(t, api, "correct-key")

	_, _, err := runCLI(t, cfg, hostsUp(), "--remote", "fans", "off")
	if err == nil {
		t.Fatal("--remote fans off without --force succeeded")
	}
	if !strings.Contains(err.Error(), "needs confirmation") {
		t.Errorf("error = %v, want it to say confirmation is needed", err)
	}
	if got := api.forceValues(); len(got) != 0 {
		t.Errorf("POST /fans/off calls = %v, want none", got)
	}
}

// TestRunRemoteWithAWrongAPIKeyIsRejected pins the negative auth case at the
// cli layer: cfg's key (via F3SCTL_KEY) not matching what the server expects
// must surface as the API's rejection, not a generic transport error.
func TestRunRemoteWithAWrongAPIKeyIsRejected(t *testing.T) {
	api := newFakeRemoteAPI(t, "correct-key")
	cfg := remoteConfig(t, api, "wrong-key")

	_, _, err := runCLI(t, cfg, hostsUp(), "--remote", "fans", "off", "--force")
	if err == nil {
		t.Fatal("--remote fans off with a wrong API key succeeded")
	}
	if !strings.Contains(err.Error(), "rejected the key") {
		t.Errorf("error = %v, want it to say the key was rejected", err)
	}
}

// TestRunVerboseImpliesRemoteAndTraces pins the -v/--verbose rule
// parseGlobalFlags encodes (remote.go) end to end: passing -v with no
// --remote must still reach the API (verbose implies remote), and must write
// a trace to stderr.
//
// "API base" alone would not prove much: runRemote prints that line
// unconditionally inside the same `if flags.verbose` block, whether or not
// the next line (c.Verbose(stderr), wiring the client's own tracer) actually
// ran -- deleting that call would still leave "API base" in stderr and this
// test green. The "-> " request-trace line, by contrast, is only ever
// written by Client.tracef (internal/client/client.go, called from do()), so
// seeing it here is the one observation that Client.Verbose was genuinely
// wired up, not just that runRemote's surrounding log line printed.
func TestRunVerboseImpliesRemoteAndTraces(t *testing.T) {
	api := newFakeRemoteAPI(t, "correct-key")
	cfg := remoteConfig(t, api, "correct-key")

	_, errOut, err := runCLI(t, cfg, hostsUp(), "-v", "fans", "off", "--force")
	if err != nil {
		t.Fatalf("-v fans off --force: %v", err)
	}
	if got := api.forceValues(); len(got) != 1 || got[0] != "true" {
		t.Fatalf("force values sent to POST /fans/off = %v, want exactly one \"true\"", got)
	}
	if !strings.Contains(errOut, "API base") {
		t.Errorf("stderr = %q, want a trace showing the API base (Client.Verbose was wired up)", errOut)
	}
	if !strings.Contains(errOut, "→ ") {
		t.Errorf("stderr = %q, want a %q request-trace line: the only output Client.tracef "+
			"produces, and thus the only proof c.Verbose(stderr) was actually called", errOut, "→ ")
	}
}
