package power

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/inventory"
)

// TestPartitionLiveSkipsHostsThatAreAlreadyOff is the regression test for a
// `power off` run that aborted with one host already down and left the rest of
// the rack running.
//
// Every step of a shutdown speaks SSH, which a powered-off host cannot answer.
// On 2026-08-09, f0 being off made the zusb pre-flight fail with "connect to
// host 192.168.1.130 port 22: Operation timed out" and the whole sequence
// stopped, leaving f1 and f2 up.
func TestPartitionLiveSkipsHostsThatAreAlreadyOff(t *testing.T) {
	hosts := []inventory.Host{
		{Name: "f1", IP: "192.168.1.131"},
		{Name: "f2", IP: "192.168.1.132"},
		{Name: "f0", IP: "192.168.1.130"},
	}

	// f0 is powered off; the other two answer.
	live, off := partitionLive(hosts, func(ip string) bool {
		return ip != "192.168.1.130"
	})

	if got := names(live); len(got) != 2 || got[0] != "f1" || got[1] != "f2" {
		t.Errorf("live = %v, want [f1 f2]", got)
	}
	if got := names(off); len(got) != 1 || got[0] != "f0" {
		t.Errorf("already off = %v, want [f0]", got)
	}
}

// TestPartitionLivePreservesOrder pins that filtering does not disturb the
// shutdown order.
//
// The order is load-bearing: the CARP storage master must go last, or the VIP
// fails over onto a host that is itself about to be powered off. That wedged f1
// on 2026-08-08. A filter that reordered would silently reintroduce it.
func TestPartitionLivePreservesOrder(t *testing.T) {
	hosts := inventory.Default().ShutdownOrder()
	live, _ := partitionLive(hosts, func(string) bool { return true })

	if len(live) != len(hosts) {
		t.Fatalf("live has %d hosts, want %d", len(live), len(hosts))
	}
	for i := range hosts {
		if live[i].Name != hosts[i].Name {
			t.Fatalf("order changed at %d: got %s, want %s", i, live[i].Name, hosts[i].Name)
		}
	}
	if last := live[len(live)-1].Name; last != inventory.StorageMaster {
		t.Errorf("last host is %s, want the storage master %s", last, inventory.StorageMaster)
	}
}

// TestPartitionLiveWithEverythingOff pins that a fully powered-down rack is not
// an error: there is simply nothing left to shut down.
func TestPartitionLiveWithEverythingOff(t *testing.T) {
	hosts := inventory.Default().ShutdownOrder()
	live, off := partitionLive(hosts, func(string) bool { return false })

	if len(live) != 0 {
		t.Errorf("live = %v, want none", names(live))
	}
	if len(off) != len(hosts) {
		t.Errorf("already off = %d hosts, want %d", len(off), len(hosts))
	}
}

func names(hosts []inventory.Host) []string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, h.Name)
	}
	return out
}

// fakeShelly stands in for the rack-fan Shelly plug.
//
// It answers the two RPCs the engine uses and records every Switch.Set, so the
// tests can ask the only question that matters here -- "did the plug actually
// get switched off" -- rather than reading log text. Digest auth is not
// offered: shellyRPC only performs the challenge-response after a 401, so
// answering 200 immediately keeps the fixture to what is under test.
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

// testEngine returns an Engine wired to the fake plug, with liveness and the
// NFS listing injected, a negligible probe gap, and no gateways in the
// inventory.
//
// The seams are what make these tests hermetic. isUp replaces ping(8), so "f3
// is running" costs no packets; nfsMounts replaces the mount table, so
// checkLocalNFS cannot run umount(8) over whatever this machine happens to
// have mounted (this box does mount NFS -- an earlier skip-if-mounted guard
// silently removed four of these tests when it was); the probe gap collapses
// the consecutive-miss wait the fan guard now performs. Dropping the gateways
// makes the Gogios mute a no-op instead of an SSH connection to a WireGuard
// address that is not there.
//
// What is NOT seamed is e.ssh, so nothing here reaches shutdownEach,
// zusbPreflight or awaitPowerDown with a non-empty host list: the tests that
// call Off/OffAll report every power-group host as already down, which empties
// the list at partitionLive. They exercise the pre-flight, the fan guard and
// the plug -- the parts this fix is about -- and not the SSH-driven middle of a
// shutdown. Seaming that is a separate, queued task.
func testEngine(t *testing.T, shelly *fakeShelly, up ...string) *Engine {
	t.Helper()
	return buildTestEngine(t, shelly, true, up)
}

// testEngineFullInventory is testEngine over the whole compiled-in inventory,
// gateways and k3s nodes included. Only for tests that must prove a lookup
// filters by role itself, rather than inheriting a pre-filtered inventory; it
// cannot be used to drive a shutdown, because the Gogios mute would then SSH
// to the gateways for real.
func testEngineFullInventory(t *testing.T, shelly *fakeShelly, up ...string) *Engine {
	t.Helper()
	return buildTestEngine(t, shelly, false, up)
}

