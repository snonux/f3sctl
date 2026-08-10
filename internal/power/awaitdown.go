package power

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/snonux/f3sctl/internal/inventory"
)

// powerDownTimeout bounds how long a host may take to actually go silent after
// accepting a shutdown.
//
// The guests are already stopped by the time poweroff is issued -- that is
// what the agent's bounded bhyve stop guarantees -- so what remains is the
// tail of the shutdown: unmounting, exporting ZFS pools, and the firmware
// powering the board down. That is seconds to a minute or so on a healthy
// host. Two minutes is generous without making a genuinely wedged host take
// long to report.
//
// Engine.powerDownWait reads it, so a test can shorten it; nothing else should.
const powerDownTimeout = 2 * time.Minute

// confirmedDownProbes is how many consecutive missed pings count as "off".
//
// Three, at downProbeInterval apart, is comfortably longer than the interface
// flap seen mid-shutdown while still declaring a genuinely dead host down
// inside half a minute.
//
// The fan guard holds to the same standard (see confirmLiveness). It has to:
// it decides whether to cut cooling, and it runs at the very moment the rack
// is flapping.
const confirmedDownProbes = 3

// downProbeInterval is the gap between those probes. Engine.probeGap reads it,
// so a test can shorten it; nothing else should.
const downProbeInterval = 10 * time.Second

// awaitPowerDown waits for each host to stop answering ICMP, and returns the
// names of any whose power-down was never confirmed.
//
// A host still answering after the timeout is the dangerous case: it is
// powered on but on its way out of multi-user, so it will drop off the network
// without ever powering down, and Wake-on-LAN cannot bring it back. Saying so
// here -- while the operator is still watching -- is far better than leaving
// it to be discovered next time someone presses "on" and nothing happens.
//
// There is a second way to end up unconfirmed, and it is not that at all: a
// host that never answered again but could not be probed either. Unknown counts
// as "not confirmed off" throughout (that direction is deliberate; see
// Engine.isRunning), so a machine whose ping(8) is broken never confirms
// anything and every host it shut down perfectly ends up in this list. That is
// the right list -- nothing here established that they powered down -- but it
// is emphatically not the hung-host emergency, and reportUnconfirmed keeps the
// two apart rather than sending the operator to fetch a console for a rack that
// is already cold.
func (e *Engine) awaitPowerDown(ctx context.Context, log io.Writer, hosts []inventory.Host) []string {
	if len(hosts) == 0 {
		return nil
	}

	fmt.Fprintln(log, "Confirming the hosts actually powered down...")

	pending := make(map[string]inventory.Host, len(hosts))
	misses := make(map[string]int, len(hosts))
	// answered records that a host was heard from at least once during the
	// wait, which is what separates the two outcomes above.
	answered := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		pending[h.Name] = h
	}

	deadline := time.Now().Add(e.powerDownWait())
	for len(pending) > 0 && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			// Cancelled: whoever was watching has gone, so report the state
			// rather than a diagnosis nobody asked for.
			return namesOf(pending)
		case <-time.After(e.probeGap()):
		}

		for name, h := range pending {
			up, known := e.liveness()(ctx, h.IP)
			if up || !known {
				// Still answering, or nothing could be learned. A probe that
				// could not be carried out is not evidence that a host powered
				// down, and recording one as safely off on the strength of a
				// probe that never ran is the failure this step exists to
				// catch. Either way: reset, so only a sustained silence counts.
				answered[name] = answered[name] || up
				misses[name] = 0
				continue
			}

			// One missed ping is not proof of a power-down. The interface goes
			// down and comes back up again during a normal shutdown -- f0's
			// and f1's logs both show "re0: link state changed to DOWN" then
			// "... to UP" seconds apart, as devd and carp shuffle around --
			// so a single miss would let a host that is still very much alive
			// be recorded as safely off. Require consecutive misses.
			misses[name]++
			if misses[name] >= confirmedDownProbes {
				fmt.Fprintf(log, "  %s is down\n", name)
				e.report.HostState(name, HostDone, "powered off")
				delete(pending, name)
			}
		}
	}

	if len(pending) == 0 {
		return nil
	}
	return e.reportUnconfirmed(log, pending, answered)
}

// reportUnconfirmed says what kind of unconfirmed each remaining host is.
//
// The two diagnoses lead somewhere completely different -- one to the JetKVM
// console, the other to the probe on this machine -- so telling an operator the
// wrong one costs them a trip to the garage or, worse, a rack they believe is
// still hot. Both are still failures of the run: nothing here proved the host
// powered down, the caller turns that into a non-zero exit, and the rack fans
// stay on.
func (e *Engine) reportUnconfirmed(log io.Writer, pending map[string]inventory.Host,
	answered map[string]bool) []string {

	var stillAnswering, unprobed []string
	for _, name := range namesOf(pending) {
		if answered[name] {
			stillAnswering = append(stillAnswering, name)
			continue
		}
		unprobed = append(unprobed, name)
	}
	// Sorted, because the source is a map and the log is read by people.
	sort.Strings(stillAnswering)
	sort.Strings(unprobed)

	if len(stillAnswering) > 0 {
		e.reportStillAnswering(log, stillAnswering)
	}
	if len(unprobed) > 0 {
		e.reportUnprobed(log, unprobed)
	}
	return append(stillAnswering, unprobed...)
}

// reportStillAnswering is the hung-host case: the host took the shutdown, kept
// replying, and is on its way to being powered on with no network.
func (e *Engine) reportStillAnswering(log io.Writer, hosts []string) {
	waited := e.powerDownWait()
	for _, name := range hosts {
		e.report.HostState(name, HostFailed,
			"still answering after "+waited.String()+"; likely hung and not wakeable")
		fmt.Fprintf(log, "  ! %s accepted the shutdown but is still answering after %s.\n", name, waited)
	}
	fmt.Fprintln(log, "    A host that hangs in the last phase of shutdown stays powered on with")
	fmt.Fprintln(log, "    no network, and Wake-on-LAN will NOT wake it -- WoL only wakes a NIC")
	fmt.Fprintln(log, "    that powered down. Recovering needs the JetKVM console or the physical")
	fmt.Fprintln(log, "    power button, so deal with it before relying on a remote wake.")
}

// reportUnprobed is the broken-probe case: the host may well have powered off
// exactly as asked, and this machine has no way of telling.
//
// Pointing at the probe rather than at the host is the whole point. The likely
// cause is local and already documented -- no usable ping(8) here, which is why
// pingCandidates exists -- and it is a whole-fleet fault, so it will have taken
// every host in the run down with it.
func (e *Engine) reportUnprobed(log io.Writer, hosts []string) {
	for _, name := range hosts {
		e.report.HostState(name, HostFailed,
			"never answered again, but could not be probed either; power-down unconfirmed")
		fmt.Fprintf(log, "  ! %s accepted the shutdown and then went quiet, but it could not be\n", name)
		fmt.Fprintf(log, "    probed, so nothing here proves it powered down.\n")
	}
	fmt.Fprintln(log, "    This is a fault on THIS machine, not on the hosts: they may well be off.")
	fmt.Fprintln(log, "    Check that ping(8) is present and usable here, then confirm the hosts by")
	fmt.Fprintln(log, "    hand before waking anything -- and note that every other liveness answer")
	fmt.Fprintln(log, "    this run gave you, the rack-fan decision included, rests on the same probe.")
}

func namesOf(hosts map[string]inventory.Host) []string {
	out := make([]string, 0, len(hosts))
	for name := range hosts {
		out = append(out, name)
	}
	return out
}
