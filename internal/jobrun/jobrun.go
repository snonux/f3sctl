// Package jobrun is the composition root for the API's detached job child: it
// is what `f3sctl job-run` runs, and the one place the job lifecycle is wired
// to the CLI action it actually performs.
//
// It exists as its own package, rather than living in internal/coordination,
// specifically so internal/coordination can be a self-contained leaf (Manager +
// PeerSet, stdlib-only). coordination.RunJob used to import internal/cli and
// call cli.RunLocal, which made the layering httpapi -> coordination -> cli ->
// power/client/presenter: a change to cli.RunLocal broke coordination's build,
// and the whole cli/power/client/presenter graph got dragged into the
// coordination package for one function. Moving that one function here -- to
// the composition root -- leaves coordination importing nothing internal at
// all, and collects the "talk to cli, then record the outcome" glue where
// glue belongs: at the top, not inside a leaf.
package jobrun

import (
	"fmt"
	"os"

	"github.com/snonux/f3sctl/internal/cli"
	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/power"
)

// jobReporter persists progress from the running operation into job.json, so
// a polling client sees an operation advance rather than a bare "running".
//
// It is the bridge between the power package's Reporter interface (what
// cli.RunLocal reports progress through) and coordination's JobRecorder (how
// the Manager writes job.json). It holds a JobRecorder -- the narrow seam --
// rather than a *coordination.Manager, so this package depends on the
// behaviour the job runner needs (Progress + Finish), not on the whole
// Manager.
type jobReporter struct {
	rec coordination.JobRecorder
}

func (r jobReporter) Step(name string) { r.rec.Progress(name, "", "", "") }

func (r jobReporter) HostState(host string, phase power.HostPhase, detail string) {
	r.rec.Progress("", host, string(phase), detail)
}

// Run executes a power action on behalf of the API and records the outcome.
//
// This is what the detached child started by coordination.Manager.spawn runs.
// It is an internal entry point, not part of the documented CLI surface: it
// exists so the child updates job.json on its way out, which is how a polling
// client learns the operation finished and with what result.
//
// Run re-enters the same code path `f3sctl power off` would run interactively
// (cli.RunLocal), which is what keeps the CLI and the API from ever
// disagreeing about what an action actually does. Living in this composition
// root rather than inside coordination is what keeps that re-entry from
// turning coordination into a dependency of cli: coordination stays a leaf,
// and only this package imports both.
//
// Failures are still written to job.json rather than only being returned, so
// a client that never sees this process's exit status can still see what
// happened.
func Run(cfg config.Config, args []string) error {
	dir := os.Getenv("F3SCTL_JOB_DIR")
	if dir == "" {
		dir = cfg.StateDir
	}
	mgr := coordination.NewManager(dir, cfg.UnmuteTimeout.D(), power.ShutdownWorstCase(cfg))

	err := cli.RunLocal(cfg, args, os.Stdout, os.Stderr, jobReporter{rec: mgr})

	rc, msg := 0, ""
	if err != nil {
		rc, msg = 1, err.Error()
		fmt.Fprintf(os.Stderr, "job failed: %v\n", err)
	}

	if ferr := mgr.Finish(rc, msg); ferr != nil {
		fmt.Fprintf(os.Stderr, "could not record job completion: %v\n", ferr)
	}
	return err
}
