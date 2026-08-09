package power

import (
	"context"
	"strings"
	"sync"
	"time"

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

// LiveHosts returns the names of the f-hosts that may still be drawing power,
// in inventory order.
//
// "May still be" rather than "are": a host whose liveness could not be
// determined is included, because both consumers are cooling guards and an
// unprobeable host is not a host that is known to be off. See rackStillBusy.
//
// ICMP rather than SSH on purpose: this answers "is anything drawing power in
// that rack", which is what the fan guards need to know. A host mid-boot has
// no sshd yet but is very much running and generating heat.
//
// Both fan guards consult this and no other source -- the CLI's `fans off`
// refusal (cli.fansOff) and the fans-off step of a shutdown
// (Engine.fansOffOnceTheRackIsIdle, through rackStillBusy) -- so the everyday
// command and the explicit one cannot disagree about when the rack is cold
// enough to cut cooling.
func (e *Engine) LiveHosts(ctx context.Context) []string {
	return e.rackStillBusy(ctx).names()
}

// rackActivity is what the fan guards decide on: which f-hosts may still be
// drawing power, and why they might be.
type rackActivity struct {
	// running is every host that may still be drawing power, in inventory
	// order. It is the union of up and unknown; kept separately because the
	// order is what the operator sees and the two sub-lists are not
	// interleaved in it.
	running []string
	// up answered ICMP: known to be running.
	up []string
	// unknown could not be probed at all -- no ping(8), a probe that could not
	// be started, a cancelled context. Counted as running, because the guard
	// has no evidence that they are not, and a probe that cannot run must not
	// read as a cold rack.
	unknown []string
}

func (a rackActivity) any() bool       { return len(a.running) > 0 }
func (a rackActivity) names() []string { return a.running }

// why explains what is keeping the fans on, distinguishing hosts that answered
// from hosts nothing could be learned about. The difference matters to whoever
// reads it: the first is the rack working as intended, the second means this
// machine cannot probe the rack and every other liveness answer it gives is
// equally worthless.
func (a rackActivity) why() string {
	var parts []string
	if len(a.up) > 0 {
		parts = append(parts, strings.Join(a.up, ", ")+" still running")
	}
	if len(a.unknown) > 0 {
		parts = append(parts, strings.Join(a.unknown, ", ")+
			" could not be probed, so assumed running")
	}
	return strings.Join(parts, "; ")
}

// rackStillBusy probes every f-host and reports which of them may still be
// drawing power.
//
// It pings directly instead of going through Probe because Probe also dials
// port 22 on every host, and a host that is off makes that dial wait out the
// full timeout for an answer nothing here looks at.
//
// Concurrently, because a host that is off costs several ProbeTimeouts (see
// confirmLiveness) and the caller is usually a person waiting at a terminal or
// a shutdown holding the rack in an undefined state.
func (e *Engine) rackStillBusy(ctx context.Context) rackActivity {
	hosts := e.cfg.Inventory.ByRole(inventory.RoleF)

	state := make([]hostLiveness, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, ip string) {
			defer wg.Done()
			state[i] = e.confirmLiveness(ctx, ip)
		}(i, h.IP)
	}
	wg.Wait()

	var a rackActivity
	for i, h := range hosts {
		switch state[i] {
		case livenessUp:
			a.up = append(a.up, h.Name)
			a.running = append(a.running, h.Name)
		case livenessUnknown:
			a.unknown = append(a.unknown, h.Name)
			a.running = append(a.running, h.Name)
		}
	}
	return a
}

// hostLiveness is one host's probed state, with "could not tell" kept apart
// from "silent" -- the distinction the fan guards turn on.
type hostLiveness int

const (
	livenessDown hostLiveness = iota
	livenessUp
	livenessUnknown
)

// confirmLiveness probes one host until it has an answer worth acting on.
//
// Up or unknown ends it immediately; down only counts after
// confirmedDownProbes consecutive silences, the same evidence awaitPowerDown
// demands before it records a host as safely powered off. It has to be at
// least that strict, because it decides the more dangerous question. One
// missed ping is not proof of a power-down -- the interface goes DOWN and back
// UP again during a normal shutdown, which is exactly the moment this runs (see
// awaitPowerDown for the observed f0/f1 logs) -- and on a single probe one
// dropped echo reply would have been enough to cut cooling to a running host.
//
// The cost is real: an idle rack takes confirmedDownProbes probes per host,
// spaced by probeGap, before `fans off` or a shutdown's last step will accept
// that it is idle. The hosts are probed in parallel and a busy rack answers on
// the first round, so what this actually slows down is switching the fans off
// -- the one operation worth being sure about.
func (e *Engine) confirmLiveness(ctx context.Context, ip string) hostLiveness {
	for misses := 0; ; {
		up, known := e.liveness()(ctx, ip)
		switch {
		case up:
			return livenessUp
		case !known:
			return livenessUnknown
		}

		if misses++; misses >= confirmedDownProbes {
			return livenessDown
		}

		select {
		case <-ctx.Done():
			// Cancelled mid-way: fewer misses than required, so nothing has
			// been established. Say so rather than rounding down to "off".
			return livenessUnknown
		case <-time.After(e.probeGap()):
		}
	}
}
