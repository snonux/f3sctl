package cli

// The Shelly plug nouns: `fans` (the rack-fan plug) and `ac` (the f-host mains
// AC plug, shelly2), and the liveness guard both consult before switching off.
//
// The two nouns are the same command over a different plug: one grammar
// (status|on|off), one output shape, and one off guard that refuses while a
// host may still be running or a power job is in flight. What differs --
// which engine methods reach the plug, which hosts count as running, and the
// wording -- is carried by a plug value (fansPlug, acPlug) rather than by a
// twin copy of every function.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/jobcoord"
	"github.com/snonux/f3sctl/internal/power"
	"github.com/snonux/f3sctl/internal/presenter"
)

// liveHostsFunc reports which f-hosts may still be drawing power, by name.
//
// "May still be": power.Engine.LiveHosts includes hosts it could not probe at
// all, because this feeds a cooling guard and an unprobeable host is not a host
// known to be off.
//
// plugOff's guard is its consumer, for the rack fans and f-host AC alike.
// It is a function rather than a direct call to power.Engine.LiveHosts /
// ACActivity so the guards can be exercised without real ICMP: those methods
// shell out to ping(8), which would make the guard's tests depend on the
// machine's network stack and on packets actually leaving the box. Same
// reasoning as power.Engine.isUp.
type liveHostsFunc func(ctx context.Context) []string

// plugEngine is the subset of *power.Engine methods the plug commands call.
//
// Like powerEngine (cli.go), it is a test seam rather than production
// flexibility: it lets plugOff's guard be pinned against a fake that records
// every switch, without a Shelly (fake or real) on the other end.
// *power.Engine satisfies it unchanged.
type plugEngine interface {
	FansStatus(ctx context.Context) (power.FansState, error)
	FansSet(ctx context.Context, on bool) (power.FansState, error)
	ACStatus(ctx context.Context) (power.ACState, error)
	ACSet(ctx context.Context, on bool) (power.ACState, error)
}

// plugState is what either plug reports. power.FansState and power.ACState
// share this exact shape, so each converts to it directly.
type plugState struct {
	On bool
	IP string
}

// plug is one Shelly plug as the CLI drives it: everything that differs
// between `fans` and `ac`.
type plug struct {
	noun    string // the CLI noun, for "unknown <noun> command"
	label   string // the output prefix, e.g. "rack fans: on"
	subject string // what guardInterrupted says was left untouched
	refusal string // the busy-rack refusal, after "<hosts> may still be running; "

	status func(ctx context.Context, eng plugEngine) (plugState, error)
	set    func(ctx context.Context, eng plugEngine, on bool) (plugState, error)
	// liveHosts is the production off guard's probe, used when run was given
	// no liveHostsFunc of its own.
	liveHosts func(eng *power.Engine) liveHostsFunc
}

// fansPlug is the rack-fan plug.
//
// The f-hosts switch this plug on at boot precisely so the fans run whenever
// any host does, so switching it off under a running rack is a thermal risk
// rather than a preference. Its guard counts the fan-cooled power group
// (f0/f1/f2) only: the plug does not cool f3.
var fansPlug = plug{
	noun:    "fans",
	label:   "rack fans",
	subject: "the rack fans",
	refusal: "refusing to switch the rack fans off. Use --force if you mean it",
	status: func(ctx context.Context, eng plugEngine) (plugState, error) {
		st, err := eng.FansStatus(ctx)
		return plugState(st), err
	},
	set: func(ctx context.Context, eng plugEngine, on bool) (plugState, error) {
		st, err := eng.FansSet(ctx, on)
		return plugState(st), err
	},
	liveHosts: func(eng *power.Engine) liveHostsFunc { return eng.LiveHosts },
}

// acPlug is the f-host mains AC plug (shelly2).
//
// Independent of power on/off: restoring or cutting AC is never an automatic
// side-effect of a wake or shutdown. Its guard counts every f-host (f0–f3),
// because shelly2 powers all of them and hard-cutting mains under a live host
// risks ZFS / bhyve damage -- use --force only after a graceful shutdown (or
// when you mean a last-resort kill).
var acPlug = plug{
	noun:    "ac",
	label:   "f-host AC",
	subject: "f-host AC",
	refusal: "refusing to cut f-host AC. " +
		"Shut the hosts down first (f3sctl power off / power all off), " +
		"or use --force if you mean a hard cut",
	status: func(ctx context.Context, eng plugEngine) (plugState, error) {
		st, err := eng.ACStatus(ctx)
		return plugState(st), err
	},
	set: func(ctx context.Context, eng plugEngine, on bool) (plugState, error) {
		st, err := eng.ACSet(ctx, on)
		return plugState(st), err
	},
	liveHosts: func(eng *power.Engine) liveHostsFunc {
		return func(ctx context.Context) []string { return eng.ACActivity(ctx).Hosts() }
	},
}

