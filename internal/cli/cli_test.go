package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/snonux/f3sctl/internal/power"
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

// TestFansRejectsTrailingArgs is the regression test for the iz0 bug: fans has
// no per-host concept, so runFans used to resolve its verb from args[0] alone
// and silently drop everything after it. "fans on f0" switched the WHOLE
// rack's fans on and discarded "f0" -- a misleading success (the action named
// did happen) rather than a wrong target, but the same "malformed spelling
// silently reinterpreted" hazard hy0 fixed for power. Every case here must be
// a usage error with the plug left untouched, exactly like an unknown verb.
func TestFansRejectsTrailingArgs(t *testing.T) {
	for _, args := range [][]string{
		{"on", "f0"},
		{"off", "f0"},
		{"status", "f0"},
		{"on", "f0", "f1"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			shelly := newFakeShelly(t, true)
			cfg := testConfig(t, shelly)

			out, errOut, err := runCLI(t, cfg, hostsUp(), append([]string{"fans"}, args...)...)
			if err == nil {
				t.Fatalf("fans %s succeeded; trailing args must be a usage error", strings.Join(args, " "))
			}
			want := fmt.Sprintf("unknown fans command %q", strings.Join(args, " "))
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to contain %q", err, want)
			}
			if !strings.Contains(errOut, "f3sctl fans off") {
				t.Errorf("stderr = %q, want the usage text", errOut)
			}
			if out != "" {
				t.Errorf("stdout = %q, want nothing: no fans action may have run", out)
			}
			if got := shelly.setCalls(); len(got) != 0 {
				t.Errorf("Switch.Set calls = %v, want none: the fan plug must be untouched", got)
			}
		})
	}
}

// TestFansStatusReportsThePlugWithoutTouchingIt pins the read-only verb: it
// reports the plug's current state, switches nothing, and never consults the
// liveness probe -- there is no guard on a read.
func TestFansStatusReportsThePlugWithoutTouchingIt(t *testing.T) {
	for _, on := range []bool{true, false} {
		name := "off"
		if on {
			name = "on"
		}
		t.Run(name, func(t *testing.T) {
			shelly := newFakeShelly(t, on)
			cfg := testConfig(t, shelly)
			live := hostsUp("f0")

			out, _, err := runCLI(t, cfg, live, "fans", "status")
			if err != nil {
				t.Fatalf("fans status: %v", err)
			}
			if want := "rack fans: " + name; !strings.Contains(out, want) {
				t.Errorf("output = %q, want it to contain %q", out, want)
			}
			if !strings.Contains(out, shelly.addr()) {
				t.Errorf("output = %q, want it to name the plug at %s", out, shelly.addr())
			}
			if got := shelly.setCalls(); len(got) != 0 {
				t.Errorf("Switch.Set calls = %v, want none: status must not switch", got)
			}
			if live.calls != 0 {
				t.Errorf("liveness consulted %d times, want none: status has no guard", live.calls)
			}
		})
	}
}

// TestMonitoringRejectsTrailingArgs is the monitoring half of the iz0
// regression: runMonitoring used to resolve its verb from args[0] alone and
// silently drop everything after it. "monitoring mute junk" muted Gogios and
// discarded "junk" instead of failing. None of these route to the API --
// isMonitoring requires the single word right after "monitoring" to be a real
// verb, so a trailing token also fails that check and the command reaches
// runMonitoring's own validation locally, which is what this test pins.
func TestMonitoringRejectsTrailingArgs(t *testing.T) {
	for _, args := range [][]string{
		{"mute", "junk"},
		{"unmute", "junk"},
		{"status", "junk"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			cfg := testConfig(t, newFakeShelly(t, true))

			out, errOut, err := runCLI(t, cfg, hostsUp(), append([]string{"monitoring"}, args...)...)
			if err == nil {
				t.Fatalf("monitoring %s succeeded; trailing args must be a usage error", strings.Join(args, " "))
			}
			want := fmt.Sprintf("unknown monitoring command %q", strings.Join(args, " "))
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to contain %q", err, want)
			}
			if !strings.Contains(errOut, "f3sctl monitoring mute") {
				t.Errorf("stderr = %q, want the usage text", errOut)
			}
			if out != "" {
				t.Errorf("stdout = %q, want nothing: no monitoring action may have run", out)
			}
		})
	}
}

