package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeMonitoringAPI serves the discovery chain SetMute follows (root ->
// /gogios -> /monitoring) and the mute pair, advertising an action only while
// it would change something, as the real server does. Gateway state is
// scripted per test. As on the real server, a POST of an action not currently
// advertised is refused with 409, and one that cannot reach an unreadable
// (gwErr) gateway answers 502 naming it, with no gateway state -- the
// engine's eachGateway error, via gogiosapi's setMute.
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
		if !f.advertises(r.URL.Path) {
			// The real server's serve() backstop: an action it is not
			// currently offering is refused before any handler runs.
			w.WriteHeader(http.StatusConflict)
			writeEntity(w, Entity{Properties: map[string]any{"message": "not available right now"}})
			return
		}
		f.perform(w, r.URL.Path)
	default:
		http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
	}
}

// perform answers an advertised POST of the mute action at path.
func (f *fakeMonitoringAPI) perform(w http.ResponseWriter, path string) {
	if f.failPost {
		w.WriteHeader(http.StatusBadGateway)
		writeEntity(w, Entity{Properties: map[string]any{"message": "could not gogios-unmute Gogios on: [blowfish]"}})
		return
	}
	mute := path == "/monitoring/mute"
	var failed []string
	for _, gw := range []string{"blowfish", "fishfinger"} {
		switch {
		case f.gwErr[gw] != "":
			failed = append(failed, gw)
		case !f.sticky[gw]:
			f.muted[gw] = mute
		}
	}
	if len(failed) > 0 {
		verb := "gogios-unmute"
		if mute {
			verb = "gogios-mute"
		}
		w.WriteHeader(http.StatusBadGateway)
		writeEntity(w, Entity{Properties: map[string]any{"message": fmt.Sprintf("could not %s Gogios on: %v", verb, failed)}})
		return
	}
	writeEntity(w, f.monitoring())
}

func (f *fakeMonitoringAPI) monitoring() Entity {
	e := Entity{}
	anyMuted, notAllMuted := false, false
	for _, name := range []string{"blowfish", "fishfinger"} {
		props := map[string]any{"name": name}
		if msg := f.gwErr[name]; msg != "" {
			props["error"] = msg
			notAllMuted = true
		} else {
			props["muted"] = f.muted[name]
			anyMuted = anyMuted || f.muted[name]
			notAllMuted = notAllMuted || !f.muted[name]
		}
		e.Entities = append(e.Entities, Entity{Properties: props})
	}
	// The server's rule (gogiosapi.Muted / NotAllMuted): un-mute for a mute
	// actually read, mute unless every gateway reads muted -- both after a
	// partial mute.
	if anyMuted {
		e.Actions = append(e.Actions, Action{Name: "monitoring-unmute", Method: "POST", Href: "/monitoring/unmute", CLIVerb: "monitoring unmute"})
	}
	if notAllMuted {
		e.Actions = append(e.Actions, Action{Name: "monitoring-mute", Method: "POST", Href: "/monitoring/mute", CLIVerb: "monitoring mute"})
	}
	return e
}

