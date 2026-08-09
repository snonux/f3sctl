package power

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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

// testEngine returns an Engine wired to the fake plug, with liveness injected
// and no gateways in the inventory.
//
// Both omissions are what make a whole shutdown runnable in a test. The isUp
// seam replaces ping(8), so "f3 is running" costs no packets; dropping the
// gateways makes the Gogios mute a no-op instead of an SSH connection to a
// WireGuard address that is not there. The f-hosts are the real ones from the
// compiled-in inventory, because the f3-shaped hole in PowerGroup is exactly
// what these tests are about.
func testEngine(t *testing.T, shelly *fakeShelly, up ...string) *Engine {
	t.Helper()

	pwFile := filepath.Join(t.TempDir(), "shelly_plug")
	if err := os.WriteFile(pwFile, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatalf("writing the Shelly password file: %v", err)
	}

	cfg := config.Default()
	cfg.ShellyPasswordFile = []string{pwFile}
	cfg.Inventory.ShellyIP = shelly.addr()
	cfg.Inventory.Hosts = cfg.Inventory.ByRole(inventory.RoleF)

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
	e.isUp = func(_ context.Context, ip string) bool { return liveIPs[ip] }
	return e
}

// skipIfNFSMounted keeps a test that drives a whole shutdown away from the
// umount in Engine.checkLocalNFS.
//
// That check is deliberately not behind a seam -- it is a safety step, and one
// more injection point on the shutdown path is a worse trade than skipping
// here -- but it does run real umount(8) against whatever this machine has
// mounted. No developer box or Pi in the fleet mounts NFS, so in practice this
// never skips; if one ever does, skipping beats unmounting its filesystems.
func skipIfNFSMounted(t *testing.T) {
	t.Helper()
	mounts, err := localNFSMounts(context.Background())
	if err != nil {
		t.Skipf("cannot tell whether NFS is mounted here: %v", err)
	}
	if len(mounts) > 0 {
		t.Skipf("NFS is mounted at %v here; Engine.off would try to unmount it", mounts)
	}
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
	skipIfNFSMounted(t)

	shelly := newFakeShelly(t, true)
	eng := testEngine(t, shelly, "f3")

	var log bytes.Buffer
	if err := eng.Off(context.Background(), &log); err != nil {
		t.Fatalf("power off: %v", err)
	}
	if got := shelly.setCalls(); len(got) != 0 {
		t.Fatalf("Switch.Set calls = %v, want none: f3 is still running", got)
	}
	if !strings.Contains(log.String(), "Leaving the rack fans on") {
		t.Errorf("log = %q, want it to say the fans were left on", log.String())
	}
	if !strings.Contains(log.String(), "f3") {
		t.Errorf("log = %q, want it to name f3 as the reason", log.String())
	}
}

// TestRackOffStillSwitchesTheFansOffWhenNothingAnswers pins the other half:
// `power all off` takes f3 down too, so once the rack is silent the plug must
// still be cut. A guard that simply stopped cutting the fans would pass the
// test above and leave them running for good.
func TestRackOffStillSwitchesTheFansOffWhenNothingAnswers(t *testing.T) {
	skipIfNFSMounted(t)

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
	skipIfNFSMounted(t)

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
			if err := eng.fansOffOnceTheRackIsIdle(context.Background(), &log); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			got := shelly.setCalls()
			if !tc.wantSet {
				if len(got) != 0 {
					t.Fatalf("Switch.Set calls = %v, want none while %v is up", got, tc.up)
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
		})
	}
}

// TestOffHostNeverTouchesTheFans pins that powering one host is still not a
// rack-wide operation: `power f3 off` leaves the plug alone even though every
// other host is already silent, because the operator asked for one host, not
// for the rack to go cold.
func TestOffHostNeverTouchesTheFans(t *testing.T) {
	skipIfNFSMounted(t)

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
func TestLiveHostsReportsEveryFHostInInventoryOrder(t *testing.T) {
	shelly := newFakeShelly(t, true)
	eng := testEngine(t, shelly, "f3", "f0")

	got := eng.LiveHosts(context.Background())
	if len(got) != 2 || got[0] != "f0" || got[1] != "f3" {
		t.Errorf("LiveHosts = %v, want [f0 f3]", got)
	}
}