func buildTestEngine(t *testing.T, shelly *fakeShelly, fHostsOnly bool, up []string) *Engine {
	t.Helper()

	pwFile := filepath.Join(t.TempDir(), "shelly_plug")
	if err := os.WriteFile(pwFile, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatalf("writing the Shelly password file: %v", err)
	}

	cfg := config.Default()
	cfg.ShellyPasswordFile = []string{pwFile}
	cfg.Inventory.ShellyIP = shelly.addr()
	if fHostsOnly {
		cfg.Inventory.Hosts = cfg.Inventory.ByRole(inventory.RoleF)
	}

	liveIPs := map[string]bool{}
	for _, name := range up {
		h, ok := cfg.Inventory.ByName(name)
		if !ok {
			t.Fatalf("no host %q in the inventory", name)
		}
		liveIPs[h.IP] = true
	}

	e, err := New(cfg)
	if err != nil {
		t.Fatalf("power.New: %v", err)
	}
	// Injected liveness is always *known*: these tests answer "is it up", and
	// the unknown case has tests of its own.
	e.isUp = func(_ context.Context, ip string) (bool, bool) { return liveIPs[ip], true }
	e.nfsMounts = func(context.Context) ([]string, error) { return nil, nil }
	e.downProbeInterval = time.Millisecond
	return e
}

// hostIP returns a host's address from the engine's own inventory, so a test
// can talk about "f3" without hard-coding 192.168.1.133.
func hostIP(t *testing.T, e *Engine, name string) string {
	t.Helper()
	h, ok := e.cfg.Inventory.ByName(name)
	if !ok {
		t.Fatalf("no host %q in the inventory", name)
	}
	return h.IP
}

// TestClusterOffLeavesTheRackFansOnWhileF3IsRunning is the regression test for
// the thermal hazard: `f3sctl power off` cut the fan plug on the strength of
// f0/f1/f2 alone.
//
// f3 is not in PowerGroup, so a bare `power off` deliberately leaves it
// running -- and then switched off the only cooling in the rack, which is the
// very thing `fans off` refuses to do without --force. The fans must stay on,
// and the run must still succeed: the hosts it was asked to power off did go
// down.
func TestClusterOffLeavesTheRackFansOnWhileF3IsRunning(t *testing.T) {
	shelly := newFakeShelly(t, true)
	eng := testEngine(t, shelly, "f3")

	var log bytes.Buffer
	if err := eng.Off(context.Background(), &log); err != nil {
		t.Fatalf("power off: %v", err)
	}
	if got := shelly.setCalls(); len(got) != 0 {
		t.Fatalf("Switch.Set calls = %v, want none: f3 is still running", got)
	}
	if !strings.Contains(log.String(), fansLeftOn) {
		t.Errorf("log = %q, want it to say %q", log.String(), fansLeftOn)
	}
	if !strings.Contains(log.String(), "f3") {
		t.Errorf("log = %q, want it to name f3 as the reason", log.String())
	}

	// The closing line must carry it too: a client tailing the job log sees
	// the end of a successful run, and "All hosts accepted shutdown." on its
	// own reads as a rack that went fully cold.
	tail := log.String()[strings.LastIndex(log.String(), "All hosts accepted shutdown"):]
	if !strings.Contains(tail, fansLeftOn) {
		t.Errorf("closing line = %q, want it to repeat %q", tail, fansLeftOn)
	}
}

// TestClusterOffRecordsTheFansAsLeftOn pins the machine-readable half of the
// same outcome. The run succeeds (rc=0 for the API job), so the progress step
// is the only thing distinguishing "shut the cluster down and cut the cooling"
// from "shut the cluster down and deliberately kept it running" -- and a client
// polling job.json has nothing else to look at. See docs/CLIENT.md.
func TestClusterOffRecordsTheFansAsLeftOn(t *testing.T) {
	shelly := newFakeShelly(t, true)
	eng := testEngine(t, shelly, "f3")

	steps := &recordingReporter{}
	eng.WithReporter(steps)

	var log bytes.Buffer
	if err := eng.Off(context.Background(), &log); err != nil {
		t.Fatalf("power off: %v", err)
	}

	last := steps.lastStep()
	if !strings.HasPrefix(last, fansLeftOn) {
		t.Errorf("last step = %q, want it to start with %q", last, fansLeftOn)
	}
	if !strings.Contains(last, "f3") {
		t.Errorf("last step = %q, want it to name f3", last)
	}
}

