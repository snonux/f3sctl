package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/inventory"
)

// fakeShelly stands in for the rack-fan Shelly plug.
//
// It answers the two RPCs the engine uses and records every Switch.Set, which
// is what the tests below assert on: "did the fans actually get switched off"
// is the question, not "what did the CLI print". Digest auth is deliberately
// not offered -- shellyRPC only performs the challenge-response when it gets a
// 401, so answering 200 straight away keeps the fixture to what is under test.
type fakeShelly struct {
	srv *httptest.Server

	mu   sync.Mutex
	on   bool
	sets []bool // the requested state of every Switch.Set, in order
}

func newFakeShelly(t *testing.T, initiallyOn bool) *fakeShelly {
	t.Helper()
	s := &fakeShelly{on: initiallyOn}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *fakeShelly) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch r.URL.Path {
	case "/rpc/Switch.Set":
		on := r.URL.Query().Get("on") == "true"
		s.sets = append(s.sets, on)
		s.on = on
		_ = json.NewEncoder(w).Encode(map[string]bool{"was_on": !on})
	case "/rpc/Switch.GetStatus":
		_ = json.NewEncoder(w).Encode(map[string]bool{"output": s.on})
	default:
		http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
	}
}

// setCalls returns the state requested by each Switch.Set so far.
func (s *fakeShelly) setCalls() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.sets...)
}

// addr is what goes into Inventory.ShellyIP: the engine builds its URL as
// "http://" + ShellyIP + path, so a host:port belongs there.
func (s *fakeShelly) addr() string { return strings.TrimPrefix(s.srv.URL, "http://") }

// testConfig wires the CLI up to the fake plug and to a single f-host at
// fHostIP, with a Shelly password on disk so ResolveShellyPassword succeeds.
func testConfig(t *testing.T, s *fakeShelly, fHostIP string) config.Config {
	t.Helper()

	pwFile := filepath.Join(t.TempDir(), "shelly_plug")
	if err := os.WriteFile(pwFile, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatalf("writing the Shelly password file: %v", err)
	}

	cfg := config.Default()
	cfg.ShellyPasswordFile = []string{pwFile}
	cfg.ProbeTimeout = config.Duration(time.Second)
	cfg.Inventory = inventory.Inventory{
		ShellyIP: s.addr(),
		Hosts: []inventory.Host{
			{Name: "f0", Role: inventory.RoleF, IP: fHostIP, SSHPort: 22, SSHUser: "f3sctl"},
		},
	}
	return cfg
}

// loopbackIP is the address used for a host that must look alive: pinging
// 127.0.0.1 answers on any machine that has ping(8) at all.
const loopbackIP = "127.0.0.1"

// deadIP is TEST-NET-1, reserved for documentation and never routed, so the
// ping times out and the host looks powered off.
const deadIP = "192.0.2.1"

// requirePing skips a test that depends on LiveHosts seeing a host answer.
// Sandboxes without ping(8), or with ICMP blocked to the loopback, would
// otherwise report a missing refusal as a regression in the guard.
func requirePing(t *testing.T) {
	t.Helper()
	bin, err := exec.LookPath("ping")
	if err != nil {
		t.Skip("ping(8) not available; cannot make a host look alive")
	}
	if err := exec.Command(bin, "-c", "1", "-w", "1", loopbackIP).Run(); err != nil {
		t.Skipf("ping %s failed (%v); cannot make a host look alive", loopbackIP, err)
	}
}

// runCLI drives one invocation the way main does and returns stdout.
func runCLI(t *testing.T, cfg config.Config, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := Run(cfg, args, &stdout, &stderr)
	return stdout.String(), err
}

