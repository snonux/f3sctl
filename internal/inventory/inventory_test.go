package inventory

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestShutdownOrderPutsStorageMasterLast pins the ordering rule that keeps the
// CARP storage VIP from failing over onto a host that is about to be powered
// off. Getting this wrong does not fail loudly -- it wedges a host that then
// cannot be woken remotely -- so it is worth an explicit test.
func TestShutdownOrderPutsStorageMasterLast(t *testing.T) {
	order := Default().ShutdownOrder()

	if len(order) == 0 {
		t.Fatal("shutdown order is empty")
	}
	if last := order[len(order)-1]; !last.IsStorageMaster() || last.Name != "f0" {
		t.Errorf("last host to be shut down is %q, want the storage master f0", last.Name)
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
	if last := order[len(order)-1]; !last.IsStorageMaster() {
		t.Errorf("ShutdownOrderAll ends with %s, want the storage master", last.Name)
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

// TestPowerGroupIsDrivenByTheStandaloneFlag proves the cluster/standalone
// split is data, not a host name: a differently named host flagged Standalone
// is the one left out, and a host called f3 without the flag IS in the power
// group. The flag on a non-f host changes nothing.
func TestPowerGroupIsDrivenByTheStandaloneFlag(t *testing.T) {
	inv := Inventory{Hosts: []Host{
		{Name: "f0", Role: RoleF},
		{Name: "f3", Role: RoleF},
		{Name: "f9", Role: RoleF, Standalone: true},
		{Name: "r0", Role: RoleCluster, Standalone: true},
	}}

	if got := names(inv.PowerGroup()); !slices.Equal(got, []string{"f0", "f3"}) {
		t.Errorf("PowerGroup = %v, want [f0 f3]: f3 is unflagged here, f9 is standalone", got)
	}
	if got := names(inv.EveryFHost()); !slices.Equal(got, []string{"f0", "f3", "f9"}) {
		t.Errorf("EveryFHost = %v, want [f0 f3 f9]", got)
	}
}

// TestOnlyF3IsStandaloneByDefault pins the compiled-in data the power group
// is derived from.
func TestOnlyF3IsStandaloneByDefault(t *testing.T) {
	for _, h := range Default().Hosts {
		if h.Standalone != (h.Name == "f3") {
			t.Errorf("%s: Standalone = %v, want %v", h.Name, h.Standalone, h.Name == "f3")
		}
	}
}

// TestUnmarshalReplacesTheHostListWholesale is the regression test for
// encoding/json decoding an array into the slice's existing elements: a
// configured host list must not inherit, by index, the Standalone flag or MAC
// of the compiled-in host that used to sit at the same position.
func TestUnmarshalReplacesTheHostListWholesale(t *testing.T) {
	inv := Default()
	// Four hosts, so index 3 lands on the default f3 (Standalone, with a MAC).
	raw := `{"hosts":[
		{"name":"a0","role":"f","mac":"00:00:00:00:00:01","standalone":false,"storage":"master"},
		{"name":"a1","role":"f","mac":"00:00:00:00:00:02","standalone":false},
		{"name":"a2","role":"f","mac":"00:00:00:00:00:03","standalone":true},
		{"name":"a3","role":"cluster"}]}`
	if err := json.Unmarshal([]byte(raw), &inv); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	a3, ok := inv.ByName("a3")
	if !ok {
		t.Fatal("a3 missing after Unmarshal")
	}
	if a3.Standalone || a3.MAC != "" {
		t.Errorf("a3 = %+v, inherited the default f3's Standalone/MAC", a3)
	}
	if a1, _ := inv.ByName("a1"); a1.Storage != "" {
		t.Errorf("a1.Storage = %q, inherited the default f1's storage role", a1.Storage)
	}
	if got := names(inv.PowerGroup()); !slices.Equal(got, []string{"a0", "a1"}) {
		t.Errorf("PowerGroup = %v, want [a0 a1]", got)
	}
	if inv.Broadcast != Default().Broadcast {
		t.Errorf("Broadcast = %q, want the default kept (absent key)", inv.Broadcast)
	}
}

// TestUnmarshalKeepsTheHostsWhenAbsentOrNull pins the overlay cases that must
// leave the compiled-in hosts alone: an overlay that does not mention them,
// an explicit null, and a JSON null for the whole inventory.
func TestUnmarshalKeepsTheHostsWhenAbsentOrNull(t *testing.T) {
	for _, tc := range []struct {
		name, raw, broadcast string
	}{
		{"absent", `{"broadcast":"10.0.0.255"}`, "10.0.0.255"},
		{"hosts null", `{"hosts":null,"broadcast":"10.0.0.255"}`, "10.0.0.255"},
		{"inventory null", `null`, Default().Broadcast},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := Default()
			if err := json.Unmarshal([]byte(tc.raw), &inv); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if inv.Broadcast != tc.broadcast {
				t.Errorf("Broadcast = %q, want %q", inv.Broadcast, tc.broadcast)
			}
			if !reflect.DeepEqual(inv.Hosts, Default().Hosts) {
				t.Error("hosts changed although the overlay did not replace them")
			}
		})
	}
}

// TestUnmarshalRoundTripsTheDefault pins that what json.Marshal writes -- the
// standalone key included on every host -- is accepted back unchanged, so a
// dumped config is a valid config.
func TestUnmarshalRoundTripsTheDefault(t *testing.T) {
	raw, err := json.Marshal(Default())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if n := strings.Count(string(raw), `"standalone":`); n != len(Default().Hosts) {
		t.Errorf("marshalled %d standalone keys for %d hosts; the key must always be written", n, len(Default().Hosts))
	}
	var back Inventory
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal round trip: %v", err)
	}
	if !reflect.DeepEqual(back, Default()) {
		t.Errorf("round trip lost data: got %+v", back)
	}
}

