// Package power implements everything f3sctl actually does to the homelab:
// waking hosts, shutting them down safely, probing their state, and switching
// the rack fans.
//
// It is the single implementation behind both the CLI and the HTTP API, so the
// two cannot disagree about what "power off" means.
package power

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/inventory"
)

// Engine performs power operations against the inventory in cfg.
type Engine struct {
	cfg    config.Config
	ssh    *runner
	report Reporter

	// isUp probes one host: whether it answered ICMP, and -- separately --
	// whether the probe reached any conclusion at all.
	//
	// A field rather than a direct pingOnce call because four things read it:
	// which hosts to skip as already off (Engine.off), whether a host that
	// accepted a shutdown actually went silent (awaitPowerDown), whether the
	// rack is idle enough to cut the fans (RackActivity, for all three fan
	// guards -- see that type),
	// and the ping column of `power status` (probeOne, and through it ProbeAll
	// and gogios.waitForCluster). None of them can be tested against real
	// ping(8) without depending on the machine's network stack and on packets
	// leaving the box. New wires it to pingOnce; only tests substitute
	// anything else. Same reasoning as cli.liveHostsFunc.
	//
	// The second result exists because "the host is silent" and "the probe
	// could not be carried out" are indistinguishable in a bare bool, and the
	// safety guards must never confuse them: a CGI whose PATH lacked /sbin
	// made every host report false once already (see infra.pingCandidates), which
	// through a single-bool probe reads exactly like a rack that is safely
	// cold. Most decisions read this through isRunning, which folds unknown
	// into "assume it is running". Three callers deliberately do not: probeOne,
	// which describes what was observed rather than acting on it and keeps both
	// halves in HostStatus for whoever does act; confirmLiveness, the fan
	// guard's own probe, which needs all three answers because unknown ends its
	// loop immediately while silence has to be seen confirmedDownProbes times
	// running; and awaitPowerDown, which needs up on its own to record whether a
	// host was ever heard from, so it can tell a hung host apart from one it
	// simply could not probe. Folding early would cost them exactly that
	// distinction.
	isUp func(ctx context.Context, ip string) (up, known bool)

	// nfsMounts lists the NFS filesystems mounted on the machine f3sctl runs
	// on. A seam for the same reason as isUp: checkLocalNFS runs real
	// umount(8) over whatever this returns, so a test that drives a whole
	// shutdown must be able to say "nothing is mounted here" rather than
	// depend on -- and act on -- the mount table of the machine running it.
	// New wires it to localNFSMounts.
	nfsMounts func(ctx context.Context) ([]string, error)

	// downProbeInterval is the gap between the consecutive probes a host must
	// miss before it counts as powered off, in awaitPowerDown and in the fan
	// guard alike. A field only so tests need not wait real seconds; read it
	// through probeGap. New wires it to downProbeInterval.
	downProbeInterval time.Duration

	// powerDownTimeout bounds awaitPowerDown's wait. A field for the same
	// reason as downProbeInterval: what happens when the wait runs out is one
	// of the outcomes an operator actually sees, and a test of it should not
	// take two real minutes. Read it through powerDownWait. New wires it to
	// powerDownTimeout.
	powerDownTimeout time.Duration

	// power, probe, fans, nfs and zusb are the abstractions Engine's policy
	// methods (off, on, awaitPowerDown, zusbPreflight, and the
	// smaller steps they call) act through, rather than shelling out to
	// ssh(1), dialling the Shelly plug's HTTP RPC or exec'ing umount(8)
	// themselves. See backends.go for what each interface promises and why
	// the boundary sits where it does; see powerBackend, probeBackend,
	// fansBackend, nfsBackend and zusbBackend below for the nil-safe
	// accessors, following the same pattern as isUp, nfsMounts and report
	// above. New wires each of them to an adapter that performs the real
	// mechanism.
	power PowerBackend
	probe ProbeBackend
	fans  FansBackend
	nfs   NFSChecker
	zusb  ZusbChecker
	// monitor is the Gogios monitoring concern: muting, un-muting and reading
	// the gateways' alerting state, plus the wake path's wait for the k3s nodes
	// before it clears a mute. Split off Engine (see o51) so the gateway and
	// cluster-wait mechanism is held here, not mixed into the shutdown/fan-guard
	// policy; Engine delegates its MuteGogios/UnmuteGogios/UnmuteNow/
	// MonitoringStatus methods to monitorBackend(). New wires it; only tests
	// substitute anything else, following the same nil-safe seam pattern as the
	// backends above.
	monitor *Monitor
}

