// Package inventory holds the f3s host inventory: which machines exist, how to
// reach them, and how to wake them.
//
// These values are compiled in so that f3sctl works with no configuration at
// all (as its bash predecessor wol-f3s did). Everything here can be overridden
// at runtime from /usr/local/etc/f3sctl.json — see package config.
package inventory

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Role classifies a host by what f3sctl may do with it.
type Role string

const (
	// RoleF marks the FreeBSD bhyve hosts f0-f3: the only machines f3sctl
	// powers on or off.
	RoleF Role = "f"
	// RoleCluster marks the k3s Rocky VMs r0-r2. They are probed for status
	// and used as the readiness signal when un-muting Gogios, but they are
	// never powered directly — they follow their bhyve host.
	RoleCluster Role = "cluster"
	// RoleGateway marks the OpenBSD frontends. f3sctl only reaches them to
	// set and clear the Gogios mute marker.
	RoleGateway Role = "gateway"
)

// Host is one machine f3sctl knows about.
type Host struct {
	Name string `json:"name"`
	Role Role   `json:"role"`
	// IP is the LAN address f3sctl connects and probes on. The WireGuard
	// addresses are deliberately not used: f3sctl runs on pi0/pi1, which sit
	// on the same flat 192.168.1.0/24 as everything it talks to.
	IP string `json:"ip"`
	// MAC is the Wake-on-LAN target. Empty for hosts that cannot be woken.
	MAC string `json:"mac,omitempty"`
	// SSHPort is the port to reach this host's sshd on. The OpenBSD gateways
	// run sshd on 2; everything else uses 22.
	SSHPort int `json:"ssh_port"`
	// SSHUser is the account the restricted f3sctl key authenticates as.
	SSHUser string `json:"ssh_user"`
	// Standalone marks an f-host that is not part of the k3s cluster (f3: it
	// runs a standalone Rocky VM, is racked apart from the others and the fan
	// plug does not cool it). It is left out of PowerGroup -- and so out of a
	// bare `power on|off` and the fan guard -- but stays in EveryFHost.
	// Meaningful only for RoleF hosts, and required on every one of them in a
	// configured host list (see UnmarshalJSON).
	Standalone bool `json:"standalone"`
}

// Wakeable reports whether this host can be started with a magic packet.
func (h Host) Wakeable() bool { return h.MAC != "" }

// Inventory is the full set of machines plus the shared network facts needed
// to reach them.
type Inventory struct {
	Hosts []Host `json:"hosts"`
	// Broadcast is where Wake-on-LAN magic packets are sent. It must be the
	// broadcast address of the LAN the f-hosts are on, and f3sctl must be
	// running on that LAN — a magic packet is not routed.
	Broadcast string `json:"broadcast"`
	// ShellyIP is the Shelly Plug M Gen 3 that powers the rack fans
	// (shelly1). Driven automatically by power on/off and by `f3sctl fans`.
	ShellyIP string `json:"shelly_ip"`
	// ShellyACIP is the Shelly Plug M Gen 3 that switches mains AC for the
	// f-hosts (shelly2: f0–f3 plus their JetKVM switches). Exposed only via
	// `f3sctl ac` / the /ac API — never flipped by power on/off or boot.
	ShellyACIP string `json:"shelly_ac_ip"`
	// GogiosMuteFile is the marker Gogios checks (OnlyIfNotExists) to stay
	// quiet while the cluster is deliberately down. It lives on both
	// gateways.
	GogiosMuteFile string `json:"gogios_mute_file"`
}

// ErrInvalid is wrapped by every error UnmarshalJSON returns for a host list
// that decodes but must not be used.
var ErrInvalid = errors.New("invalid inventory")

// wireHost is Host as a config file spells it. Standalone is a pointer so an
// absent key can be told apart from an explicit false; it shadows the
// embedded Host.Standalone for encoding/json.
type wireHost struct {
	Host
	Standalone *bool `json:"standalone"`
}

// UnmarshalJSON overlays a configured inventory onto inv the way config.Load
// overlays everything else -- absent keys keep their current value -- except
// that a present "hosts" list replaces inv.Hosts wholesale, and is validated.
//
// Wholesale, because encoding/json decodes an array into the slice's existing
// elements: a configured host list would otherwise inherit, by index, every
// field it leaves out from the compiled-in host that used to sit there -- a
// Standalone flag, or a MAC, silently landing on an unrelated host.
//
// "hosts" absent or null keeps the current list; an empty list is an error.
// Nothing is changed unless the whole inventory is accepted.
func (inv *Inventory) UnmarshalJSON(data []byte) error {
	type plain Inventory // no methods, so no recursion
	var wire struct {
		plain
		Hosts *[]wireHost `json:"hosts"` // shadows plain.Hosts
	}
	wire.plain = plain(*inv)
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("decoding inventory: %w", err)
	}
	out := Inventory(wire.plain)
	if wire.Hosts != nil {
		hosts, err := hostsFromWire(*wire.Hosts)
		if err != nil {
			return err
		}
		out.Hosts = hosts
	}
	*inv = out
	return nil
}

