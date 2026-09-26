package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeMonitoringAPI serves the discovery chain SetMute follows (root ->
// /gogios -> /monitoring) and the mute pair, advertising an action only while
// it would change something, as the real server does. Gateway state is
// scripted per test.
type fakeMonitoringAPI struct {
	srv *httptest.Server

	mu    sync.Mutex
	muted map[string]bool   // gateway -> muted
	gwErr map[string]string // gateway -> unreadable reason
	// sticky gateways ignore a change: a partial success.
	sticky map[string]bool
	// failPost answers the action with 502, as the server does when the
	// engine's SSH round trips fail outright.
	failPost bool
	posts    []string
}

func newFakeMonitoringAPI(t *testing.T) *fakeMonitoringAPI {
	t.Helper()
	f := &fakeMonitoringAPI{muted: map[string]bool{}, gwErr: map[string]string{}, sticky: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMonitoringAPI) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/":
		writeEntity(w, Entity{
			Properties: map[string]any{"apiVersion": float64(SupportedAPIVersion)},
			Links:      []Link{{Rel: []string{"gogios"}, Href: "/gogios"}},
		})
	case "/gogios":
		writeEntity(w, Entity{Links: []Link{{Rel: []string{"monitoring"}, Href: "/monitoring"}}})
	case "/monitoring":
		writeEntity(w, f.monitoring())
	case "/monitoring/mute", "/monitoring/unmute":
		f.posts = append(f.posts, r.URL.Path)
		if f.failPost {
			w.WriteHeader(http.StatusBadGateway)
			writeEntity(w, Entity{Properties: map[string]any{"message": "could not gogios-unmute Gogios on: [blowfish]"}})
			return
		}
		want := r.URL.Path == "/monitoring/mute"
		for gw := range f.muted {
			if !f.sticky[gw] {
				f.muted[gw] = want
			}
		}
		writeEntity(w, f.monitoring())
	default:
		http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
	}
}

func (f *fakeMonitoringAPI) monitoring() Entity {
	e := Entity{}
	anyMuted := false
	for _, name := range []string{"blowfish", "fishfinger"} {
		props := map[string]any{"name": name}
		if msg := f.gwErr[name]; msg != "" {
			props["error"] = msg
		} else {
			props["muted"] = f.muted[name]
			anyMuted = anyMuted || f.muted[name]
		}
		e.Entities = append(e.Entities, Entity{Properties: props})
	}
	if anyMuted {
		e.Actions = append(e.Actions, Action{Name: "monitoring-unmute", Method: "POST", Href: "/monitoring/unmute", CLIVerb: "monitoring unmute"})
	} else if len(f.gwErr) == 0 {
		e.Actions = append(e.Actions, Action{Name: "monitoring-mute", Method: "POST", Href: "/monitoring/mute", CLIVerb: "monitoring mute"})
	}
	return e
}

func TestSetMuteUnmutesAndReturnsTheServersState(t *testing.T) {
	api := newFakeMonitoringAPI(t)
	api.muted["blowfish"], api.muted["fishfinger"] = true, true
	c := newTestClient(t, api.srv.URL, "k")

	states, err := c.SetMute(context.Background(), false)
	if err != nil {
		t.Fatalf("SetMute: %v", err)
	}
	if len(states) != 2 || states[0].Muted || states[1].Muted {
		t.Errorf("states = %+v, want both un-muted", states)
	}
	if len(api.posts) != 1 || api.posts[0] != "/monitoring/unmute" {
		t.Errorf("posts = %v, want one POST /monitoring/unmute", api.posts)
	}
}

