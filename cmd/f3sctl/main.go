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
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

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
	// documented CLI surface. Checked before the CGI switch because the child
	// may inherit the CGI's GATEWAY_INTERFACE.
	if len(os.Args) > 1 && os.Args[1] == "job-run" {
		return withSignals(func(ctx context.Context) error {
			return jobrun.Run(ctx, cfg, os.Args[2:])
		})
	}

	// bozohttpd sets GATEWAY_INTERFACE=CGI/1.1 (verified on NetBSD 11.0), and
	// nothing else in this deployment does, so it is a reliable mode switch.
	// The CGI gets no signal handling: it takes no context and runs nothing
	// long, so the default actions -- bozohttpd's SIGTERM ending it at once --
	// are the right ones, and catching them would only make it unkillable.
	if os.Getenv("GATEWAY_INTERFACE") != "" {
		return httpapi.ServeCGI(cfg, os.Stdout)
	}

	return withSignals(func(ctx context.Context) error {
		return cli.Run(ctx, cfg, os.Args[1:], os.Stdout, os.Stderr)
	})
}

// withSignals runs fn on a context the termination signals cancel, printing
// the first one's notice to stderr.
func withSignals(fn func(ctx context.Context) error) error {
	ctx, stop := signalContext(os.Stderr)
	defer stop()
	return fn(ctx)
}

// signalContext returns a context cancelled by the first SIGINT, SIGTERM or
// SIGHUP, and a stop function that restores default signal behaviour.
//
// Catching them, rather than leaving the default action to kill the process,
// is what lets an interrupted `power all cycle` restore f-host AC: the signal
// cuts the AC-off dwell short and the cycle then switches mains back on
// before returning. Dying mid-dwell would leave the rack without mains, the
// one state nothing remote can wake it from. SIGHUP is here because an SSH
// session dropping mid-run sends it.
//
// The first signal prints a notice to notice and cancels; every later one is
// ignored until stop, so an impatient second Ctrl-C cannot kill the restore.
//
// SIGPIPE is caught and discarded rather than cancelling anything: with it
// caught, a write to a closed stdout/stderr (the `| tee` a Ctrl-C also
// killed) fails with EPIPE instead of killing the process, and a lost log
// reader is no reason to abandon a shutdown. It is caught rather than
// signal.Ignore'd because an ignored disposition is inherited by the ssh(1)
// and ping(8) children, a caught one is reset to default for them.
func signalContext(notice io.Writer) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)
	done := make(chan struct{})

	go func() {
		for {
			select {
			case sig := <-sigs:
				if sig == syscall.SIGPIPE || ctx.Err() != nil {
					continue
				}
				fmt.Fprintf(notice, "f3sctl: %v received, winding down (an interrupted "+
					"power cycle restores f-host AC first); please wait...\n", sig)
				cancel()
			case <-done:
				return
			}
		}
	}()

	return ctx, func() {
		signal.Stop(sigs)
		close(done)
		cancel()
	}
}
