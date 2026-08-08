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
  f3sctl power f3 on           Wake f3 only
  f3sctl power f3 off          Power off f3 only
  f3sctl fans status           Rack-fan Shelly plug state
  f3sctl fans on               Switch the rack fans on
  f3sctl fans off [--force]    Switch the rack fans off
  f3sctl version               Print the version

Global flags:
  --remote, -r   Drive the command through the HTTP API on pi0/pi1 instead of
                 acting locally. Required when off the LAN: a Wake-on-LAN
                 magic packet is not routed, so only a host on the homelab
                 broadcast domain can send one. Needs api_url and an API key
                 (config, or F3SCTL_URL / F3SCTL_KEY).
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
	return RunWithReporter(cfg, args, stdout, stderr, nil)
}

// RunWithReporter is Run with progress reporting attached.
//
// The API's detached child uses this so a polling client can watch a shutdown
// advance through its stages; a human at a terminal passes nil, because the
// same information is already scrolling past them.
func RunWithReporter(cfg config.Config, args []string, stdout, stderr io.Writer, reporter power.Reporter) error {
	args, flags := parseGlobalFlags(args)

	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errUsage
	}

	// In remote mode the same verbs are driven through the HTTP API instead of
	// performed locally. That is the only way to work off-LAN: a Wake-on-LAN
	// magic packet is not routed, so a laptop elsewhere physically cannot wake
	// an f-host -- but pi0/pi1 can, on its behalf.
	if flags.remote && args[0] != "version" && args[0] != "help" {
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

	switch args[0] {
	case "status":
		return printStatus(ctx, eng, stdout)
	case "on":
		return eng.On(ctx, stdout)
	case "off":
		return eng.Off(ctx, stdout)
	}

	// `f3sctl power <host> on|off` -- currently only meaningful for f3, but
	// spelled generally so a future host needs no new parsing.
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
