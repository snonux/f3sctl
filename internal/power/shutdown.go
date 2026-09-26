package power

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/snonux/f3sctl/internal/inventory"
)

// Off shuts down the k3s bhyve hosts f0/f1/f2, and switches the rack fans off
// once f0/f1/f2 are silent.
//
// The sequence is ordered so that everything which can refuse does so before
// anything irreversible happens:
//
//  1. unmount local NFS      -- abort while the cluster is still fully up
//  2. export zusb where held -- ditto; needs the host it lives on to be alive
//  3. mute Gogios            -- only now, once the shutdown is going ahead
//  4. stop guests, power off -- host by host, storage master LAST
//  5. fans off               -- only if f0/f1/f2 are now all silent
//
// Step 5 does not wait on f3, whether or not this run touched it: f3 is
// racked separately from f0-f2 and the fan plug does not cool it (see
// inventory.PowerGroup), so f3's liveness has no bearing on whether cutting
// the plug is safe. See fansOffOnceTheRackIsIdle.
//
// The host order is not incidental: taking the CARP storage master first fails
// the VIP over onto a host that is itself about to be shut down, which is what
// wedged f1 on 2026-08-08. See inventory.ShutdownOrder.
func (e *Engine) Off(ctx context.Context, log io.Writer) error {
	return e.off(ctx, log, e.cfg.Inventory.ShutdownOrder(), true)
}

// OffAll shuts down every f-host, f3 included, and switches the rack fans off
// once f0/f1/f2 are confirmed silent.
//
// Identical to Off apart from the host set: same NFS and zusb pre-flight, same
// Gogios mute, same storage-master-last ordering, same fans-off guard. This is
// "the whole rack goes dark", which previously meant running `power off` and
// `power f3 off` separately. The fan guard itself does not distinguish the two
// commands -- f3's state was never part of it -- so both cut the plug as soon
// as f0/f1/f2 go quiet; OffAll additionally takes f3 down along the way.
func (e *Engine) OffAll(ctx context.Context, log io.Writer) error {
	return e.off(ctx, log, e.cfg.Inventory.ShutdownOrderAll(), true)
}

// OffHost shuts down a single named host.
//
// The fans and the Gogios marker are deliberately left alone: f3 going down
// does not mean the rack is idle, and the muted checks are the cluster's, not
// f3's.
func (e *Engine) OffHost(ctx context.Context, log io.Writer, name string) error {
	h, err := e.powerHost(name)
	if err != nil {
		return err
	}
	return e.off(ctx, log, []inventory.Host{h}, false)
}

// off is the shared shutdown sequence behind Off, OffAll and OffHost.
//
// clusterWide says whether this run is a rack-wide operation rather than one
// host being taken down on its own. It gates the two things that belong to the
// rack as a whole rather than to any host in the list: the Gogios mute, and the
// rack-fan plug. It does NOT mean "every host is in the list" -- Off's list
// leaves f3 out -- which is why the fans-off step re-checks what is actually
// still running instead of trusting this flag.
//
// The log is serialized before anything else happens because from here on it
// has concurrent writers: shutdownTogether runs a host per goroutine, and the
// stderr diagnostics logWarnings routes here arrive from those same
// goroutines.
func (e *Engine) off(ctx context.Context, log io.Writer, hosts []inventory.Host, clusterWide bool) error {
	log = serialized(log)
	e.logWarnings(log)

	tl := newTimeline()
	defer tl.report(log)

	for _, h := range hosts {
		e.reporter().HostState(h.Name, HostPending, "")
	}

	// The pre-flight is everything that can still refuse: it runs while the
	// rack is untouched, and its error is the cheapest possible outcome.
	hosts, err := e.offPreflight(ctx, log, tl, hosts, clusterWide)
	if err != nil {
		return err
	}

	accepted, failed := e.shutdownEach(ctx, log, tl, hosts)
	failed = append(failed, e.confirmPowerDown(ctx, log, tl, accepted)...)
	// Before shutdownFailure: a cancelled wait reports every host it had not
	// confirmed yet, which is not evidence that any of them failed.
	if err := ctx.Err(); err != nil {
		return shutdownInterrupted(failed, err)
	}
	if len(failed) > 0 {
		return shutdownFailure(failed)
	}

	if clusterWide {
		defer tl.track("rack fans")()
		return e.fansOffAndReport(ctx, log)
	}

	fmt.Fprintln(log, "All hosts accepted shutdown.")
	return nil
}

