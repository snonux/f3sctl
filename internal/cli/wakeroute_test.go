package cli

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/powertest"
)

// ipNet parses a CIDR into the *net.IPNet shape net.InterfaceAddrs returns,
// with the interface's own address (not the network address) as IP.
func ipNet(t *testing.T, cidr string) net.Addr {
	t.Helper()
	ip, n, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", cidr, err)
	}
	return &net.IPNet{IP: ip, Mask: n.Mask}
}

// piAddrs is what pi0 lists: loopback plus its lab LAN address.
func piAddrs(t *testing.T) []net.Addr {
	return []net.Addr{ipNet(t, "127.0.0.1/8"), ipNet(t, "10.20.0.125/24"), ipNet(t, "fe80::1/64")}
}

// laptopAddrs is what the laptop lists on the Wi-Fi: loopback, the Wi-Fi
// address, and two WireGuard tunnels -- wg2 reaches the lab LAN, routed, but
// is not on it.
func laptopAddrs(t *testing.T) []net.Addr {
	return []net.Addr{
		ipNet(t, "127.0.0.1/8"), ipNet(t, "10.30.0.110/24"),
		ipNet(t, "192.168.3.2/24"), ipNet(t, "10.99.0.2/32"),
	}
}

// fixedAddrs is an interfaceAddrsFunc answering addrs.
func fixedAddrs(addrs []net.Addr) interfaceAddrsFunc {
	return func() ([]net.Addr, error) { return addrs, nil }
}

// onLANOf is an interfaceAddrsFunc for a host on bcast's LAN, whatever bcast
// is: a single /32 whose own address is bcast, whose subnet broadcast is
// therefore bcast itself. The CLI tests send their magic packets to
// 127.0.0.1, which is no real subnet's broadcast.
func onLANOf(bcast string) interfaceAddrsFunc {
	return func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP(bcast), Mask: net.CIDRMask(32, 32)}}, nil
	}
}

func TestOnBroadcastLAN(t *testing.T) {
	for _, tc := range []struct {
		name  string
		addrs []net.Addr
		bcast string
		want  bool
	}{
		{"pi on the lab LAN", piAddrs(t), "10.20.0.255", true},
		{"laptop on the Wi-Fi", laptopAddrs(t), "10.20.0.255", false},
		{"laptop plugged into the lab LAN", append(laptopAddrs(t), ipNet(t, "10.20.0.50/24")), "10.20.0.255", true},
		{"same network, other mask", []net.Addr{ipNet(t, "10.20.0.50/16")}, "10.20.0.255", false},
		{"no interfaces", nil, "10.20.0.255", false},
		{"maskless address is skipped", []net.Addr{&net.IPAddr{IP: net.ParseIP("10.20.0.50")}}, "10.20.0.255", false},
		{"limited broadcast leaves on any link", laptopAddrs(t), "255.255.255.255", true},
		{"unparsable broadcast is left to Wake", laptopAddrs(t), "not-an-ip", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := onBroadcastLAN(tc.addrs, tc.bcast); got != tc.want {
				t.Errorf("onBroadcastLAN(%v, %q) = %v, want %v", tc.addrs, tc.bcast, got, tc.want)
			}
		})
	}
}