// TestRackOffStillSwitchesTheFansOffWhenNothingAnswers pins the other half:
// `power all off` takes f3 down too, so once the rack is silent the plug must
// still be cut. A guard that simply stopped cutting the fans would pass the
// test above and leave them running for good.
func TestRackOffStillSwitchesTheFansOffWhenNothingAnswers(t *testing.T) {
	shelly := newFakeShelly(t, true)
	eng := testEngine(t, shelly) // every f-host already silent

	var log bytes.Buffer
	if err := eng.OffAll(context.Background(), &log); err != nil {
		t.Fatalf("power all off: %v", err)
	}
	if got := shelly.setCalls(); len(got) != 1 || got[0] {
		t.Fatalf("Switch.Set calls = %v, want exactly one with on=false", got)
	}
}

// TestClusterOffSwitchesTheFansOffOnceF3IsAlsoDown checks the guard keys on
// the rack being idle, not on f3 being special: run the cluster shutdown with
// nothing answering at all and the fans go off, exactly as they always did.
func TestClusterOffSwitchesTheFansOffOnceF3IsAlsoDown(t *testing.T) {
	shelly := newFakeShelly(t, true)
	eng := testEngine(t, shelly)

	var log bytes.Buffer
	if err := eng.Off(context.Background(), &log); err != nil {
		t.Fatalf("power off: %v", err)
	}
	if got := shelly.setCalls(); len(got) != 1 || got[0] {
		t.Fatalf("Switch.Set calls = %v, want exactly one with on=false", got)
	}
}

// TestFansOffOnceTheRackIsIdle covers the guard on its own, over every shape
// of "something is still up" -- including a member of the power group that
// failed to go silent, which is the case awaitPowerDown catches but which must
// also not reach an unguarded plug.
func TestFansOffOnceTheRackIsIdle(t *testing.T) {
	for _, tc := range []struct {
		name    string
		up      []string
		wantSet bool
	}{
		{name: "idle rack", wantSet: true},
		{name: "f3 alone", up: []string{"f3"}},
		{name: "a power-group host", up: []string{"f1"}},
		{name: "everything", up: []string{"f0", "f1", "f2", "f3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shelly := newFakeShelly(t, true)
			eng := testEngine(t, shelly, tc.up...)

			var log bytes.Buffer
			leftOn, err := eng.fansOffOnceTheRackIsIdle(context.Background(), &log)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			got := shelly.setCalls()
			if !tc.wantSet {
				if len(got) != 0 {
					t.Fatalf("Switch.Set calls = %v, want none while %v is up", got, tc.up)
				}
				if len(leftOn) != len(tc.up) {
					t.Errorf("hosts keeping the fans on = %v, want %v", leftOn, tc.up)
				}
				for _, name := range tc.up {
					if !strings.Contains(log.String(), name) {
						t.Errorf("log = %q, want it to name %s", log.String(), name)
					}
				}
				return
			}
			if len(got) != 1 || got[0] {
				t.Fatalf("Switch.Set calls = %v, want exactly one with on=false", got)
			}
			if len(leftOn) != 0 {
				t.Errorf("hosts keeping the fans on = %v, want none", leftOn)
			}
		})
	}
}

