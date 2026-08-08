package power

import (
	"context"
	"fmt"
	"io"
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
const powerDownTimeout = 2 * time.Minute

// confirmedDownProbes is how many consecutive missed pings count as "off".
//
// Three, at ten seconds apart, is comfortably longer than the interface flap
// seen mid-shutdown while still declaring a genuinely dead host down inside
// half a minute.
const confirmedDownProbes = 3

// awaitPowerDown waits for each host to stop answering ICMP, and returns the
// names of any that never do.
//
// A host still answering after the timeout is the dangerous case: it is
// powered on but on its way out of multi-user, so it will drop off the network
// without ever powering down, and Wake-on-LAN cannot bring it back. Saying so
// here -- while the operator is still watching -- is far better than leaving
// it to be discovered next time someone presses "on" and nothing happens.
func (e *Engine) awaitPowerDown(ctx context.Context, log io.Writer, hosts []inventory.Host) []string {
	if len(hosts) == 0 {
		return nil
	}

	fmt.Fprintln(log, "Confirming the hosts actually powered down...")

	pending := make(map[string]inventory.Host, len(hosts))
	misses := make(map[string]int, len(hosts))
	for _, h := range hosts {
		pending[h.Name] = h
	}

	deadline := time.Now().Add(powerDownTimeout)
	for len(pending) > 0 && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return namesOf(pending)
		case <-time.After(10 * time.Second):
		}

		for name, h := range pending {
			if e.pingOnce(ctx, h.IP) {
				// Still answering: reset, so only a sustained silence counts.
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

	stuck := namesOf(pending)
	for _, name := range stuck {
		e.report.HostState(name, HostFailed, "still answering after "+powerDownTimeout.String()+"; likely hung and not wakeable")
		fmt.Fprintf(log, "  ! %s accepted the shutdown but is still answering after %s.\n", name, powerDownTimeout)
	}
	fmt.Fprintln(log, "    A host that hangs in the last phase of shutdown stays powered on with")
	fmt.Fprintln(log, "    no network, and Wake-on-LAN will NOT wake it -- WoL only wakes a NIC")
	fmt.Fprintln(log, "    that powered down. Recovering needs the JetKVM console or the physical")
	fmt.Fprintln(log, "    power button, so deal with it before relying on a remote wake.")
	return stuck
}

func namesOf(hosts map[string]inventory.Host) []string {
	out := make([]string, 0, len(hosts))
	for name := range hosts {
		out = append(out, name)
	}
	return out
}
