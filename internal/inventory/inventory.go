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
	"regexp"
	"slices"
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

// StorageRole is a host's part in the CARP storage pair that holds the
// storage VIP (f3s-storage-ha, 192.168.1.138) and serves NFS from it. Most
// hosts have none. See ShutdownOrder and CARPMembers for what it decides.
type StorageRole string

const (
	// StorageMaster normally holds the storage VIP. A shutdown powers it off
	// last, after every host whose guests mount their PVs from it.
	StorageMaster StorageRole = "master"
	// StorageBackup is the other half of the pair: it takes the VIP, and with
	// it the NFS export, when the master stops advertising.
	StorageBackup StorageRole = "backup"
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
	// Storage is this host's role in the CARP storage pair, or empty for a
	// host outside it. Only RoleF hosts may carry one; a valid host list has
	// exactly one StorageMaster and at most one StorageBackup (see
	// validateStorageRoles).
	Storage StorageRole `json:"storage,omitempty"`
}

// IsStorageMaster reports whether h normally holds the CARP storage VIP.
func (h Host) IsStorageMaster() bool { return h.Storage == StorageMaster }

// InCARPPair reports whether h is either half of the CARP storage pair.
func (h Host) InCARPPair() bool { return h.Storage != "" }

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
//
// The storage roles are the one exception to "no defaults": a list in which no
// host says "storage" at all inherits them from Default() by name (see
// inheritStorageRoles), which is exactly how a config written before the key
// existed was interpreted. That fallback cannot fail open, because validation
// still demands a master: a renamed rack without the key is rejected rather
// than shut down in the wrong order.
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
	inheritStorageRoles(hosts)
	if err := validateHosts(hosts); err != nil {
		return nil, err
	}
	return hosts, nil
}

// validateHosts rejects a host list f3sctl cannot act on sensibly: empty, with
// an invalid host (see validateHost), with a name used twice (every lookup is
// by name), or without a single f-host in the power group -- a bare
// `power on|off` would then have nothing to act on, and the fan guard nothing
// to judge.
func validateHosts(hosts []Host) error {
	if len(hosts) == 0 {
		return fmt.Errorf("%w: hosts is empty", ErrInvalid)
	}
	seen := make(map[string]bool, len(hosts))
	for i, h := range hosts {
		if err := validateHost(h); err != nil {
			return fmt.Errorf("%w: hosts[%d] (%s): %w", ErrInvalid, i, h.Name, err)
		}
		if seen[h.Name] {
			return fmt.Errorf("%w: hosts[%d]: duplicate name %q", ErrInvalid, i, h.Name)
		}
		seen[h.Name] = true
	}
	if len(Inventory{Hosts: hosts}.PowerGroup()) == 0 {
		return fmt.Errorf(`%w: hosts has no role "f" host with "standalone": false, `+
			"so the power group would be empty", ErrInvalid)
	}
	return validateStorageRoles(hosts)
}

// validateStorageRoles demands exactly one storage master and at most one
// backup. Master and backup are necessarily different hosts, since a host
// has one role; that each is an f-host is validateHost's check.
//
// A missing master is an error rather than "no ordering": the master-last
// shutdown and the CARP quiesce both key on it, and a rack without one would
// power the VIP holder off in the middle of the batch -- the 2026-08-08 wedge
// on ShutdownOrder -- without any indication that the rule had been lost.
func validateStorageRoles(hosts []Host) error {
	holder := map[StorageRole]int{} // role -> index of the host holding it
	for i, h := range hosts {
		if h.Storage == "" {
			continue
		}
		if j, taken := holder[h.Storage]; taken {
			return fmt.Errorf(`%w: hosts[%d] (%s): "storage": %q is already held by hosts[%d] (%s); `+
				"the CARP pair has one master and at most one backup",
				ErrInvalid, i, h.Name, h.Storage, j, hosts[j].Name)
		}
		holder[h.Storage] = i
	}
	if _, ok := holder[StorageMaster]; !ok {
		return fmt.Errorf(`%w: no host has "storage": %q; name the f-host that holds the CARP storage VIP, `+
			"so a shutdown can power it off last", ErrInvalid, StorageMaster)
	}
	return nil
}

// inheritStorageRoles gives a configured host list that assigns no storage
// role at all the roles Default() assigns, by host name, to f-hosts of the
// same name. A list that assigns any role is left alone: it has said who the
// pair is, and completing it from the defaults would be guessing.
func inheritStorageRoles(hosts []Host) {
	if slices.ContainsFunc(hosts, Host.InCARPPair) {
		return
	}
	defaults := Default()
	for i, h := range hosts {
		if d, ok := defaults.ByName(h.Name); ok && h.Role == RoleF {
			hosts[i].Storage = d.Storage
		}
	}
}

