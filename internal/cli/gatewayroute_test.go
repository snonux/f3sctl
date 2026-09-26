package cli

import (
	"bytes"
	"context"
	"errors"
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

// fakeMonitorAPI is a minimal /monitoring surface: every gateway muted until
// the unmute action is posted, except the ones listed in stuck. Like the real
// server, it advertises un-mute while any gateway is muted and mute while any
// is alerting -- both after a partial mute.
type fakeMonitorAPI struct {
	srv   *httptest.Server
	mu    sync.Mutex
	muted map[string]bool
	stuck map[string]bool
	posts int
}

func newFakeMonitorAPI(t *testing.T) *fakeMonitorAPI {
	t.Helper()
	f := &fakeMonitorAPI{muted: map[string]bool{"blowfish": true, "fishfinger": true}, stuck: map[string]bool{}}
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
		for gw := range f.muted {
			if !f.stuck[gw] {
				f.muted[gw] = r.URL.Path == "/monitoring/mute"
			}
		}
		writeRemoteEntity(w, f.monitoring())
	case "/monitoring":
		writeRemoteEntity(w, f.monitoring())
	default:
		http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
	}
}

func (f *fakeMonitorAPI) monitoring() client.Entity {
	e := client.Entity{}
	anyMuted, anyAlerting := false, false
	for _, gw := range []string{"blowfish", "fishfinger"} {
		e.Entities = append(e.Entities, client.Entity{Properties: map[string]any{"name": gw, "muted": f.muted[gw]}})
		anyMuted = anyMuted || f.muted[gw]
		anyAlerting = anyAlerting || !f.muted[gw]
	}
	if anyMuted {
		e.Actions = append(e.Actions, client.Action{Name: "monitoring-unmute", Method: "POST", Href: "/monitoring/unmute", CLIVerb: "monitoring unmute"})
	}
	if anyAlerting {
		e.Actions = append(e.Actions, client.Action{Name: "monitoring-mute", Method: "POST", Href: "/monitoring/mute", CLIVerb: "monitoring mute"})
	}
	return e
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