// TestRunFallsBackToTheEngineProbeWhenNoLivenessIsInjected covers the nil
// default in runFans, the single line stopping the exported entry points from
// calling a nil function: Run and RunLocal both pass a nil liveHostsFunc, so
// without the fallback `f3sctl fans off` panics in fansOff -- on the thermal
// guard path, the one that must never fail open or crash.
//
// It has to drive the exported Run, not runCLI: runCLI takes a *fakeLiveness
// and always passes its non-nil method value, so it cannot express "nothing was
// injected". The inventory is emptied so the real Engine.LiveHosts has no
// f-host to probe: it returns nil without a single packet leaving the box, and
// the guard then sees an idle rack and switches the plug off for real.
func TestRunFallsBackToTheEngineProbeWhenNoLivenessIsInjected(t *testing.T) {
	shelly := newFakeShelly(t, true)
	cfg := testConfig(t, shelly)
	cfg.Inventory.Hosts = nil

	var outBuf, errBuf bytes.Buffer
	if err := Run(cfg, []string{"fans", "off"}, &outBuf, &errBuf); err != nil {
		t.Fatalf("fans off with no liveness injected: %v", err)
	}
	if got := shelly.setCalls(); len(got) != 1 || got[0] {
		t.Fatalf("Switch.Set calls = %v, want exactly one with on=false", got)
	}
	if !strings.Contains(outBuf.String(), "rack fans: off") {
		t.Errorf("output = %q, want it to report the fans off", outBuf.String())
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

// powerConfig is testConfig with what the wake path needs to run to completion.
//
// Two f-hosts, because f0 and f3 are what tell the three wake spellings apart:
// `power on` takes the power group (f0, no f3), `power all on` every f-host,
// `power f3 on` just the one named. MACs so Engine.Wake has a packet to build,
// and a loopback broadcast address so the packet goes to 127.0.0.1:9 instead of
// out of the machine -- UDP to a discard port nobody listens on, needing no
// privileges and reaching no host.
//
// Everything else on that path is inert with this inventory: no RoleCluster
// hosts, so UnmuteGogios's wait for the k3s nodes returns immediately, and no
// RoleGateway hosts, so it has nobody to SSH to.
func powerConfig(t *testing.T, s *fakeShelly) config.Config {
	t.Helper()
	cfg := testConfig(t, s)
	cfg.Inventory.Broadcast = "127.0.0.1"
	cfg.Inventory.Hosts = []inventory.Host{
		{Name: "f0", Role: inventory.RoleF, IP: fHostIP, MAC: "00:11:22:33:44:50", SSHPort: 22, SSHUser: "f3sctl"},
		{Name: "f3", Role: inventory.RoleF, IP: fHostIP, MAC: "00:11:22:33:44:53", SSHPort: 22, SSHUser: "f3sctl"},
	}
	return cfg
}

// TestPowerRejectsMalformedSpellings is the regression test for the bug where
// `power` dispatched on its first word alone and dropped whatever followed:
// `power on f3` ran the CLUSTER-WIDE wake and discarded "f3", and
// `power off f2` powered the whole cluster off. The documented per-host
// spelling is `power f3 on`, so writing the words the other way round is an
// ordinary slip -- and it was answered by doing something far larger than
// asked, silently.
//
// Each case asserts both halves: a usage error comes back, AND nothing was
// done. "Nothing was done" is checked at the plug and at stdout, which is what
// gives the test its teeth -- every cluster-wide verb touches the fan plug
// (on before waking, off once the rack is idle), and every per-host verb and
// the status table print. A build with the old dispatch fails on those, not
// merely on the error text.
func TestPowerRejectsMalformedSpellings(t *testing.T) {
	// None of these route to the API: isShutdown only matches `power off` and
	// `power <host> off`, so they all reach runPower locally, which is exactly
	// where the misreading used to happen.
	//
	// "on off" and "status off" are here specifically for the routing bug: the
	// old isShutdown checked only that a 3-token command ended in "off",
	// ignoring the middle word, so both were misclassified as legitimate
	// shutdowns and sent to runRemote instead of ever reaching runPower's
	// validation -- producing a confusing API-key/HTTP error here rather than
	// the "unknown power command" this test asserts on. See TestIsShutdown
	// AgreesWithPowerActionFor below for the direct unit-level check.
	for _, args := range [][]string{
		{"on", "f3"},
		{"off", "f2"},
		{"on", "all"},
		{"off", "all"},
		{"status", "f0"},
		{"status", "all"},
		{"on", "f0", "f1"},
		{"f0", "on", "off"},
		{"all", "on", "off"},
		{"on", "off"},
		{"status", "off"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			shelly := newFakeShelly(t, true)
			cfg := powerConfig(t, shelly)

			out, errOut, err := runCLI(t, cfg, hostsUp(), append([]string{"power"}, args...)...)
			if err == nil {
				t.Fatalf("power %s succeeded; a malformed spelling must be a usage error",
					strings.Join(args, " "))
			}
			want := fmt.Sprintf("unknown power command %q", strings.Join(args, " "))
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to contain %q", err, want)
			}
			if !strings.Contains(errOut, "f3sctl power f0|f1|f2|f3 on") {
				t.Errorf("stderr = %q, want the usage text showing the per-host spelling", errOut)
			}
			if out != "" {
				t.Errorf("stdout = %q, want nothing: no power action may have run", out)
			}
			if got := shelly.setCalls(); len(got) != 0 {
				t.Errorf("Switch.Set calls = %v, want none: the fan plug must be untouched", got)
			}
		})
	}
}

