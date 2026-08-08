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