// advertises reports whether the monitoring resource currently offers the
// action at href.
func (f *fakeMonitoringAPI) advertises(href string) bool {
	for _, a := range f.monitoring().Actions {
		if a.Href == href {
			return true
		}
	}
	return false
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

// Negative: an unreadable gateway comes back as Err, not as "un-muted". Here
// nothing is muted, so the un-mute is withheld and the state is read as is.
func TestSetMuteSurfacesAnUnreadableGateway(t *testing.T) {
	api := newFakeMonitoringAPI(t)
	api.gwErr["fishfinger"] = "ssh: connect timed out"
	c := newTestClient(t, api.srv.URL, "k")

	states, err := c.SetMute(context.Background(), false)
	if err != nil {
		t.Fatalf("SetMute: %v", err)
	}
	if len(api.posts) != 0 {
		t.Errorf("posts = %v, want none: nothing readable is muted", api.posts)
	}
	if states[1].Name != "fishfinger" || states[1].Err == nil || !strings.Contains(states[1].Err.Error(), "timed out") {
		t.Errorf("fishfinger = %+v, want its read error", states[1])
	}
}

// Negative: an action that cannot reach an unreadable gateway fails with the
// server's 502 naming it -- the case where the state cannot be learned from
// the response and has to be re-read.
func TestSetMuteReportsAnUnreachableGatewayOnTheAction(t *testing.T) {
	api := newFakeMonitoringAPI(t)
	api.muted["blowfish"] = true
	api.gwErr["fishfinger"] = "ssh: connect timed out"
	c := newTestClient(t, api.srv.URL, "k")

	_, err := c.SetMute(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "could not gogios-unmute Gogios on: [fishfinger]") {
		t.Fatalf("err = %v, want fishfinger named", err)
	}
	if len(api.posts) != 1 {
		t.Errorf("posts = %v, want the un-mute attempted once", api.posts)
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
// to end: the line lists the mute actions the server currently offers, by CLI
// verb. An unreadable gateway does not withhold mute -- the real server
// offers it unless every gateway reads muted (gogiosapi.NotAllMuted) -- so
// that case still lists "monitoring mute", and a partial mute lists both.
// The no-actions branch of printAvailable is covered by
// TestRunStatusWithNoActionsPrintsNoAvailableLine.
func TestRunMonitoringStatusListsTheAvailableActions(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakeMonitoringAPI)
		want  string
	}{
		{"nothing muted", func(*fakeMonitoringAPI) {}, "available now: monitoring mute\n"},
		{"all muted", func(f *fakeMonitoringAPI) { f.muted["blowfish"], f.muted["fishfinger"] = true, true }, "available now: monitoring unmute\n"},
		{"partial mute", func(f *fakeMonitoringAPI) { f.muted["blowfish"] = true }, "available now: monitoring unmute, monitoring mute\n"},
		{"a gateway unreadable", func(f *fakeMonitoringAPI) { f.gwErr["fishfinger"] = "ssh: timeout" }, "available now: monitoring mute\n"},
		{"muted and unreadable", func(f *fakeMonitoringAPI) {
			f.muted["blowfish"] = true
			f.gwErr["fishfinger"] = "ssh: timeout"
		}, "available now: monitoring unmute, monitoring mute\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeMonitoringAPI(t)
			tc.setup(api)
			c, out := newCapturingClient(t, api.srv.URL, "k")

			if err := Run(context.Background(), c, []string{"monitoring", "status"}, false); err != nil {
				t.Fatalf("Run(monitoring status): %v", err)
			}
			if got := out.String(); !strings.Contains(got, tc.want) {
				t.Errorf("output = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// After a partial mute (one gateway muted, one still alerting) both actions
// are advertised, and a mute picks the mute rather than mistaking the offered
// un-mute for "nothing to do" (task ka).
func TestSetMuteFinishesAPartialMute(t *testing.T) {
	api := newFakeMonitoringAPI(t)
	api.muted["blowfish"], api.muted["fishfinger"] = true, false
	c := newTestClient(t, api.srv.URL, "k")

	states, err := c.SetMute(context.Background(), true)
	if err != nil {
		t.Fatalf("SetMute: %v", err)
	}
	if len(api.posts) != 1 || api.posts[0] != "/monitoring/mute" {
		t.Errorf("posts = %v, want one POST /monitoring/mute", api.posts)
	}
	if len(states) != 2 || !states[0].Muted || !states[1].Muted {
		t.Errorf("states = %+v, want both muted", states)
	}
}

// `f3sctl --remote monitoring mute` after a partial mute: Run finds the mute
// among both advertised actions by its CLI verb and POSTs it, rather than
// printing "not available right now" (task ka).
func TestRunMonitoringMuteFinishesAPartialMute(t *testing.T) {
	api := newFakeMonitoringAPI(t)
	api.muted["blowfish"], api.muted["fishfinger"] = true, false
	c, out := newCapturingClient(t, api.srv.URL, "k")

	if err := Run(context.Background(), c, []string{"monitoring", "mute"}, false); err != nil {
		t.Fatalf("Run(monitoring mute): %v", err)
	}
	if len(api.posts) != 1 || api.posts[0] != "/monitoring/mute" {
		t.Errorf("posts = %v, want one POST /monitoring/mute", api.posts)
	}
	got := out.String()
	if strings.Contains(got, "not available") || !strings.Contains(got, "fishfinger: MUTED") {
		t.Errorf("output = %q, want the mute performed and fishfinger shown muted", got)
	}
}

// Negative: with every gateway already muted the mute is withheld, so Run
// reports it unavailable and posts nothing -- and the fake refuses a POST it
// is not advertising, as the server does, so a client that posted anyway
// would fail here rather than pass by accident.
func TestRunMonitoringMuteWithEverythingMutedPostsNothing(t *testing.T) {
	api := newFakeMonitoringAPI(t)
	api.muted["blowfish"], api.muted["fishfinger"] = true, true
	c, out := newCapturingClient(t, api.srv.URL, "k")

	if err := Run(context.Background(), c, []string{"monitoring", "mute"}, false); err != nil {
		t.Fatalf("Run(monitoring mute): %v", err)
	}
	if len(api.posts) != 0 {
		t.Errorf("posts = %v, want none", api.posts)
	}
	if !strings.Contains(out.String(), `"monitoring mute" is not available right now`) {
		t.Errorf("output = %q, want the mute reported unavailable", out.String())
	}
}
