package httpapi

import (
	"fmt"
	"os"

	"github.com/snonux/f3sctl/internal/cli"
	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/power"
)

// jobReporter persists progress from the running operation into job.json, so a
// polling client sees an operation advance rather than a bare "running".
type jobReporter struct{ store jobStore }

func (r jobReporter) Step(name string) { r.store.progress(name, "", "", "") }

func (r jobReporter) HostState(host string, phase power.HostPhase, detail string) {
	r.store.progress("", host, string(phase), detail)
}

// RunJob executes a power action on behalf of the API and records the outcome.
//
// This is what the detached child started by jobStore.spawn runs. It is an
// internal entry point, not part of the CLI surface: it exists so the child
// updates job.json on its way out, which is how a polling client learns the
// operation finished and with what result.
//
// Failures are still written to job.json rather than only being returned, so
// a client that never sees this process's exit status can still see what
// happened.
func RunJob(cfg config.Config, args []string) error {
	dir := os.Getenv("F3SCTL_JOB_DIR")
	if dir == "" {
		dir = cfg.StateDir
	}
	store := jobStore{dir: dir}

	err := cli.RunWithReporter(cfg, args, os.Stdout, os.Stderr, jobReporter{store: store})

	rc, msg := 0, ""
	if err != nil {
		rc, msg = 1, err.Error()
		fmt.Fprintf(os.Stderr, "job failed: %v\n", err)
	}

	if ferr := store.finish(rc, msg); ferr != nil {
		fmt.Fprintf(os.Stderr, "could not record job completion: %v\n", ferr)
	}
	return err
}
