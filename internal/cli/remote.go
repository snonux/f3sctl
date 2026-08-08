package cli

import (
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
func parseGlobalFlags(args []string) (rest []string, remote, force bool) {
	for _, a := range args {
		switch a {
		case "--remote", "-r":
			remote = true
		case "--force", "-f":
			force = true
		default:
			rest = append(rest, a)
		}
	}
	return rest, remote, force
}

// runRemote drives the command through the HTTP API.
func runRemote(cfg config.Config, args []string, force bool, stdout io.Writer) error {
	key, err := cfg.ResolveAPIKey()
	if err != nil {
		return err
	}

	c, err := client.New(cfg.ResolveAPIURL(), key, stdout)
	if err != nil {
		return err
	}
	return client.Run(c, args, force)
}