// TestFansOffForceSwitchesThePlugWhileAHostIsUp is the regression test for the
// bug where --force never reached fansOff: parseGlobalFlags consumes it, so the
// old scan of the remaining arguments always found nothing and every
// `fans off --force` hit the refusal instead of switching the plug.
func TestFansOffForceSwitchesThePlugWhileAHostIsUp(t *testing.T) {
	// No ping needed: with --force the guard is not consulted at all.
	for _, flag := range []string{"--force", "-f"} {
		t.Run(flag, func(t *testing.T) {
			shelly := newFakeShelly(t, true)
			cfg := testConfig(t, shelly, loopbackIP)

			out, err := runCLI(t, cfg, "fans", "off", flag)
			if err != nil {
				t.Fatalf("fans off %s: %v", flag, err)
			}
			if got := shelly.setCalls(); len(got) != 1 || got[0] {
				t.Fatalf("Switch.Set calls = %v, want exactly one with on=false", got)
			}
			if !strings.Contains(out, "rack fans: off") {
				t.Errorf("output = %q, want it to report the fans off", out)
			}
		})
	}
}

// TestFansOffForceBeforeTheVerb checks the flag is honoured wherever it sits.
// parseGlobalFlags accepts global flags anywhere on purpose, so `--force fans
// off` must behave exactly like `fans off --force`.
func TestFansOffForceBeforeTheVerb(t *testing.T) {
	shelly := newFakeShelly(t, true)
	cfg := testConfig(t, shelly, loopbackIP)

	if _, err := runCLI(t, cfg, "--force", "fans", "off"); err != nil {
		t.Fatalf("--force fans off: %v", err)
	}
	if got := shelly.setCalls(); len(got) != 1 || got[0] {
		t.Fatalf("Switch.Set calls = %v, want exactly one with on=false", got)
	}
}

// TestFansOffWithoutForceRefusesWhileAHostIsUp is the other half of the fix:
// threading the flag through must not weaken the thermal guard.
func TestFansOffWithoutForceRefusesWhileAHostIsUp(t *testing.T) {
	requirePing(t)

	shelly := newFakeShelly(t, true)
	cfg := testConfig(t, shelly, loopbackIP)

	_, err := runCLI(t, cfg, "fans", "off")
	if err == nil {
		t.Fatal("fans off succeeded while a host was up; the guard did not fire")
	}
	if !strings.Contains(err.Error(), "refusing to switch the rack fans off") {
		t.Errorf("error = %v, want the refusal", err)
	}
	if got := shelly.setCalls(); len(got) != 0 {
		t.Errorf("Switch.Set calls = %v, want none: the plug must be untouched", got)
	}
}

// TestFansOffWithNothingUpNeedsNoForce checks the guard only stands in the way
// when it has a reason to: an idle rack switches off without ceremony.
func TestFansOffWithNothingUpNeedsNoForce(t *testing.T) {
	shelly := newFakeShelly(t, true)
	cfg := testConfig(t, shelly, deadIP)

	if _, err := runCLI(t, cfg, "fans", "off"); err != nil {
		t.Fatalf("fans off with no host up: %v", err)
	}
	if got := shelly.setCalls(); len(got) != 1 || got[0] {
		t.Fatalf("Switch.Set calls = %v, want exactly one with on=false", got)
	}
}

// TestFansOnIgnoresTheGuard checks switching the fans on is never gated: the
// force flag belongs to the off path only.
func TestFansOnIgnoresTheGuard(t *testing.T) {
	shelly := newFakeShelly(t, false)
	cfg := testConfig(t, shelly, loopbackIP)

	out, err := runCLI(t, cfg, "fans", "on")
	if err != nil {
		t.Fatalf("fans on: %v", err)
	}
	if got := shelly.setCalls(); len(got) != 1 || !got[0] {
		t.Fatalf("Switch.Set calls = %v, want exactly one with on=true", got)
	}
	if !strings.Contains(out, "rack fans: on") {
		t.Errorf("output = %q, want it to report the fans on", out)
	}
}

// TestParseGlobalFlagsConsumesForce pins down why fansOff cannot re-derive the
// flag from its arguments: it is gone by the time any command sees them. If
// this ever changes, the threading in run/runFans should be revisited rather
// than quietly duplicated.
func TestParseGlobalFlagsConsumesForce(t *testing.T) {
	rest, flags := parseGlobalFlags([]string{"fans", "off", "--force"})
	if !flags.force {
		t.Error("--force was not parsed into flags.force")
	}
	if got := fmt.Sprint(rest); got != "[fans off]" {
		t.Errorf("rest = %s, want [fans off]: --force must not reach the command", got)
	}
}
