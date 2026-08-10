// Package cli implements f3sctl's command line: a noun-verb surface over the
// same power.Engine the HTTP API uses.
//
// The spelling is a clean break from the bash predecessor wol-f3s, whose verbs
// had drifted into confusion -- "all" meant "wake f0/f1/f2 but not f3", and
// "shutdown" and "shutdown-all" differed by whether they also stopped the
// Raspberry Pis. Old spellings are rejected outright rather than aliased, so
// muscle memory fails loudly instead of doing something unintended.
package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/snonux/f3sctl/internal"
	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/power"
	"github.com/snonux/f3sctl/internal/presenter"
)

const usage = `f3sctl - control the f3s homelab

Usage:
  f3sctl power status          Probe f0-f3 and the k3s nodes
  f3sctl power on              Fans on, wake f0/f1/f2, un-mute Gogios
  f3sctl power off             Export zusb, mute Gogios, stop guests, power off f0/f1/f2
                               (goes through the API: only pi0/pi1 may shut hosts down;
                               the fans stay on unless nothing in the rack answers,
                               so normally they keep running -- f3 does)
  f3sctl power all on          Fans on, wake f0/f1/f2/f3, un-mute Gogios
  f3sctl power all off         As "power off", but f3 too: the whole rack dark, then
                               fans off once nothing answers
  f3sctl power f0|f1|f2|f3 on  Wake one host only
  f3sctl power f0|f1|f2|f3 off Power off one host only (fans and Gogios untouched)
  f3sctl fans status           Rack-fan Shelly plug state
  f3sctl fans on               Switch the rack fans on
  f3sctl fans off [--force]    Switch the rack fans off
  f3sctl monitoring status     Is Gogios alerting muted?
  f3sctl monitoring mute       Suppress Gogios alerting
  f3sctl monitoring unmute     Resume Gogios alerting (clears a stranded mute)
  f3sctl version               Print the version

Global flags:
  --remote, -r   Drive the command through the HTTP API on pi0/pi1 instead of
                 acting locally. Required when off the LAN: a Wake-on-LAN
                 magic packet is not routed, so only a host on the homelab
                 broadcast domain can send one. Needs api_url and an API key
                 (config, or F3SCTL_URL / F3SCTL_KEY).
  --local, -l    Act on the homelab directly instead of calling the API. Only
                 works where an authorised SSH key exists, i.e. on pi0/pi1:
                 the key is pinned to those two hosts. Use for debugging or
                 when running on a Pi.
  --force, -f    Confirm an action the server guards, e.g. switching the rack
                 fans off while hosts are still running.
  --verbose, -v  Trace every API call to stderr: method, URL, status, which of
                 pi0/pi1 answered, and how long it took. Implies --remote,
                 since there is nothing to trace when acting locally.

f3sctl powers only the FreeBSD bhyve hosts f0-f3. It never powers a Raspberry
Pi: pi0 and pi1 are where it runs, and powering them off would remove the only
way to power anything back on.
`

// liveHostsFunc reports which f-hosts may still be drawing power, by name.
//
// "May still be": power.Engine.LiveHosts includes hosts it could not probe at
// all, because this feeds a cooling guard and an unprobeable host is not a host
// known to be off.
//
// The rack-fan guard in fansOff is its only consumer. It is a function rather
// than a direct call to power.Engine.LiveHosts so the guard can be exercised
// without real ICMP: LiveHosts shells out to ping(8), which would make the
// guard's tests depend on the machine's network stack and on packets actually
// leaving the box. Same reasoning as power.Engine.isUp.
type liveHostsFunc func(ctx context.Context) []string

// Run executes one CLI invocation.
func Run(cfg config.Config, args []string, stdout, stderr io.Writer) error {
	return run(cfg, args, stdout, stderr, nil, false, nil)
}

// RunLocal executes an invocation that must act on the homelab directly,
// never through the API.
//
// This is what the API's own detached child uses. It has to bypass the routing
// in globalFlags.useAPI: a shutdown started by the API which then called the
// API would simply recurse.
func RunLocal(cfg config.Config, args []string, stdout, stderr io.Writer, reporter power.Reporter) error {
	return run(cfg, args, stdout, stderr, reporter, true, nil)
}

