package power

import (
	"io"
	"sync"
)

// HostPhase is where one host has got to within a power operation.
type HostPhase string

const (
	// HostPending means the operation has not reached this host yet.
	HostPending HostPhase = "pending"
	// HostWorking means the host is being acted on right now: guests
	// stopping, or a magic packet just sent.
	HostWorking HostPhase = "working"
	// HostConfirming means the host accepted a shutdown and we are waiting
	// for it to actually stop answering. Accepting is not completing.
	HostConfirming HostPhase = "confirming"
	// HostDone means this host finished successfully.
	HostDone HostPhase = "done"
	// HostFailed means this host did not complete.
	HostFailed HostPhase = "failed"
)

// Reporter receives progress as an operation runs, so a polling client can see
// an operation advance rather than staring at "running" for eleven minutes.
//
// A shutdown takes minutes and moves through distinct, nameable stages -- NFS
// check, zusb pre-flight, Gogios mute, then each host in turn. Without this the
// job resource can only say "running", which tells a watch app nothing about
// whether anything is wrong or how much longer to wait.
//
// Implementations must be safe for concurrent use: probes run in parallel.
type Reporter interface {
	// Step names the stage the operation has reached.
	Step(name string)
	// HostState records where one host has got to.
	HostState(host string, phase HostPhase, detail string)
}

// nopReporter discards progress. It is what the CLI uses: a human watching the
// terminal already sees every step scroll past, so there is nothing to track.
type nopReporter struct{}

func (nopReporter) Step(string)                         {}
func (nopReporter) HostState(string, HostPhase, string) {}

// reporterOrNop returns r, or a discarding reporter when r is nil, so callers
// never have to nil-check.
func reporterOrNop(r Reporter) Reporter {
	if r == nil {
		return nopReporter{}
	}
	return r
}

// WithReporter attaches a progress reporter to the engine.
//
// Returns the same engine for chaining; progress is opt-in so the CLI path is
// unchanged.
func (e *Engine) WithReporter(r Reporter) *Engine {
	e.report = reporterOrNop(r)
	return e
}

// teeLogger duplicates the human-readable log to a second writer, so the job
// log on disk and the terminal show the same thing.
type teeLogger struct {
	mu sync.Mutex
	w  []io.Writer
}

func (t *teeLogger) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, w := range t.w {
		_, _ = w.Write(p)
	}
	return len(p), nil
}
