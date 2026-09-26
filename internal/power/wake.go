package power

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/snonux/f3sctl/internal/inventory"
)

// On wakes the k3s bhyve hosts f0/f1/f2.
//
// Order matters: the fans go on before the hosts, not after, so the rack is
// never under load without cooling. If the plug cannot be switched, nothing is
// woken at all — running the cluster with no fans is worse than leaving it
// off.
func (e *Engine) On(ctx context.Context, log io.Writer) error {
	return e.on(ctx, log, e.cfg.Inventory.PowerGroup())
}

// on is the shared wake sequence: fans first, then magic packets, then wait for
// the cluster and clear the Gogios mute.
func (e *Engine) on(ctx context.Context, log io.Writer, hosts []inventory.Host) error {
	e.logWarnings(log)

	e.reporter().Step("switching the rack fans on")
	fmt.Fprintln(log, "Switching the rack fans on...")
	if _, err := e.fansBackend().Set(ctx, true); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// Interrupted, not refused: the plug was never judged.
			return fmt.Errorf("wake interrupted before any host was woken: %w", ctxErr)
		}
		return fmt.Errorf("refusing to wake hosts with the fans off: %w", err)
	}

	e.reporter().Step("sending Wake-on-LAN packets")
	for _, h := range hosts {
		fmt.Fprintf(log, "Sending a magic packet to %s (%s)...\n", h.Name, h.MAC)
		if err := e.powerBackend().Wake(h); err != nil {
			e.reporter().HostState(h.Name, HostFailed, err.Error())
			return err
		}
		e.reporter().HostState(h.Name, HostDone, "magic packet sent")
	}

	e.reporter().Step("waiting for the k3s nodes, then un-muting Gogios")

	// While it waits for the cluster, UnmuteGogios re-sends the magic packets
	// (rewake), since a host can ignore the first one. On timeout it un-mutes
	// anyway, so a host that never came back alerts instead of hiding behind
	// the mute, and names the missing nodes. That is the wake's error: the
	// packets went out, but the cluster did not come back, and a job that
	// says "done" would contradict the page. A gateway that could not be
	// un-muted after a complete wake is reported too -- a mute left on one
	// gateway is a monitoring gap -- but worded as what it is.
	rewake := func() {
		e.reporter().Step("re-sending Wake-on-LAN packets to hosts still down")
		e.rewake(log, hosts)
		e.reporter().Step("waiting for the k3s nodes, then un-muting Gogios")
	}
	if err := e.UnmuteGogios(ctx, log, rewake); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// UnmuteGogios leaves the marker on a cancel; "woke, but" would
			// claim a wake nobody saw finish.
			return fmt.Errorf("wake interrupted before every k3s node answered; Gogios left "+
				"muted, clear it with `f3sctl monitoring unmute` once the nodes are up: %w", ctxErr)
		}
		if errors.Is(err, ErrClusterIncomplete) {
			return fmt.Errorf("wake incomplete: %w", err)
		}
		return fmt.Errorf("woke, but Gogios is not fully un-muted: %w", err)
	}

	fmt.Fprintln(log, "All k3s nodes answer; Gogios monitoring is un-muted.")
	return nil
}

// rewake re-sends a magic packet to every host of a wake. Hosts already up
// ignore it, so there is no need to work out which ones are still down.
// Failures are logged, not returned: this is a retry of a step that already
// succeeded once, and the caller keeps waiting either way. A successful
// re-send refreshes the host's "done" entry in the job's host map.
func (e *Engine) rewake(log io.Writer, hosts []inventory.Host) {
	for _, h := range hosts {
		if err := e.powerBackend().Wake(h); err != nil {
			// Logged only: the host's first packet already went out, and
			// marking it failed would leave a failed host in a job that ends
			// done once the cluster answers.
			fmt.Fprintf(log, "  ! re-sending the magic packet to %s: %v\n", h.Name, err)
			continue
		}
		e.reporter().HostState(h.Name, HostDone, "magic packet re-sent")
	}
}

// OnAll wakes every f-host, f3 included.
func (e *Engine) OnAll(ctx context.Context, log io.Writer) error {
	return e.on(ctx, log, e.cfg.Inventory.EveryFHost())
}

// OnHost wakes a single named host, without touching the fans or the Gogios
// marker. Used for f3, which is not part of the cluster.
func (e *Engine) OnHost(ctx context.Context, log io.Writer, name string) error {
	h, err := e.powerHost(name)
	if err != nil {
		return err
	}
	fmt.Fprintf(log, "Sending a magic packet to %s (%s)...\n", h.Name, h.MAC)
	return e.powerBackend().Wake(h)
}