// run executes one invocation.
//
// liveHosts is the seam the rack-fan guard consults; a nil one means the real
// ICMP probe on the engine runFans builds, which is what both exported entry
// points pass. Only the tests substitute anything else.
func run(cfg config.Config, args []string, stdout, stderr io.Writer,
	reporter power.Reporter, forceLocal bool, liveHosts liveHostsFunc) error {

	args, flags := parseGlobalFlags(args)
	if forceLocal {
		flags.local = true
	}

	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errUsage
	}

	// Shutdowns go through the API by default, from anywhere: only pi0/pi1
	// hold a key the f-hosts will accept, so this is the difference between
	// the same command working everywhere and failing on a laptop. See
	// globalFlags.useAPI.
	if args[0] != "version" && args[0] != "help" && flags.useAPI(args) {
		return runRemote(cfg, args, flags, stdout, stderr)
	}

	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "f3sctl %s\n", internal.Version)
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	case "power":
		return runPower(cfg, args[1:], stdout, stderr, reporter)
	case "fans":
		return runFans(cfg, args[1:], flags.force, liveHosts, stdout, stderr)
	case "monitoring":
		return runMonitoring(cfg, args[1:], stdout, stderr)
	}

	if hint := retiredVerbHint(args[0]); hint != "" {
		fmt.Fprintf(stderr, "%s\n\n", hint)
	}
	fmt.Fprint(stderr, usage)
	return fmt.Errorf("unknown command %q", args[0])
}

func runPower(cfg config.Config, args []string, stdout, stderr io.Writer, reporter power.Reporter) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errUsage
	}

	// Resolve before building an Engine or touching the SSH identity: what was
	// asked for is decided from the arguments alone, and a spelling nobody
	// accepts must cost nothing and change nothing.
	act, err := powerActionFor(args)
	if err != nil {
		fmt.Fprint(stderr, usage)
		return err
	}

	// A local shutdown needs a key the f-hosts will accept, and that key is
	// pinned to pi0/pi1. Say so up front rather than letting it surface as an
	// unreadable-file error part way through the sequence, after the NFS and
	// zusb checks have already run.
	if isShutdown(append([]string{"power"}, args...)) {
		if _, idErr := cfg.ResolveSSHIdentity(); idErr != nil {
			return fmt.Errorf("cannot shut hosts down from here: %w.\n"+
				"The f3sctl key is pinned to pi0/pi1, so only they may do this. "+
				"Drop --local to go through the API instead", idErr)
		}
	}

	eng, err := power.New(cfg)
	if err != nil {
		return err
	}
	eng.WithReporter(reporter)
	return act(context.Background(), eng, stdout)
}

// powerEngine is the subset of *power.Engine methods a powerAction may call.
//
// It exists as a test seam, not for production flexibility: Off, OffAll and
// OffHost dial real SSH connections on a genuine *power.Engine, so pinning
// which method powerActionFor bound to a given spelling (see
// TestPowerActionForBindsTheRightMethod in cli_test.go) needs a fake standing
// in for the engine rather than driving real SSH. *power.Engine already has
// every method below with a matching signature, so it satisfies this
// interface without any change on its side.
type powerEngine interface {
	On(ctx context.Context, log io.Writer) error
	Off(ctx context.Context, log io.Writer) error
	OnAll(ctx context.Context, log io.Writer) error
	OffAll(ctx context.Context, log io.Writer) error
	OnHost(ctx context.Context, log io.Writer, name string) error
	OffHost(ctx context.Context, log io.Writer, name string) error
	ProbeAll(ctx context.Context) []power.HostStatus
	FansStatus(ctx context.Context) (power.FansState, error)
}

// powerAction is one resolved power operation, ready to run against a
// powerEngine. printStatus has exactly this shape, so the read-only verb
// needs no adapter.
type powerAction func(ctx context.Context, eng powerEngine, out io.Writer) error

// rackOp adapts a whole-rack powerEngine method (On, Off, OnAll, OffAll) to a
// powerAction. Passing a method expression on the powerEngine interface (e.g.
// powerEngine.On) keeps the table in powerActionForSpelling to one line per
// accepted spelling.
func rackOp(op func(powerEngine, context.Context, io.Writer) error) powerAction {
	return func(ctx context.Context, eng powerEngine, out io.Writer) error {
		return op(eng, ctx, out)
	}
}

// hostOp adapts a single-host powerEngine method (OnHost, OffHost) to a
// powerAction, binding the host name the command named.
func hostOp(op func(powerEngine, context.Context, io.Writer, string) error, name string) powerAction {
	return func(ctx context.Context, eng powerEngine, out io.Writer) error {
		return op(eng, ctx, out, name)
	}
}

