package power

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// timeline records how long each stage of a run took and prints the summary
// at the end.
//
// "Why does a shutdown take so long" was, until this existed, unanswerable
// from the job log: the log says what happened, never how long any of it
// took, and the only visible clock was the operator's patience. With hosts
// now going down in parallel the question gets harder, not easier -- wall
// time is the slowest host in the batch, and nothing in a stream of
// interleaved lines says which one that was.
//
// Every span is recorded, not just the slow ones, because the useful reading
// is comparative: a 4-minute f2 next to a 40-second f3 says "look at r2's
// containerd", where 4 minutes on its own says nothing at all.
type timeline struct {
	mu    sync.Mutex
	start time.Time
	spans []span
}

// span is one named, measured stage.
type span struct {
	name string
	took time.Duration
}

func newTimeline() *timeline {
	return &timeline{start: time.Now()}
}

// track starts a span and returns the function that ends it, so a caller can
// write `defer tl.track("name")()` and not have to hold a start time.
func (t *timeline) track(name string) func() {
	started := time.Now()
	return func() { t.record(name, time.Since(started)) }
}

// record adds an already-measured span. Safe to call from the per-host
// goroutines of a parallel batch, which is why the mutex is here.
func (t *timeline) record(name string, took time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.spans = append(t.spans, span{name: name, took: took})
}

// report writes the summary, longest span called out by name.
//
// Spans are printed in the order they were recorded rather than sorted by
// duration: the reader is following a sequence of stages, and re-ordering
// them would make the one interesting number harder to place, not easier.
// Concurrent spans (the per-host ones in a parallel batch) therefore appear
// in completion order, nested under the batch span that contains them.
func (t *timeline) report(w io.Writer) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.spans) == 0 {
		return
	}

	slowest := 0
	width := 0
	for i, s := range t.spans {
		if s.took > t.spans[slowest].took {
			slowest = i
		}
		if len(s.name) > width {
			width = len(s.name)
		}
	}

	fmt.Fprintf(w, "\nTiming (total %s):\n", round(time.Since(t.start)))
	for i, s := range t.spans {
		marker := ""
		if i == slowest {
			marker = "   <- longest"
		}
		fmt.Fprintf(w, "  %-*s  %8s%s\n", width, s.name, round(s.took), marker)
	}
}

// round trims a duration to something a human reads at a glance: whole
// seconds are the resolution every stage here is measured in, and
// "1m10.283917s" in a log is noise pretending to be precision.
func round(d time.Duration) time.Duration {
	return d.Round(time.Second)
}