// TestFansStayOnWhenLivenessCannotBeProbed is the regression test for a guard
// whose fail-safe pointed the wrong way.
//
// A probe that cannot be carried out used to be indistinguishable from a host
// that is silent: both were a bare false. So "ping(8) is missing" read as
// "every f-host is off", and the shutdown's last step cut the cooling to a
// fully running rack -- while the same broken probe had already told
// partitionLive that there was nothing to shut down. That is not hypothetical:
// a CGI whose PATH lacked /sbin made every host report false once already, in
// the very environment `power off` runs in (see pingCandidates).
//
// Unknown must therefore count as running, and nothing may be switched.
func TestFansStayOnWhenLivenessCannotBeProbed(t *testing.T) {
	shelly := newFakeShelly(t, true)
	eng := testEngine(t, shelly) // nothing "answers"...
	// ...but nothing is known either: every probe fails to happen.
	eng.isUp = func(context.Context, string) (bool, bool) { return false, false }

	var log bytes.Buffer
	leftOn, err := eng.fansOffOnceTheRackIsIdle(context.Background(), &log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := shelly.setCalls(); len(got) != 0 {
		t.Fatalf("Switch.Set calls = %v, want none: nothing is known about the rack", got)
	}
	if want := []string{"f0", "f1", "f2", "f3"}; !reflect.DeepEqual(leftOn, want) {
		t.Errorf("hosts keeping the fans on = %v, want %v: unknown counts as running", leftOn, want)
	}
	if !strings.Contains(log.String(), "could not be probed") {
		t.Errorf("log = %q, want it to say the hosts could not be probed", log.String())
	}
}

// TestFansStayOnWhenPingIsMissing is the same hazard driven through the real
// classification in pingWith rather than a hand-written unknown: with no
// ping(8) to run, every host is unknown and the plug must not be touched.
//
// The failure this pins is precise. Before the second return value existed,
// pingOnce returned false when it could not find a binary, LiveHosts dropped
// every host, and the guard read len(up)==0 as an idle rack.
func TestFansStayOnWhenPingIsMissing(t *testing.T) {
	shelly := newFakeShelly(t, true)
	eng := testEngine(t, shelly)
	eng.isUp = func(ctx context.Context, ip string) (bool, bool) {
		return eng.pingWith(ctx, "", ip) // no ping(8) anywhere
	}

	var log bytes.Buffer
	leftOn, err := eng.fansOffOnceTheRackIsIdle(context.Background(), &log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := shelly.setCalls(); len(got) != 0 {
		t.Fatalf("Switch.Set calls = %v, want none: ping(8) could not be run", got)
	}
	if len(leftOn) != 4 {
		t.Errorf("hosts keeping the fans on = %v, want all four f-hosts", leftOn)
	}
}

// TestFansStayOnWhenEveryPingFailsHard drives the same hazard as
// TestFansStayOnWhenPingIsMissing through a ping(8) that exists and fails.
//
// The failure modes that matter here hit every host identically -- no route,
// ICMP dropped by the switch, a ping(8) that is neither setuid nor setcap and
// so cannot open its socket -- so the rack does not look partly broken, it
// looks entirely powered off. While that counted as a confirmed silence, all
// three of the guard's probes agreed and the plug went off under a running
// rack.
func TestFansStayOnWhenEveryPingFailsHard(t *testing.T) {
	shelly := newFakeShelly(t, true)
	eng := testEngine(t, shelly)

	bin := filepath.Join(t.TempDir(), "ping")
	script := "#!/bin/sh\necho 'ping: socket: Operation not permitted' >&2\nexit 2\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the ping stub: %v", err)
	}
	eng.isUp = func(ctx context.Context, ip string) (bool, bool) {
		return eng.pingWith(ctx, bin, ip)
	}

	var log bytes.Buffer
	leftOn, err := eng.fansOffOnceTheRackIsIdle(context.Background(), &log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := shelly.setCalls(); len(got) != 0 {
		t.Fatalf("Switch.Set calls = %v, want none: ping(8) never sent a packet", got)
	}
	if len(leftOn) != 4 {
		t.Errorf("hosts keeping the fans on = %v, want all four f-hosts", leftOn)
	}
	if !strings.Contains(log.String(), "could not be probed") {
		t.Errorf("log = %q, want it to say the hosts could not be probed", log.String())
	}
}

// TestPingSeparatesSilenceFromAFailedProbe covers the distinction everything
// above rests on, with ordinary executables standing in for ping(8).
//
// What makes an answer an answer is the statistics line, not the exit status. A
// ping that printed "1 packets transmitted, 0 received" measured the host and
// heard nothing; a ping that exits non-zero having printed no statistics never
// got as far as measuring anything, and the two are indistinguishable by exit
// code across the platforms this runs on. The stubs reproduce both shapes,
// including the real output of `ping no.such.host.invalid`.
func TestPingSeparatesSilenceFromAFailedProbe(t *testing.T) {
	dir := t.TempDir()
	n := 0
	stub := func(exit int, output string) string {
		n++
		p := filepath.Join(dir, fmt.Sprintf("ping%d", n))
		script := fmt.Sprintf("#!/bin/sh\ncat <<'EOF'\n%s\nEOF\nexit %d\n", output, exit)
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatalf("writing the ping stub: %v", err)
		}
		return p
	}

	const stats = "--- 192.0.2.1 ping statistics ---\n" +
		"1 packets transmitted, 0 received, 100% packet loss, time 0ms"

	eng := &Engine{cfg: config.Default()}
	for _, tc := range []struct {
		name      string
		bin       string
		cancel    bool
		up, known bool
	}{
		{name: "answered", bin: stub(0, "64 bytes from 192.0.2.1"), up: true, known: true},
		{name: "no reply (linux code)", bin: stub(1, stats), known: true},
		{name: "no reply (bsd code)", bin: stub(2, stats), known: true},

		// The gap this closes. A hard failure exits non-zero and prints no
		// statistics: it never transmitted, so it says nothing about the host.
		// Counting it as a confirmed silence is how an unroutable network, a
		// switch dropping ICMP, or a ping(8) that cannot open its socket used
		// to report the entire rack down -- on every probe, identically -- and
		// take the fan plug with it.
		{name: "name resolution failed",
			bin: stub(2, "ping: no.such.host.invalid: Name or service not known")},
		{name: "socket not permitted",
			bin: stub(2, "ping: socket: Operation not permitted")},
		{name: "no route to host", bin: stub(1, "ping: connect: Network is unreachable")},

		{name: "no ping(8) at all", bin: ""},
		{name: "ping(8) cannot be run", bin: filepath.Join(dir, "absent")},
		{name: "probe cut short", bin: stub(0, ""), cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}

			up, known := eng.pingWith(ctx, tc.bin, "192.0.2.1")
			if up != tc.up || known != tc.known {
				t.Errorf("pingWith = (up=%v, known=%v), want (up=%v, known=%v)",
					up, known, tc.up, tc.known)
			}
		})
	}
}

