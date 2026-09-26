package powerapi

import (
	"testing"

	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power"
)

// TestHostCountsFollowAConfiguredInventory checks the availability counts
// against an inventory that differs from the compiled-in one: the standalone
// host is flagged by data (f9, while an unflagged f3 is a cluster host), an
// extra f-host is counted, and a snapshot entry the inventory does not know --
// even one with Role f -- is not, nor are the k3s guests.
//
// That the selected sets are exactly what Engine.On/Off and OnAll/OffAll act
// on is cross-checked against a driven engine in the power package
// (TestSnapshotSelectorsMatchWhatTheEngineTouches); this pins that the cluster
// pair counts through the power group and the `all` pair through every f-host.
func TestHostCountsFollowAConfiguredInventory(t *testing.T) {
	inv := inventory.Inventory{Hosts: []inventory.Host{
		{Name: "f0", Role: inventory.RoleF},
		{Name: "f3", Role: inventory.RoleF},
		{Name: "f9", Role: inventory.RoleF, Standalone: true},
		{Name: "r0", Role: inventory.RoleCluster},
	}}
	sf := &Surface{Inv: inv}
	s := contract.State{Hosts: []power.HostStatus{
		{Name: "f0", Role: "f"},
		{Name: "f3", Role: "f", Ping: true},
		{Name: "f9", Role: "f", Ping: true, SSH: true},
		{Name: "stray", Role: "f", Ping: true, SSH: true},
		{Name: "r0", Role: "cluster", Ping: true, SSH: true},
	}}

	if up, sshUp, total := sf.clusterHostsUp(s); up != 1 || sshUp != 0 || total != 2 {
		t.Errorf("clusterHostsUp = (%d, %d, %d), want (1, 0, 2): f0 down, f3 ping-only", up, sshUp, total)
	}
	if up, sshUp, total := sf.everyFHostUp(s); up != 2 || sshUp != 1 || total != 3 {
		t.Errorf("everyFHostUp = (%d, %d, %d), want (2, 1, 3): f9 up, f3 ping-only", up, sshUp, total)
	}
}

// TestGuardsNeedTheInventory pins the documented behaviour of a Surface
// without an inventory: it fails closed. Nothing is counted, so no power
// action is offered, and both snapshot guards read a silent rack as busy, so
// cutting the fans or AC still asks for force.
func TestGuardsNeedTheInventory(t *testing.T) {
	s := contract.State{Hosts: []power.HostStatus{
		{Name: "f0", Role: "f", PingKnown: true},
	}}
	bare := &Surface{}
	if up, sshUp, total := bare.everyFHostUp(s); up+sshUp+total != 0 {
		t.Errorf("everyFHostUp with no inventory = (%d, %d, %d), want nothing counted", up, sshUp, total)
	}
	if !bare.rackBusy(s).Busy() || !bare.acBusy(s).Busy() {
		t.Error("rackBusy/acBusy with no inventory read the rack as cold; they must fail safe")
	}

	configured := &Surface{Inv: inventory.Default()}
	if configured.rackBusy(s).Busy() || configured.acBusy(s).Busy() {
		t.Error("rackBusy/acBusy with the default inventory call a silent f0 busy")
	}
	s.Hosts[0].Ping = true
	if !configured.rackBusy(s).Busy() || !configured.acBusy(s).Busy() {
		t.Error("rackBusy/acBusy with the default inventory ignore a running f0")
	}
}
