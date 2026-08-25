package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/snonux/f3sctl/internal/client"
	"github.com/snonux/f3sctl/internal/config"
)

// parseGlobalFlags pulls the flags that apply to any command out of args.
//
// Hand-rolled rather than flag.FlagSet because the CLI is noun-verb
// ("power f3 on") and the standard parser stops at the first non-flag, which
// would make `f3sctl power off --force` behave differently from
// `f3sctl --force power off`. Accepting them anywhere is friendlier than
// explaining the difference.
func parseGlobalFlags(args []string) (rest []string, flags globalFlags) {
	for _, a := range args {
		switch a {
		case "--remote", "-r":
			flags.remote = true
		case "--force", "-f":
			flags.force = true
		case "--verbose", "-v":
			flags.verbose = true
			// Tracing only makes sense against the API; imply --remote so
			// `f3sctl -v power status` does the useful thing rather than
			// silently running locally with tracing that never fires.
			flags.remote = true
		case "--local", "-l":
			flags.local = true
		default:
			rest = append(rest, a)
		}
	}
	return rest, flags
}

// globalFlags are the options that apply to any command.
type globalFlags struct {
	remote  bool
	local   bool
	force   bool
	verbose bool
}

// useAPI reports whether this invocation should go through the HTTP API rather
// than acting on the homelab directly.
//
// The default for a shutdown is the API, everywhere. Only pi0 and pi1 can
// actually perform one -- the restricted SSH key is pinned to them with
// from="..." -- so a laptop running it locally cannot work, and silently
// failing with "no readable SSH identity" is a poor way to say so. Routing
// through the API means the same command works from anywhere.
//
// Waking, status and the fan plug stay local by default: a magic packet is an
// unprivileged broadcast any LAN host may send, and the other two need no
// authorisation at all. Keeping them local also leaves a way to wake the rack
// when the API itself is unreachable.
//
// --local overrides all of this and is the escape hatch for running on a Pi,
// or for debugging without the API in the path.
func (f globalFlags) useAPI(args []string) bool {
	if f.local {
		return false
	}
	if f.remote {
		return true
	}
	return isShutdown(args) || isMonitoring(args)
}

// isMonitoring reports whether args name a valid `monitoring` command.
//
// These route through the API for the same reason shutdowns do: the marker
// lives on the two OpenBSD gateways, reached with the restricted key that is
// pinned to pi0/pi1, so running it locally from a laptop cannot work.
//
// Built on parseMonitoringArgs -- the same parse runMonitoring uses (cli.go)
// -- rather than a bare arity check, so this and the local dispatch cannot
// disagree about which spellings are valid. It used to check only that there
// were exactly two tokens, not that the second was one of status/mute/unmute,
// so a malformed verb such as `monitoring xyz` was still routed to the API:
// the server would report "not available right now" with a nil error (an
// exit-0 no-op) instead of the local, non-zero "unknown monitoring command"
// that reaching runMonitoring's validation gives every other bad spelling.
func isMonitoring(args []string) bool {
	if len(args) == 0 || args[0] != "monitoring" {
		return false
	}
	_, ok := parseMonitoringArgs(args[1:])
	return ok
}

// isShutdown reports whether args ask for a host, group, or the whole rack to
// be powered off.
//
// Built on parsePowerArgs -- the same parse powerActionFor uses (cli.go) --
// rather than pattern-matching args itself. It used to check only that a
// 3-token command ended in "off", ignoring the middle word entirely, so any
// `power <anything> off` -- including the malformed `power on off` and
// `power status off` -- was classified as a legitimate shutdown and routed
// straight to runRemote, never reaching powerActionFor's validation. Sharing
// the parse means the routing decision here and the local dispatch cannot
// disagree about which spellings are valid.
func isShutdown(args []string) bool {
	if len(args) == 0 || args[0] != "power" {
		return false
	}
	sp, ok := parsePowerArgs(args[1:])
	return ok && sp.verb == "off"
}

// runRemote drives the command through the HTTP API.
//
// The work -- a remote power-off -- is long: the job it starts polls for up
// to jobWaitTimeout (~25m) while the rack shuts down and comes back up. A
// context bound to SIGINT lets a Ctrl-C cancel that poll (and any in-flight
// HTTP request) instead of being ignored for the whole wait, which is the
// difference between an interruptible tool and one a stuck operator has to
// kill from another terminal. The cancellation is cooperative: every client
// round trip and waitForJob's loop honors ctx.Done().
func runRemote(cfg config.Config, args []string, flags globalFlags, stdout, stderr io.Writer) error {
	key, err := cfg.ResolveAPIKey()
	if err != nil {
		return err
	}

	url := cfg.ResolveAPIURL()
	c, err := client.New(url, key, cfg, stdout)
	if err != nil {
		return err
	}

	if flags.verbose {
		// To stderr, so a trace never lands in output something else parses.
		fmt.Fprintf(stderr, "f3sctl: API base %s\n", url)
		c.Verbose(stderr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return client.Run(ctx, c, args, flags.force)
}