// confirmPowerDown waits for every host that accepted its shutdown to actually
// go silent, returning the names of those that did not.
//
// Accepting the command is not the same as completing it. A host can run the
// whole shutdown sequence and then wedge in the final phase -- after syslogd
// has exited, so nothing is logged -- leaving it powered on, off the network,
// and NOT wakeable by Wake-on-LAN, which only wakes a NIC that actually
// powered down. Recovering from that needs a console or the physical button.
//
// f1 did exactly this on 2026-08-08 while f0 and f2 powered off cleanly.
// Nothing reported a problem at the time: the tool said "shutdown sent" and
// moved on, and the failure only surfaced later when the host would not wake.
// Confirming each host actually goes silent turns that into an error at the
// moment it happens.
func (e *Engine) confirmPowerDown(ctx context.Context, log io.Writer, tl *timeline,
	accepted []inventory.Host) []string {

	e.reporter().Step("confirming the hosts actually powered down")
	defer tl.track("confirm power-down")()
	return e.awaitPowerDown(ctx, log, accepted)
}

// offPreflight runs everything that happens before the first host is touched:
// dropping the hosts that are already down, refusing over a locally mounted
// NFS filesystem, exporting the zusb pool, and muting Gogios. It returns the
// hosts that remain to be shut down.
//
// Split out of off() so that the shutdown sequence proper reads as the four
// steps it is (pre-flight, quiesce, shut down, confirm) rather than as one
// long function where the interesting ordering is buried among the checks.
// The pre-flight also has a property worth naming: everything in it either
// changes nothing or changes something that a failed run leaves in a state
// someone can reason about, which is why its errors abort the run outright.
func (e *Engine) offPreflight(ctx context.Context, log io.Writer, tl *timeline,
	hosts []inventory.Host, clusterWide bool) ([]inventory.Host, error) {

	defer tl.track("pre-flight checks")()

	hosts = e.skipAlreadyOff(ctx, log, hosts)

	e.reporter().Step("checking for locally mounted NFS filesystems")
	fmt.Fprintln(log, "Checking for locally mounted NFS filesystems...")
	if err := e.checkLocalNFS(ctx, log); err != nil {
		return nil, err
	}

	e.reporter().Step("checking the zusb backup pool")
	fmt.Fprintln(log, "Checking whether the zusb backup pool is imported anywhere...")
	if err := e.zusbPreflight(ctx, log, hosts); err != nil {
		return nil, err
	}

	if clusterWide {
		e.reporter().Step("muting Gogios monitoring")
		fmt.Fprintln(log, "Muting Gogios monitoring...")
		if err := e.MuteGogios(ctx, log); err != nil {
			// Not fatal: an un-muted alert is noise, and refusing to shut
			// down over noise would be worse. But say so loudly.
			fmt.Fprintf(log, "  ! %v\n", err)
			fmt.Fprintln(log, "  Continuing; expect alerts while the cluster is down.")
		}
	}

	return hosts, nil
}

