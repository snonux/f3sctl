package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

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

// unplug makes the fixture unreachable, the way a plug that has lost power or
// fallen off the wifi is. Closing twice is safe: t.Cleanup closes it again.
func (s *fakeShelly) unplug() { s.srv.Close() }

// setCalls returns the state requested by each Switch.Set so far.
func (s *fakeShelly) setCalls() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.sets...)
}

// addr is what goes into Inventory.ShellyIP: the engine builds its URL as
// "http://" + ShellyIP + path, so a host:port belongs there.
func (s *fakeShelly) addr() string { return strings.TrimPrefix(s.srv.URL, "http://") }

// fHostIP is the address of the single f-host in the test inventory. Nothing
// ever contacts it: liveness is injected (see runCLI) and no test below reaches
// a code path that pings or dials a host.
const fHostIP = "192.0.2.1" // TEST-NET-1, reserved and never routed

// testConfig wires the CLI up to the fake plug and to a single f-host, with a
// Shelly password on disk so ResolveShellyPassword succeeds.
func testConfig(t *testing.T, s *fakeShelly) config.Config {
	t.Helper()

	pwFile := filepath.Join(t.TempDir(), "shelly_plug")
	if err := os.WriteFile(pwFile, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatalf("writing the Shelly password file: %v", err)
	}

	cfg := config.Default()
	cfg.ShellyPasswordFile = []string{pwFile}
	cfg.Inventory = inventory.Inventory{
		ShellyIP: s.addr(),
		Hosts: []inventory.Host{
			{Name: "f0", Role: inventory.RoleF, IP: fHostIP, SSHPort: 22, SSHUser: "f3sctl"},
		},
	}
	return cfg
}

// fakeLiveness stands in for the ICMP probe behind the fan guard.
//
// Injecting it is what makes the guard testable at all: the real one shells out
// to ping(8), so "a host is up" would depend on the machine's network stack and
// on packets leaving the box. It also counts calls, so a test can assert the
// guard was not consulted rather than merely that it did not refuse.
//
// Not concurrency-safe: one CLI invocation consults it at most once, in the
// calling goroutine.
type fakeLiveness struct {
	up    []string
	calls int
}

func (f *fakeLiveness) hosts(context.Context) []string {
	f.calls++
	return f.up
}

// hostsUp returns a liveness probe reporting exactly these hosts as running.
// With no names it reports an idle rack.
func hostsUp(names ...string) *fakeLiveness { return &fakeLiveness{up: names} }

// runCLI drives one invocation with the fan guard's liveness probe replaced.
//
// It calls run rather than the exported Run because Run's only job is to pass a
// nil liveness, meaning "ask the engine over ICMP". Everything under test --
// global flag parsing, dispatch, the guard, the plug -- is the same code path
// main takes.
func runCLI(t *testing.T, cfg config.Config, live *fakeLiveness, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	err = run(cfg, args, &outBuf, &errBuf, nil, false, live.hosts)
	return outBuf.String(), errBuf.String(), err
}