// runPlug reads or switches one plug.
//
// force arrives from the global flag parser rather than from args: --force is
// stripped out of args by parseGlobalFlags before any command sees them, so
// re-deriving it here would always see nothing and the guard in plugOff could
// never be overridden.
//
// liveHosts is that guard's view of what is still running. A nil one means the
// plug's own probe on the engine built here, which is what production does.
func runPlug(ctx context.Context, cfg config.Config, p plug, args []string, force bool,
	liveHosts liveHostsFunc, stdout, stderr io.Writer) error {

	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errUsage
	}

	// Resolved before building an Engine or touching the Shelly password: what
	// was asked for is decided from the arguments alone, the same reasoning as
	// powerActionFor. A plug has no per-host concept, so `fans on f0` used to
	// dispatch on args[0] alone, silently switch the WHOLE rack's fans on, and
	// drop "f0" on the floor -- a misleading success rather than an error.
	verb, ok := parsePlugArgs(args)
	if !ok {
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown %s command %q", p.noun, strings.Join(args, " "))
	}

	eng, err := power.New(cfg)
	if err != nil {
		return err
	}
	if liveHosts == nil {
		liveHosts = p.liveHosts(eng)
	}
	// Only a guarded off consults the job state: on is never gated, and
	// --force skips every guard, so neither reads the API key or asks a peer.
	var jobs jobGuard
	if verb == "off" && !force {
		jobs = newJobGuard(cfg)
	}
	return plugVerb(ctx, eng, p, verb, force, liveHosts, jobs, stdout)
}

// plugVerb performs one already-parsed plug verb against eng. jobs is only
// consulted by a guarded off, and may be nil otherwise.
func plugVerb(ctx context.Context, eng plugEngine, p plug, verb string, force bool,
	liveHosts liveHostsFunc, jobs jobGuard, stdout io.Writer) error {

	switch verb {
	case "status":
		st, err := p.status(ctx, eng)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s: %s (%s)\n", p.label, presenter.OnOff(st.On), st.IP)
		return nil

	case "on":
		// Never gated: more cooling, or mains restored, is never the risky
		// direction, so neither liveness nor the job state is consulted. That
		// also keeps `ac on` / `fans on` the way out when a job's process died
		// and its job.json still reads "running" -- the recovery path
		// docs/CLIENT.md ("Stuck job: recovering the plugs") points at.
		return plugSwitch(ctx, eng, p, true, stdout)

	default: // "off", the only spelling parsePlugArgs has left standing
		return plugOff(ctx, eng, p, force, liveHosts, jobs, stdout)
	}
}

// parsePlugArgs parses a `fans` or `ac` argument list -- args with the leading
// noun already stripped -- into the one verb it names. ok is false for
// anything not documented: wrong arity (a plug takes exactly one word, since
// it has no per-host or --force-in-args concept) or an unknown word. This is
// what turns both trailing junk (`fans on f0`) and a misspelling (`fans of`)
// into a usage error instead of a guess.
func parsePlugArgs(args []string) (verb string, ok bool) {
	if len(args) != 1 {
		return "", false
	}
	switch args[0] {
	case "status", "on", "off":
		return args[0], true
	}
	return "", false
}

// plugOff refuses to switch a plug off while a host it serves may still be
// answering, or while a power job is running, unless told explicitly to.
//
// force is the parsed --force/-f global flag and liveHosts the liveness probe,
// both threaded down from run rather than derived here: see runPlug for why
// force cannot be re-read from the arguments, and the liveHostsFunc type for
// why the probe is a seam rather than a direct engine call. --force skips both
// guards, the job one included.
//
// The guard is slow on an idle rack, by design: the probe behind it wants
// several consecutive missed pings per host before it will call one off, so
// that a single dropped echo reply cannot cut cooling or mains to a running
// rack. Half a minute is a fair price for that, and --force skips it for
// anyone who is sure.
//
// The job guard mirrors the API's fans-off / ac-off: the rack-wide jobs switch
// the fan plug themselves and `power all cycle` cuts and restores AC, so a
// switch mid-job races them -- worst of all an `ac off` in a cycle's standby
// wait, when the hosts are silent and the liveness guard has nothing to say.
// It is asked twice, as the API asks: once up front, so a busy rack is refused
// before the half-minute probe, and again after the probe with the switch
// itself inside it (jobGuard.whileIdle holds this node's job lock across the
// write, so a job cannot start under it). The second answer comes before the
// probe's verdict: a job waking the hosts makes the probe hear them, and "use
// --force" would then be exactly the wrong advice.
func plugOff(ctx context.Context, eng plugEngine, p plug, force bool,
	liveHosts liveHostsFunc, jobs jobGuard, stdout io.Writer) error {

	if force {
		return plugSwitch(ctx, eng, p, false, stdout)
	}

	if err := jobs.whileIdle(ctx, func() error { return nil }); err != nil {
		return jobRefusal(p, err)
	}
	up := liveHosts(ctx)
	if err := ctx.Err(); err != nil {
		return guardInterrupted(p.subject, err)
	}

	// ran tells the guard's own refusal from fn's errors, which are the
	// liveness refusal and the plug write and must come back unchanged.
	ran := false
	err := jobs.whileIdle(ctx, func() error {
		ran = true
		if len(up) > 0 {
			return fmt.Errorf("%v may still be running; %s", up, p.refusal)
		}
		return plugSwitch(ctx, eng, p, false, stdout)
	})
	if err != nil && !ran {
		return jobRefusal(p, err)
	}
	return err
}

