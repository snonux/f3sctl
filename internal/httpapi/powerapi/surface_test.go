package powerapi

import (
	"testing"

	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power"
)

// snapshotOf returns a snapshot in which every host of inv answers both ICMP
// and SSH.
func snapshotOf(inv inventory.Inventory) contract.State {
	var s contract.State
	for _, h := range inv.Hosts {
		s.Hosts = append(s.Hosts, power.HostStatus{
			Name: h.Name, Role: string(h.Role), Ping: true, PingKnown: true, SSH: true,
		})
	}
	return s
}

// TestHostCountsCoverWhatTheEngineActsOn is the drift guard behind deriving
// the availability counts from the inventory: the power-on/off pair is judged
// against exactly inv.PowerGroup (what Engine.On/Off wakes and shuts down),
// and the `all` pair against exactly inv.EveryFHost.
func TestHostCountsCoverWhatTheEngineActsOn(t *testing.T) {
	inv := inventory.Default()
	sf := &Surface{Inv: inv}
	s := snapshotOf(inv)

	for _, tc := range []struct {
		name  string
		count func(contract.State) (int, int, int)
		want  int
	}{
		{"ClusterHostsUp", sf.ClusterHostsUp, len(inv.PowerGroup())},
		{"EveryFHostUp", sf.EveryFHostUp, len(inv.EveryFHost())},
	} {
		up, sshUp, total := tc.count(s)
		if up != tc.want || sshUp != tc.want || total != tc.want {
			t.Errorf("%s = (%d, %d, %d), want all %d", tc.name, up, sshUp, total, tc.want)
		}
	}
}

// TestHostCountsFollowAConfiguredInventory checks the counts against an
// inventory that differs from the compiled-in one: an extra f-host is
// counted, and a snapshot entry the inventory does not know -- even one with
// Role f -- is not, nor are the k3s guests or (for the cluster pair) the
// standalone host.
func TestHostCountsFollowAConfiguredInventory(t *testing.T) {
	inv := inventory.Inventory{Hosts: []inventory.Host{
		{Name: "f0", Role: inventory.RoleF},
		{Name: inventory.StandaloneHost, Role: inventory.RoleF},
		{Name: "f4", Role: inventory.RoleF},
		{Name: "r0", Role: inventory.RoleCluster},
	}}
	sf := &Surface{Inv: inv}
	s := contract.State{Hosts: []power.HostStatus{
		{Name: "f0", Role: "f"},
		{Name: inventory.StandaloneHost, Role: "f", Ping: true, SSH: true},
		{Name: "f4", Role: "f", Ping: true},
		{Name: "stray", Role: "f", Ping: true, SSH: true},
		{Name: "r0", Role: "cluster", Ping: true, SSH: true},
	}}

	if up, sshUp, total := sf.ClusterHostsUp(s); up != 1 || sshUp != 0 || total != 2 {
		t.Errorf("ClusterHostsUp = (%d, %d, %d), want (1, 0, 2): f0 down, f4 ping-only", up, sshUp, total)
	}
	if up, sshUp, total := sf.EveryFHostUp(s); up != 2 || sshUp != 1 || total != 3 {
		t.Errorf("EveryFHostUp = (%d, %d, %d), want (2, 1, 3): f3 up, f4 ping-only", up, sshUp, total)
	}
	if up, sshUp, total := (&Surface{}).EveryFHostUp(s); up+sshUp+total != 0 {
		t.Errorf("EveryFHostUp with no inventory = (%d, %d, %d), want nothing counted", up, sshUp, total)
	}
}
