package power

import (
	"context"
	"fmt"
	"io"
	"time"
)

// acOffDwell is how long a power cycle leaves the f-host mains AC cut before
// restoring it.
//
// Long enough for the Beelinks' and JetKVMs' supplies to actually drain, so
// the boards see a genuine cold start rather than a brown-out they ride
// through on their capacitors -- the whole point of cycling AC is to clear
// state a warm reboot keeps (a wedged NIC, an embedded controller that has
// stopped honouring Wake-on-LAN). Short enough that nobody waiting on the job
// wonders whether it hung.
const acOffDwell = 15 * time.Second

// acSettleWait is how long a power cycle waits after restoring AC before it
// sends the magic packets.
//
// A board that has just regained mains needs a moment before its NIC is in
// the standby state Wake-on-LAN relies on: the supply comes up, the embedded
// controller boots, and only then does the NIC power its PHY and start
// listening. A packet sent before that is simply lost. on() does re-send the
// packets while the cluster is still down, but only every rewakeInterval
// (2m), so skipping this wait would delay the wake by minutes, not save time.
const acSettleWait = 30 * time.Second

// CycleAll power-cycles every f-host, f3 included, through mains AC:
//
//  1. power all off -- the full graceful OffAll sequence (NFS and zusb
//     pre-flight, Gogios mute, CARP quiesce, storage master last, confirmed
//     power-down, fans off once the rack is idle)
//  2. cut f-host AC  -- only once every f-host is confirmed dark
//  3. restore AC     -- after acOffDwell, so the supplies really drain
//  4. power all on   -- the full OnAll sequence (fans on, magic packets,
//     wait for the k3s nodes, un-mute Gogios), after acSettleWait
//
// Every step that can refuse still does so before AC is touched: a failed or
// partial shutdown ends the run with AC untouched and the hosts that did go
// down left off, exactly as `power all off` would, rather than hard-cutting
// mains under a host that is still running. That is the whole difference
// between this and running `ac off --force`.
//
// Once AC has been cut, though, the run is committed to bringing it back: an
// AC restore that fails is the one outcome that leaves the rack unwakeable, so
// it is reported as such and names the command that fixes it.
func (e *Engine) CycleAll(ctx context.Context, log io.Writer) error {
	if err := e.OffAll(ctx, log); err != nil {
		return fmt.Errorf("power cycle stopped before touching AC: %w", err)
	}
	if err := e.cycleAC(ctx, log); err != nil {
		return err
	}

	e.reporter().Step("waiting for the f-host NICs to come up on standby power")
	fmt.Fprintf(log, "Waiting %s for the f-hosts' NICs to come up on standby power...\n",
		e.acSettle())
	if err := sleepCtx(ctx, e.acSettle()); err != nil {
		return err
	}
	return e.OnAll(ctx, log)
}

// cycleAC is CycleAll's middle: confirm the rack is dark, cut AC, wait, and
// restore it.
//
// The dark check is the same strict probe the `ac off` guard uses
// (ACActivity: every f-host, consecutive silences, unknown counts as running).
// OffAll has already confirmed the hosts it shut down went silent, but it
// skipped the ones that were already off by a single ping, and it is this
// step -- not OffAll -- that is about to remove their power, so it asks again
// with the stricter evidence.
func (e *Engine) cycleAC(ctx context.Context, log io.Writer) error {
	e.reporter().Step("confirming every f-host is dark before cutting AC")
	fmt.Fprintln(log, "Confirming every f-host is dark before cutting mains AC...")
	if busy := e.ACActivity(ctx); busy.Busy() {
		return fmt.Errorf("refusing to cut f-host AC: %s; AC left ON", busy.Why())
	}

	e.reporter().Step("cutting f-host mains AC")
	fmt.Fprintln(log, "Cutting f-host mains AC...")
	if _, err := e.acBackend().Set(ctx, false); err != nil {
		// The plug may or may not have switched; either way the hosts are
		// off, so nothing is at risk -- but the cycle did not happen, and
		// waking them now would hide that.
		return fmt.Errorf("cutting f-host AC failed, hosts left powered off: %w. "+
			"Check with `f3sctl ac status`, then `f3sctl ac on` and `f3sctl power all on`", err)
	}

	fmt.Fprintf(log, "AC is off; waiting %s before restoring it...\n", e.acDwell())
	dwellErr := sleepCtx(ctx, e.acDwell())

	// Restored whether or not the dwell was interrupted, and on a context that
	// cannot be cancelled: a run that is being torn down must not leave the
	// rack without mains, which is the one state nothing remote can wake it
	// from. The Shelly client's own HTTP timeout still bounds the call.
	e.reporter().Step("restoring f-host mains AC")
	fmt.Fprintln(log, "Restoring f-host mains AC...")
	if _, err := e.acBackend().Set(context.WithoutCancel(ctx), true); err != nil {
		return e.restoreACAfter(err)
	}
	return dwellErr
}

// restoreACAfter is the error for a cycle that cut AC and then could not
// bring it back: the f-hosts have no mains, so nothing can wake them until
// someone restores it.
func (e *Engine) restoreACAfter(err error) error {
	return fmt.Errorf("f-host AC is still OFF, the hosts cannot be woken: %w. "+
		"Restore it with `f3sctl ac on`, then `f3sctl power all on`", err)
}

// acDwell and acSettle are the cycle's two waits, falling back to their
// defaults when the fields are unset (a hand-built Engine). Fields only so
// tests need not wait real seconds, the same seam as probeGap.
func (e *Engine) acDwell() time.Duration {
	if e.acOffDwell <= 0 {
		return acOffDwell
	}
	return e.acOffDwell
}

func (e *Engine) acSettle() time.Duration {
	if e.acSettleWait <= 0 {
		return acSettleWait
	}
	return e.acSettleWait
}

// sleepCtx waits for d, or returns ctx's error if it is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
