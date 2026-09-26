package cli

// The Shelly plug nouns: `fans` (the rack-fan plug) and `ac` (the f-host mains
// AC plug, shelly2), and the liveness guard both consult before switching off.

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/power"
	"github.com/snonux/f3sctl/internal/presenter"
)

// liveHostsFunc reports which f-hosts may still be drawing power, by name.
//
// "May still be": power.Engine.LiveHosts includes hosts it could not probe at
// all, because this feeds a cooling guard and an unprobeable host is not a host
// known to be off.
//
// The rack-fan guard in fansOff and the AC guard in acOff are its consumers.
// It is a function rather than a direct call to power.Engine.LiveHosts /
// ACActivity so the guards can be exercised without real ICMP: those methods
// shell out to ping(8), which would make the guard's tests depend on the
// machine's network stack and on packets actually leaving the box. Same
// reasoning as power.Engine.isUp.
type liveHostsFunc func(ctx context.Context) []string

// runFans reads or switches the rack-fan plug.
//
// force arrives from the global flag parser rather than from args: --force is
// stripped out of args by parseGlobalFlags before any command sees them, so
// re-deriving it here would always see nothing and the thermal guard in fansOff
// could never be overridden.
//
// liveHosts is that guard's view of what is still running. A nil one means ask
// the engine over ICMP, which is what production does.
func runFans(ctx context.Context, cfg config.Config, args []string, force bool, liveHosts liveHostsFunc,
	stdout, stderr io.Writer) error {

	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errUsage
	}

	// Resolved before building an Engine or touching the Shelly password: what
	// was asked for is decided from the arguments alone, the same reasoning as
	// powerActionFor. Fans has no per-host concept, so `fans on f0` used to
	// dispatch on args[0] alone, silently switch the WHOLE rack's fans on, and
	// drop "f0" on the floor -- a misleading success rather than an error.
	verb, ok := parseFansArgs(args)
	if !ok {
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown fans command %q", strings.Join(args, " "))
	}

	eng, err := power.New(cfg)
	if err != nil {
		return err
	}
	if liveHosts == nil {
		liveHosts = eng.LiveHosts
	}

	switch verb {
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

	default: // "off", the only spelling parseFansArgs has left standing
		return fansOff(ctx, eng, force, liveHosts, stdout)
	}
}

// runAC reads or switches the f-host mains AC plug (shelly2).
//
// Independent of power on/off: restoring or cutting AC is never an automatic
// side-effect of a wake or shutdown. The off path refuses while any f-host
// (f0–f3) may still be drawing power, because hard-cutting mains under a live
// host risks ZFS / bhyve damage — use --force only after a graceful shutdown
// (or when you mean a last-resort kill).
func runAC(ctx context.Context, cfg config.Config, args []string, force bool, liveHosts liveHostsFunc,
	stdout, stderr io.Writer) error {

	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errUsage
	}

	verb, ok := parseACArgs(args)
	if !ok {
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown ac command %q", strings.Join(args, " "))
	}

	eng, err := power.New(cfg)
	if err != nil {
		return err
	}
	if liveHosts == nil {
		liveHosts = func(ctx context.Context) []string { return eng.ACActivity(ctx).Hosts() }
	}

	switch verb {
	case "status":
		st, err := eng.ACStatus(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "f-host AC: %s (%s)\n", presenter.OnOff(st.On), st.IP)
		return nil

	case "on":
		st, err := eng.ACSet(ctx, true)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "f-host AC: %s\n", presenter.OnOff(st.On))
		return nil

	default: // "off"
		return acOff(ctx, eng, force, liveHosts, stdout)
	}
}

// parseACArgs mirrors parseFansArgs for the `ac` noun.
func parseACArgs(args []string) (verb string, ok bool) {
	if len(args) != 1 {
		return "", false
	}
	switch args[0] {
	case "status", "on", "off":
		return args[0], true
	}
	return "", false
}

// parseFansArgs parses a `fans` argument list -- args with the leading "fans"
// token already stripped -- into the one verb it names. ok is false for
// anything not documented: wrong arity (fans takes exactly one word, since it
// has no per-host or --force-in-args concept) or an unknown word. This is what
// turns both trailing junk (`fans on f0`) and a misspelling (`fans of`) into a
// usage error instead of a guess.
func parseFansArgs(args []string) (verb string, ok bool) {
	if len(args) != 1 {
		return "", false
	}
	switch args[0] {
	case "status", "on", "off":
		return args[0], true
	}
	return "", false
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
		up := liveHosts(ctx)
		if err := ctx.Err(); err != nil {
			return guardInterrupted("the rack fans", err)
		}
		if len(up) > 0 {
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

// acOff refuses to cut f-host mains AC while any f-host may still be answering,
// unless told explicitly to. Same force/liveHosts threading as fansOff; the
// liveness set is every f-host (f0–f3), because shelly2 powers all of them.
func acOff(ctx context.Context, eng *power.Engine, force bool,
	liveHosts liveHostsFunc, stdout io.Writer) error {

	if !force {
		up := liveHosts(ctx)
		if err := ctx.Err(); err != nil {
			return guardInterrupted("f-host AC", err)
		}
		if len(up) > 0 {
			return fmt.Errorf("%v may still be running; refusing to cut f-host AC. "+
				"Shut the hosts down first (f3sctl power off / power all off), "+
				"or use --force if you mean a hard cut", up)
		}
	}

	st, err := eng.ACSet(ctx, false)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "f-host AC: %s\n", presenter.OnOff(st.On))
	return nil
}

// guardInterrupted is fansOff's and acOff's error for a Ctrl-C during their
// liveness probe. Checked before the probe's verdict: probes the cancel cut
// short read as unknown, i.e. running, and "may still be running ... use
// --force" would send the operator after hosts nobody saw.
func guardInterrupted(plug string, err error) error {
	return fmt.Errorf("interrupted while checking whether the f-hosts are off; %s left "+
		"untouched: %w", plug, err)
}