// skipAlreadyOff drops the hosts that are already powered off from the list,
// saying so for each.
//
// Everything after it speaks SSH -- the zusb pre-flight, the guest stop, the
// poweroff itself -- and a powered-off host cannot answer any of it. Treating
// that as an error is wrong twice over: it is not a failure, and it aborts work
// that still needs doing on the hosts that *are* up.
//
// This is not hypothetical. On 2026-08-09 a `power off` run with f0 already
// down failed at the zusb pre-flight with "connect to host 192.168.1.130 port
// 22: Operation timed out" and left f1 and f2 running. Shutting the rack down
// in stages, or re-running after a partial run, is ordinary use.
//
// Ping decides this, not SSH. A host answering ICMP but not SSH is powered on
// and unreachable, and that case must still abort: there is no way to tell
// whether it has the zusb pool imported, and guessing risks cutting USB power
// to a mounted backup pool.
//
// A host whose liveness could not be probed at all is kept in the list
// (isRunning says so). It then fails at the zusb pre-flight if it really is
// off, which is loud, undoes nothing and leaves the fans alone -- whereas
// dropping it would silently shut nothing down and report success.
func (e *Engine) skipAlreadyOff(ctx context.Context, log io.Writer,
	hosts []inventory.Host) []inventory.Host {

	live, alreadyOff := partitionLive(hosts, func(ip string) bool {
		return e.isRunning(ctx, ip)
	})
	for _, h := range alreadyOff {
		fmt.Fprintf(log, "%s is already powered off; skipping it\n", h.Name)
		e.reporter().HostState(h.Name, HostDone, "already powered off")
	}
	return live
}

// fansOffAndReport ends a rack-wide run: the fan plug, then the closing line.
//
// The line says which of the two outcomes happened, because a run that
// deliberately keeps the cooling on still succeeds -- the hosts it was asked to
// power off did go down -- so it exits 0 either way, and its last progress step
// is otherwise the only thing distinguishing them. Repeating it here means
// neither a human reading the log tail nor a job.log reader has to infer it
// from silence.
func (e *Engine) fansOffAndReport(ctx context.Context, log io.Writer) error {
	leftOn, err := e.fansOffOnceTheRackIsIdle(ctx, log)
	if err != nil {
		return err
	}
	if len(leftOn) > 0 {
		fmt.Fprintf(log, "All hosts accepted shutdown. %s.\n", fansLeftOnReason(leftOn))
		return nil
	}

	fmt.Fprintln(log, "All hosts accepted shutdown.")
	return nil
}

// shutdownFailure is the error for hosts that did not complete their shutdown.
//
// It carries fansLeftOn for the same reason the progress step does: this is the
// other exit through which a rack-wide run ends with the plug untouched, and
// both need to say so in the same words, so "did the fans stay on" is one
// string to look for rather than two phrasings to keep in sync.
func shutdownFailure(failed []string) error {
	return fmt.Errorf("these hosts did not complete shutdown: %v. %s", failed, fansLeftOn)
}

// shutdownEach shuts down every host, returning those that accepted and the
// names of those that did not.
//
// It runs in two waves, and the split is the whole design. The storage master
// goes alone, last; everything else goes in one batch beforehand, in parallel
// when the CARP failover daemons have been stopped (see quiesceCARP) and one
// at a time when they could not be.
//
// Two separate hazards force that shape, and only one of them is CARP:
//
//   - a host that receives the CARP VIP while shutting down wedges, which is
//     what quiesceCARP removes and what used to make the whole run sequential
//     (see inventory.ShutdownOrder for the 2026-08-08 evidence);
//   - the k3s guests on every other host mount their PVs from the VIP over
//     NFS, and they are still writing to it while they stop. Powering the
//     master off before they are gone would hang them on NFS I/O until the
//     agent's bounded wait expires and SIGKILLs them -- a torn etcd WAL,
//     traded for a minute of wall clock.
//
// So stopping those daemons buys concurrency within the batch, not the right
// to take the master down with it. PowerOff returns only once a host's guests have
// actually stopped, which makes "the batch has finished" exactly the
// condition the master is waiting for.
//
// One host failing does not stop the rest: they are independent machines, and
// abandoning a shutdown half way leaves the rack in a worse state than
// finishing it and reporting what went wrong. The caller turns a non-empty
// failed list into an error -- and, importantly, into "leaving the rack fans
// on".
func (e *Engine) shutdownEach(ctx context.Context, log io.Writer, tl *timeline,
	hosts []inventory.Host) (accepted []inventory.Host, failed []string) {

	parallel := e.quiesceCARP(ctx, log, tl, hosts)
	batch, master := splitStorageMaster(hosts)

	if parallel && len(batch) > 1 {
		accepted, failed = e.shutdownTogether(ctx, log, tl, batch)
	} else {
		accepted, failed = e.shutdownInTurn(ctx, log, tl, batch)
	}

	// master holds at most one host, and the loop is what keeps "the master is
	// not in this run" from needing a branch of its own.
	masterUp, masterDown := e.shutdownInTurn(ctx, log, tl, master)
	return append(accepted, masterUp...), append(failed, masterDown...)
}