// TestIsShutdownAgreesWithPowerActionFor is the direct unit-level regression
// test for the routing bug: isShutdown used to check only that a 3-token
// command ended in "off", ignoring args[1] entirely, so
// isShutdown([]string{"power", "on", "off"}) and
// isShutdown([]string{"power", "status", "off"}) both came back true --
// misreading a malformed spelling as a legitimate shutdown and sending it to
// runRemote before powerActionFor ever saw it. It must agree with
// powerActionFor on every case here: shutdown if and only if
// powerActionFor(args[1:]) resolves and the verb it names is "off".
func TestIsShutdownAgreesWithPowerActionFor(t *testing.T) {
	for _, args := range [][]string{
		{"power", "off"},
		{"power", "on"},
		{"power", "status"},
		{"power", "all", "off"},
		{"power", "all", "on"},
		{"power", "f3", "off"},
		{"power", "f3", "on"},
		{"power", "on", "off"},     // the misclassified spelling
		{"power", "status", "off"}, // the other misclassified spelling
		{"power", "off", "on"},
		{"power", "on", "f3"},
		{"power", "off", "f2"},
		{"monitoring", "mute"},
		{},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var want bool
			if len(args) > 0 && args[0] == "power" {
				sp, ok := parsePowerArgs(args[1:])
				want = ok && sp.verb == "off"
			}
			if got := isShutdown(args); got != want {
				t.Errorf("isShutdown(%v) = %v, want %v", args, got, want)
			}
		})
	}

	// The two malformed spellings the reviewer verified concretely: neither
	// may be classified as a shutdown, or globalFlags.useAPI routes them to
	// the API before powerActionFor ever gets a look.
	for _, args := range [][]string{
		{"power", "on", "off"},
		{"power", "status", "off"},
	} {
		if isShutdown(args) {
			t.Errorf("isShutdown(%v) = true, want false: not a real shutdown spelling", args)
		}
		if (globalFlags{}).useAPI(args) {
			t.Errorf("globalFlags{}.useAPI(%v) = true, want false: must stay local and hit powerActionFor's validation", args)
		}
	}
}

// TestPowerOnWakesThePowerGroupOnly pins the bare cluster wake: fans first,
// then a magic packet to every host of the power group -- which excludes f3.
func TestPowerOnWakesThePowerGroupOnly(t *testing.T) {
	shelly := newFakeShelly(t, false)
	cfg := powerConfig(t, shelly)

	out, _, err := runCLI(t, cfg, hostsUp(), "power", "on")
	if err != nil {
		t.Fatalf("power on: %v", err)
	}
	if got := shelly.setCalls(); len(got) != 1 || !got[0] {
		t.Fatalf("Switch.Set calls = %v, want exactly one with on=true: fans go on first", got)
	}
	if !strings.Contains(out, "magic packet to f0") {
		t.Errorf("output = %q, want f0 woken", out)
	}
	if strings.Contains(out, "magic packet to f3") {
		t.Errorf("output = %q, want f3 left alone: it is not in the power group", out)
	}
}