// New returns an Engine.
//
// It cannot fail: the SSH identity is resolved lazily, on the first operation
// that actually needs it, so status probing works on a machine that has no
// f3sctl key at all.
//
// This is the one place in the package allowed to name a concrete adapter --
// execPower, execProbe, execFans, execNFS, execZusb -- for the same reason a
// composition root always is: something has to choose the real mechanism
// before the interfaces in backends.go can be used for anything. That is not
// a gap in the OCP story backends.go describes, it is the other half of it:
// off, on, shutdownEach, fansOffOnceTheRackIsIdle and the rest of Engine's
// policy methods depend only on PowerBackend/ProbeBackend/FansBackend/
// NFSChecker/ZusbChecker, never on the Shelly RPC client, magicPacket or
// ssh(1) directly, so a second fan switch or wake mechanism (IPMI, say) is a
// new type implementing the relevant interface plus a few lines here choosing
// it -- never an edit to those policy methods. Exactly this seam is what
// backends_test.go/engine_test.go already exercise, substituting fakes for
// every one of these fields; nothing about a real second implementation
// would work any differently.
//
// New deliberately takes no options to pick among adapters: this project has
// exactly one fan switch (a Shelly plug) and one wake mechanism (WoL), and no
// second implementation of either is asked for anywhere in this codebase.
// Adding a functional-options API (or a config-driven switch) here now would
// be a seam built for a hypothetical that does not exist -- were a second
// implementation ever actually needed, wiring it in is the few lines this
// comment describes, not a redesign.
func New(cfg config.Config) (*Engine, error) {
	e := &Engine{
		cfg:               cfg,
		ssh:               newRunner(cfg),
		report:            nopReporter{},
		nfsMounts:         localNFSMounts,
		downProbeInterval: downProbeInterval,
		powerDownTimeout:  powerDownTimeout,
	}
	e.isUp = e.pingOnce
	e.power = execPower{e}
	e.probe = execProbe{client: newProbeClient(cfg.ProbeTimeout.D())}
	e.fans = execFans{shelly: newShellyClient(cfg.Inventory.ShellyIP, cfg.ResolveShellyPassword)}
	e.nfs = execNFS{e}
	e.zusb = execZusb{e}
	e.monitor = NewMonitor(cfg, e.powerBackend(), e.Probe)
	return e, nil
}

// liveness returns the ICMP probe, falling back to the real one.
//
// Engine is exported and isUp is a plain func field, so an Engine built by
// anything other than New carries a nil there and would panic on the first
// probe -- on the fan guard's path, the one thing here that must neither crash
// nor fail open. cli.liveHostsFunc grew the same fallback (see runFans) for the
// same reason; this is the engine's half of it.
func (e *Engine) liveness() func(ctx context.Context, ip string) (up, known bool) {
	if e.isUp == nil {
		return e.pingOnce
	}
	return e.isUp
}

// isRunning reports whether the host at ip may still be drawing power.
//
// A host that could not be probed counts as running, and that direction is the
// entire point of the probe's second result. Every question asked here is
// really "is it safe to treat this host as off": safe to skip its shutdown,
// safe to call it powered down, safe to cut its cooling. Unknown is never a
// safe yes, and the cost of the two mistakes is not symmetric -- a host wrongly
// believed to be running costs a retry or some idle fan noise, one wrongly
// believed to be off can lose its cooling while it runs.
func (e *Engine) isRunning(ctx context.Context, ip string) bool {
	up, known := e.liveness()(ctx, ip)
	return up || !known
}

// probeGap is the wait between consecutive liveness probes of the same host,
// defaulting when the field is unset (a hand-built Engine): a zero gap would
// turn awaitPowerDown's two-minute wait into a two-minute busy loop.
func (e *Engine) probeGap() time.Duration {
	if e.downProbeInterval <= 0 {
		return downProbeInterval
	}
	return e.downProbeInterval
}

// powerDownWait is how long a host gets to actually go silent after accepting a
// shutdown, defaulting when the field is unset (a hand-built Engine).
func (e *Engine) powerDownWait() time.Duration {
	if e.powerDownTimeout <= 0 {
		return powerDownTimeout
	}
	return e.powerDownTimeout
}

