package cli

// The Gogios-facing nouns: `monitoring` (the alert mute on the two gateways)
// and `gogios` (the cached alert report).

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/gogios"
	"github.com/snonux/f3sctl/internal/power"
)

// runMonitoring reads or changes the Gogios mute on both gateways.
//
// Separate from `power` on purpose: the mute outlives the operation that set
// it, and clearing a stranded one must not require powering anything.
func runMonitoring(ctx context.Context, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errUsage
	}

	// Resolved before building an Engine: what was asked for is decided from
	// the arguments alone, the same reasoning as powerActionFor/parseFansArgs.
	// `monitoring mute junk` used to dispatch on args[0] alone, silently mute
	// Gogios, and drop "junk" on the floor.
	verb, ok := parseMonitoringArgs(args)
	if !ok {
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown monitoring command %q", strings.Join(args, " "))
	}

	eng, err := power.New(cfg)
	if err != nil {
		return err
	}

	switch verb {
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
	}

	printMonitoring(stdout, eng.MonitoringStatus(ctx))
	return nil
}

// parseMonitoringArgs parses a `monitoring` argument list -- args with the
// leading "monitoring" token already stripped -- into the one verb it names.
// ok is false for anything not documented: wrong arity (monitoring takes
// exactly one word) or an unknown word.
//
// isMonitoring (remote.go) parses with this same function before deciding
// whether a `monitoring` command routes to the API, so the routing decision
// and this local dispatch cannot disagree about which spellings are valid --
// the same reasoning as parsePowerArgs/isShutdown.
func parseMonitoringArgs(args []string) (verb string, ok bool) {
	if len(args) != 1 {
		return "", false
	}
	switch args[0] {
	case "status", "mute", "unmute":
		return args[0], true
	}
	return "", false
}

func printMonitoring(out io.Writer, states []gogios.GatewayMute) {
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

// gogiosSpelling is the parsed shape of a `gogios` argument list.
type gogiosSpelling struct {
	verb string // "status", one of gogios.Statuses(), "detail", or "cache-clear"
	name string // the check name, for "detail" only
}

// parseGogiosArgs parses a `gogios` argument list -- args with the leading
// "gogios" token already stripped -- into the one spelling it names. ok is
// false for anything not documented.
//
// No arguments at all means "status", same as an explicit `gogios status`:
// unlike power/fans/monitoring, a bare noun is the common case here (an
// operator glancing at the alert report), so it is not a usage error.
//
// isGogios (remote.go) parses with this same function before deciding
// whether a `gogios` command routes to the API, so the routing decision and
// this local dispatch cannot disagree about which spellings are valid -- the
// same reasoning as parsePowerArgs/isShutdown and
// parseMonitoringArgs/isMonitoring.
func parseGogiosArgs(args []string) (sp gogiosSpelling, ok bool) {
	switch {
	case len(args) == 0:
		return gogiosSpelling{verb: "status"}, true
	case len(args) == 1 && args[0] == "status":
		return gogiosSpelling{verb: "status"}, true
	case len(args) == 1 && slices.Contains(gogios.Statuses(), args[0]):
		return gogiosSpelling{verb: args[0]}, true
	case len(args) >= 2 && args[0] == "detail":
		// The name is everything after "detail", space-joined: a check's
		// name mirrors the monitored command and may itself contain spaces
		// (an operator quotes it at the shell the same way any other
		// multi-word argument is quoted).
		return gogiosSpelling{verb: "detail", name: strings.Join(args[1:], " ")}, true
	case len(args) == 2 && args[0] == "cache" && args[1] == "clear":
		return gogiosSpelling{verb: "cache-clear"}, true
	}
	return gogiosSpelling{}, false
}

// runGogios reads or clears the cached Gogios alert report.
//
// Unlike monitoring, every gogios verb -- cache-clear included -- would work
// from anywhere: internal/gogios.Fetch is a plain HTTPS GET, not something
// reached through the SSH key pinned to pi0/pi1. It defaults through the API
// anyway (globalFlags.useAPI, via isGogios) because the on-disk cache
// Fetch/ClearCache read and write is per-process-tree state: a laptop's own
// local cache is invisible to the CGI that actually serves reads, so a local
// `gogios cache clear` run from a laptop would only ever clear a cache
// nobody reads from. --local exists for debugging, or for running directly
// on pi0/pi1, where the local cache and the CGI's are the same file.
func runGogios(ctx context.Context, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	sp, ok := parseGogiosArgs(args)
	if !ok {
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown gogios command %q", strings.Join(args, " "))
	}

	if sp.verb == "cache-clear" {
		if err := gogios.ClearCache(cfg); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "gogios cache cleared")
	}

	report, err := gogios.Fetch(ctx, cfg)
	if err != nil {
		return err
	}

	switch sp.verb {
	case "status", "cache-clear":
		printGogiosOverview(stdout, report)
	case "detail":
		check, found := report.Check(sp.name)
		if !found {
			return fmt.Errorf("no such Gogios check: %q", sp.name)
		}
		printGogiosCheck(stdout, check)
	default: // one of gogios.Statuses()
		printGogiosChecks(stdout, sp.verb, report.ChecksFor(sp.verb))
	}
	return nil
}

func printGogiosOverview(out io.Writer, r *gogios.Report) {
	fmt.Fprintln(out, r.Subject)
	fmt.Fprintf(out, "last updated: %s\n", r.LastUpdated)
	fmt.Fprintf(out, "critical=%d warning=%d unknown=%d stale=%d suppressed=%d ok=%d\n",
		r.Summary.Critical, r.Summary.Warning, r.Summary.Unknown,
		r.Summary.Stale, r.Summary.Suppressed, r.Summary.Ok)
}

func printGogiosChecks(out io.Writer, status string, checks []gogios.Check) {
	if len(checks) == 0 {
		fmt.Fprintf(out, "no %s checks\n", status)
		return
	}
	for _, c := range checks {
		fmt.Fprintf(out, "%s: %s - %s\n", c.Status, c.Name, c.Output)
	}
}

func printGogiosCheck(out io.Writer, c gogios.Check) {
	fmt.Fprintf(out, "name:   %s\n", c.Name)
	fmt.Fprintf(out, "status: %s\n", c.Status)
	if c.PrevStatus != "" {
		fmt.Fprintf(out, "prev:   %s\n", c.PrevStatus)
	}
	fmt.Fprintf(out, "output: %s\n", c.Output)
	if c.FederatedFrom != "" {
		fmt.Fprintf(out, "from:   %s\n", c.FederatedFrom)
	}
	if c.LastCheckedAgeSeconds != 0 {
		fmt.Fprintf(out, "age:    %ds\n", c.LastCheckedAgeSeconds)
	}
}