// hostNamePattern is what a host name must look like: a simple lower-case
// token. Names become URL path segments (/power/<name>/on), action names
// (<name>-on), CLI words and job argv entries, so anything with slashes,
// spaces or upper case would break one of those rather than fail here.
var hostNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// reservedHostNames are the words a host must not be called: each is already
// a CLI word or route segment (`power all on`, `/power/all/cycle`, `power
// status`), or would make a host's <name>-on/<name>-off action collide with
// a plug's (fans-off, ac-off), so a host by that name would shadow it or be
// shadowed by it. httpapi.NewRouter refuses duplicate action names as the
// structural backstop.
var reservedHostNames = []string{"all", "on", "off", "status", "cycle", "power", "fans", "ac"}

// validateHost rejects a host that would silently fall out of every group or
// collide with the command and route vocabulary: no name, a name that is not
// a simple token or is a reserved word, or a role f3sctl does not know (a
// typo such as "F" matches neither RoleF nor anything else). The standalone
// flag and a storage role are refused on a non-f host, where they mean
// nothing, and a storage role must be one f3sctl knows.
func validateHost(h Host) error {
	if err := ValidateHostName(h.Name); err != nil {
		return err
	}
	switch h.Storage {
	case "", StorageMaster, StorageBackup:
	default:
		return fmt.Errorf(`unknown "storage" %q (want %q or %q, or leave it out)`,
			h.Storage, StorageMaster, StorageBackup)
	}
	switch h.Role {
	case RoleF:
		return nil
	case RoleCluster, RoleGateway:
	default:
		return fmt.Errorf(`unknown role %q (want %q, %q or %q)`, h.Role, RoleF, RoleCluster, RoleGateway)
	}
	if h.Standalone {
		return fmt.Errorf(`"standalone" is only meaningful for role "f", not %q`, h.Role)
	}
	if h.Storage != "" {
		return fmt.Errorf(`"storage" is only meaningful for role "f", not %q`, h.Role)
	}
	return nil
}

// ValidateHostName reports whether name may be a host's name: a simple token
// (hostNamePattern) that is not a reserved word (reservedHostNames). It is
// exported so the API's tests can check the reserved words against the
// routes they protect.
func ValidateHostName(name string) error {
	switch {
	case name == "":
		return errors.New("empty name")
	case !hostNamePattern.MatchString(name):
		return fmt.Errorf("name %q is not a simple token (want %s)", name, hostNamePattern)
	case slices.Contains(reservedHostNames, name):
		return fmt.Errorf("name %q is reserved: it is already a command or route word", name)
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
			{Name: "f0", Role: RoleF, IP: "192.168.1.130", MAC: "e8:ff:1e:d7:1c:ac", SSHPort: 22, SSHUser: "f3sctl",
				Storage: StorageMaster},
			{Name: "f1", Role: RoleF, IP: "192.168.1.131", MAC: "e8:ff:1e:d7:1e:44", SSHPort: 22, SSHUser: "f3sctl",
				Storage: StorageBackup},
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

// CARPMembers returns the hosts in the CARP pair (Host.Storage set) that are
// present in hosts, in the order given.
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
		if h.InCARPPair() {
			out = append(out, h)
		}
	}
	return out
}

// ShutdownOrder returns the power group ordered so the CARP storage master
// (the host with Storage == StorageMaster, f0 by default) is powered off LAST.
//
// Order matters, and getting it wrong wedges a host. Powering the master off
// first fails the storage VIP over to the backup, whose carpcontrol.sh
// promptly starts rpcbind/mountd/nfsd/nfsuserd and restarts stunnel — and
// then, seconds later, the backup is itself told to shut down. It goes down
// as a freshly-started NFS server with clients still able to reach it, and
// hangs in the final phase: powered on, off the network, and unwakeable by
// Wake-on-LAN.
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
	rest, master := SplitStorageMaster(hosts)
	return append(rest, master...)
}

// SplitStorageMaster partitions hosts into the storage master and the rest,
// preserving order. master holds the one host with Storage == StorageMaster,
// or none when hosts does not include it (a `power f3 off`).
//
// It is exported for the shutdown engine, which runs the two halves as
// separate waves: the rest (in parallel when it can), then the master alone.
func SplitStorageMaster(hosts []Host) (rest, master []Host) {
	for _, h := range hosts {
		if h.IsStorageMaster() {
			master = append(master, h)
			continue
		}
		rest = append(rest, h)
	}
	return rest, master
}
