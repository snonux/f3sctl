package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/snonux/f3sctl/internal/client"
	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power"
	"github.com/snonux/f3sctl/internal/powertest"
)

// These pin task 5l2: a local wake on a machine without the pinned key must
// reach the gateways' Gogios mute through the API, the route `monitoring
// unmute` already takes, and keep using SSH wherever the key is readable.

// configWithIdentity returns a config whose only SSH identity candidate is
// either a real readable file or a path that does not exist.
func configWithIdentity(t *testing.T, readable bool) config.Config {
	t.Helper()
	cfg := config.Default()
	p := filepath.Join(t.TempDir(), "id_ed25519")
	if readable {
		if err := os.WriteFile(p, []byte("key"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg.SSHIdentity = []string{p}
	return cfg
}

func TestGatewaySwitchForUsesTheAPIOnlyWithoutAKey(t *testing.T) {
	cases := []struct {
		name     string
		readable bool
		local    bool
		wantAPI  bool
	}{
		{"laptop without a key goes through the API", false, false, true},
		{"pi with the key keeps SSH", true, false, false},
		{"--local without a key keeps SSH (API out of the path)", false, true, false},
		{"--local with the key keeps SSH", true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sw := gatewaySwitchFor(configWithIdentity(t, tc.readable), tc.local)
			_, isAPI := sw.(apiGatewaySwitch)
			if isAPI != tc.wantAPI || (!tc.wantAPI && sw != nil) {
				t.Errorf("gatewaySwitchFor = %#v, want API=%v", sw, tc.wantAPI)
			}
		})
	}
}

// fakeMonitorAPI is a minimal /monitoring surface over two gateways, both
// muted by default. A posted mute or un-mute sets every gateway except the
// ones listed in stuck (ignores the change) or unreadable (reports an error
// instead of a state, and is never changed). Like the real server, it
// advertises un-mute while a readable gateway is muted and mute unless every
// gateway reads muted -- both after a partial mute -- refuses with 409 a POST
// of an action it is not currently advertising, and answers a POST that could
// not reach an unreadable gateway with 502 naming it and no gateway state
// (the engine's eachGateway error, via gogiosapi's setMute).
type fakeMonitorAPI struct {
	srv        *httptest.Server
	mu         sync.Mutex
	muted      map[string]bool
	stuck      map[string]bool
	unreadable map[string]bool
	posts      int
}

func newFakeMonitorAPI(t *testing.T) *fakeMonitorAPI {
	t.Helper()
	f := &fakeMonitorAPI{
		muted: map[string]bool{"blowfish": true, "fishfinger": true},
		stuck: map[string]bool{}, unreadable: map[string]bool{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMonitorAPI) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("X-API-Key") != "secret" {
		w.WriteHeader(http.StatusUnauthorized)
		writeRemoteEntity(w, client.Entity{})
		return
	}
	switch r.URL.Path {
	case "/":
		writeRemoteEntity(w, client.Entity{
			Properties: map[string]any{"apiVersion": float64(client.SupportedAPIVersion)},
			Links:      []client.Link{{Rel: []string{"gogios"}, Href: "/gogios"}},
		})
	case "/gogios":
		writeRemoteEntity(w, client.Entity{Links: []client.Link{{Rel: []string{"monitoring"}, Href: "/monitoring"}}})
	case "/monitoring/mute", "/monitoring/unmute":
		f.posts++
		f.perform(w, r.URL.Path)
	case "/monitoring":
		writeRemoteEntity(w, f.monitoring())
	default:
		http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
	}
}

// perform answers a POST of the mute action at path.
func (f *fakeMonitorAPI) perform(w http.ResponseWriter, path string) {
	if !f.advertises(path) {
		w.WriteHeader(http.StatusConflict)
		writeRemoteEntity(w, client.Entity{Properties: map[string]any{"message": "not available right now"}})
		return
	}
	mute := path == "/monitoring/mute"
	var failed []string
	for _, gw := range []string{"blowfish", "fishfinger"} {
		switch {
		case f.unreadable[gw]:
			failed = append(failed, gw)
		case !f.stuck[gw]:
			f.muted[gw] = mute
		}
	}
	if len(failed) > 0 {
		verb := "gogios-unmute"
		if mute {
			verb = "gogios-mute"
		}
		w.WriteHeader(http.StatusBadGateway)
		writeRemoteEntity(w, client.Entity{Properties: map[string]any{
			"message": fmt.Sprintf("could not %s Gogios on: %v", verb, failed),
		}})
		return
	}
	writeRemoteEntity(w, f.monitoring())
}

func (f *fakeMonitorAPI) monitoring() client.Entity {
	e := client.Entity{}
	anyMuted, notAllMuted := false, false
	for _, gw := range []string{"blowfish", "fishfinger"} {
		props := map[string]any{"name": gw}
		if f.unreadable[gw] {
			props["error"] = "ssh: connect timed out"
			notAllMuted = true
		} else {
			props["muted"] = f.muted[gw]
			anyMuted = anyMuted || f.muted[gw]
			notAllMuted = notAllMuted || !f.muted[gw]
		}
		e.Entities = append(e.Entities, client.Entity{Properties: props})
	}
	if anyMuted {
		e.Actions = append(e.Actions, client.Action{Name: "monitoring-unmute", Method: "POST", Href: "/monitoring/unmute", CLIVerb: "monitoring unmute"})
	}
	if notAllMuted {
		e.Actions = append(e.Actions, client.Action{Name: "monitoring-mute", Method: "POST", Href: "/monitoring/mute", CLIVerb: "monitoring mute"})
	}
	return e
}

// advertises reports whether /monitoring currently offers the action at href.
func (f *fakeMonitorAPI) advertises(href string) bool {
	for _, a := range f.monitoring().Actions {
		if a.Href == href {
			return true
		}
	}
	return false
}

func apiConfig(t *testing.T, url, key string) config.Config {
	t.Helper()
	t.Setenv("F3SCTL_URL", url)
	t.Setenv("F3SCTL_KEY", key)
	return configWithIdentity(t, false)
}

func TestAPIGatewaySwitchUnmutesThroughTheAPI(t *testing.T) {
	api := newFakeMonitorAPI(t)
	sw := gatewaySwitchFor(apiConfig(t, api.srv.URL, "secret"), false)

	var log bytes.Buffer
	if err := sw.SetMute(context.Background(), &log, false); err != nil {
		t.Fatalf("SetMute: %v (log %q)", err, log.String())
	}
	if api.posts != 1 {
		t.Errorf("unmute posts = %d, want 1", api.posts)
	}
	for _, gw := range []string{"blowfish", "fishfinger"} {
		if !strings.Contains(log.String(), "Gogios un-muted on "+gw+" (via the API)") {
			t.Errorf("log = %q, want a line for %s", log.String(), gw)
		}
	}
}

// Negative: a gateway the API could not clear is named in the same error
// shape the SSH path returns.
func TestAPIGatewaySwitchNamesAGatewayLeftMuted(t *testing.T) {
	api := newFakeMonitorAPI(t)
	api.stuck["fishfinger"] = true
	sw := gatewaySwitchFor(apiConfig(t, api.srv.URL, "secret"), false)

	err := sw.SetMute(context.Background(), &bytes.Buffer{}, false)
	if err == nil || err.Error() != "could not gogios-unmute Gogios on: [fishfinger]" {
		t.Fatalf("err = %v, want fishfinger named", err)
	}
}

// A mute that reached one gateway but not the other can be finished through
// the API: the server offers the mute again while a gateway is alerting, so
// the switch reaches both instead of reporting fishfinger "still alerting"
// (task ka).
func TestAPIGatewaySwitchFinishesAPartialMute(t *testing.T) {
	api := newFakeMonitorAPI(t)
	api.muted["fishfinger"] = false
	sw := gatewaySwitchFor(apiConfig(t, api.srv.URL, "secret"), false)

	var log bytes.Buffer
	if err := sw.SetMute(context.Background(), &log, true); err != nil {
		t.Fatalf("SetMute: %v (log %q)", err, log.String())
	}
	if api.posts != 1 {
		t.Errorf("mute posts = %d, want 1", api.posts)
	}
	for _, gw := range []string{"blowfish", "fishfinger"} {
		if !strings.Contains(log.String(), "Gogios muted on "+gw+" (via the API)") {
			t.Errorf("log = %q, want a line for %s", log.String(), gw)
		}
	}
}

// Negative: a gateway that ignores the finishing mute is still named, in the
// same error shape the SSH path returns.
func TestAPIGatewaySwitchNamesAGatewayLeftAlerting(t *testing.T) {
	api := newFakeMonitorAPI(t)
	api.muted["fishfinger"] = false
	api.stuck["fishfinger"] = true
	sw := gatewaySwitchFor(apiConfig(t, api.srv.URL, "secret"), false)

	err := sw.SetMute(context.Background(), &bytes.Buffer{}, true)
	if err == nil || err.Error() != "could not gogios-mute Gogios on: [fishfinger]" {
		t.Fatalf("err = %v, want fishfinger named", err)
	}
}

// The usual aftermath of a partial mute is {muted, unreachable}: the mute is
// still offered and is posted, and the server's 502 for the gateway it still
// cannot reach comes back as an error naming it -- unknown is not muted.
func TestAPIGatewaySwitchMuteNamesAnUnreachableGateway(t *testing.T) {
	api := newFakeMonitorAPI(t)
	api.unreadable["fishfinger"] = true
	sw := gatewaySwitchFor(apiConfig(t, api.srv.URL, "secret"), false)

	err := sw.SetMute(context.Background(), &bytes.Buffer{}, true)
	if err == nil || !strings.Contains(err.Error(), "via the API") || !strings.Contains(err.Error(), "fishfinger") {
		t.Fatalf("err = %v, want the API route and fishfinger named", err)
	}
	if api.posts != 1 {
		t.Errorf("mute posts = %d, want 1: the mute must be offered while a gateway is unknown", api.posts)
	}
}

// Negative: a rejected key is an error, not a silent success.
func TestAPIGatewaySwitchReportsARejectedKey(t *testing.T) {
	api := newFakeMonitorAPI(t)
	sw := gatewaySwitchFor(apiConfig(t, api.srv.URL, "wrong"), false)

	err := sw.SetMute(context.Background(), &bytes.Buffer{}, false)
	if err == nil || !strings.Contains(err.Error(), "via the API") || !strings.Contains(err.Error(), "rejected the key") {
		t.Fatalf("err = %v, want the rejected key reported", err)
	}
}

// Negative: no API configured at all says what to set up.
func TestAPIGatewaySwitchWithoutAnAPIKeySaysSo(t *testing.T) {
	t.Setenv("F3SCTL_URL", "")
	t.Setenv("F3SCTL_KEY", "")
	cfg := configWithIdentity(t, false)
	cfg.APIKeyFile = filepath.Join(t.TempDir(), "missing")
	cfg.APIURL = "http://192.0.2.1/"

	err := apiGatewaySwitch{cfg: cfg}.SetMute(context.Background(), &bytes.Buffer{}, false)
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("err = %v, want the missing API key named", err)
	}
}

func TestReportGatewaysTreatsUnreadableAsFailed(t *testing.T) {
	var log bytes.Buffer
	err := reportGateways(&log, []power.GatewayMute{
		{Name: "blowfish", Muted: true},
		{Name: "fishfinger", Err: errors.New("ssh: timed out")},
	}, true, "gogios-mute", "muted")
	if err == nil || err.Error() != "could not gogios-mute Gogios on: [fishfinger]" {
		t.Fatalf("err = %v, want only fishfinger named", err)
	}
	if !strings.Contains(log.String(), "Gogios muted on blowfish") || !strings.Contains(log.String(), "fishfinger: ssh: timed out") {
		t.Errorf("log = %q", log.String())
	}
}

// wakeConfigWithoutKey is powerConfig plus the two gateways, with no readable
// SSH identity and the API pointed at api -- the laptop from task 5l2.
func wakeConfigWithoutKey(t *testing.T, api *fakeMonitorAPI) config.Config {
	t.Helper()
	cfg := powerConfig(t, powertest.NewFakeShelly(t, false))
	cfg.SSHIdentity = []string{filepath.Join(t.TempDir(), "missing")}
	for _, gw := range []string{"blowfish", "fishfinger"} {
		cfg.Inventory.Hosts = append(cfg.Inventory.Hosts,
			inventory.Host{Name: gw, Role: inventory.RoleGateway, IP: "192.0.2.1", SSHPort: 2, SSHUser: "f3sctl"})
	}
	t.Setenv("F3SCTL_URL", api.srv.URL)
	t.Setenv("F3SCTL_KEY", "secret")
	return cfg
}

// End to end through run(): the reported failure, a local `power all on`
// without a key, now un-mutes through the API.
func TestPowerAllOnWithoutAKeyUnmutesThroughTheAPI(t *testing.T) {
	api := newFakeMonitorAPI(t)
	cfg := wakeConfigWithoutKey(t, api)

	out, _, err := runCLI(t, cfg, hostsUp(), "power", "all", "on")
	if err != nil {
		t.Fatalf("power all on: %v\n%s", err, out)
	}
	if api.posts != 1 || !strings.Contains(out, "Gogios un-muted on fishfinger (via the API)") {
		t.Errorf("posts = %d, output = %q, want the un-mute sent through the API", api.posts, out)
	}
}

// Negative: --local keeps the API out of the path, so the same wake fails on
// the missing key and the API is never asked.
func TestPowerAllOnLocalWithoutAKeyDoesNotUseTheAPI(t *testing.T) {
	api := newFakeMonitorAPI(t)
	cfg := wakeConfigWithoutKey(t, api)

	_, _, err := runCLI(t, cfg, hostsUp(), "--local", "power", "all", "on")
	if err == nil || !strings.Contains(err.Error(), "could not gogios-unmute Gogios on: [blowfish fishfinger]") {
		t.Fatalf("err = %v, want the SSH un-mute failure", err)
	}
	if api.posts != 0 {
		t.Errorf("posts = %d, want the API untouched under --local", api.posts)
	}
}
