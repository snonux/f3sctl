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
		return cycleInterrupted(err)
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
//
// A cancelled ctx (Ctrl-C, SIGTERM) cuts the dwell short but never skips the
// restore: a run that is being torn down must not leave the rack without
// mains, which is the one state nothing remote can wake it from.
func (e *Engine) cycleAC(ctx context.Context, log io.Writer) error {
	e.reporter().Step("confirming every f-host is dark before cutting AC")
	fmt.Fprintln(log, "Confirming every f-host is dark before cutting mains AC...")
	busy := e.ACActivity(ctx)
	if err := ctx.Err(); err != nil {
		// Checked first: probes cut short by the cancel read as "unknown",
		// which Busy counts as running, and "refusing" would misreport it.
		return fmt.Errorf("power cycle interrupted before AC was cut: AC left ON, the hosts "+
			"left powered off; wake them with `f3sctl power all on`: %w", err)
	}
	if busy.Busy() {
		return fmt.Errorf("refusing to cut f-host AC: %s; AC left ON", busy.Why())
	}

	if err := e.cutAC(ctx, log); err != nil {
		return err
	}

	fmt.Fprintf(log, "AC is off; waiting %s before restoring it...\n", e.acDwell())
	dwellErr := sleepCtx(ctx, e.acDwell())
	if err := e.restoreAC(ctx, log); err != nil {
		return err
	}
	if dwellErr != nil {
		return cycleInterrupted(dwellErr)
	}
	return nil
}

// cutAC switches the f-host mains AC off.
//
// The switch runs on a context detached from ctx. Cancelling it half way --
// Switch.Set already sent, settleShelly still polling for the read-back --
// would turn an interrupt into "cut failed" with the plug in an unknown
// state; the Shelly client's HTTP timeout and the settle budget bound it
// instead, a few seconds at most. An interrupt that lands meanwhile is seen by
// the dwell, which then returns at once.
//
// A cut that fails may still have switched the plug, so it is followed by a
// restore: the hosts are off either way, and waking them later needs mains.
func (e *Engine) cutAC(ctx context.Context, log io.Writer) error {
	e.reporter().Step("cutting f-host mains AC")
	fmt.Fprintln(log, "Cutting f-host mains AC...")
	_, err := e.acBackend().Set(context.WithoutCancel(ctx), false)
	if err == nil {
		return nil
	}
	if rerr := e.restoreAC(ctx, log); rerr != nil {
		return fmt.Errorf("cutting f-host AC failed (%v), and then: %w", err, rerr)
	}
	return fmt.Errorf("cutting f-host AC failed, AC switched back on and the hosts left "+
		"powered off: %w. Wake them with `f3sctl power all on`", err)
}

// restoreAC switches the f-host mains AC back on, on a context that cannot be
// cancelled; the Shelly client's own HTTP timeout still bounds the call.
//
// The Set goes out before anything is logged. After an interrupt the log's
// reader may be gone (a Ctrl-C that also killed a `| tee`), and nothing may
// stand between a run being torn down and mains coming back.
func (e *Engine) restoreAC(ctx context.Context, log io.Writer) error {
	_, err := e.acBackend().Set(context.WithoutCancel(ctx), true)
	if err != nil {
		return e.restoreACAfter(err)
	}
	e.reporter().Step("f-host mains AC restored")
	fmt.Fprintln(log, "Restored f-host mains AC.")
	return nil
}

// cycleInterrupted is the error for a cycle cancelled after AC was cut. By
// then AC has been restored (restoreAC runs regardless), so what is left is a
// rack that is powered off and muted but wakeable, and the command that
// finishes the job.
func cycleInterrupted(err error) error {
	return fmt.Errorf("power cycle interrupted: f-host AC is back on, but the hosts are "+
		"left powered off and Gogios muted; wake them with `f3sctl power all on`: %w", err)
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