// splitStorageMaster separates the CARP storage master from the rest,
// preserving order. master holds one host, or none when this run does not
// include it.
func splitStorageMaster(hosts []inventory.Host) (batch, master []inventory.Host) {
	for _, h := range hosts {
		if h.Name == inventory.StorageMaster {
			master = append(master, h)
			continue
		}
		batch = append(batch, h)
	}
	return batch, master
}

// shutdownInTurn shuts hosts down one at a time, in order.
func (e *Engine) shutdownInTurn(ctx context.Context, log io.Writer, tl *timeline,
	hosts []inventory.Host) ([]inventory.Host, []string) {

	ok := make([]bool, len(hosts))
	for i, h := range hosts {
		e.reporter().Step("shutting down " + h.Name)
		ok[i] = e.shutdownOne(ctx, log, tl, h)
	}
	return partitionAccepted(hosts, ok)
}

// shutdownTogether shuts every host in the batch down at once.
//
// Each host writes into its own buffer, flushed to the log in a single write
// when that host finishes, so the log reads as one block per host instead of
// three interleaved shutdowns. The progress step and the per-host states go
// out immediately, which is what a client watching the job actually follows;
// the log block is for reading afterwards.
func (e *Engine) shutdownTogether(ctx context.Context, log io.Writer, tl *timeline,
	hosts []inventory.Host) ([]inventory.Host, []string) {

	names := hostNames(hosts)
	e.reporter().Step("shutting down " + strings.Join(names, ", ") + " together")
	fmt.Fprintf(log, "Shutting down %s in parallel...\n", strings.Join(names, ", "))
	defer tl.track("shutdown batch")()

	ok := make([]bool, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h inventory.Host) {
			defer wg.Done()
			var buf bytes.Buffer
			ok[i] = e.shutdownOne(ctx, &buf, tl, h)
			_, _ = log.Write(buf.Bytes())
		}(i, h)
	}
	wg.Wait()

	return partitionAccepted(hosts, ok)
}

// shutdownOne asks one host to stop its guests and power off, writing its part
// of the log to w. It reports whether the host accepted.
func (e *Engine) shutdownOne(ctx context.Context, w io.Writer, tl *timeline,
	h inventory.Host) bool {

	defer tl.track("shutdown " + h.Name)()

	e.reporter().HostState(h.Name, HostWorking, "stopping guests")
	fmt.Fprintf(w, "Shutting down %s (%s)...\n", h.Name, h.IP)
	out, diag, err := e.powerBackend().PowerOff(ctx, h)
	if out != "" {
		fmt.Fprintf(w, "  %s\n", indent(out))
	}
	if err != nil {
		fmt.Fprintf(w, "  ! %v\n", err)
		e.reporter().HostState(h.Name, HostFailed, err.Error())
		return false
	}

	// A forced guest stop still exits 0, so it arrives here rather than in
	// the error branch. Carry it into the host's progress detail: a run
	// that SIGKILLed a k3s guest may have torn an etcd write-ahead log,
	// and that has to be visible to whoever reads the job, not buried in a
	// log file on whichever node happened to run it.
	detail := "accepted; waiting for it to go silent"
	if diag != "" {
		detail = "accepted, but the guests were force-stopped; check etcd on next boot"
	}
	fmt.Fprintf(w, "  %s accepted the shutdown\n", h.Name)
	e.reporter().HostState(h.Name, HostConfirming, detail)
	return true
}