// Already un-muted: the server withholds the action, nothing is posted, and
// the returned state says what the caller wanted.
func TestSetMuteWithTheActionWithheldReturnsCurrentState(t *testing.T) {
	api := newFakeMonitoringAPI(t)
	api.muted["blowfish"], api.muted["fishfinger"] = false, false
	c := newTestClient(t, api.srv.URL, "k")

	states, err := c.SetMute(context.Background(), false)
	if err != nil {
		t.Fatalf("SetMute: %v", err)
	}
	if len(api.posts) != 0 {
		t.Errorf("posts = %v, want none", api.posts)
	}
	if len(states) != 2 || states[0].Muted || states[1].Muted {
		t.Errorf("states = %+v, want both alerting", states)
	}
}

// Negative: an unreadable gateway comes back as Err, not as "un-muted".
func TestSetMuteSurfacesAnUnreadableGateway(t *testing.T) {
	api := newFakeMonitoringAPI(t)
	api.muted["blowfish"] = true
	api.gwErr["fishfinger"] = "ssh: connect timed out"
	c := newTestClient(t, api.srv.URL, "k")

	states, err := c.SetMute(context.Background(), false)
	if err != nil {
		t.Fatalf("SetMute: %v", err)
	}
	if states[1].Name != "fishfinger" || states[1].Err == nil || !strings.Contains(states[1].Err.Error(), "timed out") {
		t.Errorf("fishfinger = %+v, want its read error", states[1])
	}
}

// Negative: a 502 from the action is an error, not a silent no-op.
func TestSetMuteReportsAFailedAction(t *testing.T) {
	api := newFakeMonitoringAPI(t)
	api.muted["blowfish"] = true
	api.failPost = true
	c := newTestClient(t, api.srv.URL, "k")

	if _, err := c.SetMute(context.Background(), false); err == nil || !strings.Contains(err.Error(), "blowfish") {
		t.Fatalf("err = %v, want the server's message", err)
	}
}

// Negative: a server that shows no gateways gives the caller nothing to
// judge, which must not read as success.
func TestSetMuteWithNoGatewaysIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEntity(w, Entity{Properties: map[string]any{"apiVersion": float64(SupportedAPIVersion)}})
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL, "k")

	if _, err := c.SetMute(context.Background(), false); err == nil {
		t.Fatal("SetMute succeeded against a server reporting no gateways")
	}
}

// Negative: a partial success (one gateway ignored the change) is reported
// as that gateway still muted, for the caller to name.
func TestSetMuteReportsAPartialSuccess(t *testing.T) {
	api := newFakeMonitoringAPI(t)
	api.muted["blowfish"], api.muted["fishfinger"] = true, true
	api.sticky["fishfinger"] = true
	c := newTestClient(t, api.srv.URL, "k")

	states, err := c.SetMute(context.Background(), false)
	if err != nil {
		t.Fatalf("SetMute: %v", err)
	}
	if states[0].Muted || !states[1].Muted {
		t.Errorf("states = %+v, want blowfish alerting and fishfinger still muted", states)
	}
}

// TestRunMonitoringStatusListsTheAvailableActions drives showMonitoring end
// to end: the line lists the mute half the server currently offers, by CLI
// verb, and is left out entirely when the server offers neither (the fake
// withholds mute while a gateway is unreadable, as the real server does).
func TestRunMonitoringStatusListsTheAvailableActions(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakeMonitoringAPI)
		want  string // "" means no available-now line at all
	}{
		{"nothing muted", func(*fakeMonitoringAPI) {}, "available now: monitoring mute\n"},
		{"a gateway muted", func(f *fakeMonitoringAPI) { f.muted["blowfish"] = true }, "available now: monitoring unmute\n"},
		{"no actions", func(f *fakeMonitoringAPI) { f.gwErr["fishfinger"] = "ssh: timeout" }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeMonitoringAPI(t)
			tc.setup(api)
			c, out := newCapturingClient(t, api.srv.URL, "k")

			if err := Run(context.Background(), c, []string{"monitoring", "status"}, false); err != nil {
				t.Fatalf("Run(monitoring status): %v", err)
			}
			got := out.String()
			if tc.want == "" && strings.Contains(got, "available now") {
				t.Errorf("output = %q, want no available-now line", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("output = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}