// reporter returns the progress reporter, falling back to a discarding one.
//
// Same nil-safety reasoning as liveness, and the same path: report is an
// interface field on an exported struct, so an Engine built anywhere but New
// carries a nil there -- and fansOffOnceTheRackIsIdle calls Step twice, which
// made `(&Engine{cfg: config.Default()}).fansOffOnceTheRackIsIdle(...)` panic on
// the one path documented as having to neither crash nor fail open. Everything
// in this package goes through here rather than touching report directly.
func (e *Engine) reporter() Reporter {
	if e.report == nil {
		return nopReporter{}
	}
	return e.report
}

// localMounts lists locally mounted NFS filesystems, falling back to the real
// lookup when the seam is unset. Same nil-safety reasoning as liveness.
func (e *Engine) localMounts(ctx context.Context) ([]string, error) {
	if e.nfsMounts == nil {
		return localNFSMounts(ctx)
	}
	return e.nfsMounts(ctx)
}

// powerBackend returns the backend that reaches a host to wake it, run an
// agent verb, or power it off, falling back to the real ssh(1)/UDP mechanism
// when the seam is unset (a hand-built Engine). Same nil-safety reasoning as
// liveness.
func (e *Engine) powerBackend() PowerBackend {
	if e.power == nil {
		return execPower{e}
	}
	return e.power
}

// probeBackend returns the backend for the SSH-reachability half of a probe.
// Ping deliberately stays behind isUp/liveness rather than being read from
// here; see ProbeBackend's doc in backends.go. Same nil-safety reasoning as
// liveness.
func (e *Engine) probeBackend() ProbeBackend {
	if e.probe == nil {
		return execProbe{client: newProbeClient(e.cfg.ProbeTimeout.D())}
	}
	return e.probe
}

// fansBackend returns the backend for the rack-fan Shelly plug, falling back
// to the real HTTP RPC when the seam is unset. Same nil-safety reasoning as
// liveness.
func (e *Engine) fansBackend() FansBackend {
	if e.fans == nil {
		return execFans{shelly: newShellyClient(e.cfg.Inventory.ShellyIP, e.cfg.ResolveShellyPassword)}
	}
	return e.fans
}

// nfsBackend returns the backend for local NFS mounts, falling back to the
// real mount table and umount(8) when the seam is unset. Same nil-safety
// reasoning as liveness.
func (e *Engine) nfsBackend() NFSChecker {
	if e.nfs == nil {
		return execNFS{e}
	}
	return e.nfs
}

// zusbBackend returns the backend for the zusb backup pool's import state,
// falling back to the real agent verbs when the seam is unset. Same
// nil-safety reasoning as liveness.
func (e *Engine) zusbBackend() ZusbChecker {
	if e.zusb == nil {
		return execZusb{e}
	}
	return e.zusb
}

// Config exposes the resolved configuration to callers that need the
// inventory (the CLI's status table, the API's registry).
func (e *Engine) Config() config.Config { return e.cfg }

// logWarnings routes diagnostics from successful agent verbs into log.
//
// Called at the start of each operation that has somewhere to write, because
// the Engine holds no log of its own. Without it these messages are dropped:
// an agent that force-kills a bhyve guest warns on stderr and still exits 0,
// so the warning arrives on the success path or not at all.
func (e *Engine) logWarnings(log io.Writer) {
	e.ssh.warn = func(host, verb, msg string) {
		fmt.Fprintf(log, "  ! %s (%s): %s\n", host, verb, indent(msg))
	}
}

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

	// Un-muting is best-effort: the hosts are already waking, and a monitoring
	// marker left behind is a smaller problem than reporting the whole wake as
	// failed. UnmuteGogios prints how to clear it by hand if it gives up.
	if err := e.UnmuteGogios(ctx, log); err != nil {
		fmt.Fprintf(log, "  (continuing anyway; the hosts are waking)\n")
	}

	fmt.Fprintln(log, "Magic packets sent. The hosts should be reachable in a minute or so.")
	return nil
}

// Off shuts down the k3s bhyve hosts f0/f1/f2, and switches the rack fans off
// only if that leaves nothing running in the rack.
//
// The sequence is ordered so that everything which can refuse does so before
// anything irreversible happens:
//
//  1. unmount local NFS      -- abort while the cluster is still fully up
//  2. export zusb where held -- ditto; needs the host it lives on to be alive
//  3. mute Gogios            -- only now, once the shutdown is going ahead
//  4. stop guests, power off -- host by host, storage master LAST
//  5. fans off               -- only if the whole rack is now silent
//
// Step 5 is not "every host this run touched": f3 is deliberately left running
// by a bare `power off` (see inventory.PowerGroup), and the fan plug cools the
// whole rack. See fansOffOnceTheRackIsIdle.
//
// The host order is not incidental: taking the CARP storage master first fails
// the VIP over onto a host that is itself about to be shut down, which is what
// wedged f1 on 2026-08-08. See inventory.ShutdownOrder.
func (e *Engine) Off(ctx context.Context, log io.Writer) error {
	return e.off(ctx, log, e.cfg.Inventory.ShutdownOrder(), true)
}

