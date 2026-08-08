package power

import (
	"context"

	"github.com/snonux/f3sctl/internal/inventory"
)

// ProbeAll returns the status of every host worth reporting: the f-hosts that
// can be powered, and the k3s nodes that indicate whether the cluster is
// actually usable.
//
// The gateways are excluded: they are infrastructure f3sctl talks *through*,
// they are not part of what it powers, and if they were down this call could
// not have been served in the first place.
func (e *Engine) ProbeAll(ctx context.Context) []HostStatus {
	var hosts []inventory.Host
	hosts = append(hosts, e.cfg.Inventory.ByRole(inventory.RoleF)...)
	hosts = append(hosts, e.cfg.Inventory.ByRole(inventory.RoleCluster)...)
	return e.Probe(ctx, hosts)
}

// LiveHosts returns the names of the f-hosts currently answering ICMP.
//
// ICMP rather than SSH on purpose: this answers "is anything drawing power in
// that rack", which is what the fan guard needs to know. A host mid-boot has
// no sshd yet but is very much running and generating heat.
func (e *Engine) LiveHosts(ctx context.Context) []string {
	var up []string
	for _, st := range e.Probe(ctx, e.cfg.Inventory.ByRole(inventory.RoleF)) {
		if st.Ping {
			up = append(up, st.Name)
		}
	}
	return up
}