// powerSpelling is the parsed shape of a `power` argument list: which verb it
// names and, for a per-host or per-group command, which target.
//
// It exists so the remote-routing decision (isShutdown, in remote.go) and the
// local dispatch (powerActionFor, below) are both built from one parse of the
// arguments rather than two independent ones that could disagree. They used
// to disagree: isShutdown checked only that a 3-token command ended in "off",
// ignoring the middle word entirely, so malformed spellings like
// `power on off` and `power status off` were classified as legitimate
// shutdowns and routed straight to the API, never reaching powerActionFor's
// validation at all.
type powerSpelling struct {
	verb   string // "status", "on", or "off"
	target string // "" (whole rack), "all", or a host name
}

// parsePowerArgs parses a `power` argument list -- args with the leading
// "power" token already stripped -- into the one spelling it names. ok is
// false for anything not documented: wrong arity, trailing junk, or a verb
// where a host name belongs (`power off on`, `power status on`).
func parsePowerArgs(args []string) (sp powerSpelling, ok bool) {
	switch len(args) {
	case 1:
		if isPowerVerb(args[0]) {
			return powerSpelling{verb: args[0]}, true
		}

	case 2:
		switch {
		// "all" is a group, not a host name, so it is matched before the
		// per-host spellings below.
		case args[0] == "all" && (args[1] == "on" || args[1] == "off"):
			return powerSpelling{verb: args[1], target: "all"}, true
		case isPowerVerb(args[0]):
			// A verb where a host name belongs: `power off on`, `power status
			// on`. Deliberately matched and rejected here, rather than falling
			// into the per-host case below, so it is reported as the malformed
			// spelling it is rather than as `unknown host "off"`.
		case args[1] == "on" || args[1] == "off":
			return powerSpelling{verb: args[1], target: args[0]}, true
		}
	}

	return powerSpelling{}, false
}

// isPowerVerb reports whether w names an operation rather than a host.
func isPowerVerb(w string) bool {
	return w == "on" || w == "off" || w == "status"
}

// powerActionFor resolves a `power` argument list to the one operation it
// names, or reports that it names none.
//
// Arity is part of every match via parsePowerArgs, and that is the whole
// reason this exists rather than a switch on args[0]: dispatching on the
// first word alone made `power on f3` run the CLUSTER-WIDE wake and silently
// drop "f3" on the floor, and `power off f2` power the whole cluster off. The
// documented per-host spelling is `power f3 on`, so writing the words the
// other way round is an easy mistake to make -- and it was answered by doing
// something far larger than asked, with no warning. A near-miss is a usage
// error here, never a cluster-wide action.
//
// Trailing junk is rejected for the same reason: `power status f0` asks for
// something this tool does not do (printStatus always probes the whole
// inventory), so it must say so rather than quietly print the full table.
func powerActionFor(args []string) (powerAction, error) {
	sp, ok := parsePowerArgs(args)
	if !ok {
		return nil, fmt.Errorf("unknown power command %q", strings.Join(args, " "))
	}
	return powerActionForSpelling(sp), nil
}

// powerActionForSpelling maps an already-validated powerSpelling onto the
// Engine method it names. Split out of powerActionFor so that function stays
// about parsing+validating and this one about the on/off, rack/group/host
// method table.
func powerActionForSpelling(sp powerSpelling) powerAction {
	switch {
	case sp.target == "" && sp.verb == "status":
		return printStatus
	case sp.target == "" && sp.verb == "on":
		return rackOp(powerEngine.On)
	case sp.target == "" && sp.verb == "off":
		return rackOp(powerEngine.Off)
	case sp.target == "all" && sp.verb == "on":
		return rackOp(powerEngine.OnAll)
	case sp.target == "all" && sp.verb == "off":
		return rackOp(powerEngine.OffAll)
	case sp.verb == "on":
		return hostOp(powerEngine.OnHost, sp.target)
	default: // sp.verb == "off"
		return hostOp(powerEngine.OffHost, sp.target)
	}
}

