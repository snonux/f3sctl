// Command f3sctl controls the f3s homelab: powering the FreeBSD bhyve hosts
// f0-f3 on and off, reporting their status, and switching the rack fans.
//
// The same binary runs in three modes, chosen at startup:
//
//	CGI    when GATEWAY_INTERFACE is set (bozohttpd on pi0/pi1)
//	agent  when invoked as `f3sctl agent` (SSH forced command on the targets)
//	CLI    otherwise (earth, pi0, pi1)
//
// One binary keeps the three from drifting: a subsystem added to the registry
// gains a CLI verb and an HTTP route at the same time.
package main

import (
	"fmt"
	"os"

	"github.com/snonux/f3sctl/internal/agent"
	"github.com/snonux/f3sctl/internal/cli"
	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/httpapi"
	"github.com/snonux/f3sctl/internal/jobrun"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "f3sctl: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Agent modes are dispatched before the config is loaded: they run on the
	// targets, where /usr/local/etc/f3sctl.json may not exist and where the
	// less this code touches, the smaller the surface behind the forced
	// command.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "agent":
			return agent.Run(os.Args[2:])
		case "agent-root":
			return agent.RunPrivileged(os.Args[2:])
		}
	}

	cfg, err := config.Load(os.Getenv("F3SCTL_CONFIG"))
	if err != nil {
		return err
	}

	// job-run is the API's detached child: it performs a CLI action and then
	// records the outcome for a polling client. Internal, not part of the
	// documented CLI surface.
	if len(os.Args) > 1 && os.Args[1] == "job-run" {
		return jobrun.Run(cfg, os.Args[2:])
	}

	// bozohttpd sets GATEWAY_INTERFACE=CGI/1.1 (verified on NetBSD 11.0), and
	// nothing else in this deployment does, so it is a reliable mode switch.
	if os.Getenv("GATEWAY_INTERFACE") != "" {
		return httpapi.ServeCGI(cfg, os.Stdout)
	}

	return cli.Run(cfg, os.Args[1:], os.Stdout, os.Stderr)
}
