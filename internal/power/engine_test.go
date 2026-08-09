package power

import (
	"testing"

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