// TestPowerAllOnWakesEveryFHost pins that the two-word group spelling still
// resolves to the whole-rack wake, f3 included -- the case `power on f3` used
// to be silently confused with.
func TestPowerAllOnWakesEveryFHost(t *testing.T) {
	shelly := newFakeShelly(t, false)
	cfg := powerConfig(t, shelly)

	out, _, err := runCLI(t, cfg, hostsUp(), "power", "all", "on")
	if err != nil {
		t.Fatalf("power all on: %v", err)
	}
	if got := shelly.setCalls(); len(got) != 1 || !got[0] {
		t.Fatalf("Switch.Set calls = %v, want exactly one with on=true: fans go on first", got)
	}
	for _, host := range []string{"f0", "f3"} {
		if !strings.Contains(out, "magic packet to "+host) {
			t.Errorf("output = %q, want %s woken too", out, host)
		}
	}
}

// TestPowerHostOnWakesThatHostAlone pins the documented per-host spelling: one
// packet, to the host named, and the fan plug untouched.
func TestPowerHostOnWakesThatHostAlone(t *testing.T) {
	shelly := newFakeShelly(t, false)
	cfg := powerConfig(t, shelly)

	out, _, err := runCLI(t, cfg, hostsUp(), "power", "f3", "on")
	if err != nil {
		t.Fatalf("power f3 on: %v", err)
	}
	if !strings.Contains(out, "magic packet to f3") {
		t.Errorf("output = %q, want f3 woken", out)
	}
	if strings.Contains(out, "magic packet to f0") {
		t.Errorf("output = %q, want only the named host woken", out)
	}
	if got := shelly.setCalls(); len(got) != 0 {
		t.Errorf("Switch.Set calls = %v, want none: a single host does not own the rack fans", got)
	}
}

// TestPowerHostOnRejectsAnUnknownHost pins that the host name reaches the
// engine rather than being dropped: a name no inventory knows must fail, and
// name it.
func TestPowerHostOnRejectsAnUnknownHost(t *testing.T) {
	shelly := newFakeShelly(t, false)
	cfg := powerConfig(t, shelly)

	_, _, err := runCLI(t, cfg, hostsUp(), "power", "f9", "on")
	if err == nil {
		t.Fatal("power f9 on succeeded; f9 is in no inventory")
	}
	if !strings.Contains(err.Error(), `unknown host "f9"`) {
		t.Errorf("error = %v, want it to name the unknown host", err)
	}
	if got := shelly.setCalls(); len(got) != 0 {
		t.Errorf("Switch.Set calls = %v, want none", got)
	}
}

// TestPowerStatusPrintsTheTable pins the read-only verb in its only accepted
// arity. The inventory is emptied so ProbeAll has no host to ping: the table
// header and the fan line are what this is about, and nothing leaves the box.
func TestPowerStatusPrintsTheTable(t *testing.T) {
	shelly := newFakeShelly(t, true)
	cfg := powerConfig(t, shelly)
	cfg.Inventory.Hosts = nil

	out, _, err := runCLI(t, cfg, hostsUp(), "power", "status")
	if err != nil {
		t.Fatalf("power status: %v", err)
	}
	if !strings.Contains(out, "HOST") {
		t.Errorf("output = %q, want the status table", out)
	}
	if !strings.Contains(out, "rack fans: on") {
		t.Errorf("output = %q, want the fan state", out)
	}
	if got := shelly.setCalls(); len(got) != 0 {
		t.Errorf("Switch.Set calls = %v, want none: status must not switch", got)
	}
}

// TestPowerActionForCoversEveryAcceptedSpelling is the table's own test: it
// pins which spellings resolve to an action at all and which are rejected.
// It does NOT tell the accepted spellings apart from one another -- a
// non-nil action and no error looks the same whether powerActionFor bound
// On or Off, which is exactly what TestPowerActionForBindsTheRightMethod
// below is for.
func TestPowerActionForCoversEveryAcceptedSpelling(t *testing.T) {
	for _, args := range [][]string{
		{"status"}, {"on"}, {"off"},
		{"all", "on"}, {"all", "off"},
		{"f0", "on"}, {"f0", "off"}, {"f3", "on"}, {"f3", "off"},
	} {
		act, err := powerActionFor(args)
		if err != nil {
			t.Errorf("powerActionFor(%v) = %v, want it accepted", args, err)
		}
		if err == nil && act == nil {
			t.Errorf("powerActionFor(%v) returned no action and no error", args)
		}
	}

	for _, args := range [][]string{
		{}, {"onn"}, {"on", "off"}, {"all"}, {"f0"}, {"status", "f0"},
		{"on", "f3"}, {"off", "f2"}, {"all", "on", "off"},
		// A verb where a host name belongs, the two spellings the doc comment
		// on the isPowerVerb branch (cli.go) actually cites as motivating
		// examples -- kept alongside {"on", "off"} above rather than just it.
		{"off", "on"}, {"status", "on"},
	} {
		if _, err := powerActionFor(args); err == nil {
			t.Errorf("powerActionFor(%v) accepted a spelling nothing documents", args)
		}
	}
}