// partitionAccepted splits hosts by whether their shutdown was accepted,
// keeping the input order so the log and the error read the same way whatever
// order the goroutines of a parallel batch finished in.
func partitionAccepted(hosts []inventory.Host, ok []bool) (accepted []inventory.Host, failed []string) {
	for i, h := range hosts {
		if ok[i] {
			accepted = append(accepted, h)
			continue
		}
		failed = append(failed, h.Name)
	}
	return accepted, failed
}

// hostNames is the host list as it appears in prose.
func hostNames(hosts []inventory.Host) []string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, h.Name)
	}
	return out
}

// fansLeftOn is the stable phrase for "this run did not cut the rack fans".
//
// It goes into the progress step a client polls for and into the error of a
// failed shutdown, so the deliberate outcome is one token to match on rather
// than prose that drifts between the two places that produce it.
const fansLeftOn = "rack fans left ON"

// fansOffOnceTheRackIsIdle switches the rack fans off, unless a power-group
// host (f0/f1/f2) may still be running. It returns the hosts that kept the
// fans on, empty when the plug was switched off.
//
// The plug cools f0-f2, not the whole rack: f3 is racked separately and is not
// on this circuit, so it is deliberately left out of the check (see
// inventory.PowerGroup and RackActivity). A bare `power off` never touches f3
// either way, so this step waits only on the hosts it actually shut down.
// `power all off` (OffAll) additionally takes f3 down along the way, but that
// does not change what this guard is judged on.
//
// Liveness is re-probed rather than inferred from the shutdown that just ran:
// the hosts this run skipped as already off are equally capable of generating
// heat. ICMP does not settle that either -- a host wedged in the last phase of
// a shutdown is powered on with no network, and answers nothing (see
// awaitPowerDown) -- so silence is the weaker claim "nothing here can be shown
// to be running", and the guard is only as good as that. What it does buy is
// that a host which plainly IS running can no longer lose its cooling.
//
// A rack that is still busy is not a failed shutdown -- the hosts this run was
// asked to power off did go down -- so this says why the fans stay on and
// returns without an error.
//
// Every uncertainty resolves towards "leave them on", and it has to: the
// probe's failure modes all look like silence. A dropped echo reply, a ping(8)
// that could not be found, a cancelled context -- each one used to subtract a
// host from the live set and so bring the rack one host closer to "idle", i.e.
// the guard failed in the direction of cutting cooling to a running rack. See
// RackActivity, which counts unknown as running and wants consecutive misses
// before it accepts that a host is off.
func (e *Engine) fansOffOnceTheRackIsIdle(ctx context.Context, log io.Writer) ([]string, error) {
	if busy := e.RackActivity(ctx); busy.Busy() {
		e.reporter().Step(fansLeftOnReason(busy.Hosts()))
		fmt.Fprintf(log, "%s: %s.\n", fansLeftOn, busy.Why())
		return busy.Hosts(), nil
	}

	e.reporter().Step("switching the rack fans off")
	fmt.Fprintln(log, "Switching the rack fans off...")
	_, err := e.fansBackend().Set(ctx, false)
	return nil, err
}

// fansLeftOnReason is the one-line, machine-greppable form of the outcome:
// the stable phrase, then the hosts responsible for it.
func fansLeftOnReason(hosts []string) string {
	return fansLeftOn + ": " + strings.Join(hosts, ", ")
}

// partitionLive splits hosts into those that may still be running and those
// known to be off.
//
// The split is deliberately not "answered ICMP": its only caller passes
// Engine.isRunning, so a host whose probe could not be carried out lands in
// live. Keeping an unprobeable host in the shutdown is the loud outcome; see
// skipAlreadyOff.
//
// Pulled out as a plain function so the rule can be tested without an SSH
// client, a Shelly plug or a live rack: the predicate is all it touches.
func partitionLive(hosts []inventory.Host,
	mayBeRunning func(ip string) bool) (live, off []inventory.Host) {

	for _, h := range hosts {
		if mayBeRunning(h.IP) {
			live = append(live, h)
			continue
		}
		off = append(off, h)
	}
	return live, off
}
