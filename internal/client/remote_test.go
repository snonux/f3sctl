package client

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/snonux/f3sctl/internal/config"
)

// fakeAPI is a minimal stand-in for the httpapi server, wired just well enough
// to drive the client's --remote path end to end over real HTTP: GET /
// (root, with links to "fans" and "status"), GET /fans (advertising the
// fans-off action with its force checkbox, exactly as
// internal/httpapi/registry.go does), POST /fans/off (the action itself), and
// GET /status (so runAction's post-action showStatus has somewhere to land).
//
// This is deliberately not a copy of httpapi's own Siren rendering -- it only
// has to satisfy the wire contract client.Entity decodes, the same contract
// docs/CLIENT.md documents and internal/httpapi/handlers_test.go exercises
// from the server side. Keeping the two independent is the point: a test
// built by importing httpapi's renderer could pass by construction even if
// the two packages had quietly drifted apart on the wire format.
type fakeAPI struct {
	srv     *httptest.Server
	wantKey string

	// coldSnapshot, when true, reproduces the gz0 disagreement: handleFans
	// omits the "force" checkbox from the fans-off advertisement, as
	// registry.go's cheap single-probe rackBusy() does when it reads the rack
	// as idle, while handleFansOff still 409s without force=true, as
	// handlers.go's stricter multi-probe rackStillBusy() does when it
	// disagrees and finds a host up within the same request. When false (the
	// zero value, used by every test predating gz0), the field is always
	// advertised and never enforced without being offered first.
	coldSnapshot bool

	mu        sync.Mutex
	forceSent []string // the "force" form value on every POST /fans/off, in order
}

func newFakeAPI(t *testing.T, wantKey string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{wantKey: wantKey}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	// Every route, root included, requires the key: internal/httpapi/server.go's
	// Server.serve calls s.auth.Check(req.APIKey) unconditionally before
	// dispatching to any handler, and docs/CLIENT.md says a client MUST send
	// X-API-Key on every request with no root exception. Client.do (client.go)
	// always sets the header regardless of route, so this gate never sees an
	// unauthenticated request in the correct-key tests below.
	if r.Header.Get("X-API-Key") != f.wantKey {
		w.WriteHeader(http.StatusUnauthorized)
		writeEntity(w, Entity{Properties: map[string]any{"message": "bad API key"}})
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
		writeEntity(w, Entity{}) // no hosts, no fan entity: enough for showStatus to render
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

// handleRoot answers GET / with the two links runAction needs to reach the
// fans-off action and the post-action status: "fans" and "status".
func (f *fakeAPI) handleRoot(w http.ResponseWriter) {
	writeEntity(w, Entity{
		Properties: map[string]any{"apiVersion": float64(SupportedAPIVersion)},
		Links: []Link{
			{Rel: []string{"fans"}, Href: "/fans"},
			{Rel: []string{"status"}, Href: "/status"},
		},
	})
}

// handleFans answers GET /fans, advertising fans-off with a required force
// checkbox -- the same shape internal/httpapi/registry.go's real advertising
// takes, which is what makes Client.Perform decide whether to fill the
// field -- unless coldSnapshot is set, in which case the field is omitted
// entirely, mirroring a cheap snapshot that read the rack as idle.
func (f *fakeAPI) handleFans(w http.ResponseWriter) {
	action := Action{
		Name:    "fans-off",
		Title:   "Switch the rack fans off",
		Method:  http.MethodPost,
		Href:    "/fans/off",
		CLIVerb: "fans off",
	}
	if !f.coldSnapshot {
		action.Fields = []Field{
			{Name: "force", Type: "checkbox", Title: "hosts may still be running", Required: true},
		}
	}
	writeEntity(w, Entity{Class: []string{"fans"}, Actions: []Action{action}})
}

// handleFansOff answers POST /fans/off, recording the "force" form value the
// tests assert on and reporting a synchronous state change -- fans-off never
// runs as a background job on the real server either.
//
// In coldSnapshot mode it also enforces the stricter half of the real gate:
// a request without force=true is 409ed, standing in for handlers.go's
// rackStillBusy finding a host up even though the (omitted) advertisement
// above was built from a snapshot that read the rack as cold.
func (f *fakeAPI) handleFansOff(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	force := r.PostForm.Get("force")
	f.mu.Lock()
	f.forceSent = append(f.forceSent, force)
	f.mu.Unlock()

	if f.coldSnapshot && force != "true" {
		w.WriteHeader(http.StatusConflict)
		writeEntity(w, Entity{Properties: map[string]any{
			"message": "the rack may still be drawing power; re-send with force=true",
		}})
		return
	}
	writeEntity(w, Entity{Properties: map[string]any{"on": false}})
}

// forceValues returns the "force" field sent with every POST /fans/off so
// far, in order.
func (f *fakeAPI) forceValues() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.forceSent...)
}

