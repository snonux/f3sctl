package inventory

import "testing"

// TestShutdownOrderPutsStorageMasterLast pins the ordering rule that keeps the
// CARP storage VIP from failing over onto a host that is about to be powered
// off. Getting this wrong does not fail loudly -- it wedges a host that then
// cannot be woken remotely -- so it is worth an explicit test.
func TestShutdownOrderPutsStorageMasterLast(t *testing.T) {
	order := Default().ShutdownOrder()

	if len(order) == 0 {
		t.Fatal("shutdown order is empty")
	}
	if got := order[len(order)-1].Name; got != StorageMaster {
		t.Errorf("last host to be shut down is %q, want the storage master %q", got, StorageMaster)
	}

	// Same set as PowerGroup, just reordered: nothing may be dropped or added.
	group := Default().PowerGroup()
	if len(order) != len(group) {
		t.Fatalf("shutdown order has %d hosts, power group has %d", len(order), len(group))
	}
	seen := map[string]bool{}
	for _, h := range order {
		if seen[h.Name] {
			t.Errorf("%s appears twice in the shutdown order", h.Name)
		}
		seen[h.Name] = true
	}
	for _, h := range group {
		if !seen[h.Name] {
			t.Errorf("%s is in the power group but missing from the shutdown order", h.Name)
		}
	}
}

// TestPowerGroupExcludesF3 keeps f3 out of the cluster-wide operations: it runs
// a standalone Rocky VM, is not part of k3s, and is addressed explicitly.
func TestPowerGroupExcludesF3(t *testing.T) {
	for _, h := range Default().PowerGroup() {
		if h.Name == "f3" {
			t.Error("f3 must not be in the power group")
		}
	}
}

// TestOnlyFHostsAreWakeable guards the inventory against a MAC being pasted
// onto a host f3sctl must never power, and against an f-host losing its MAC
// (which would make it unwakeable without any obvious error).
func TestOnlyFHostsAreWakeable(t *testing.T) {
	for _, h := range Default().Hosts {
		switch h.Role {
		case RoleF:
			if !h.Wakeable() {
				t.Errorf("%s is an f-host but has no MAC, so it could never be woken", h.Name)
			}
		default:
			if h.Wakeable() {
				t.Errorf("%s has a MAC but is not an f-host; f3sctl must not wake it", h.Name)
			}
		}
	}
}

// TestGatewaysUseMeshAddresses guards the source-IP pin on the restricted SSH
// key. Reaching a gateway by its public name leaves the site through NAT, so
// the key arrives from the public address and is refused.
func TestGatewaysUseMeshAddresses(t *testing.T) {
	for _, h := range Default().ByRole(RoleGateway) {
		if h.SSHPort != 2 {
			t.Errorf("%s: gateway sshd listens on port 2, inventory says %d", h.Name, h.SSHPort)
		}
		if len(h.IP) < 12 || h.IP[:12] != "192.168.2.11" {
			t.Errorf("%s: expected a WireGuard mesh address, got %q", h.Name, h.IP)
		}
	}
}

// TestShutdownOrderAllCoversEveryFHost pins that `power all off` reaches f3,
// which is the entire reason the group exists.
func TestShutdownOrderAllCoversEveryFHost(t *testing.T) {
	order := Default().ShutdownOrderAll()

	if len(order) != len(Default().ByRole(RoleF)) {
		t.Fatalf("ShutdownOrderAll has %d hosts, want every f-host (%d)",
			len(order), len(Default().ByRole(RoleF)))
	}

	var seenF3 bool
	for _, h := range order {
		if h.Name == "f3" {
			seenF3 = true
		}
	}
	if !seenF3 {
		t.Error("f3 missing from ShutdownOrderAll; `power all off` would leave it running")
	}
}

// TestShutdownOrderAllStillEndsWithTheStorageMaster pins that widening the
// group did not lose the ordering rule.
//
// Taking the CARP storage master first fails the VIP onto a host that is itself
// about to be shut down, which wedged f1 on 2026-08-08. That hazard does not
// care whether f3 is in the list.
func TestShutdownOrderAllStillEndsWithTheStorageMaster(t *testing.T) {
	order := Default().ShutdownOrderAll()
	if last := order[len(order)-1].Name; last != StorageMaster {
		t.Errorf("ShutdownOrderAll ends with %s, want the storage master %s", last, StorageMaster)
	}
}

// TestEveryFHostIsASupersetOfThePowerGroup pins the relationship between the
// two groups: `all` is `power off` plus f3, never something different.
func TestEveryFHostIsASupersetOfThePowerGroup(t *testing.T) {
	all := make(map[string]bool)
	for _, h := range Default().EveryFHost() {
		all[h.Name] = true
	}
	for _, h := range Default().PowerGroup() {
		if !all[h.Name] {
			t.Errorf("%s is in the power group but not in EveryFHost", h.Name)
		}
	}
	if len(all) <= len(Default().PowerGroup()) {
		t.Error("EveryFHost is no larger than PowerGroup; f3 should make it bigger")
	}
}