func TestIsWake(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"power", "on"}, true},
		{[]string{"power", "all", "on"}, true},
		{[]string{"power", "f3", "on"}, true},
		{[]string{"power", "off"}, false},
		{[]string{"power", "status"}, false},
		{[]string{"power", "on", "f3"}, false}, // malformed: left to the usage error
		{[]string{"fans", "on"}, false},
		{nil, false},
	} {
		if got := isWake(tc.args); got != tc.want {
			t.Errorf("isWake(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// labConfig is config.Default() with an API URL and, when key is non-empty,
// an API key in the environment; with an empty key none is readable.
func labConfig(t *testing.T, url, key string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.APIURL = url
	cfg.APIKeyFile = filepath.Join(t.TempDir(), "no_such_apikey")
	t.Setenv("F3SCTL_URL", "")
	t.Setenv("F3SCTL_KEY", key)
	return cfg
}

func TestWakeViaAPI(t *testing.T) {
	t.Run("on the lab LAN wakes locally", func(t *testing.T) {
		cfg := labConfig(t, "https://api.example/", "secret")
		if viaAPI, err := wakeViaAPI(cfg, fixedAddrs(piAddrs(t))); err != nil || viaAPI {
			t.Errorf("wakeViaAPI = (%v, %v), want local", viaAPI, err)
		}
	})
	t.Run("off the LAN with a key goes through the API", func(t *testing.T) {
		cfg := labConfig(t, "https://api.example/", "secret")
		if viaAPI, err := wakeViaAPI(cfg, fixedAddrs(laptopAddrs(t))); err != nil || !viaAPI {
			t.Errorf("wakeViaAPI = (%v, %v), want via the API", viaAPI, err)
		}
	})
	t.Run("off the LAN without a key is refused", func(t *testing.T) {
		cfg := labConfig(t, "https://api.example/", "")
		_, err := wakeViaAPI(cfg, fixedAddrs(laptopAddrs(t)))
		if !errors.Is(err, errWakeOffLAN) {
			t.Fatalf("err = %v, want errWakeOffLAN", err)
		}
		for _, want := range []string{"--remote", "lab LAN", "10.20.0.255", "no API key"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %q, want it to mention %q", err, want)
			}
		}
	})
	t.Run("off the LAN without a URL is refused", func(t *testing.T) {
		cfg := labConfig(t, "", "secret")
		_, err := wakeViaAPI(cfg, fixedAddrs(laptopAddrs(t)))
		if !errors.Is(err, errWakeOffLAN) || !strings.Contains(err.Error(), "no API URL") {
			t.Errorf("err = %v, want errWakeOffLAN naming the missing URL", err)
		}
	})
	t.Run("an unreadable interface list is refused", func(t *testing.T) {
		cfg := labConfig(t, "https://api.example/", "secret")
		broken := func() ([]net.Addr, error) { return nil, errors.New("boom") }
		if _, err := wakeViaAPI(cfg, broken); !errors.Is(err, errWakeOffLAN) || !strings.Contains(err.Error(), "boom") {
			t.Errorf("err = %v, want errWakeOffLAN carrying the cause", err)
		}
	})
}

// countingAPI is an API endpoint that only counts the requests reaching it,
// failing every one: enough to prove a wake was routed there, without
// modelling the Siren documents behind it.
func countingAPI(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "test server", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// wakeFromLaptop runs args through run() with the laptop's interface list,
// the fan plug faked, and the API at api (key "secret" when withKey).
func wakeFromLaptop(t *testing.T, apiURL string, withKey bool, args ...string) (
	stderr string, shelly *powertest.FakeShelly, err error) {

	t.Helper()
	shelly = powertest.NewFakeShelly(t, false)
	cfg := powerConfig(t, shelly)
	key := ""
	if withKey {
		key = "secret"
	}
	lab := labConfig(t, apiURL, key)
	// powerConfig's broadcast stays 127.0.0.1, so a wake that does run
	// locally (--local) sends nothing beyond loopback; the laptop list is
	// off that "LAN" too (127.0.0.1/8 broadcasts to 127.255.255.255).
	cfg.APIURL, cfg.APIKeyFile = lab.APIURL, lab.APIKeyFile

	var out, errBuf bytes.Buffer
	err = run(context.Background(), cfg, args, &out, &errBuf, nil, false,
		runSeams{liveHosts: hostsUp().hosts, interfaceAddrs: fixedAddrs(laptopAddrs(t))})
	return errBuf.String(), shelly, err
}

// TestOffLANWakeGoesThroughTheAPI: the laptop on the Wi-Fi with an API key
// configured wakes through the API, and nothing is done locally.
func TestOffLANWakeGoesThroughTheAPI(t *testing.T) {
	for _, args := range [][]string{{"power", "on"}, {"power", "all", "on"}, {"power", "f3", "on"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			srv, hits := countingAPI(t)
			stderr, shelly, err := wakeFromLaptop(t, srv.URL, true, args...)
			if hits.Load() == 0 {
				t.Error("the API was never asked")
			}
			// The test API answers 503 to everything; that answer, not a
			// local outcome, must be what the command reports.
			if err == nil || !strings.Contains(err.Error(), "503") {
				t.Errorf("err = %v, want the test API's 503 surfaced", err)
			}
			if !strings.Contains(stderr, "waking through the API") {
				t.Errorf("stderr = %q, want it to say the wake went through the API", stderr)
			}
			if got := shelly.SetCalls(); len(got) != 0 {
				t.Errorf("Switch.Set calls = %v, want none: the local wake must not run", got)
			}
		})
	}
}

// TestOffLANWakeWithoutAKeyIsRefused: no API to fall back on, so the wake
// fails loudly instead of sending a packet nobody receives and succeeding.
func TestOffLANWakeWithoutAKeyIsRefused(t *testing.T) {
	for _, args := range [][]string{{"power", "on"}, {"power", "all", "on"}, {"power", "f3", "on"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			srv, hits := countingAPI(t)
			_, shelly, err := wakeFromLaptop(t, srv.URL, false, args...)
			if !errors.Is(err, errWakeOffLAN) {
				t.Fatalf("err = %v, want errWakeOffLAN", err)
			}
			if hits.Load() != 0 {
				t.Errorf("API hits = %d, want none without a key", hits.Load())
			}
			if got := shelly.SetCalls(); len(got) != 0 {
				t.Errorf("Switch.Set calls = %v, want none on a refused wake", got)
			}
		})
	}
}

// wakeWithAddrs runs args through run() with the interface list addrsFn, the
// fan plug faked, and a counting API with a key configured, so a refusal is
// never down to a missing key. forceLocal is RunLocal's flag, as the API's
// job child passes it.
func wakeWithAddrs(t *testing.T, addrsFn interfaceAddrsFunc, forceLocal bool, args ...string) (
	stdout string, shelly *powertest.FakeShelly, apiHits int32, err error) {

	t.Helper()
	srv, hits := countingAPI(t)
	shelly = powertest.NewFakeShelly(t, false)
	cfg := powerConfig(t, shelly)
	lab := labConfig(t, srv.URL, "secret")
	cfg.APIURL, cfg.APIKeyFile = lab.APIURL, lab.APIKeyFile

	var out, errBuf bytes.Buffer
	err = run(context.Background(), cfg, args, &out, &errBuf, nil, forceLocal,
		runSeams{liveHosts: hostsUp().hosts, interfaceAddrs: addrsFn})
	return out.String(), shelly, hits.Load(), err
}

// TestLocalFlagRefusesAnOffLANWake: --local from the laptop on the Wi-Fi is
// refused -- no fans, no packet, no API -- even though an API key is
// configured: --local means "not through the API", and from here a local
// packet reaches nobody. Up to v0.11.5 it was sent with a warning.
func TestLocalFlagRefusesAnOffLANWake(t *testing.T) {
	broken := func() ([]net.Addr, error) { return nil, errors.New("boom") }
	laptop := fixedAddrs(laptopAddrs(t))
	for _, tc := range []struct {
		name       string
		addrs      interfaceAddrsFunc
		forceLocal bool
		args       []string
		wantInErr  []string
	}{
		{"--local power on", laptop, false, []string{"--local", "power", "on"},
			[]string{"--local was given", "no packet was sent", "Drop --local", "lab LAN"}},
		{"-l power all on", laptop, false, []string{"-l", "power", "all", "on"}, []string{"--local was given"}},
		{"--local power f3 on", laptop, false, []string{"--local", "power", "f3", "on"}, []string{"--local was given"}},
		{"RunLocal off the LAN", laptop, true, []string{"power", "f3", "on"}, []string{"--local was given"}},
		{"--local with no interfaces", fixedAddrs(nil), false, []string{"--local", "power", "f3", "on"},
			[]string{"--local was given"}},
		{"--local with an unreadable interface list", broken, false, []string{"--local", "power", "f3", "on"},
			[]string{"boom", "Drop --local"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, shelly, hits, err := wakeWithAddrs(t, tc.addrs, tc.forceLocal, tc.args...)
			assertWakeRefused(t, err, tc.wantInErr, stdout, shelly, hits)
		})
	}
}

// assertWakeRefused checks that a wake was refused with errWakeOffLAN naming
// every wantInErr, and that nothing ran: no API request, no fan plug switch,
// no output. Every cluster-wide wake switches the fans on first and every
// per-host wake prints its host, so silence on both shows the local wake
// never ran.
func assertWakeRefused(t *testing.T, err error, wantInErr []string, stdout string,
	shelly *powertest.FakeShelly, apiHits int32) {

	t.Helper()
	if !errors.Is(err, errWakeOffLAN) {
		t.Fatalf("err = %v, want errWakeOffLAN", err)
	}
	for _, want := range wantInErr {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
	if apiHits != 0 {
		t.Errorf("API hits = %d, want none under --local", apiHits)
	}
	if got := shelly.SetCalls(); len(got) != 0 {
		t.Errorf("Switch.Set calls = %v, want none on a refused wake", got)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing on a refused wake", stdout)
	}
}

// TestLocalFlagWakesOnTheLAN: --local on the lab LAN -- a Pi by hand, or the
// API's job child via RunLocal -- still wakes locally and leaves the API alone.
func TestLocalFlagWakesOnTheLAN(t *testing.T) {
	for _, tc := range []struct {
		name       string
		forceLocal bool
		args       []string
	}{
		{"--local power f3 on", false, []string{"--local", "power", "f3", "on"}},
		{"RunLocal power f3 on", true, []string{"power", "f3", "on"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, _, hits, err := wakeWithAddrs(t, onLANOf("127.0.0.1"), tc.forceLocal, tc.args...)
			if err != nil {
				t.Fatalf("wake on the lab LAN: %v", err)
			}
			if hits != 0 {
				t.Errorf("API hits = %d, want none under --local", hits)
			}
			if !strings.Contains(stdout, "f3") {
				t.Errorf("stdout = %q, want the local wake's output", stdout)
			}
		})
	}
}

func TestLocalWakeError(t *testing.T) {
	cfg := config.Default() // broadcast 10.20.0.255, the lab LAN
	broken := func() ([]net.Addr, error) { return nil, errors.New("boom") }
	for _, tc := range []struct {
		name    string
		addrs   interfaceAddrsFunc
		wantErr string // "" means the wake may go ahead
	}{
		{"pi on the lab LAN", fixedAddrs(piAddrs(t)), ""},
		{"laptop on the Wi-Fi with wg2 up", fixedAddrs(laptopAddrs(t)), "10.20.0.255"},
		{"laptop plugged into the lab LAN", fixedAddrs(append(laptopAddrs(t), ipNet(t, "10.20.0.50/24"))), ""},
		{"same network, other mask", fixedAddrs([]net.Addr{ipNet(t, "10.20.0.50/16")}), "10.20.0.255"},
		{"unreadable interface list", broken, "boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := localWakeError(cfg, tc.addrs)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("localWakeError = %v, want the wake allowed", err)
				}
				return
			}
			if !errors.Is(err, errWakeOffLAN) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("localWakeError = %v, want errWakeOffLAN mentioning %q", err, tc.wantErr)
			}
		})
	}
}