// runFans reads or switches the rack-fan plug.
//
// force arrives from the global flag parser rather than from args: --force is
// stripped out of args by parseGlobalFlags before any command sees them, so
// re-deriving it here would always see nothing and the thermal guard in fansOff
// could never be overridden.
//
// liveHosts is that guard's view of what is still running. A nil one means ask
// the engine over ICMP, which is what production does.
func runFans(cfg config.Config, args []string, force bool, liveHosts liveHostsFunc,
	stdout, stderr io.Writer) error {

	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errUsage
	}

	eng, err := power.New(cfg)
	if err != nil {
		return err
	}
	if liveHosts == nil {
		liveHosts = eng.LiveHosts
	}
	ctx := context.Background()

	switch args[0] {
	case "status":
		st, err := eng.FansStatus(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "rack fans: %s (%s)\n", presenter.OnOff(st.On), st.IP)
		return nil

	case "on":
		st, err := eng.FansSet(ctx, true)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "rack fans: %s\n", presenter.OnOff(st.On))
		return nil

	case "off":
		return fansOff(ctx, eng, force, liveHosts, stdout)
	}

	fmt.Fprint(stderr, usage)
	return fmt.Errorf("unknown fans command %q", args[0])
}

// runMonitoring reads or changes the Gogios mute on both gateways.
//
// Separate from `power` on purpose: the mute outlives the operation that set
// it, and clearing a stranded one must not require powering anything.
func runMonitoring(cfg config.Config, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errUsage
	}

	eng, err := power.New(cfg)
	if err != nil {
		return err
	}
	ctx := context.Background()

	switch args[0] {
	case "status":
		printMonitoring(stdout, eng.MonitoringStatus(ctx))
		return nil
	case "mute":
		if err := eng.MuteGogios(ctx, stdout); err != nil {
			return err
		}
	case "unmute":
		if err := eng.UnmuteNow(ctx, stdout); err != nil {
			return err
		}
	default:
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown monitoring command %q", args[0])
	}

	printMonitoring(stdout, eng.MonitoringStatus(ctx))
	return nil
}

func printMonitoring(out io.Writer, states []power.GatewayMute) {
	for _, gw := range states {
		switch {
		case gw.Err != nil:
			// Unreachable is not "alerting is fine" -- say which it is.
			fmt.Fprintf(out, "%s: unknown (%v)\n", gw.Name, gw.Err)
		case gw.Muted:
			fmt.Fprintf(out, "%s: MUTED\n", gw.Name)
		default:
			fmt.Fprintf(out, "%s: alerting\n", gw.Name)
		}
	}
}

// fansOff refuses to cut the rack fans while a host is still answering, unless
// told explicitly to.
//
// The f-hosts switch this plug on at boot precisely so the fans run whenever
// any host does, so switching it off under a running rack is a thermal risk
// rather than a preference.
//
// force is the parsed --force/-f global flag and liveHosts the liveness probe,
// both threaded down from run rather than derived here: see runFans for why
// force cannot be re-read from the arguments, and the liveHostsFunc type for
// why the probe is a seam rather than a direct engine call.
//
// The guard is slow on an idle rack, by design: the probe behind it wants
// several consecutive missed pings per host before it will call one off, so
// that a single dropped echo reply cannot cut cooling to a running rack. Half a
// minute is a fair price for that, and --force skips it for anyone who is sure.
func fansOff(ctx context.Context, eng *power.Engine, force bool,
	liveHosts liveHostsFunc, stdout io.Writer) error {

	if !force {
		if up := liveHosts(ctx); len(up) > 0 {
			return fmt.Errorf("%v may still be running; refusing to switch the rack fans off. "+
				"Use --force if you mean it", up)
		}
	}

	st, err := eng.FansSet(ctx, false)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "rack fans: %s\n", presenter.OnOff(st.On))
	return nil
}

// retiredVerbHint maps a wol-f3s spelling to its replacement, so the clean
// break is a signpost rather than a dead end.
func retiredVerbHint(v string) string {
	switch v {
	case "all":
		return "`all` was wol-f3s. Use: f3sctl power on"
	case "shutdown", "poweroff", "down":
		return "`shutdown` was wol-f3s. Use: f3sctl power off"
	case "shutdown-f3", "poweroff-f3", "down-f3":
		return "`shutdown-f3` was wol-f3s. Use: f3sctl power f3 off"
	case "f0", "f1", "f2", "f3":
		return fmt.Sprintf("wol-f3s took a bare host name. Use: f3sctl power %s on", v)
	case "shutdown-pis", "shutdown-all":
		return "f3sctl does not power the Raspberry Pis. Shut them down by hand if you need to."
	}
	return ""
}

// errUsage signals that usage has already been printed.
var errUsage = fmt.Errorf("no command given")