// hostsFromWire converts and validates a configured host list.
//
// Every f-host must say whether it is standalone. Defaulting an absent key to
// false would fail open: a config written before the flag existed would
// silently put f3 into the power group, and a bare `power off` would take it
// down with the cluster.
func hostsFromWire(wire []wireHost) ([]Host, error) {
	hosts := make([]Host, 0, len(wire))
	for i, w := range wire {
		h := w.Host
		if h.Role == RoleF && w.Standalone == nil {
			return nil, fmt.Errorf(`%w: hosts[%d] (%s): role "f" needs an explicit "standalone"`,
				ErrInvalid, i, h.Name)
		}
		if w.Standalone != nil {
			h.Standalone = *w.Standalone
		}
		hosts = append(hosts, h)
	}
	if err := validateHosts(hosts); err != nil {
		return nil, err
	}
	return hosts, nil
}

// validateHosts rejects a host list f3sctl cannot act on sensibly: empty, with
// no f-host to power, with a name used twice (every lookup is by name), or
// with the standalone flag on a host it means nothing for.
func validateHosts(hosts []Host) error {
	if len(hosts) == 0 {
		return fmt.Errorf("%w: hosts is empty", ErrInvalid)
	}
	seen := make(map[string]bool, len(hosts))
	fHosts := 0
	for i, h := range hosts {
		if seen[h.Name] {
			return fmt.Errorf("%w: hosts[%d]: duplicate name %q", ErrInvalid, i, h.Name)
		}
		seen[h.Name] = true
		if h.Role == RoleF {
			fHosts++
		} else if h.Standalone {
			return fmt.Errorf(`%w: hosts[%d] (%s): "standalone" is only meaningful for role "f", not %q`,
				ErrInvalid, i, h.Name, h.Role)
		}
	}
	if fHosts == 0 {
		return fmt.Errorf(`%w: hosts has no role "f" host`, ErrInvalid)
	}
	return nil
}

// Default returns the compiled-in inventory.
//
// MAC addresses were read off the hosts with `ifconfig re0 | grep ether`; if a
// Beelink's board is ever replaced, this is the one place to update.
func Default() Inventory {
	return Inventory{
		Broadcast:      "192.168.1.255",
		ShellyIP:       "192.168.1.28",
		ShellyACIP:     "192.168.1.29",
		GogiosMuteFile: "/tmp/f3s_taken_down",
		Hosts: []Host{
			{Name: "f0", Role: RoleF, IP: "192.168.1.130", MAC: "e8:ff:1e:d7:1c:ac", SSHPort: 22, SSHUser: "f3sctl"},
			{Name: "f1", Role: RoleF, IP: "192.168.1.131", MAC: "e8:ff:1e:d7:1e:44", SSHPort: 22, SSHUser: "f3sctl"},
			{Name: "f2", Role: RoleF, IP: "192.168.1.132", MAC: "e8:ff:1e:d7:1c:a0", SSHPort: 22, SSHUser: "f3sctl"},
			{Name: "f3", Role: RoleF, IP: "192.168.1.133", MAC: "e8:ff:1e:d7:f3:d7", SSHPort: 22, SSHUser: "f3sctl", Standalone: true},

			{Name: "r0", Role: RoleCluster, IP: "192.168.1.120", SSHPort: 22, SSHUser: "f3sctl"},
			{Name: "r1", Role: RoleCluster, IP: "192.168.1.121", SSHPort: 22, SSHUser: "f3sctl"},
			{Name: "r2", Role: RoleCluster, IP: "192.168.1.122", SSHPort: 22, SSHUser: "f3sctl"},

			// The gateways are reached over the WireGuard mesh, not by their
			// public names. Two reasons, and the first is not optional:
			// the restricted key is pinned with from="192.168.2.203,..." to
			// the Pis' addresses, and a connection to the public name leaves
			// the house through NAT, arriving with the site's public source
			// address -- which the pin correctly refuses. Going over the mesh
			// also keeps the traffic off the internet entirely.
			// Their sshd listens on port 2, not 22.
			{Name: "blowfish", Role: RoleGateway, IP: "192.168.2.110", SSHPort: 2, SSHUser: "f3sctl"},
			{Name: "fishfinger", Role: RoleGateway, IP: "192.168.2.111", SSHPort: 2, SSHUser: "f3sctl"},
		},
	}
}