// plugSwitch switches p and reports the state the plug read back.
func plugSwitch(ctx context.Context, eng plugEngine, p plug, on bool, stdout io.Writer) error {
	st, err := p.set(ctx, eng, on)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s: %s\n", p.label, presenter.OnOff(st.On))
	return nil
}

// guardInterrupted is plugOff's error for a Ctrl-C during its liveness probe.
// Checked before the probe's verdict: probes the cancel cut short read as
// unknown, i.e. running, and "may still be running ... use --force" would send
// the operator after hosts nobody saw.
func guardInterrupted(subject string, err error) error {
	return fmt.Errorf("interrupted while checking whether the f-hosts are off; %s left "+
		"untouched: %w", subject, err)
}

// jobRefusal is plugOff's error when the job guard would not let the switch
// run: a job in flight, or a job state it could not establish. Neither
// switched anything. The guard's own error says what to fix in the second
// case (see coordGuard).
func jobRefusal(p plug, err error) error {
	if errors.Is(err, coordination.ErrJobRunning) {
		return fmt.Errorf("%w; %s left untouched. Wait for it to finish "+
			"(`f3sctl -r power status` shows the rack meanwhile), "+
			"or use --force if you mean it", err, p.subject)
	}
	return fmt.Errorf("cannot tell whether a power job is running: %w; %s left untouched. "+
		"Fix that, or use --force if you mean it", err, p.subject)
}

// jobGuard is plugOff's view of the power jobs: whileIdle runs fn only while
// no job is running, and keeps one from starting on this node until fn has
// returned. An error from the guard itself wraps
// coordination.ErrJobRunning when a job is in flight; fn's own error comes
// back unchanged.
//
// A seam for the same reason liveHostsFunc is one: the production guard reads
// job state from disk and asks the other API nodes over HTTP.
type jobGuard interface {
	whileIdle(ctx context.Context, fn func() error) error
}

// coordGuard is the production jobGuard, built from the same coordination
// pieces the API's own plug routes use: the API nodes' PeerSet first, then
// this host's Manager, the order powerapi's jobStartedMeanwhile asks them
// in, so the local check and lock sit right against the write.
//
// Like the API it can lock this host only: the peers cannot be locked, just
// asked. Unlike the API it asks them strictly (PeerSet.RunningJob): a peer
// that cannot be reached at all counts as idle, so a dead Pi cannot block
// the plugs, but one that answers with anything but a job -- a 401 for a
// wrong or rotated key, a 404 for a wrong path -- refuses. So does having no
// API key to ask with: on a laptop, which has no job state of its own, the
// peers are the whole answer, and not asking them would be no check at all.
// Not knowing is not idle; --force is the way past it.
type coordGuard struct {
	jobs     *coordination.Manager
	stateDir string
	peers    *coordination.PeerSet // nil when cfg lists no peer nodes
	apiKey   string
	keyErr   error // why there is no apiKey, when peers are to be asked
}

func (g coordGuard) whileIdle(ctx context.Context, fn func() error) error {
	if g.peers != nil {
		if g.keyErr != nil {
			return fmt.Errorf("no API key to ask the API nodes %v with (%w); set F3SCTL_KEY "+
				"or api_key_file", g.peers.Nodes, g.keyErr)
		}
		// "" is this host: PeerSet then skips its own addresses.
		job, err := g.peers.RunningJob(ctx, "", g.apiKey)
		if err != nil {
			return fmt.Errorf("%w; check that the API key (F3SCTL_KEY / api_key_file) is one "+
				"the API nodes accept, and peer_job_path", err)
		}
		if job != nil {
			return coordination.RunningJobError(*job)
		}
	}
	// ran keeps fn's own errors (the liveness refusal, the plug write) from
	// being dressed up as an unreadable job state.
	ran := false
	err := g.jobs.WhileIdle(func() error { ran = true; return fn() })
	if err != nil && !ran && !errors.Is(err, coordination.ErrJobRunning) {
		return fmt.Errorf("this host's job state in %s: %w; run as a user that can read it",
			g.stateDir, err)
	}
	return err
}

// newJobGuard builds the production jobGuard from cfg: this host's job state
// in cfg.StateDir and the API nodes in cfg.PeerNodes, both built by jobcoord
// exactly as the API builds its own, asked with this CLI's API key. A
// missing key is not an error here but on use, so that only a guarded off
// -- the one caller -- reports it.
func newJobGuard(cfg config.Config) jobGuard {
	g := coordGuard{jobs: jobcoord.ManagerFor(cfg), stateDir: cfg.StateDir}
	if len(cfg.PeerNodes) == 0 {
		return g
	}
	g.peers = jobcoord.PeersFor(cfg, "")
	g.apiKey, g.keyErr = cfg.ResolveAPIKey()
	return g
}
