// Package power implements everything f3sctl actually does to the homelab:
// waking hosts, shutting them down safely, probing their state, and switching
// the rack fans.
//
// It is the single implementation behind both the CLI and the HTTP API, so the
// two cannot disagree about what "power off" means.
package power

import (
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
	cfg config.Config

	// ssh runs agent verbs over ssh(1). Read it through sshRunner, never
	// directly: a hand-built Engine carries a nil here, and sshRunner fills it
	// in exactly once (sshInit) so the warn hook logWarnings installs and the
	// lazily resolved identity survive across calls -- unlike the stateless
	// backends below, a fresh runner per call would silently drop both.
	ssh     *runner
	sshInit sync.Once

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

	// acOffDwell and acSettleWait are CycleAll's two fixed waits: how long AC
	// stays cut, and how long the NICs get on standby power before the magic
	// packets go out. Fields for the same reason as powerDownTimeout; read
	// them through acDwell and acSettle. New wires them to the constants of
	// the same names (see cycle.go).
	acOffDwell   time.Duration
	acSettleWait time.Duration

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
	ac    ACBackend
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
// execPower, execProbe, execFans, execAC, execNFS, execZusb -- for the same reason a
// composition root always is: something has to choose the real mechanism
// before the interfaces in backends.go can be used for anything. That is not
// a gap in the OCP story backends.go describes, it is the other half of it:
// off, on, shutdownEach, fansOffOnceTheRackIsIdle and the rest of Engine's
// policy methods depend only on PowerBackend/ProbeBackend/FansBackend/
// ACBackend/NFSChecker/ZusbChecker, never on the Shelly RPC client, magicPacket or
// ssh(1) directly, so a second fan switch or wake mechanism (IPMI, say) is a
// new type implementing the relevant interface plus a few lines here choosing
// it -- never an edit to those policy methods. Exactly this seam is what
// backends_test.go/engine_test.go already exercise, substituting fakes for
// every one of these fields; nothing about a real second implementation
// would work any differently.
//
// New deliberately takes no options to pick among adapters: this project has
// exactly two Shelly plugs (fans on shelly1, f-host AC on shelly2) and one
// wake mechanism (WoL), and no second implementation of either is asked for
// anywhere in this codebase.
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
		acOffDwell:        acOffDwell,
		acSettleWait:      acSettleWait,
	}
	e.isUp = e.pingOnce
	e.power = execPower{e}
	e.probe = execProbe{client: newProbeClient(cfg.ProbeTimeout.D())}
	e.fans = execFans{shelly: newShellyClient(cfg.Inventory.ShellyIP, cfg.ResolveShellyPassword)}
	e.ac = execAC{shelly: newShellyClient(cfg.Inventory.ShellyACIP, cfg.ResolveShellyPassword)}
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

// acBackend returns the backend for the f-host mains AC Shelly plug, falling
// back to the real HTTP RPC when the seam is unset. Same nil-safety
// reasoning as fansBackend.
func (e *Engine) acBackend() ACBackend {
	if e.ac == nil {
		return execAC{shelly: newShellyClient(e.cfg.Inventory.ShellyACIP, e.cfg.ResolveShellyPassword)}
	}
	return e.ac
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

// sshRunner returns the SSH runner, creating one when the field is unset (a
// hand-built Engine). Same nil-safety reasoning as liveness; it stores the
// fallback rather than returning a throwaway because the runner is stateful
// (see the ssh field). The sync.Once makes that first store safe against
// shutdownTogether's goroutines reaching it concurrently.
func (e *Engine) sshRunner() *runner {
	e.sshInit.Do(func() {
		if e.ssh == nil {
			e.ssh = newRunner(e.cfg)
		}
	})
	return e.ssh
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
	e.sshRunner().warn = func(host, verb, msg string) {
		fmt.Fprintf(log, "  ! %s (%s): %s\n", host, verb, indent(msg))
	}
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
// under the host it came from. One trailing newline is dropped first, so
// output ending in the usual '\n' does not leave a dangling indented line.
func indent(s string) string {
	return strings.ReplaceAll(strings.TrimSuffix(s, "\n"), "\n", "\n  ")
}
