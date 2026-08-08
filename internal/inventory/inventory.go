// Package inventory holds the f3s host inventory: which machines exist, how to
// reach them, and how to wake them.
//
// These values are compiled in so that f3sctl works with no configuration at
// all (as its bash predecessor wol-f3s did). Everything here can be overridden
// at runtime from /usr/local/etc/f3sctl.json — see package config.
package inventory

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
	// ShellyIP is the Shelly Plug M Gen 3 that powers the rack fans.
	ShellyIP string `json:"shelly_ip"`
	// GogiosMuteFile is the marker Gogios checks (OnlyIfNotExists) to stay
	// quiet while the cluster is deliberately down. It lives on both
	// gateways.
	GogiosMuteFile string `json:"gogios_mute_file"`
}

// Default returns the compiled-in inventory.
//
// MAC addresses were read off the hosts with `ifconfig re0 | grep ether`; if a
// Beelink's board is ever replaced, this is the one place to update.
func Default() Inventory {
	return Inventory{
		Broadcast:      "192.168.1.255",
		ShellyIP:       "192.168.1.28",
		GogiosMuteFile: "/tmp/f3s_taken_down",
		Hosts: []Host{
			{Name: "f0", Role: RoleF, IP: "192.168.1.130", MAC: "e8:ff:1e:d7:1c:ac", SSHPort: 22, SSHUser: "f3sctl"},
			{Name: "f1", Role: RoleF, IP: "192.168.1.131", MAC: "e8:ff:1e:d7:1e:44", SSHPort: 22, SSHUser: "f3sctl"},
			{Name: "f2", Role: RoleF, IP: "192.168.1.132", MAC: "e8:ff:1e:d7:1c:a0", SSHPort: 22, SSHUser: "f3sctl"},
			{Name: "f3", Role: RoleF, IP: "192.168.1.133", MAC: "e8:ff:1e:d7:f3:d7", SSHPort: 22, SSHUser: "f3sctl"},

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
// three k3s bhyve hosts. f3 is deliberately excluded — it runs a standalone
// Rocky VM, is not part of the cluster, and is addressed explicitly.
func (inv Inventory) PowerGroup() []Host {
	var out []Host
	for _, h := range inv.ByRole(RoleF) {
		if h.Name != "f3" {
			out = append(out, h)
		}
	}
	return out
}
