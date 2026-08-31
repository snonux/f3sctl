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

// LiveHosts returns the names of the power-group hosts (f0/f1/f2) that may
// still be drawing power, in inventory order. f3 is never in this list: the
// fan plug does not cool it. See RackActivity.
//
// "May still be" rather than "are": a host whose liveness could not be
// determined is included, because this feeds a cooling guard and an
// unprobeable host is not a host that is known to be off. See RackActivity.
//
// ICMP rather than SSH on purpose: this answers "is anything drawing power in
// that rack", which is what the fan guards need to know. A host mid-boot has
// no sshd yet but is very much running and generating heat.
//
// This is the CLI's view of the guard (cli.fansOff). It is one of three; see
// RackActivity for the rule they share and for what separates them.
func (e *Engine) LiveHosts(ctx context.Context) []string {
	return e.RackActivity(ctx).Hosts()
}

// RackActivity is what the fan guards decide on: which of the power-group
// hosts (f0/f1/f2) may still be drawing power, and why they might be.
//
// f3 is deliberately not part of this: it is racked separately from f0-f2 and
// the Shelly plug does not cool it, so f3's liveness has no bearing on whether
// switching the plug off is safe. This is the same fact inventory.PowerGroup
// already encodes for the wake/shutdown pair; RackActivity applies it to the
// fan guard too. Before this, a bare `power off` (which never touches f3)
// would leave the fans on for good whenever f3 happened to be running for its
// own reasons -- correct if the plug cooled f3, which it does not.
//
// There are three guards, and this type is the single rule behind all of them:
//
//   - `f3sctl fans off` refuses while the rack is busy (cli.fansOff, via
//     Engine.LiveHosts),
//   - the last step of a rack-wide shutdown leaves the plug alone while the
//     rack is busy (Engine.fansOffOnceTheRackIsIdle),
//   - and the HTTP API both advertises its `force` field and enforces it on
//     POST /fans/off (httpapi, via RackActivityFrom and Engine.RackActivity).
//
// They used to be three separate readings of "is anything up", and the third
// one disagreed: it read HostStatus.Ping, which had already thrown away whether
// the probe ran at all, so `f3sctl fans off` and `f3sctl fans off --remote`
// gave opposite answers in exactly the environment the whole guard was written
// for. Sharing the rule is what stops that recurring; the comment that used to
// assert they could not disagree did not.
//
// What still differs is the *evidence*, not the rule. Engine.RackActivity
// probes on demand and wants confirmedDownProbes consecutive silences per host;
// RackActivityFrom folds an already-taken snapshot, one probe per host. The API
// advertises from the cheap one and enforces with the strict one, so it can
// only ever be more cautious than a client expects, never less.
type RackActivity struct {
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

// Busy reports whether anything in the rack may still be drawing power, and so
// whether the fans must stay on.
func (a RackActivity) Busy() bool { return len(a.running) > 0 }

// Hosts names those hosts, in inventory order.
func (a RackActivity) Hosts() []string { return a.running }

// add records one host's probed liveness. Down contributes nothing: the whole
// type is a list of reasons to leave the cooling alone.
func (a *RackActivity) add(name string, l hostLiveness) {
	switch l {
	case livenessUp:
		a.up = append(a.up, name)
		a.running = append(a.running, name)
	case livenessUnknown:
		a.unknown = append(a.unknown, name)
		a.running = append(a.running, name)
	}
}

// RackActivityFrom applies the same rule to probe results that have already
// been gathered, rather than probing again.
//
// This is what the HTTP API judges its per-request snapshot with. It exists
// because that snapshot (Engine.ProbeAll) is taken once for the whole response
// and every availability predicate is judged against that one instant -- and
// because re-probing per request would make a watchface poll wait out the
// confirmation gaps. The evidence is therefore weaker than Engine.RackActivity
// gives, one probe per host; the rule applied to it is identical, including
// that an unprobeable host counts as running.
//
// Non-f hosts are ignored: the plug cools f0-f2, and neither the k3s guests
// nor f3 are in that set. See RackActivity's doc comment for why f3 is
// excluded even though it is Role f.
func RackActivityFrom(statuses []HostStatus) RackActivity {
	var a RackActivity
	for _, st := range statuses {
		if st.Role != string(inventory.RoleF) || st.Name == "f3" {
			continue
		}
		a.add(st.Name, st.liveness())
	}
	return a
}

// Why explains what is keeping the fans on, distinguishing hosts that answered
// from hosts nothing could be learned about. The difference matters to whoever
// reads it: the first is the rack working as intended, the second means this
// machine cannot probe the rack and every other liveness answer it gives is
// equally worthless.
func (a RackActivity) Why() string {
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

// RackActivity probes the power group (f0/f1/f2) and reports which of them
// may still be drawing power. This is the strict half of the guard: each host
// that stays silent is probed confirmedDownProbes times before it counts as
// off.
//
// f3 is not probed here: the plug does not cool it, so its liveness cannot
// change the answer. See RackActivity's doc comment.
//
// It pings directly instead of going through Probe because Probe also dials
// port 22 on every host, and a host that is off makes that dial wait out the
// full timeout for an answer nothing here looks at.
//
// Concurrently, because a host that is off costs several ProbeTimeouts (see
// confirmLiveness) and the caller is usually a person waiting at a terminal or
// a shutdown holding the rack in an undefined state.
func (e *Engine) RackActivity(ctx context.Context) RackActivity {
	hosts := e.cfg.Inventory.PowerGroup()

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

	var a RackActivity
	for i, h := range hosts {
		a.add(h.Name, state[i])
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
