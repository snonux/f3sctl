package power

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/snonux/f3sctl/internal/inventory"
)

// carpQuiesceVerb is the agent verb that stops a host's CARP failover
// daemons. See internal/agent/carp.go for which daemons those are and why
// the NFS daemons themselves are left running.
const carpQuiesceVerb = "carp-quiesce"

// quiesceCARP stops the CARP failover daemons on every member of the pair
// that this run is about to shut down, and reports whether it may now take
// hosts down in parallel.
//
// This is what buys the parallel shutdown. Taking hosts down one at a time,
// storage master last (inventory.ShutdownOrder), was never about politeness:
// it was the only way to guarantee that no host received the VIP -- and with
// it carpcontrol.sh's "start NFS now" -- while it was itself shutting down.
// Stopping the daemons removes the hazard at its source instead of ordering
// around it, so the remaining hosts become independent machines that can go
// down together.
//
// A member whose daemons could not be stopped returns false, and the caller
// falls back to the old sequential order. That is the whole point of
// returning a bool rather than an error: a rack whose failover daemons are
// still running is not a rack that cannot be shut down, it is a rack that
// must be shut down carefully. The slower path is still correct, and choosing
// it silently would be wrong -- hence the log line.
//
// Only the two hosts the inventory names as the pair are asked; f2 and f3
// have no CARP configuration and no such daemons to stop. The agent-side verb still
// checks for the CARP script before doing anything, so a member whose CARP
// setup has been removed answers "nothing to quiesce" instead of failing the
// run -- the inventory says who the pair is, the host says whether it is
// still configured as such, and the two disagreeing must not cost a
// shutdown.
func (e *Engine) quiesceCARP(ctx context.Context, log io.Writer, tl *timeline,
	hosts []inventory.Host) bool {

	members := inventory.CARPMembers(hosts)
	if len(members) == 0 {
		// Nothing in this run can take the VIP, so there is nothing to stop
		// and no reason to withhold the parallel path.
		return true
	}

	defer tl.track("CARP quiesce")()

	e.reporter().Step("stopping the CARP failover daemons")
	fmt.Fprintln(log, "Stopping the CARP failover daemons before shutting hosts down...")
	results := e.quiesceEach(ctx, members)

	ok := true
	for _, r := range results {
		if r.err != nil {
			fmt.Fprintf(log, "  ! %s: %v\n", r.host, r.err)
			ok = false
			continue
		}
		fmt.Fprintf(log, "  %s: %s\n", r.host, indent(r.out))
	}

	if !ok {
		fmt.Fprintln(log, "  The CARP failover daemons are not stopped everywhere, so hosts go "+
			"down one at a time with the storage master last, as before.")
		return false
	}

	// Worth saying out loud: if this run does not finish, the daemons stay
	// stopped on a host that is still up. See runCARPQuiesce's doc.
	fmt.Fprintln(log, "  They start again by themselves when these hosts next boot.")
	return true
}

// quiesceResult is one host's answer, kept so the log stays in host order
// however the goroutines finish.
type quiesceResult struct {
	host string
	out  string
	err  error
}

// quiesceEach runs the verb on every member concurrently.
//
// Concurrently because the members are independent and this sits directly in
// front of the thing it exists to speed up; two SSH round trips in sequence
// would be a small but pointless tax on every rack shutdown.
func (e *Engine) quiesceEach(ctx context.Context, members []inventory.Host) []quiesceResult {
	results := make([]quiesceResult, len(members))

	var wg sync.WaitGroup
	for i, h := range members {
		wg.Add(1)
		go func(i int, h inventory.Host) {
			defer wg.Done()
			out, err := e.powerBackend().AgentVerb(ctx, h, carpQuiesceVerb)
			results[i] = quiesceResult{host: h.Name, out: out, err: err}
		}(i, h)
	}
	wg.Wait()

	return results
}
