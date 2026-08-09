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
)

const usage = `f3sctl - control the f3s homelab

Usage:
  f3sctl power status          Probe f0-f3 and the k3s nodes
  f3sctl power on              Fans on, wake f0/f1/f2, un-mute Gogios
  f3sctl power off             Export zusb, mute Gogios, stop guests, power off f0/f1/f2, fans off
                               (goes through the API: only pi0/pi1 may shut hosts down)
  f3sctl power all on          Fans on, wake f0/f1/f2/f3, un-mute Gogios
  f3sctl power all off         As "power off", but f3 too: the whole rack dark
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

// Run executes one CLI invocation.
func Run(cfg config.Config, args []string, stdout, stderr io.Writer) error {
	return run(cfg, args, stdout, stderr, nil, false)
}

// RunLocal executes an invocation that must act on the homelab directly,
// never through the API.
//
// This is what the API's own detached child uses. It has to bypass the routing
// in globalFlags.useAPI: a shutdown started by the API which then called the
// API would simply recurse.
func RunLocal(cfg config.Config, args []string, stdout, stderr io.Writer, reporter power.Reporter) error {
	return run(cfg, args, stdout, stderr, reporter, true)
}

func run(cfg config.Config, args []string, stdout, stderr io.Writer,
	reporter power.Reporter, forceLocal bool) error {

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
		return runFans(cfg, args[1:], stdout, stderr)
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

	eng, err := power.New(cfg)
	if err != nil {
		return err
	}
	eng.WithReporter(reporter)
	ctx := context.Background()

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

	switch args[0] {
	case "status":
		return printStatus(ctx, eng, stdout)
	case "on":
		return eng.On(ctx, stdout)
	case "off":
		return eng.Off(ctx, stdout)
	}

	// `f3sctl power all on|off` -- every f-host including f3. Matched before
	// the per-host branch below, since "all" is a group, not a host name.
	if len(args) == 2 && args[0] == "all" {
		switch args[1] {
		case "on":
			return eng.OnAll(ctx, stdout)
		case "off":
			return eng.OffAll(ctx, stdout)
		}
	}

	// `f3sctl power <host> on|off`.
	if len(args) == 2 {
		switch args[1] {
		case "on":
			return eng.OnHost(ctx, stdout, args[0])
		case "off":
			return eng.OffHost(ctx, stdout, args[0])
		}
	}

	fmt.Fprint(stderr, usage)
	return fmt.Errorf("unknown power command %q", strings.Join(args, " "))
}

func runFans(cfg config.Config, args []string, stdout, stderr io.Writer) error {
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
		st, err := eng.FansStatus(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "rack fans: %s (%s)\n", onOff(st.On), st.IP)
		return nil

	case "on":
		st, err := eng.FansSet(ctx, true)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "rack fans: %s\n", onOff(st.On))
		return nil

	case "off":
		return fansOff(ctx, eng, args[1:], stdout)
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
func fansOff(ctx context.Context, eng *power.Engine, args []string, stdout io.Writer) error {
	force := len(args) > 0 && (args[0] == "--force" || args[0] == "-f")

	if !force {
		if up := eng.LiveHosts(ctx); len(up) > 0 {
			return fmt.Errorf("%v still up; refusing to switch the rack fans off. "+
				"Use --force if you mean it", up)
		}
	}

	st, err := eng.FansSet(ctx, false)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "rack fans: %s\n", onOff(st.On))
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

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// errUsage signals that usage has already been printed.
var errUsage = fmt.Errorf("no command given")