func writeEntity(w http.ResponseWriter, e Entity) {
	w.Header().Set("Content-Type", "application/vnd.siren+json")
	w.Header().Set("X-F3sctl-Node", "test-node")
	_ = json.NewEncoder(w).Encode(e)
}

// newTestClient builds a Client against srv the same way runRemote
// (internal/cli/remote.go) does in production: through client.New, not by
// constructing a &Client{} literal, so a bug in the constructor itself (see
// mz0) would be caught here rather than bypassed.
func newTestClient(t *testing.T, base, key string) *Client {
	t.Helper()
	c, err := New(base, key, config.Default(), io.Discard)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return c
}

// TestRunFansOffForceSendsTheForceFieldWhenAdvertised is the end-to-end
// regression coverage for the --remote --force path: internal/cli/remote.go's
// runRemote -> client.Run -> Client.runAction -> Client.Perform -> Client.do,
// against a real (fake) HTTP server rather than mocked at any layer in
// between. Before this test, that whole chain ran at 0% coverage: the only
// exercised client code was ActionForVerb, jobWaitTimeout and the parseX
// helpers, all called directly rather than through an HTTP round trip.
//
// The assertion that matters is not "the command succeeded" but that the
// server actually received force=true -- the field Perform fills in only
// because /fans advertised it as a required checkbox on the fans-off action.
func TestRunFansOffForceSendsTheForceFieldWhenAdvertised(t *testing.T) {
	api := newFakeAPI(t, "correct-key")
	c := newTestClient(t, api.srv.URL, "correct-key")

	if err := Run(c, []string{"fans", "off"}, true); err != nil {
		t.Fatalf("Run(fans off, force=true): %v", err)
	}

	got := api.forceValues()
	if len(got) != 1 || got[0] != "true" {
		t.Fatalf("force values sent to POST /fans/off = %v, want exactly one \"true\"", got)
	}
}

// TestRunFansOffWithoutForceIsRefusedBeforeAnyRequestIsSent pins the other
// half: Perform refuses locally (see client.go Perform) when a required
// checkbox is not confirmed, so an unconfirmed `fans off` must never even
// reach the server -- there is no ambiguity here between "the server said no"
// and "the client never asked".
func TestRunFansOffWithoutForceIsRefusedBeforeAnyRequestIsSent(t *testing.T) {
	api := newFakeAPI(t, "correct-key")
	c := newTestClient(t, api.srv.URL, "correct-key")

	err := Run(c, []string{"fans", "off"}, false)
	if err == nil {
		t.Fatal("Run(fans off, force=false) succeeded; the required checkbox was never confirmed")
	}
	if !strings.Contains(err.Error(), "needs confirmation") {
		t.Errorf("error = %v, want it to say confirmation is needed", err)
	}
	if got := api.forceValues(); len(got) != 0 {
		t.Errorf("POST /fans/off calls = %v, want none: refused before the request went out", got)
	}
}