// TestProbeKeepsWhetherTheProbeRanAtAll pins that probeOne carries both halves
// of the liveness answer into HostStatus.
//
// It used to keep only "did it answer" and throw the rest away, and HostStatus
// is the evidence the HTTP API's fan guard reads -- so a probe that could not
// run arrived there as an ordinary powered-off host, and POST /fans/off cut the
// cooling to a running rack without even offering the confirmation field. The
// SSH half is irrelevant here and is pointed at TEST-NET-1 so it fails fast.
func TestProbeKeepsWhetherTheProbeRanAtAll(t *testing.T) {
	cfg := config.Default()
	cfg.ProbeTimeout = config.Duration(10 * time.Millisecond)
	e := &Engine{cfg: cfg}
	e.isUp = func(context.Context, string) (bool, bool) { return false, false }

	st := e.probeOne(context.Background(), inventory.Host{
		Name: "f0", Role: inventory.RoleF, IP: "192.0.2.1", SSHPort: 22,
	})

	if st.Ping {
		t.Errorf("Ping = true, want false: nothing answered")
	}
	if st.PingKnown {
		t.Error("PingKnown = true, want false: the probe never reached a conclusion")
	}
	if !RackActivityFrom([]HostStatus{st}).Busy() {
		t.Error("a host whose probe never ran does not keep the rack fans on")
	}
}

// TestBothHalvesOfTheGuardAgree is the invariant behind having one rack-activity
// rule instead of three readings of "is anything up".
//
// The HTTP API judges a snapshot it already holds (RackActivityFrom); the CLI
// and the shutdown's last step probe on demand (Engine.RackActivity). They are
// allowed to differ in how much evidence they gather -- one probe versus
// several -- but never in what they conclude from it. They did: the snapshot
// side read Ping alone, so "the probe could not run" came out as an idle rack
// there and as a busy one everywhere else, and `f3sctl fans off` and
// `f3sctl fans off --remote` gave opposite answers on the same Pi.
func TestBothHalvesOfTheGuardAgree(t *testing.T) {
	for _, tc := range []struct {
		name       string
		up, known  bool
		wantBusy   bool
		wantReason string
	}{
		{name: "answering", up: true, known: true, wantBusy: true, wantReason: "still running"},
		{name: "silent", known: true},
		{name: "unprobeable", wantBusy: true, wantReason: "could not be probed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shelly := newFakeShelly(t, true)
			eng := testEngine(t, shelly)
			eng.isUp = func(context.Context, string) (bool, bool) { return tc.up, tc.known }

			probed := eng.RackActivity(context.Background())

			var snapshot []HostStatus
			for _, h := range eng.cfg.Inventory.ByRole(inventory.RoleF) {
				snapshot = append(snapshot, HostStatus{
					Name: h.Name, Role: string(h.Role), IP: h.IP,
					Ping: tc.up, PingKnown: tc.known,
				})
			}
			folded := RackActivityFrom(snapshot)

			if probed.Busy() != tc.wantBusy {
				t.Errorf("Engine.RackActivity busy = %v, want %v", probed.Busy(), tc.wantBusy)
			}
			if folded.Busy() != probed.Busy() {
				t.Errorf("RackActivityFrom busy = %v, but the live probe says %v",
					folded.Busy(), probed.Busy())
			}
			if !reflect.DeepEqual(folded.Hosts(), probed.Hosts()) {
				t.Errorf("hosts differ: folded %v, probed %v", folded.Hosts(), probed.Hosts())
			}
			if folded.Why() != probed.Why() {
				t.Errorf("reason differs: folded %q, probed %q", folded.Why(), probed.Why())
			}
			if tc.wantReason != "" && !strings.Contains(folded.Why(), tc.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", folded.Why(), tc.wantReason)
			}
		})
	}
}

