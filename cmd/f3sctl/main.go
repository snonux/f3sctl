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
	// documented CLI surface. Checked before the CGI switch as defence in
	// depth: coordination's spawn already blanks GATEWAY_INTERFACE for the
	// child, but a job-run must never be mistaken for a CGI request.
	if len(os.Args) > 1 && os.Args[1] == "job-run" {
		return withSignals(func(ctx context.Context) error {
			return jobrun.Run(ctx, cfg, os.Args[2:])
		})
	}

	// bozohttpd sets GATEWAY_INTERFACE=CGI/1.1 (verified on NetBSD 11.0), and
	// nothing else in this deployment does, so it is a reliable mode switch.
	// The CGI gets no signal handling. It bounds its own work with a
	// CGITimeout context, and nothing it runs in-process is a multi-step AC
	// cut -- a power cycle goes to the detached job-run above, it only ever
	// does single plug switches -- so the default actions (bozohttpd's
	// SIGTERM ending it at once) are the right ones.
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
// SIGHUP, and a stop function that restores default signal behaviour. A
// SIGHUP or SIGINT the process was started with ignored stays ignored: see
// terminationSignals.
//
// Catching them, rather than leaving the default action to kill the process,
// is what lets an interrupted `power all cycle` restore f-host AC: the signal
// cuts the AC-off dwell short and the cycle then switches mains back on
// before returning. Dying mid-dwell would leave the rack without mains, the
// one state nothing remote can wake it from. SIGHUP is here because an SSH
// session dropping mid-run sends it.
//
// The first signal cancels and then prints a notice to notice; every later
// one is ignored until stop, so an impatient second Ctrl-C cannot kill the
// restore.
//
// SIGPIPE is caught and discarded rather than cancelling anything: with it
// caught, a write to a closed stdout/stderr (the `| tee` a Ctrl-C also
// killed) fails with EPIPE instead of killing the process, and a lost log
// reader is no reason to abandon a shutdown. It is caught rather than
// signal.Ignore'd because an ignored disposition is inherited by the ssh(1)
// and ping(8) children, a caught one is reset to default for them. It has a
// channel of its own because os/signal drops a signal when the channel is
// full: sharing the one-slot channel, a burst of broken-pipe writes could
// crowd out the Ctrl-C that follows.
func signalContext(notice io.Writer) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, terminationSignals()...)
	pipes := make(chan os.Signal, 1)
	signal.Notify(pipes, syscall.SIGPIPE)
	done := make(chan struct{})

	go func() {
		for {
			select {
			case sig := <-sigs:
				if ctx.Err() != nil {
					continue
				}
				// Cancel first: a blocked notice writer (a stalled
				// terminal) must not delay the wind-down.
				cancel()
				// Generic on purpose: a local cycle restores AC before it
				// returns, but a run going through the API only stops
				// waiting -- the client says so itself (client.waitForJob).
				fmt.Fprintf(notice, "f3sctl: %v received; interrupted, winding down, please wait...\n", sig)
			case <-pipes:
				// Discarded: catching it is the whole point.
			case <-done:
				return
			}
		}
	}()

	return ctx, func() {
		signal.Stop(sigs)
		signal.Stop(pipes)
		close(done)
		cancel()
	}
}

// terminationSignals is the list signalContext cancels on: SIGTERM always,
// SIGINT and SIGHUP unless the process was started with them ignored.
//
// Those two are the only ones an inherited SIG_IGN survives for -- the Go
// runtime keeps it for SIGHUP and SIGINT and installs its own handler for
// the rest -- and signal.Notify would un-ignore them. An ignored one is a
// choice the caller made: `nohup f3sctl power all cycle` ignores SIGHUP, and
// a shell running it in the background with `&` (no job control) ignores
// SIGINT. Catching those anyway would let the very hang-up or Ctrl-C the
// caller opted out of cancel the run.
func terminationSignals() []os.Signal {
	sigs := []os.Signal{syscall.SIGTERM}
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGHUP} {
		if !signal.Ignored(sig) {
			sigs = append(sigs, sig)
		}
	}
	return sigs
}
