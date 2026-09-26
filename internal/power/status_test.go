package power

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/powertest"
)

// groupsInventory is an inventory whose standalone host is not called f3, so
// a test passing against it proves the groups come from the Standalone flag
// rather than from a host name.
func groupsInventory() inventory.Inventory {
	return inventory.Inventory{Hosts: []inventory.Host{
		{Name: "f0", Role: inventory.RoleF},
		{Name: "f3", Role: inventory.RoleF}, // not flagged: a cluster host here
		{Name: "f9", Role: inventory.RoleF, Standalone: true},
		{Name: "r0", Role: inventory.RoleCluster},
	}}
}

func upStatus(name string, role inventory.Role) HostStatus {
	return HostStatus{Name: name, Role: string(role), Ping: true, PingKnown: true}
}

func statusNames(statuses []HostStatus) []string {
	var out []string
	for _, st := range statuses {
		out = append(out, st.Name)
	}
	return out
}

// TestSnapshotSelectorsFollowTheInventoryGroups pins which statuses each
// selector keeps: the configured groups' hosts, in snapshot order. A Role-f
// status for a host the inventory does not know, and the k3s guests, are
// dropped by both.
func TestSnapshotSelectorsFollowTheInventoryGroups(t *testing.T) {
	inv := groupsInventory()
	snapshot := []HostStatus{
		upStatus("f9", inventory.RoleF),
		upStatus("stray", inventory.RoleF),
		upStatus("f3", inventory.RoleF),
		upStatus("r0", inventory.RoleCluster),
		upStatus("f0", inventory.RoleF),
	}

	if got := statusNames(PowerGroupStatuses(inv, snapshot)); !reflect.DeepEqual(got, []string{"f3", "f0"}) {
		t.Errorf("PowerGroupStatuses = %v, want [f3 f0]", got)
	}
	if got := statusNames(EveryFHostStatuses(inv, snapshot)); !reflect.DeepEqual(got, []string{"f9", "f3", "f0"}) {
		t.Errorf("EveryFHostStatuses = %v, want [f9 f3 f0]", got)
	}
	if got := PowerGroupStatuses(inventory.Inventory{}, snapshot); len(got) != 0 {
		t.Errorf("PowerGroupStatuses with an empty inventory = %v, want nothing", statusNames(got))
	}
}

// TestActivityFromJudgesTheInventorysGroups pins that the snapshot guards read
// their host set from the configured inventory -- the same set
// Engine.RackActivity and Engine.ACActivity probe -- rather than from a
// role/name rule of their own: the flagged standalone host is outside the fan
// guard but inside the AC guard, whatever it is called.
func TestActivityFromJudgesTheInventorysGroups(t *testing.T) {
	inv := groupsInventory()
	snapshot := []HostStatus{
		{Name: "f0", Role: string(inventory.RoleF), PingKnown: true},
		upStatus("f3", inventory.RoleF),
		upStatus("f9", inventory.RoleF),
		upStatus("stray", inventory.RoleF),
		upStatus("r0", inventory.RoleCluster),
	}

	if got := RackActivityFrom(inv, snapshot).Hosts(); !reflect.DeepEqual(got, []string{"f3"}) {
		t.Errorf("RackActivityFrom hosts = %v, want [f3]", got)
	}
	if got := ACActivityFrom(inv, snapshot).Hosts(); !reflect.DeepEqual(got, []string{"f3", "f9"}) {
		t.Errorf("ACActivityFrom hosts = %v, want [f3 f9]", got)
	}
}