// TestFanGuardNeedsConsecutiveMissesBeforeCallingAHostDown pins that the guard
// holds to the same evidence standard as awaitPowerDown.
//
// One missed ping is not proof a host is down: f0's and f1's logs both show
// "re0: link state changed to DOWN" and back to UP seconds apart during an
// ordinary shutdown, which is exactly when this runs. Deciding on one probe per
// host meant a single dropped echo reply could cut the cooling to a running
// rack -- the more dangerous question answered on the weaker evidence.
func TestFanGuardNeedsConsecutiveMissesBeforeCallingAHostDown(t *testing.T) {
	shelly := newFakeShelly(t, true)
	eng := testEngine(t, shelly)
	flapping := hostIP(t, eng, "f3")

	var mu sync.Mutex
	probes := map[string]int{}
	eng.isUp = func(_ context.Context, ip string) (bool, bool) {
		mu.Lock()
		defer mu.Unlock()
		probes[ip]++
		// f3 misses its first probe and answers the second; everything else is
		// genuinely off and never answers.
		return ip == flapping && probes[ip] > 1, true
	}

	var log bytes.Buffer
	leftOn, err := eng.fansOffOnceTheRackIsIdle(context.Background(), &log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := shelly.setCalls(); len(got) != 0 {
		t.Fatalf("Switch.Set calls = %v, want none: f3 answered its second probe", got)
	}
	if want := []string{"f3"}; !reflect.DeepEqual(leftOn, want) {
		t.Fatalf("hosts keeping the fans on = %v, want %v", leftOn, want)
	}

	mu.Lock()
	defer mu.Unlock()
	for ip, n := range probes {
		if ip == flapping {
			continue
		}
		if n < confirmedDownProbes {
			t.Errorf("%s was probed %d times before being called down, want %d",
				ip, n, confirmedDownProbes)
		}
	}
}

// TestSkipAlreadyOffKeepsHostsItCouldNotProbe pins the second of the three
// places liveness decides something, and the one with the widest blast radius.
//
// skipAlreadyOff drops hosts from the shutdown list. A probe that cannot run
// therefore does not merely mis-report a host: it removes it from the run, and
// with every host removed a shutdown shuts nothing down, reports success, and
// walks straight into the fans-off step -- which is how a broken probe cut the
// cooling to a fully running rack. Keeping an unprobeable host in the list is
// the loud outcome: it fails at the zusb pre-flight if it really was off, which
// undoes nothing and leaves the plug alone.
//
// The test engine's injected liveness always answers "known", so before this
// nothing in the suite drove unknown through here at all.
func TestSkipAlreadyOffKeepsHostsItCouldNotProbe(t *testing.T) {
	hosts := inventory.Default().ShutdownOrder()

	t.Run("unprobeable hosts stay in the list", func(t *testing.T) {
		eng := testEngine(t, newFakeShelly(t, true))
		eng.isUp = func(context.Context, string) (bool, bool) { return false, false }

		var log bytes.Buffer
		live := eng.skipAlreadyOff(context.Background(), &log, hosts)

		if !reflect.DeepEqual(names(live), names(hosts)) {
			t.Errorf("hosts to shut down = %v, want all of %v: nothing is known about them",
				names(live), names(hosts))
		}
		if strings.Contains(log.String(), "already powered off") {
			t.Errorf("log = %q, want no claim that a host it could not probe is off", log.String())
		}
	})

	// The other direction, so a guard that simply never skipped anything would
	// not pass: a host confirmed silent is still dropped, or a staged shutdown
	// aborts at the first host that is already down.
	t.Run("hosts confirmed silent are dropped", func(t *testing.T) {
		eng := testEngine(t, newFakeShelly(t, true))

		var log bytes.Buffer
		live := eng.skipAlreadyOff(context.Background(), &log, hosts)

		if len(live) != 0 {
			t.Errorf("hosts to shut down = %v, want none: every host answered nothing", names(live))
		}
	})
}

// TestAwaitPowerDownNeverConfirmsAHostItCouldNotProbe pins the third site.
//
// This step exists to catch a host that accepted a shutdown and then wedged, so
// recording one as safely powered off on the strength of a probe that never ran
// is the exact failure it was written to prevent -- and it would also hand the
// fans-off step a rack it believes is cold. Unknown must never count as a miss.
//
// The context is cut short rather than waiting out powerDownTimeout: the point
// is that these hosts are still pending after several probes, which is decided
// within milliseconds at the test engine's probe gap.
func TestAwaitPowerDownNeverConfirmsAHostItCouldNotProbe(t *testing.T) {
	eng := testEngine(t, newFakeShelly(t, true))
	eng.isUp = func(context.Context, string) (bool, bool) { return false, false }

	hosts := eng.cfg.Inventory.ShutdownOrder()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	var log bytes.Buffer
	stuck := eng.awaitPowerDown(ctx, &log, hosts)

	if len(stuck) != len(hosts) {
		t.Fatalf("unconfirmed hosts = %v, want all of %v: an unprobeable host is "+
			"not a host that powered down", stuck, names(hosts))
	}
	if strings.Contains(log.String(), "is down") {
		t.Errorf("log = %q, want no host reported as down", log.String())
	}
}

// TestUnconfirmedPowerDownsAreDiagnosedApart pins that "never confirmed" is
// reported as the situation it actually is.
//
// Both outcomes here are failures of the run -- nothing proved the host powered
// down -- but they lead somewhere completely different. A host that kept
// answering is on its way to being powered on with no network, and recovering
// it needs the JetKVM console or the physical button. A host that went quiet and
// could not be probed has very likely powered off exactly as asked; the fault is
// on this machine, and sending its operator to the garage with a keyboard is
// both wrong and expensive.
//
// The second case is not exotic. Unknown counts as "not confirmed off"
// throughout, deliberately, so one broken ping(8) turns a textbook shutdown into
// three hosts reported as hung.
func TestUnconfirmedPowerDownsAreDiagnosedApart(t *testing.T) {
	const (
		hungAdvice     = "JetKVM"
		hungPhrase     = "still answering"
		unprobedPhrase = "could not be"
		unprobedAdvice = "fault on THIS machine"
	)

	newEngine := func(t *testing.T) (*Engine, *recordingReporter) {
		t.Helper()
		eng := testEngine(t, newFakeShelly(t, true))
		// The wait is a field so this takes 50ms rather than two real minutes.
		eng.powerDownTimeout = 50 * time.Millisecond
		steps := &recordingReporter{}
		eng.WithReporter(steps)
		return eng, steps
	}

	t.Run("the probe could not run", func(t *testing.T) {
		eng, steps := newEngine(t)
		eng.isUp = func(context.Context, string) (bool, bool) { return false, false }
		hosts := eng.cfg.Inventory.ShutdownOrder()

		var log bytes.Buffer
		stuck := eng.awaitPowerDown(context.Background(), &log, hosts)

		if len(stuck) != len(hosts) {
			t.Fatalf("unconfirmed = %v, want all of %v", stuck, names(hosts))
		}
		if strings.Contains(log.String(), hungPhrase) || strings.Contains(log.String(), hungAdvice) {
			t.Errorf("log = %q, want no hung-host diagnosis: these hosts never answered", log.String())
		}
		if !strings.Contains(log.String(), unprobedAdvice) {
			t.Errorf("log = %q, want it to point at the probe on this machine", log.String())
		}
		if got := steps.hostState("f0"); !strings.Contains(got, unprobedPhrase) {
			t.Errorf("f0 reported as %q, want it to say the power-down is unconfirmed "+
				"because it could not be probed", got)
		}
	})

	t.Run("the host kept answering", func(t *testing.T) {
		eng, steps := newEngine(t)
		eng.isUp = func(context.Context, string) (bool, bool) { return true, true }
		hosts := eng.cfg.Inventory.ShutdownOrder()

		var log bytes.Buffer
		stuck := eng.awaitPowerDown(context.Background(), &log, hosts)

		if len(stuck) != len(hosts) {
			t.Fatalf("unconfirmed = %v, want all of %v", stuck, names(hosts))
		}
		if !strings.Contains(log.String(), hungPhrase) || !strings.Contains(log.String(), hungAdvice) {
			t.Errorf("log = %q, want the hung-host diagnosis", log.String())
		}
		if strings.Contains(log.String(), unprobedAdvice) {
			t.Errorf("log = %q, want no broken-probe diagnosis: the hosts answered", log.String())
		}
		if got := steps.hostState("f0"); !strings.Contains(got, "hung") {
			t.Errorf("f0 reported as %q, want it to say the host is likely hung", got)
		}
	})

	// A host heard from once and silent afterwards is a hung host, not an
	// unprobeable one: it is alive, and the later silence is the very interface
	// flap the consecutive-miss rule exists for.
	t.Run("a host heard from once", func(t *testing.T) {
		eng, _ := newEngine(t)
		flapping := hostIP(t, eng, "f1")

		var mu sync.Mutex
		probes := 0
		eng.isUp = func(_ context.Context, ip string) (bool, bool) {
			mu.Lock()
			defer mu.Unlock()
			probes++
			// f1 answers its first probe, then nothing can be learned about
			// anything; f0 and f2 are never probed successfully at all.
			return ip == flapping && probes <= 3, ip == flapping && probes <= 3
		}

		var log bytes.Buffer
		eng.awaitPowerDown(context.Background(), &log, eng.cfg.Inventory.ShutdownOrder())

		if !strings.Contains(log.String(), "f1 accepted the shutdown but is still answering") {
			t.Errorf("log = %q, want f1 diagnosed as hung: it answered", log.String())
		}
		if !strings.Contains(log.String(), "f0 accepted the shutdown and then went quiet") {
			t.Errorf("log = %q, want f0 diagnosed as unprobeable: it never answered", log.String())
		}
	})
}

// TestAShutdownThatAbortsNeverTouchesTheFans pins the other exit from off():
// a pre-flight that refuses must return before the fans-off step, not despite
// it. Here the local NFS listing fails, which is itself a fix -- mount(8) that
// cannot be run used to be reported as "nothing is mounted".
func TestAShutdownThatAbortsNeverTouchesTheFans(t *testing.T) {
	shelly := newFakeShelly(t, true)
	eng := testEngine(t, shelly)
	eng.nfsMounts = func(context.Context) ([]string, error) {
		return nil, errors.New("mount: not found")
	}

	var log bytes.Buffer
	if err := eng.Off(context.Background(), &log); err == nil {
		t.Fatal("power off succeeded, want the NFS pre-flight to abort it")
	}
	if got := shelly.setCalls(); len(got) != 0 {
		t.Fatalf("Switch.Set calls = %v, want none: the shutdown never happened", got)
	}
}

// TestShutdownFailureSaysTheFansWereLeftOn pins the wording of the other place
// a rack-wide run ends with the plug untouched: hosts that accepted a shutdown
// and never went silent are still running, so the cooling stays on, and the
// error has to say so in the same words as the progress step.
//
// It tests the message rather than a run that produces it: reaching that branch
// needs shutdownEach or awaitPowerDown to fail, both of which speak SSH through
// the unseamed e.ssh. Seaming that is a separate, queued task; until then the
// path from failed hosts to the returned error is this one function.
func TestShutdownFailureSaysTheFansWereLeftOn(t *testing.T) {
	err := shutdownFailure([]string{"f1"})
	if !strings.Contains(err.Error(), "f1") {
		t.Errorf("error = %v, want it to name the host", err)
	}
	if !strings.Contains(err.Error(), fansLeftOn) {
		t.Errorf("error = %v, want it to contain %q", err, fansLeftOn)
	}
}

// TestAHandBuiltEngineFallsBackToTheRealProbes covers the nil seams on an
// exported type.
//
// power.Engine is exported and its seams are plain func fields, so an Engine
// that did not come from New carries nils in them -- and the first thing to
// touch one would be the fan guard, which must neither crash nor fail open.
// cli.liveHostsFunc grew the same fallback, and a regression test with it,
// after exactly this concern; the engine's seams had neither.
func TestAHandBuiltEngineFallsBackToTheRealProbes(t *testing.T) {
	e := &Engine{cfg: config.Default(), report: nopReporter{}}

	// Method values compare by code pointer, which is what identifies the
	// fallback here: e.liveness() must be pingOnce, not nil.
	if got, want := reflect.ValueOf(e.liveness()).Pointer(),
		reflect.ValueOf(e.pingOnce).Pointer(); got != want {
		t.Error("liveness() on a hand-built Engine is not pingOnce")
	}
	if got := e.probeGap(); got != downProbeInterval {
		t.Errorf("probeGap() = %s, want the %s default: a zero gap turns "+
			"awaitPowerDown into a busy loop", got, downProbeInterval)
	}
	if _, err := e.localMounts(context.Background()); err != nil {
		t.Errorf("localMounts() on a hand-built Engine: %v", err)
	}
}

// recordingReporter keeps the progress a run reports, so a test can assert on
// what a polling API client would see rather than only on the human log.
type recordingReporter struct {
	mu    sync.Mutex
	steps []string
	hosts map[string]string
}

func (r *recordingReporter) Step(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, name)
}