// TestOnLANWakeStaysLocal: a Pi's interface list keeps the wake local and
// the API untouched -- the Pis' behaviour is unchanged.
func TestOnLANWakeStaysLocal(t *testing.T) {
	srv, hits := countingAPI(t)
	shelly := powertest.NewFakeShelly(t, false)
	cfg := powerConfig(t, shelly)
	lab := labConfig(t, srv.URL, "secret")
	cfg.APIURL, cfg.APIKeyFile = lab.APIURL, lab.APIKeyFile

	var out, errBuf bytes.Buffer
	err := run(context.Background(), cfg, []string{"power", "f3", "on"}, &out, &errBuf, nil, false,
		runSeams{liveHosts: hostsUp().hosts, interfaceAddrs: onLANOf(cfg.Inventory.Broadcast)})
	if err != nil {
		t.Errorf("local wake on the lab LAN: %v", err)
	}
	if hits.Load() != 0 {
		t.Errorf("API hits = %d, want none on the lab LAN", hits.Load())
	}
	if strings.Contains(errBuf.String(), "lab LAN") {
		t.Errorf("stderr = %q, want no off-LAN notice on the lab LAN", errBuf.String())
	}
	if !strings.Contains(out.String(), "f3") {
		t.Errorf("stdout = %q, want the local wake's output", out.String())
	}
}
