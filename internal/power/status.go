package power

import (
	"context"
	"sync"

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

// LiveHosts returns the names of the f-hosts currently answering ICMP, in
// inventory order.
//
// ICMP rather than SSH on purpose: this answers "is anything drawing power in
// that rack", which is what the fan guards need to know. A host mid-boot has
// no sshd yet but is very much running and generating heat.
//
// Both fan guards consult this and no other source -- the CLI's `fans off`
// refusal (cli.fansOff) and the fans-off step of a shutdown
// (Engine.fansOffOnceTheRackIsIdle) -- so the everyday command and the explicit
// one cannot disagree about when the rack is cold enough to cut cooling.
//
// It pings directly instead of going through Probe because Probe also dials
// port 22 on every host, and a host that is off makes that dial wait out the
// full timeout for an answer nothing here looks at.
func (e *Engine) LiveHosts(ctx context.Context) []string {
	hosts := e.cfg.Inventory.ByRole(inventory.RoleF)

	// Concurrently, because a host that is off costs a full ProbeTimeout and
	// the caller is usually a person waiting at a terminal.
	up := make([]bool, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, ip string) {
			defer wg.Done()
			up[i] = e.isUp(ctx, ip)
		}(i, h.IP)
	}
	wg.Wait()

	var names []string
	for i, h := range hosts {
		if up[i] {
			names = append(names, h.Name)
		}
	}
	return names
}