func (r *recordingReporter) HostState(host string, phase HostPhase, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hosts == nil {
		r.hosts = map[string]string{}
	}
	r.hosts[host] = string(phase) + ": " + detail
}

// hostState returns the last phase and detail reported for a host, which is
// what a client polling job.json ends up rendering.
func (r *recordingReporter) hostState(host string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hosts[host]
}

func (r *recordingReporter) lastStep() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.steps) == 0 {
		return ""
	}
	return r.steps[len(r.steps)-1]
}

// TestOffHostNeverTouchesTheFans pins that powering one host is still not a
// rack-wide operation: `power f3 off` leaves the plug alone even though every
// other host is already silent, because the operator asked for one host, not
// for the rack to go cold.
func TestOffHostNeverTouchesTheFans(t *testing.T) {
	shelly := newFakeShelly(t, true)
	eng := testEngine(t, shelly)

	var log bytes.Buffer
	if err := eng.OffHost(context.Background(), &log, "f3"); err != nil {
		t.Fatalf("power f3 off: %v", err)
	}
	if got := shelly.setCalls(); len(got) != 0 {
		t.Fatalf("Switch.Set calls = %v, want none: a single host never switches the plug", got)
	}
}

// TestLiveHostsReportsEveryFHostInInventoryOrder pins what both fan guards read
// from: the full f-host set, f3 included, in a stable order. Dropping f3 here
// would silently restore the hazard, and an unstable order would make the
// reason printed to the operator vary between runs.
//
// The inventory is deliberately the unfiltered one. With the usual test
// engine, whose inventory has already been reduced to the f-hosts, a LiveHosts
// that iterated inv.Hosts wholesale would pass this and only misbehave in
// production -- where the gateways and k3s nodes are in there too.
func TestLiveHostsReportsEveryFHostInInventoryOrder(t *testing.T) {
	shelly := newFakeShelly(t, true)
	eng := testEngineFullInventory(t, shelly, "f3", "f0", "r1", "blowfish")

	got := eng.LiveHosts(context.Background())
	if len(got) != 2 || got[0] != "f0" || got[1] != "f3" {
		t.Errorf("LiveHosts = %v, want [f0 f3]: only f-hosts, in inventory order", got)
	}
}
