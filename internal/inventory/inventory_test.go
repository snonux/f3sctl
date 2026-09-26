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
		{"name":"a0","role":"f","mac":"00:00:00:00:00:01","standalone":false},
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

func names(hosts []Host) []string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, h.Name)
	}
	return out
}