// TestFansOffForceSwitchesThePlugWhileAHostIsUp is the regression test for the
// bug where --force never reached fansOff: parseGlobalFlags consumes it, so the
// old scan of the remaining arguments always found nothing and every
// `fans off --force` hit the refusal instead of switching the plug.
//
// The injected liveness reporting f0 up is what gives the test its teeth: any
// build that loses the flag on the way down consults the guard, sees a host
// running, and refuses.
func TestFansOffForceSwitchesThePlugWhileAHostIsUp(t *testing.T) {
	for _, flag := range []string{"--force", "-f"} {
		t.Run(flag, func(t *testing.T) {
			shelly := newFakeShelly(t, true)
			cfg := testConfig(t, shelly)

			out, _, err := runCLI(t, cfg, hostsUp("f0"), "fans", "off", flag)
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

// TestFansOffForceBeforeTheVerbSwitchesThePlugWhileAHostIsUp checks the flag is
// honoured wherever it sits. parseGlobalFlags accepts global flags anywhere on
// purpose, so `--force fans off` must behave exactly like `fans off --force`.
func TestFansOffForceBeforeTheVerbSwitchesThePlugWhileAHostIsUp(t *testing.T) {
	shelly := newFakeShelly(t, true)
	cfg := testConfig(t, shelly)

	if _, _, err := runCLI(t, cfg, hostsUp("f0"), "--force", "fans", "off"); err != nil {
		t.Fatalf("--force fans off: %v", err)
	}
	if got := shelly.setCalls(); len(got) != 1 || got[0] {
		t.Fatalf("Switch.Set calls = %v, want exactly one with on=false", got)
	}
}

// TestFansOffWithoutForceRefusesWhileAHostIsUp is the other half of the fix:
// threading the flag through must not weaken the thermal guard.
func TestFansOffWithoutForceRefusesWhileAHostIsUp(t *testing.T) {
	shelly := newFakeShelly(t, true)
	cfg := testConfig(t, shelly)

	_, _, err := runCLI(t, cfg, hostsUp("f0"), "fans", "off")
	if err == nil {
		t.Fatal("fans off succeeded while a host was up; the guard did not fire")
	}
	if !strings.Contains(err.Error(), "refusing to switch the rack fans off") {
		t.Errorf("error = %v, want the refusal", err)
	}
	if !strings.Contains(err.Error(), "f0") {
		t.Errorf("error = %v, want it to name the host that is still up", err)
	}
	if got := shelly.setCalls(); len(got) != 0 {
		t.Errorf("Switch.Set calls = %v, want none: the plug must be untouched", got)
	}
}

// TestFansOffWithNothingUpNeedsNoForce checks the guard only stands in the way
// when it has a reason to: an idle rack switches off without ceremony.
func TestFansOffWithNothingUpNeedsNoForce(t *testing.T) {
	shelly := newFakeShelly(t, true)
	cfg := testConfig(t, shelly)

	if _, _, err := runCLI(t, cfg, hostsUp(), "fans", "off"); err != nil {
		t.Fatalf("fans off with no host up: %v", err)
	}
	if got := shelly.setCalls(); len(got) != 1 || got[0] {
		t.Fatalf("Switch.Set calls = %v, want exactly one with on=false", got)
	}
}

// TestFansOffReportsAnUnreachablePlug pins that a plug that cannot be reached
// is an error, not a shrug. `power off` switches the fans off as its last step
// and reports the run as failed if this fails, so swallowing it would claim a
// rack was safely shut down with the fans still spinning.
func TestFansOffReportsAnUnreachablePlug(t *testing.T) {
	shelly := newFakeShelly(t, true)
	cfg := testConfig(t, shelly)
	shelly.unplug()

	out, _, err := runCLI(t, cfg, hostsUp(), "fans", "off")
	if err == nil {
		t.Fatal("fans off succeeded against an unreachable plug")
	}
	if !strings.Contains(err.Error(), "reaching the Shelly plug") {
		t.Errorf("error = %v, want it to say the plug could not be reached", err)
	}
	if out != "" {
		t.Errorf("output = %q, want nothing: the fans were not switched", out)
	}
}

// TestFansOnSwitchesThePlugOnEvenWhileHostsAreUp pins that switching the fans
// on is never gated. There is no guard on this path at all -- more cooling is
// never the risky direction -- so --force has no business here and liveness is
// not consulted.
func TestFansOnSwitchesThePlugOnEvenWhileHostsAreUp(t *testing.T) {
	shelly := newFakeShelly(t, false)
	cfg := testConfig(t, shelly)
	live := hostsUp("f0")

	out, _, err := runCLI(t, cfg, live, "fans", "on")
	if err != nil {
		t.Fatalf("fans on: %v", err)
	}
	if got := shelly.setCalls(); len(got) != 1 || !got[0] {
		t.Fatalf("Switch.Set calls = %v, want exactly one with on=true", got)
	}
	if !strings.Contains(out, "rack fans: on") {
		t.Errorf("output = %q, want it to report the fans on", out)
	}
	if live.calls != 0 {
		t.Errorf("liveness consulted %d times, want none: the on path has no guard", live.calls)
	}
}

// TestFansWithNoVerbPrintsUsage pins that a bare `fans` is a usage error and
// says so on stderr, rather than being read as some default verb.
func TestFansWithNoVerbPrintsUsage(t *testing.T) {
	shelly := newFakeShelly(t, true)
	cfg := testConfig(t, shelly)

	out, errOut, err := runCLI(t, cfg, hostsUp("f0"), "fans")
	if err != errUsage {
		t.Fatalf("error = %v, want errUsage", err)
	}
	if !strings.Contains(errOut, "f3sctl fans off") {
		t.Errorf("stderr = %q, want the usage text", errOut)
	}
	if out != "" {
		t.Errorf("stdout = %q, want usage on stderr only", out)
	}
	if got := shelly.setCalls(); len(got) != 0 {
		t.Errorf("Switch.Set calls = %v, want none", got)
	}
}

// TestUnknownFansVerbPrintsUsage pins that a misspelled verb fails loudly and
// leaves the plug alone. The package doc makes a point of rejecting retired
// wol-f3s spellings outright; guessing at "fans of" would undo that.
func TestUnknownFansVerbPrintsUsage(t *testing.T) {
	shelly := newFakeShelly(t, true)
	cfg := testConfig(t, shelly)

	out, errOut, err := runCLI(t, cfg, hostsUp("f0"), "fans", "of")
	if err == nil {
		t.Fatal("fans of succeeded; an unknown verb must be an error")
	}
	if !strings.Contains(err.Error(), `unknown fans command "of"`) {
		t.Errorf("error = %v, want it to name the unknown verb", err)
	}
	if !strings.Contains(errOut, "f3sctl fans off") {
		t.Errorf("stderr = %q, want the usage text", errOut)
	}
	if out != "" {
		t.Errorf("stdout = %q, want usage on stderr only", out)
	}
	if got := shelly.setCalls(); len(got) != 0 {
		t.Errorf("Switch.Set calls = %v, want none", got)
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
	if want := []string{"fans", "off"}; !slices.Equal(rest, want) {
		t.Errorf("rest = %v, want %v: --force must not reach the command", rest, want)
	}
}