// TestUnmarshalRejectsInvalidHostLists pins the fail-closed validation of a
// configured host list. Every rejection leaves the inventory untouched.
func TestUnmarshalRejectsInvalidHostLists(t *testing.T) {
	const f = `{"name":"f0","role":"f","standalone":false}`
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"standalone missing on an f-host",
			`{"hosts":[` + f + `,{"name":"f1","role":"f","standalone":false},
				{"name":"f2","role":"f","standalone":false},{"name":"f3","role":"f"}]}`,
			`hosts[3] (f3): role "f" needs an explicit "standalone"`},
		{"empty list", `{"hosts":[]}`, "hosts is empty"},
		{"no f-host", `{"hosts":[{"name":"r0","role":"cluster"}]}`, `no role "f" host`},
		{"duplicate name", `{"hosts":[` + f + `,{"name":"f0","role":"cluster"}]}`, `duplicate name "f0"`},
		{"standalone on a non-f host",
			`{"hosts":[` + f + `,{"name":"r0","role":"cluster","standalone":true}]}`,
			`hosts[1] (r0): "standalone" is only meaningful for role "f"`},
		{"role with wrong case", `{"hosts":[` + f + `,{"name":"f1","role":"F","standalone":false}]}`,
			`hosts[1] (f1): unknown role "F"`},
		{"unknown role", `{"hosts":[` + f + `,{"name":"f1","role":"fhost"}]}`,
			`hosts[1] (f1): unknown role "fhost"`},
		{"empty role", `{"hosts":[` + f + `,{"name":"x"}]}`, `hosts[1] (x): unknown role ""`},
		{"empty name", `{"hosts":[` + f + `,{"name":"","role":"cluster"}]}`, `hosts[1] (): empty name`},
		{"upper-case name", `{"hosts":[` + f + `,{"name":"F1","role":"cluster"}]}`,
			`hosts[1] (F1): name "F1" is not a simple token`},
		{"name with a slash", `{"hosts":[` + f + `,{"name":"f1/on","role":"cluster"}]}`,
			`name "f1/on" is not a simple token`},
		{"name with a space", `{"hosts":[` + f + `,{"name":"f 1","role":"cluster"}]}`,
			`name "f 1" is not a simple token`},
		{"name starting with a dash", `{"hosts":[` + f + `,{"name":"-f1","role":"cluster"}]}`,
			`name "-f1" is not a simple token`},
		{"every f-host standalone",
			`{"hosts":[{"name":"f3","role":"f","standalone":true},{"name":"r0","role":"cluster"}]}`,
			"power group would be empty"},
		{"renamed hosts without a storage role",
			`{"hosts":[{"name":"g0","role":"f","standalone":false},{"name":"g1","role":"f","standalone":false}]}`,
			`no host has "storage": "master"`},
		{"only a backup declared",
			`{"hosts":[` + f + `,{"name":"f1","role":"f","standalone":false,"storage":"backup"}]}`,
			`no host has "storage": "master"`},
		{"two masters",
			`{"hosts":[{"name":"f0","role":"f","standalone":false,"storage":"master"},
				{"name":"f1","role":"f","standalone":false,"storage":"master"}]}`,
			`hosts[1] (f1): "storage": "master" is already held by hosts[0] (f0)`},
		{"two backups",
			`{"hosts":[{"name":"f0","role":"f","standalone":false,"storage":"master"},
				{"name":"f1","role":"f","standalone":false,"storage":"backup"},
				{"name":"f2","role":"f","standalone":false,"storage":"backup"}]}`,
			`hosts[2] (f2): "storage": "backup" is already held by hosts[1] (f1)`},
		{"storage role on a cluster host",
			`{"hosts":[` + f + `,{"name":"r0","role":"cluster","storage":"master"}]}`,
			`hosts[1] (r0): "storage" is only meaningful for role "f"`},
		{"storage role on a gateway",
			`{"hosts":[` + f + `,{"name":"gw","role":"gateway","storage":"backup"}]}`,
			`hosts[1] (gw): "storage" is only meaningful for role "f"`},
		{"storage role with wrong case",
			`{"hosts":[{"name":"f0","role":"f","standalone":false,"storage":"Master"}]}`,
			`hosts[0] (f0): unknown "storage" "Master"`},
		{"unknown storage role",
			`{"hosts":[{"name":"f0","role":"f","standalone":false,"storage":"primary"}]}`,
			`hosts[0] (f0): unknown "storage" "primary"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := Default()
			err := json.Unmarshal([]byte(tc.raw), &inv)
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ErrInvalid mentioning %q", err, tc.want)
			}
			if !reflect.DeepEqual(inv, Default()) {
				t.Error("a rejected Unmarshal modified the inventory")
			}
		})
	}
}

// TestReservedHostNamesAreRejected pins every reserved word: each is already a
// CLI word or route segment, or ("fans", "ac") would give a host the action
// names of a plug, so a host by that name would shadow it.
func TestReservedHostNamesAreRejected(t *testing.T) {
	const f = `{"name":"f0","role":"f","standalone":false}`
	for _, name := range []string{"all", "on", "off", "status", "cycle", "power", "fans", "ac"} {
		inv := Default()
		raw := `{"hosts":[` + f + `,{"name":"` + name + `","role":"f","standalone":false}]}`
		err := json.Unmarshal([]byte(raw), &inv)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("host %q: err = %v, want ErrInvalid saying the name is reserved", name, err)
		}
	}
}

// TestDefaultHostNamesAreValid keeps the compiled-in inventory inside the
// rules a configured one is held to.
func TestDefaultHostNamesAreValid(t *testing.T) {
	if err := validateHosts(Default().Hosts); err != nil {
		t.Errorf("Default() fails validation: %v", err)
	}
}

// TestUnmarshalRejectsAMistypedField pins that a type error inside the
// inventory is reported, not swallowed into a zero value, and changes nothing.
func TestUnmarshalRejectsAMistypedField(t *testing.T) {
	inv := Default()
	err := json.Unmarshal([]byte(`{"hosts":[{"name":"f0","role":"f","standalone":"yes"}]}`), &inv)
	var typeErr *json.UnmarshalTypeError
	if !errors.As(err, &typeErr) {
		t.Fatalf("err = %v, want a *json.UnmarshalTypeError", err)
	}
	if !reflect.DeepEqual(inv, Default()) {
		t.Error("a failed Unmarshal modified the inventory")
	}
}

// TestPowerGroupFollowsAConfiguredInventory checks that the groups are derived
// from the hosts actually configured rather than from a fixed list: an added
// f-host joins both, and an inventory without the standalone host has
// identical groups.
func TestPowerGroupFollowsAConfiguredInventory(t *testing.T) {
	inv := Inventory{Hosts: []Host{
		{Name: "f0", Role: RoleF},
		{Name: "f4", Role: RoleF},
		{Name: "r0", Role: RoleCluster},
	}}

	if got := names(inv.PowerGroup()); !slices.Equal(got, []string{"f0", "f4"}) {
		t.Errorf("PowerGroup = %v, want [f0 f4]", got)
	}
	if got := names(inv.EveryFHost()); !slices.Equal(got, []string{"f0", "f4"}) {
		t.Errorf("EveryFHost = %v, want [f0 f4]", got)
	}
}

// TestDefaultStorageRoles pins the compiled-in CARP pair: f0 is the storage
// master, f1 its backup, and no other host has a storage role.
func TestDefaultStorageRoles(t *testing.T) {
	want := map[string]StorageRole{"f0": StorageMaster, "f1": StorageBackup}
	for _, h := range Default().Hosts {
		if h.Storage != want[h.Name] {
			t.Errorf("%s: Storage = %q, want %q", h.Name, h.Storage, want[h.Name])
		}
	}
	if got := names(CARPMembers(Default().Hosts)); !slices.Equal(got, []string{"f0", "f1"}) {
		t.Errorf("CARPMembers = %v, want [f0 f1]", got)
	}
}

// TestStorageOrderingIsDrivenByTheStorageRole proves the master-last rule and
// the CARP pair are data, not host names: in an inventory where g1 is the
// master and g2 its backup, a host called f0 is just another host.
func TestStorageOrderingIsDrivenByTheStorageRole(t *testing.T) {
	inv := Inventory{Hosts: []Host{
		{Name: "f0", Role: RoleF},
		{Name: "g1", Role: RoleF, Storage: StorageMaster},
		{Name: "g2", Role: RoleF, Storage: StorageBackup},
		{Name: "g9", Role: RoleF, Standalone: true},
	}}

	if got := names(inv.ShutdownOrder()); !slices.Equal(got, []string{"f0", "g2", "g1"}) {
		t.Errorf("ShutdownOrder = %v, want [f0 g2 g1]: the configured master g1 last", got)
	}
	if got := names(inv.ShutdownOrderAll()); !slices.Equal(got, []string{"f0", "g2", "g9", "g1"}) {
		t.Errorf("ShutdownOrderAll = %v, want [f0 g2 g9 g1]", got)
	}
	if got := names(CARPMembers(inv.Hosts)); !slices.Equal(got, []string{"g1", "g2"}) {
		t.Errorf("CARPMembers = %v, want [g1 g2]", got)
	}
}

// TestSplitStorageMasterKeepsOrder pins that the split is a partition, not a
// filter: nothing may be dropped, and the rest keeps the order it arrived in.
func TestSplitStorageMasterKeepsOrder(t *testing.T) {
	hosts := []Host{{Name: "f1"}, {Name: "f2"}, {Name: "f0", Storage: StorageMaster}, {Name: "f3"}}

	rest, master := SplitStorageMaster(hosts)

	if got := names(rest); !slices.Equal(got, []string{"f1", "f2", "f3"}) {
		t.Errorf("rest = %v, want [f1 f2 f3]", got)
	}
	if got := names(master); !slices.Equal(got, []string{"f0"}) {
		t.Errorf("master = %v, want exactly the storage master f0", got)
	}
}

// TestSplitStorageMasterWithoutTheMaster pins the `power f3 off` shape: a run
// that does not include the master has no second wave at all.
func TestSplitStorageMasterWithoutTheMaster(t *testing.T) {
	rest, master := SplitStorageMaster([]Host{{Name: "f3"}, {Name: "f1", Storage: StorageBackup}})

	if got := names(rest); !slices.Equal(got, []string{"f3", "f1"}) {
		t.Errorf("rest = %v, want [f3 f1]", got)
	}
	if len(master) != 0 {
		t.Errorf("master = %v, want none", names(master))
	}
}

// TestUnmarshalARenamedRackKeepsTheOrdering is the regression test for the
// storage pair being host names in code: a configured rack whose hosts are
// not called f0/f1 still shuts its storage master down last and quiesces its
// own pair, because it says which hosts they are.
func TestUnmarshalARenamedRackKeepsTheOrdering(t *testing.T) {
	inv := Default()
	raw := `{"hosts":[
		{"name":"a0","role":"f","standalone":false,"storage":"backup"},
		{"name":"a1","role":"f","standalone":false},
		{"name":"a2","role":"f","standalone":false,"storage":"master"},
		{"name":"a3","role":"f","standalone":true}]}`
	if err := json.Unmarshal([]byte(raw), &inv); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got := names(inv.ShutdownOrder()); !slices.Equal(got, []string{"a0", "a1", "a2"}) {
		t.Errorf("ShutdownOrder = %v, want [a0 a1 a2]: the configured master a2 last", got)
	}
	if got := names(CARPMembers(inv.EveryFHost())); !slices.Equal(got, []string{"a0", "a2"}) {
		t.Errorf("CARPMembers = %v, want [a0 a2]", got)
	}
}

// TestUnmarshalInheritsTheDefaultStorageRoles pins backward compatibility: a
// configured list written before "storage" existed, and so naming no role at
// all, keeps f0 as the master and f1 as the backup -- the behaviour it had
// when the pair was hard-coded. A list that names any role inherits nothing.
func TestUnmarshalInheritsTheDefaultStorageRoles(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      map[string]StorageRole
	}{
		{"no role anywhere", `{"hosts":[
			{"name":"f0","role":"f","standalone":false},{"name":"f1","role":"f","standalone":false},
			{"name":"f2","role":"f","standalone":false},{"name":"r0","role":"cluster"}]}`,
			map[string]StorageRole{"f0": StorageMaster, "f1": StorageBackup}},
		{"f1 missing", `{"hosts":[
			{"name":"f0","role":"f","standalone":false},{"name":"f2","role":"f","standalone":false}]}`,
			map[string]StorageRole{"f0": StorageMaster}},
		{"an explicit role", `{"hosts":[
			{"name":"f0","role":"f","standalone":false},
			{"name":"f1","role":"f","standalone":false,"storage":"master"}]}`,
			map[string]StorageRole{"f1": StorageMaster}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := Default()
			if err := json.Unmarshal([]byte(tc.raw), &inv); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			for _, h := range inv.Hosts {
				if h.Storage != tc.want[h.Name] {
					t.Errorf("%s: Storage = %q, want %q", h.Name, h.Storage, tc.want[h.Name])
				}
			}
		})
	}
}

func names(hosts []Host) []string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, h.Name)
	}
	return out
}
