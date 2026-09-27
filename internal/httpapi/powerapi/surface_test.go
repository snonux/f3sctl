package powerapi

import (
	"context"
	"maps"
	"slices"
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
	s := WithSnapshot(contract.State{}, Snapshot{
		Hosts: []power.HostStatus{
			{Name: "f0", Role: "f"},
			{Name: "f3", Role: "f", Ping: true},
			{Name: "f9", Role: "f", Ping: true, SSH: true},
			{Name: "stray", Role: "f", Ping: true, SSH: true},
			{Name: "r0", Role: "cluster", Ping: true, SSH: true},
		},
	})

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
	s := WithSnapshot(contract.State{}, Snapshot{
		Hosts: []power.HostStatus{
			{Name: "f0", Role: "f", PingKnown: true},
		},
	})
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
	SnapshotOf(s).Hosts[0].Ping = true
	if !configured.rackBusy(s).Busy() || !configured.acBusy(s).Busy() {
		t.Error("rackBusy/acBusy with the default inventory ignore a running f0")
	}
}

// TestStatusMarksThePowerGroupOnHostEntities pins the powerGroup property of
// /status's host entities: true exactly for the members of the configured
// inventory's power group, so a client can judge power-on/power-off without
// re-deriving the group from host names (docs/client-reference.js relies on
// it). The custom inventory flags f1 standalone and leaves f3 unflagged, so
// the property must follow the flag, not the name.
func TestStatusMarksThePowerGroupOnHostEntities(t *testing.T) {
	custom := inventory.Default()
	for i := range custom.Hosts {
		h := &custom.Hosts[i]
		h.Standalone = h.Name == "f1"
	}
	snapshot := []power.HostStatus{
		{Name: "f0", Role: "f"}, {Name: "f1", Role: "f"}, {Name: "f2", Role: "f"},
		{Name: "f3", Role: "f"}, {Name: "r0", Role: "cluster"}, {Name: "pi0", Role: "f"},
	}

	for _, tc := range []struct {
		name string
		inv  inventory.Inventory
		want map[string]bool
	}{
		{"default", inventory.Default(),
			map[string]bool{"f0": true, "f1": true, "f2": true, "f3": false, "r0": false, "pi0": false}},
		{"f1 standalone", custom,
			map[string]bool{"f0": true, "f1": false, "f2": true, "f3": true, "r0": false, "pi0": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sf := testNew(tc.inv)
			e, _, err := sf.handleStatus(context.Background(), WithSnapshot(contract.State{}, Snapshot{Hosts: snapshot}), contract.Request{})
			if err != nil {
				t.Fatalf("handleStatus: %v", err)
			}
			got := map[string]bool{}
			for _, sub := range e.Entities {
				if !slices.Contains(sub.Class, "host") {
					continue
				}
				member, ok := sub.Properties["powerGroup"].(bool)
				if !ok {
					t.Fatalf("host %v carries no boolean powerGroup property", sub.Properties["name"])
				}
				got[sub.Properties["name"].(string)] = member
			}
			if !maps.Equal(got, tc.want) {
				t.Errorf("powerGroup = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGroupActionTitlesNameTheInventorysHosts pins that the group power
// actions are titled from the served inventory, like the per-host ones: a
// renamed inventory (g0/g1 in the power group, g9 standalone) must not be
// offered "Power off f0/f1/f2", and an empty one says so rather than naming
// nobody.
func TestGroupActionTitlesNameTheInventorysHosts(t *testing.T) {
	renamed := inventory.Inventory{Hosts: []inventory.Host{
		{Name: "g0", Role: inventory.RoleF},
		{Name: "g1", Role: inventory.RoleF},
		{Name: "g9", Role: inventory.RoleF, Standalone: true},
		{Name: "q0", Role: inventory.RoleCluster},
	}}

	for _, tc := range []struct {
		name string
		inv  inventory.Inventory
		want map[string]string
	}{
		{"default", inventory.Default(), map[string]string{
			"power-on":  "Power on f0/f1/f2",
			"power-off": "Power off f0/f1/f2",
			"all-on":    "Power on every f-host (f0/f1/f2/f3)",
			"all-off":   "Power off every f-host (f0/f1/f2/f3)",
			"all-cycle": "Power-cycle every f-host through mains AC (f0/f1/f2/f3)",
		}},
		{"renamed", renamed, map[string]string{
			"power-on":  "Power on g0/g1",
			"power-off": "Power off g0/g1",
			"all-on":    "Power on every f-host (g0/g1/g9)",
			"all-off":   "Power off every f-host (g0/g1/g9)",
			"all-cycle": "Power-cycle every f-host through mains AC (g0/g1/g9)",
		}},
		{"empty", inventory.Inventory{}, map[string]string{
			"power-on": "Power on none configured",
			"all-on":   "Power on every f-host (none configured)",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := map[string]string{}
			for _, r := range testNew(tc.inv).Routes() {
				if _, ok := tc.want[r.Name]; ok {
					got[r.Name] = r.Title
				}
			}
			if !maps.Equal(got, tc.want) {
				t.Errorf("titles = %v, want %v", got, tc.want)
			}
		})
	}
}
