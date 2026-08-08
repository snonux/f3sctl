package power

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/snonux/f3sctl/internal/inventory"
)

// MuteGogios creates the marker that suppresses the Gogios checks derived from
// the cluster's own Prometheus, on both OpenBSD gateways.
//
// Without it, deliberately taking the cluster down pages as if it had failed.
func (e *Engine) MuteGogios(ctx context.Context, log io.Writer) error {
	return e.eachGateway(ctx, log, "gogios-mute", "muted")
}

// UnmuteGogios waits for the k3s nodes to come back, then removes the marker.
//
// It waits because the alerts being suppressed are scraped from that cluster:
// clearing the marker while the nodes are still booting would fire every one
// of them. It does not wait forever, and on giving up it leaves the marker in
// place and says so — a stuck mute is a monitoring gap, so the operator needs
// to know it happened.
//
// Gogios does expire the marker itself after PrometheusOnlyIfNotExistsMaxS
// (24h). Relying on that expiry is what hid a two-day audiobookshelf outage in
// August 2026 after an early wake-up, which is why the wake path clears it
// explicitly instead.
func (e *Engine) UnmuteGogios(ctx context.Context, log io.Writer) error {
	fmt.Fprintln(log, "Waiting for the k3s nodes before un-muting Gogios monitoring...")

	if err := e.waitForCluster(ctx, log); err != nil {
		fmt.Fprintf(log, "  %v\n", err)
		fmt.Fprintf(log, "  Leaving Gogios muted. Clear it by hand once the nodes are up:\n")
		for _, gw := range e.cfg.Inventory.ByRole(inventory.RoleGateway) {
			fmt.Fprintf(log, "    ssh -p %d %s@%s gogios-unmute\n", gw.SSHPort, gw.SSHUser, gw.IP)
		}
		return err
	}

	return e.eachGateway(ctx, log, "gogios-unmute", "un-muted")
}

// waitForCluster polls r0/r1/r2 until all three answer or the timeout expires.
func (e *Engine) waitForCluster(ctx context.Context, log io.Writer) error {
	nodes := e.cfg.Inventory.ByRole(inventory.RoleCluster)
	deadline := time.Now().Add(e.cfg.UnmuteTimeout.D())

	for {
		pending := 0
		for _, st := range e.Probe(ctx, nodes) {
			if !st.Ping {
				pending++
			}
		}
		if pending == 0 {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("%d of %d k3s nodes still unreachable after %s",
				pending, len(nodes), e.cfg.UnmuteTimeout.D())
		}

		fmt.Fprintf(log, "  %d of %d k3s nodes still down; waiting...\n", pending, len(nodes))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

// eachGateway runs verb on every gateway, reporting per-gateway failures but
// continuing to the rest.
//
// Both gateways are tried even if the first fails: they are independent Gogios
// installs, and muting one of two is strictly better than muting neither.
func (e *Engine) eachGateway(ctx context.Context, log io.Writer, verb, past string) error {
	gateways := e.cfg.Inventory.ByRole(inventory.RoleGateway)
	var failed []string

	for _, gw := range gateways {
		if _, err := e.ssh.agentVerb(ctx, gw, verb); err != nil {
			fmt.Fprintf(log, "  ! %v\n", err)
			failed = append(failed, gw.Name)
			continue
		}
		fmt.Fprintf(log, "  Gogios %s on %s\n", past, gw.Name)
	}

	if len(failed) > 0 {
		return fmt.Errorf("could not %s Gogios on: %v", verb, failed)
	}
	return nil
}