// TestAnEmptyGroupFailsSafe pins that a guard with no hosts to judge reads the
// rack as busy, not cold -- in the snapshot half and the probing half alike,
// and through Hosts() as well as Busy(), because the CLI's fan and AC guards
// and the shutdown's fans-off step read Hosts(). All f-hosts standalone leaves
// the fan guard's group empty while the AC guard still has hosts to judge.
func TestAnEmptyGroupFailsSafe(t *testing.T) {
	allStandalone := inventory.Inventory{Hosts: []inventory.Host{
		{Name: "f0", Role: inventory.RoleF, Standalone: true},
	}}
	silent := []HostStatus{{Name: "f0", Role: string(inventory.RoleF), PingKnown: true}}

	for _, tc := range []struct {
		name string
		got  RackActivity
	}{
		{"RackActivityFrom/empty inventory", RackActivityFrom(inventory.Inventory{}, silent)},
		{"ACActivityFrom/empty inventory", ACActivityFrom(inventory.Inventory{}, silent)},
		{"RackActivityFrom/all standalone", RackActivityFrom(allStandalone, silent)},
		{"Engine.RackActivity/empty inventory", emptyInventoryEngine(t).RackActivity(context.Background())},
		{"Engine.ACActivity/empty inventory", emptyInventoryEngine(t).ACActivity(context.Background())},
	} {
		if !tc.got.Busy() || len(tc.got.Hosts()) == 0 {
			t.Errorf("%s: Busy = %v, Hosts = %v; an empty group must read as busy", tc.name, tc.got.Busy(), tc.got.Hosts())
		}
		if !strings.Contains(tc.got.Why(), "no hosts") {
			t.Errorf("%s: Why = %q, want it to say no hosts are configured", tc.name, tc.got.Why())
		}
	}

	if ACActivityFrom(allStandalone, silent).Busy() {
		t.Error("ACActivityFrom/all standalone is busy; f0 is in its group and silent")
	}
}

// emptyInventoryEngine is a test engine whose inventory has no hosts. Its
// probe must never run: there is nothing to probe.
func emptyInventoryEngine(t *testing.T) *Engine {
	t.Helper()
	eng := testEngine(t, powertest.NewFakeShelly(t, true))
	eng.cfg.Inventory.Hosts = nil
	eng.isUp = func(context.Context, string) (bool, bool) {
		t.Error("probed a host although the inventory has none")
		return false, true
	}
	return eng
}

// TestSnapshotSelectorsMatchWhatTheEngineTouches is the cross-check behind
// judging API availability with PowerGroupStatuses / EveryFHostStatuses: each
// selector must keep exactly the hosts the matching engine operation actually
// wakes or powers off. The standalone flag is moved from f3 to f2 first, so
// the check also proves both sides follow the flag rather than a name.
func TestSnapshotSelectorsMatchWhatTheEngineTouches(t *testing.T) {
	for _, tc := range []struct {
		name   string
		act    func(*Engine, context.Context, io.Writer) error
		sel    func(inventory.Inventory, []HostStatus) []HostStatus
		wakes  bool
		wantF2 bool
	}{
		{"On", (*Engine).On, PowerGroupStatuses, true, false},
		{"OnAll", (*Engine).OnAll, EveryFHostStatuses, true, true},
		{"Off", (*Engine).Off, PowerGroupStatuses, false, false},
		{"OffAll", (*Engine).OffAll, EveryFHostStatuses, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newOffTestRig(t, "f0", "f1", "f2", "f3")
			eng := rig.eng
			for i := range eng.cfg.Inventory.Hosts {
				h := &eng.cfg.Inventory.Hosts[i]
				h.Standalone = h.Name == "f2"
			}
			eng.fans = &fakeFans{}
			verb := &fakeGatewayVerb{out: map[string]string{}, err: map[string]error{}}
			eng.monitor = newTestMonitor(t, verb, (&downForProbes{}).probe, nil, []string{"r0"}, time.Minute)

			if err := tc.act(eng, context.Background(), &bytes.Buffer{}); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}

			touched := rig.power.calls()
			if tc.wakes {
				touched = rig.power.wakeCalls()
			}
			slices.Sort(touched)
			touched = slices.Compact(touched) // a wake may re-send packets

			var snapshot []HostStatus
			for _, h := range eng.cfg.Inventory.Hosts {
				snapshot = append(snapshot, HostStatus{Name: h.Name, Role: string(h.Role)})
			}
			selected := statusNames(tc.sel(eng.cfg.Inventory, snapshot))
			slices.Sort(selected)

			if !reflect.DeepEqual(touched, selected) {
				t.Errorf("engine touched %v, but the selector judges %v", touched, selected)
			}
			if got := slices.Contains(touched, "f2"); got != tc.wantF2 {
				t.Errorf("standalone f2 touched = %v, want %v", got, tc.wantF2)
			}
		})
	}
}