// spyEngine is a fake powerEngine that only records which method ran and
// with what host argument, standing in for a genuine *power.Engine so
// TestPowerActionForBindsTheRightMethod can pin exactly which Engine method
// powerActionFor bound to a given spelling.
//
// It exists because Off, OffAll and OffHost dial real SSH connections on a
// genuine *power.Engine -- driving those over the network is not an option
// for a hermetic unit test -- and because a method-swap mutation (binding
// the "off" slot to On instead of Off) has the identical func signature as
// the correct binding, so nothing short of actually calling the resolved
// action can tell the two apart.
type spyEngine struct {
	calls []string
	hosts []string
}

func (s *spyEngine) On(context.Context, io.Writer) error     { return s.record("On", "") }
func (s *spyEngine) Off(context.Context, io.Writer) error    { return s.record("Off", "") }
func (s *spyEngine) OnAll(context.Context, io.Writer) error  { return s.record("OnAll", "") }
func (s *spyEngine) OffAll(context.Context, io.Writer) error { return s.record("OffAll", "") }

func (s *spyEngine) OnHost(_ context.Context, _ io.Writer, name string) error {
	return s.record("OnHost", name)
}

func (s *spyEngine) OffHost(_ context.Context, _ io.Writer, name string) error {
	return s.record("OffHost", name)
}

func (s *spyEngine) ProbeAll(context.Context) []power.HostStatus {
	_ = s.record("ProbeAll", "")
	return nil
}

func (s *spyEngine) FansStatus(context.Context) (power.FansState, error) {
	return power.FansState{}, nil
}

func (s *spyEngine) record(call, host string) error {
	s.calls = append(s.calls, call)
	if host != "" {
		s.hosts = append(s.hosts, host)
	}
	return nil
}

// TestPowerActionForBindsTheRightMethod invokes each resolved powerAction
// against a spy engine and asserts which method actually ran. This is the
// regression test for a method-swap mutation on the off side (e.g. binding
// the "off" slot to powerEngine.On): TestPowerActionForCoversEveryAccepted
// Spelling above only checks that powerActionFor returns a non-nil action and
// no error, which looks identical either way. The on side already gets this
// for free from the end-to-end tests above, which pin it by which hosts get
// woken; the off side cannot be driven the same way without real SSH, so it
// needs the spy instead.
func TestPowerActionForBindsTheRightMethod(t *testing.T) {
	tests := []struct {
		args []string
		call string
		host string
	}{
		{[]string{"status"}, "ProbeAll", ""},
		{[]string{"on"}, "On", ""},
		{[]string{"off"}, "Off", ""},
		{[]string{"all", "on"}, "OnAll", ""},
		{[]string{"all", "off"}, "OffAll", ""},
		{[]string{"f0", "on"}, "OnHost", "f0"},
		{[]string{"f0", "off"}, "OffHost", "f0"},
		{[]string{"f3", "on"}, "OnHost", "f3"},
		{[]string{"f3", "off"}, "OffHost", "f3"},
	}

	for _, tt := range tests {
		t.Run(strings.Join(tt.args, "_"), func(t *testing.T) {
			act, err := powerActionFor(tt.args)
			if err != nil {
				t.Fatalf("powerActionFor(%v): %v", tt.args, err)
			}

			spy := &spyEngine{}
			if err := act(context.Background(), spy, io.Discard); err != nil {
				t.Fatalf("action for %v: %v", tt.args, err)
			}

			if len(spy.calls) != 1 || spy.calls[0] != tt.call {
				t.Errorf("calls = %v, want [%s]", spy.calls, tt.call)
			}
			if tt.host != "" && (len(spy.hosts) != 1 || spy.hosts[0] != tt.host) {
				t.Errorf("hosts = %v, want [%s]", spy.hosts, tt.host)
			}
		})
	}
}