// OffAll shuts down every f-host, f3 included, and switches the rack fans off
// once the rack is confirmed silent.
//
// Identical to Off apart from the host set: same NFS and zusb pre-flight, same
// Gogios mute, same storage-master-last ordering, same fans-off only once every
// host has actually gone silent. This is "the whole rack goes dark", which
// previously meant running `power off` and `power f3 off` and remembering that
// only the first of them touches the fans.
//
// Because this one does take f3 down, it is the variant that normally reaches
// the end of fansOffOnceTheRackIsIdle with nothing left answering, and so the
// variant that actually cuts the plug.
func (e *Engine) OffAll(ctx context.Context, log io.Writer) error {
	return e.off(ctx, log, e.cfg.Inventory.ShutdownOrderAll(), true)
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

	// Accepting the command is not the same as completing it. A host can run
	// the whole shutdown sequence and then wedge in the final phase -- after
	// syslogd has exited, so nothing is logged -- leaving it powered on, off
	// the network, and NOT wakeable by Wake-on-LAN, which only wakes a NIC
	// that actually powered down. Recovering from that needs a console or the
	// physical button.
	//
	// f1 did exactly this on 2026-08-08 while f0 and f2 powered off cleanly.
	// Nothing reported a problem at the time: the tool said "shutdown sent"
	// and moved on, and the failure only surfaced later when the host would
	// not wake. Confirming each host actually goes silent turns that into an
	// error at the moment it happens.
	e.reporter().Step("confirming the hosts actually powered down")
	confirmed := tl.track("confirm power-down")
	stuck := e.awaitPowerDown(ctx, log, accepted)
	confirmed()
	if len(stuck) > 0 {
		failed = append(failed, stuck...)
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

// fansOffOnceTheRackIsIdle switches the rack fans off, unless something in the
// rack may still be running. It returns the hosts that kept the fans on, empty
// when the plug was switched off.
//
// The plug cools the whole rack, not merely the hosts a given run shut down,
// and the two are not the same set. A bare `power off` deliberately leaves f3
// up -- f3 is standalone bhyve, not part of k3s, see inventory.PowerGroup --
// so cutting the plug at the end of it left f3 running with no cooling. That
// is precisely what `f3sctl fans off` refuses to do without --force (see
// cli.fansOff), which made the everyday command silently do what the explicit
// one guards against. `power all off` does take f3 down, so by the time it
// reaches here nothing answers and the fans go off exactly as before.
//
// Liveness is re-probed rather than inferred from the shutdown that just ran:
// the hosts this run skipped as already off, and f3, are equally capable of
// generating heat. ICMP does not settle that either -- a host wedged in the
// last phase of a shutdown is powered on with no network, and answers nothing
// (see awaitPowerDown) -- so silence is the weaker claim "nothing here can be
// shown to be running", and the guard is only as good as that. What it does
// buy is that a host which plainly IS running can no longer lose its cooling.
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

// powerHost looks up a host and confirms f3sctl is allowed to power it.
//
// Only the f-hosts qualify. The Pis run this tool, so powering them off would
// remove the only way to power anything back on; the r-VMs follow their bhyve
// host rather than being addressed directly.
func (e *Engine) powerHost(name string) (inventory.Host, error) {
	h, ok := e.cfg.Inventory.ByName(name)
	if !ok {
		return h, fmt.Errorf("unknown host %q", name)
	}
	if h.Role != inventory.RoleF {
		return h, fmt.Errorf("%s is not a host f3sctl can power (only f0-f3 are)", name)
	}
	return h, nil
}

// indent prefixes continuation lines of remote output so it reads as nested
// under the host it came from.
func indent(s string) string {
	out := ""
	for i, line := range splitLines(s) {
		if i > 0 {
			out += "\n  "
		}
		out += line
	}
	return out
}

func splitLines(s string) []string {
	var lines []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			lines = append(lines, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}
