package cli

import (
	"fmt"
	"io"

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
		default:
			rest = append(rest, a)
		}
	}
	return rest, flags
}

// globalFlags are the options that apply to any command.
type globalFlags struct {
	remote  bool
	force   bool
	verbose bool
}

// runRemote drives the command through the HTTP API.
func runRemote(cfg config.Config, args []string, flags globalFlags, stdout, stderr io.Writer) error {
	key, err := cfg.ResolveAPIKey()
	if err != nil {
		return err
	}

	url := cfg.ResolveAPIURL()
	c, err := client.New(url, key, stdout)
	if err != nil {
		return err
	}

	if flags.verbose {
		// To stderr, so a trace never lands in output something else parses.
		fmt.Fprintf(stderr, "f3sctl: API base %s\n", url)
		c.Verbose(stderr)
	}
	return client.Run(c, args, flags.force)
}