// ByRole returns every host with the given role, in inventory order.
func (inv Inventory) ByRole(r Role) []Host {
	var out []Host
	for _, h := range inv.Hosts {
		if h.Role == r {
			out = append(out, h)
		}
	}
	return out
}

// ByName returns the named host and whether it was found.
func (inv Inventory) ByName(name string) (Host, bool) {
	for _, h := range inv.Hosts {
		if h.Name == name {
			return h, true
		}
	}
	return Host{}, false
}

// PowerGroup is the set of hosts a bare `f3sctl power on|off` acts on: the
// k3s bhyve hosts, i.e. every f-host not marked Standalone.
//
// This is the one place the cluster/standalone split is decided. Everything
// else that needs it -- the fan guard, the API's availability predicates --
// asks PowerGroup or EveryFHost rather than naming hosts, so a configured
// inventory changes all of them at once.
func (inv Inventory) PowerGroup() []Host {
	var out []Host
	for _, h := range inv.ByRole(RoleF) {
		if !h.Standalone {
			out = append(out, h)
		}
	}
	return out
}

// EveryFHost is the set `f3sctl power all on|off` acts on: every f-host,
// f3 included.
//
// The distinction from PowerGroup is intent, not capability. A bare
// `power off` means "take the cluster down" and leaves f3 running, because f3
// is standalone and usually wanted independently. `power all off` means "the
// whole rack goes dark", which is the other thing people actually want and
// previously took two commands.
func (inv Inventory) EveryFHost() []Host { return inv.ByRole(RoleF) }

// StorageMaster is the host that normally holds the CARP storage VIP
// (f3s-storage-ha, 192.168.1.138) and serves NFS. f1 is its BACKUP.
const StorageMaster = "f0"

// StorageBackup is the other half of the CARP pair: the host that takes the
// VIP, and with it the NFS export, when the master stops advertising.
const StorageBackup = "f1"

// CARPMembers returns the hosts in the CARP pair that are present in hosts,
// in the order given.
//
// A shutdown needs to know this set for one reason: a CARP transition on a
// host that is on its way down runs carpcontrol.sh there, which starts (or
// stops) rpcbind/mountd/nfsd/nfsuserd and stunnel at the worst possible
// moment -- the 2026-08-08 wedge described on ShutdownOrder. Anything else in
// the rack can be powered off in any order without a daemon reacting to it;
// these two cannot, so they are the hosts Engine.quiesceCARP stops those
// daemons on before a parallel shutdown.
func CARPMembers(hosts []Host) []Host {
	var out []Host
	for _, h := range hosts {
		if h.Name == StorageMaster || h.Name == StorageBackup {
			out = append(out, h)
		}
	}
	return out
}

// ShutdownOrder returns the power group ordered so the CARP storage master is
// powered off LAST.
//
// Order matters, and getting it wrong wedges a host. Powering the master off
// first fails the storage VIP over to f1, whose carpcontrol.sh promptly starts
// rpcbind/mountd/nfsd/nfsuserd and restarts stunnel — and then, seconds later,
// f1 is itself told to shut down. It goes down as a freshly-started NFS server
// with clients still able to reach it, and hangs in the final phase: powered
// on, off the network, and unwakeable by Wake-on-LAN.
//
// Observed on 2026-08-08: f0 powered off at 21:23:22, f1 logged
// "carp: 1@re0: MASTER -> INIT" at 21:23:30 (so it had taken the VIP), was
// shut down at 21:23:33, and never powered off. f2, which is not in the CARP
// pair at all, went down cleanly in the same run. The homelab runbook already
// says to take the storage master last when rebooting; this makes the tool
// obey the same rule.
func (inv Inventory) ShutdownOrder() []Host {
	return storageMasterLast(inv.PowerGroup())
}

// ShutdownOrderAll is ShutdownOrder over every f-host, f3 included.
//
// The same rule applies for the same reason: whatever else is going down, the
// CARP storage master goes last.
func (inv Inventory) ShutdownOrderAll() []Host {
	return storageMasterLast(inv.EveryFHost())
}

// storageMasterLast moves the CARP storage master to the end of the list,
// preserving the order of everything else.
func storageMasterLast(hosts []Host) []Host {
	out := make([]Host, 0, len(hosts))
	var master []Host
	for _, h := range hosts {
		if h.Name == StorageMaster {
			master = append(master, h)
			continue
		}
		out = append(out, h)
	}
	return append(out, master...)
}