// TestRunFansOffForceIsSentEvenWhenTheColdSnapshotOmittedTheField is the gz0
// regression: the cheap snapshot behind /fans's advertisement read the rack
// as cold, so the "force" checkbox is not offered at all, but the caller
// passed --force anyway. Before the fix, Client.Perform only ever filled
// fields the advertisement listed, so this posted an empty form and got the
// same 409 handleFansOff (fake and real) returns for an unconfirmed request
// -- even though the user did everything right. The fix (client.go's
// Perform) sends "force=true" whenever confirm is true, regardless of
// whether the field was advertised, so this must now succeed in one request
// with no retry.
func TestRunFansOffForceIsSentEvenWhenTheColdSnapshotOmittedTheField(t *testing.T) {
	api := newFakeAPI(t, "correct-key")
	api.coldSnapshot = true
	c := newTestClient(t, api.srv.URL, "correct-key")

	if err := Run(c, []string{"fans", "off"}, true); err != nil {
		t.Fatalf("Run(fans off, force=true) against a cold-snapshot advertisement: %v", err)
	}

	got := api.forceValues()
	if len(got) != 1 || got[0] != "true" {
		t.Fatalf("force values sent to POST /fans/off = %v, want exactly one %q despite no advertised field (gz0)", got, "true")
	}
}

// TestRunFansOffWithoutForceStill409sWhenTheConfirmingProbeDisagrees pins the
// other half: when the caller did NOT pass --force, the client must still
// send nothing (there is no override to send), and a cold-snapshot
// advertisement whose confirming probe finds a host up must still surface as
// a 409 -- the fix sends an unsolicited force only when the user actually
// asked for it, never fabricates one.
func TestRunFansOffWithoutForceStill409sWhenTheConfirmingProbeDisagrees(t *testing.T) {
	api := newFakeAPI(t, "correct-key")
	api.coldSnapshot = true
	c := newTestClient(t, api.srv.URL, "correct-key")

	err := Run(c, []string{"fans", "off"}, false)
	if err == nil {
		t.Fatal("Run(fans off, force=false) succeeded even though the confirming probe found a host up")
	}

	got := api.forceValues()
	if len(got) != 1 || got[0] != "" {
		t.Fatalf("force values sent to POST /fans/off = %v, want exactly one empty value: no --force was passed", got)
	}
}

// TestRunRejectsAWrongAPIKey is the negative case for authentication: a key
// the server does not recognise must surface as the same "rejected the key"
// error do() reports for a genuine 401, not a generic HTTP failure -- and it
// must not be confused with the force-refusal error above.
func TestRunRejectsAWrongAPIKey(t *testing.T) {
	api := newFakeAPI(t, "correct-key")
	c := newTestClient(t, api.srv.URL, "wrong-key")

	err := Run(c, []string{"fans", "off"}, true)
	if err == nil {
		t.Fatal("Run with a wrong API key succeeded")
	}
	if !strings.Contains(err.Error(), "rejected the key") {
		t.Errorf("error = %v, want it to say the key was rejected", err)
	}
}

// TestRootRejectsAMalformedResponse pins do()'s other failure mode: a 200
// whose body is not a Siren entity at all (e.g. a CGI script that crashed and
// dumped a stack trace to stdout instead of JSON) must be reported as an
// unparseable response, not panic or silently decode into a zero-value
// Entity that the rest of the client then acts on as if the API said nothing
// is possible.
func TestRootRejectsAMalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not json")
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, "any-key")
	_, err := c.Root()
	if err == nil {
		t.Fatal("Root() against a malformed response succeeded")
	}
	if !strings.Contains(err.Error(), "not an entity") {
		t.Errorf("error = %v, want it to say the body was not an entity", err)
	}
}

// TestRunReportsAnUnreachableServer pins do()'s network-failure path: the API
// being unreachable must read as "cannot reach the API", distinct from both
// the auth rejection and the confirmation refusal above, since an operator
// diagnosing a failure needs to know which of "wrong key", "wrong answer" or
// "no answer at all" happened.
func TestRunReportsAnUnreachableServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	base := srv.URL
	srv.Close() // closed before any request is made, so the port refuses the connection

	c := newTestClient(t, base, "any-key")
	err := Run(c, []string{"fans", "off"}, true)
	if err == nil {
		t.Fatal("Run against a closed server succeeded")
	}
	if !strings.Contains(err.Error(), "cannot reach the f3sctl API") {
		t.Errorf("error = %v, want it to say the API could not be reached", err)
	}
}
