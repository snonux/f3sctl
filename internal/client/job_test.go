package client

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
)

// jobReply is one scripted answer from the fake job resource: either a job
// entity or, when status is not 200, an error entity with that status.
type jobReply struct {
	status int
	job    Entity
}

// fakeJobAPI serves a root linking "job" and "status", and answers GET /job
// with a script of replies -- standing in for relayd spreading polls across
// pi0 and pi1, where each read may land on either node's job. Once the script
// runs out, the last reply repeats. onJob, when set, runs before each /job
// answer (used to cancel the caller mid-poll).
type fakeJobAPI struct {
	srv *httptest.Server

	mu      sync.Mutex
	onJob   func()
	script  []jobReply
	jobHits int
}

func newFakeJobAPI(t *testing.T, script ...jobReply) *fakeJobAPI {
	t.Helper()
	if len(script) == 0 {
		t.Fatal("newFakeJobAPI needs at least one scripted reply")
	}
	f := &fakeJobAPI{script: script}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeJobAPI) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		writeEntity(w, Entity{Links: []Link{
			{Rel: []string{"job"}, Href: "/job"},
			{Rel: []string{"status"}, Href: "/status"},
		}})
	case "/status":
		writeEntity(w, Entity{})
	case "/job":
		reply, hook := f.next()
		if hook != nil {
			hook()
		}
		if reply.status != 0 && reply.status != http.StatusOK {
			w.WriteHeader(reply.status)
		}
		writeEntity(w, reply.job)
	default:
		http.NotFound(w, r)
	}
}

// next pops the next scripted reply, repeating the last one forever, and
// returns the onJob hook to run before answering.
func (f *fakeJobAPI) next() (jobReply, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobHits++
	reply := f.script[0]
	if len(f.script) > 1 {
		f.script = f.script[1:]
	}
	return reply, f.onJob
}

func (f *fakeJobAPI) setOnJob(hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onJob = hook
}

func (f *fakeJobAPI) hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jobHits
}

func jobEntity(props map[string]any) jobReply {
	return jobReply{status: http.StatusOK, job: Entity{Properties: props}}
}

// fastPoll is a polling seam short enough for tests to run the whole loop in
// milliseconds; waitBuffer is left to the caller.
func fastPoll(waitBuffer time.Duration) jobPolling {
	return jobPolling{interval: time.Millisecond, retryGap: time.Millisecond, waitBuffer: waitBuffer}
}

// newJobClient builds a client against api through New (as production does)
// with fast polling and stdout captured.
func newJobClient(t *testing.T, api *fakeJobAPI, cfg config.Config, poll jobPolling) (*Client, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	c, err := New(api.srv.URL, "key", cfg, &out)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	c.poll = poll
	return c, &out
}

func mustRoot(t *testing.T, c *Client) Entity {
	t.Helper()
	root, err := c.Root(context.Background())
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	return root
}

// TestWaitForJobIgnoresTheOtherNodesJob pins the 2026-08-08 fix end to end:
// reads that land on the other API node (a different id, here an old failed
// job) are "no news", never reported as this job's outcome, while this job's
// own step and final state are.
func TestWaitForJobIgnoresTheOtherNodesJob(t *testing.T) {
	other := jobEntity(map[string]any{"id": "old", "state": "failed", "error": "stale failure"})
	api := newFakeJobAPI(t,
		other,
		jobEntity(map[string]any{"id": "mine", "state": "running", "step": "f1: shutting down"}),
		other, other, other, // a whole cycle on the other node
		jobEntity(map[string]any{"id": "mine", "state": "done"}),
	)
	c, out := newJobClient(t, api, config.Default(), fastPoll(0))

	if err := c.waitForJob(context.Background(), mustRoot(t, c), "mine", 0); err != nil {
		t.Fatalf("waitForJob: %v", err)
	}

	got := out.String()
	for _, want := range []string{"f1: shutting down", "polled the other API node", "job done"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "stale failure") || strings.Contains(got, "job failed") {
		t.Errorf("output %q reports the other node's job as ours", got)
	}
}

// TestWaitForJobReportsAFailedJobsError pins the terminal non-success path:
// the job's own error message is shown alongside its state, and the wait
// ends (with the status re-rendered) rather than polling on.
func TestWaitForJobReportsAFailedJobsError(t *testing.T) {
	api := newFakeJobAPI(t, jobEntity(map[string]any{"id": "mine", "state": "failed", "error": "f2 did not wake"}))
	c, out := newJobClient(t, api, config.Default(), fastPoll(0))

	if err := c.waitForJob(context.Background(), mustRoot(t, c), "mine", 0); err != nil {
		t.Fatalf("waitForJob: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "job failed: f2 did not wake") {
		t.Errorf("output %q lacks the job's failure", got)
	}
	if got := api.hits(); got != 1 {
		t.Errorf("job reads = %d, want 1: a finished job must end the wait", got)
	}
}

// TestWaitForJobKeepsPollingThroughReadErrors pins that a transient failure
// reading the job (the cluster is being taken apart) is reported and
// retried, not returned as the wait's outcome.
func TestWaitForJobKeepsPollingThroughReadErrors(t *testing.T) {
	broken := jobReply{status: http.StatusBadGateway, job: Entity{Properties: map[string]any{"message": "upstream down"}}}
	api := newFakeJobAPI(t, broken, broken, broken, jobEntity(map[string]any{"id": "mine", "state": "done"}))
	c, out := newJobClient(t, api, config.Default(), fastPoll(0))

	if err := c.waitForJob(context.Background(), mustRoot(t, c), "mine", 0); err != nil {
		t.Fatalf("waitForJob: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "cannot read the job right now: upstream down") || !strings.Contains(got, "job done") {
		t.Errorf("output %q, want the read error reported and then the job's completion", got)
	}
}

// TestWaitForJobGivesUpAtItsOwnDeadline is the deadline half of the
// deadline-vs-cancel split: a job that never shows up (every read is the
// other node's) ends in errJobWaitTimeout once jobWaitTimeout elapses.
func TestWaitForJobGivesUpAtItsOwnDeadline(t *testing.T) {
	api := newFakeJobAPI(t, jobEntity(map[string]any{"id": "other", "state": "running"}))
	cfg := config.Default()
	cfg.UnmuteTimeout = config.Duration(time.Millisecond)
	c, _ := newJobClient(t, api, cfg, fastPoll(50*time.Millisecond))

	err := c.waitForJob(context.Background(), mustRoot(t, c), "mine", 0)
	if !errors.Is(err, errJobWaitTimeout) {
		t.Fatalf("waitForJob = %v, want errJobWaitTimeout", err)
	}
}

// TestWaitForJobSurfacesTheCallersCancellation is the cancel half: Ctrl-C
// (a cancelled ctx) and a deadline the caller's own ctx carried are both
// the caller's doing and must come back as such, never as the synthetic
// "gave up" -- the latter is what comparing ctx.Err() to DeadlineExceeded
// alone used to get wrong.
func TestWaitForJobSurfacesTheCallersCancellation(t *testing.T) {
	api := newFakeJobAPI(t, jobEntity(map[string]any{"id": "mine", "state": "running"}))
	c, out := newJobClient(t, api, config.Default(), fastPoll(0))
	root := mustRoot(t, c)

	cancelled, cancel := context.WithCancel(context.Background())
	api.setOnJob(cancel) // the operator hits Ctrl-C mid-poll
	err := c.waitForJob(cancelled, root, "mine", 0)
	if !errors.Is(err, context.Canceled) || errors.Is(err, errJobWaitTimeout) {
		t.Errorf("waitForJob after cancel = %v, want context.Canceled", err)
	}
	// The read the cancel interrupted is not a network problem to report.
	if got := out.String(); strings.Contains(got, "cannot read the job") {
		t.Errorf("output %q reports the cancellation as a failed read", got)
	}

	api.setOnJob(nil)
	short, cancelShort := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelShort()
	err = c.waitForJob(short, root, "mine", 0)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errJobWaitTimeout) {
		t.Errorf("waitForJob past the caller's deadline = %v, want context.DeadlineExceeded", err)
	}
}

// TestPollJobRetriesPastTheOtherNode pins pollJob's retry: a read landing on
// the other node is retried within the cycle, and this job is returned as
// soon as a read reaches it.
func TestPollJobRetriesPastTheOtherNode(t *testing.T) {
	api := newFakeJobAPI(t,
		jobEntity(map[string]any{"id": "other", "state": "failed"}),
		jobEntity(map[string]any{"id": "mine", "state": "running"}),
	)
	c, _ := newJobClient(t, api, config.Default(), fastPoll(0))

	job, err := c.pollJob(context.Background(), mustRoot(t, c), "mine")
	if err != nil || job == nil {
		t.Fatalf("pollJob = %v, %v; want this job", job, err)
	}
	if id, _ := job.Properties["id"].(string); id != "mine" {
		t.Errorf("pollJob returned job %q, want %q", id, "mine")
	}
	if got := api.hits(); got != 2 {
		t.Errorf("job reads = %d, want 2", got)
	}
}

// TestPollJobGivesUpAfterItsRetries is the bounded side: a cycle that only
// ever reaches the other node takes exactly jobPollRetries reads and reports
// nil, nil ("no news"), and one that only ever fails returns the last error.
func TestPollJobGivesUpAfterItsRetries(t *testing.T) {
	api := newFakeJobAPI(t, jobEntity(map[string]any{"id": "other", "state": "running"}))
	c, _ := newJobClient(t, api, config.Default(), fastPoll(0))

	job, err := c.pollJob(context.Background(), mustRoot(t, c), "mine")
	if job != nil || err != nil {
		t.Errorf("pollJob with only the other node's job = %v, %v; want nil, nil", job, err)
	}
	if got := api.hits(); got != jobPollRetries {
		t.Errorf("job reads = %d, want %d", got, jobPollRetries)
	}

	failing := newFakeJobAPI(t, jobReply{status: http.StatusInternalServerError,
		job: Entity{Properties: map[string]any{"message": "boom"}}})
	c, _ = newJobClient(t, failing, config.Default(), fastPoll(0))
	job, err = c.pollJob(context.Background(), mustRoot(t, c), "mine")
	if job != nil || err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("pollJob with only errors = %v, %v; want the last error", job, err)
	}
}

// TestPollJobStopsRetryingWhenCancelled pins that a cancelled caller stops
// the retry loop at once rather than paying the remaining reads.
func TestPollJobStopsRetryingWhenCancelled(t *testing.T) {
	api := newFakeJobAPI(t, jobEntity(map[string]any{"id": "other", "state": "running"}))
	c, _ := newJobClient(t, api, config.Default(), jobPolling{retryGap: time.Hour})
	root := mustRoot(t, c)

	ctx, cancel := context.WithCancel(context.Background())
	api.setOnJob(cancel)
	job, err := c.pollJob(ctx, root, "mine")
	if job != nil || !errors.Is(err, context.Canceled) {
		t.Errorf("pollJob after cancel = %v, %v; want nil, context.Canceled", job, err)
	}
	if got := api.hits(); got != 1 {
		t.Errorf("job reads = %d, want 1: no retry after cancellation", got)
	}
}

// TestJobPollingDefaults pins the seam's zero value: an unset timing is the
// production one, so a Client built without New still polls every
// jobPollInterval, and an explicit timing is kept.
func TestJobPollingDefaults(t *testing.T) {
	got := jobPolling{}.withDefaults()
	want := jobPolling{interval: jobPollInterval, retryGap: jobRetryGap, waitBuffer: jobWaitBuffer}
	if got != want {
		t.Errorf("jobPolling{}.withDefaults() = %+v, want %+v", got, want)
	}
	if got := (jobPolling{interval: time.Second}).withDefaults().interval; got != time.Second {
		t.Errorf("explicit interval became %s, want 1s", got)
	}
}

// TestWaitForJobSaysStillRunningWithoutAStep pins the fallback line for a
// running job that advertises no step: the operator still sees progress.
func TestWaitForJobSaysStillRunningWithoutAStep(t *testing.T) {
	api := newFakeJobAPI(t,
		jobEntity(map[string]any{"id": "mine", "state": "running"}),
		jobEntity(map[string]any{"id": "mine", "state": "done"}),
	)
	c, out := newJobClient(t, api, config.Default(), fastPoll(0))

	if err := c.waitForJob(context.Background(), mustRoot(t, c), "mine", 0); err != nil {
		t.Fatalf("waitForJob: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "still running...") || !strings.Contains(got, "job done") {
		t.Errorf("output %q, want \"still running...\" then \"job done\"", got)
	}
}

// TestJobDeadlineHonoursTheServerCeiling pins both sides of jobDeadline: a
// server ceiling below jobWaitTimeout leaves the deadline alone, and one
// above it (a power cycle's shutdown plus wake) becomes ceiling + buffer.
func TestJobDeadlineHonoursTheServerCeiling(t *testing.T) {
	c := &Client{cfg: config.Default()}
	base := c.jobWaitTimeout()

	if got := c.jobDeadline(0); got != base {
		t.Errorf("jobDeadline(0) = %s, want jobWaitTimeout %s", got, base)
	}
	if got := c.jobDeadline(time.Minute); got != base {
		t.Errorf("jobDeadline(1m) = %s, want jobWaitTimeout %s", got, base)
	}
	ceiling := base + time.Hour
	if got, want := c.jobDeadline(ceiling), ceiling+jobWaitBuffer; got != want {
		t.Errorf("jobDeadline(%s) = %s, want %s", ceiling, got, want)
	}
}

// TestWaitForJobServerCeilingExtendsTheWait proves the ceiling reaches the
// real poll loop: a job that finishes well after this side's own budget
// succeeds when the server advertised a longer ceiling, and -- the negative
// control -- gives up without one.
func TestWaitForJobServerCeilingExtendsTheWait(t *testing.T) {
	cfg := config.Default()
	cfg.UnmuteTimeout = config.Duration(time.Millisecond)
	poll := fastPoll(30 * time.Millisecond) // own budget: ~31ms

	slowJob := func(t *testing.T) *fakeJobAPI {
		script := make([]jobReply, 0, 21)
		for range 20 {
			script = append(script, jobEntity(map[string]any{"id": "mine", "state": "running"}))
		}
		script = append(script, jobEntity(map[string]any{"id": "mine", "state": "done"}))
		api := newFakeJobAPI(t, script...)
		api.setOnJob(func() { time.Sleep(5 * time.Millisecond) }) // >=100ms to finish
		return api
	}

	api := slowJob(t)
	c, out := newJobClient(t, api, cfg, poll)
	if err := c.waitForJob(context.Background(), mustRoot(t, c), "mine", 10*time.Second); err != nil {
		t.Fatalf("waitForJob with a 10s server ceiling: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "job done") {
		t.Errorf("output %q lacks the job's completion", got)
	}

	api = slowJob(t)
	c, _ = newJobClient(t, api, cfg, poll)
	if err := c.waitForJob(context.Background(), mustRoot(t, c), "mine", 0); !errors.Is(err, errJobWaitTimeout) {
		t.Errorf("waitForJob without a server ceiling = %v, want errJobWaitTimeout", err)
	}
}

// TestReportPollIsQuietWhenCancelled is the deterministic version of the
// cancel-output check above: with the ctx already cancelled every read fails
// because of the cancellation, and reportPoll must report "not finished"
// without blaming the network -- waitForJob's select says why it stopped.
func TestReportPollIsQuietWhenCancelled(t *testing.T) {
	api := newFakeJobAPI(t, jobEntity(map[string]any{"id": "mine", "state": "done"}))
	c, out := newJobClient(t, api, config.Default(), fastPoll(0))
	root := mustRoot(t, c)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c.reportPoll(ctx, root, "mine") {
		t.Error("reportPoll with a cancelled ctx reported the job finished")
	}
	if got := out.String(); got != "" {
		t.Errorf("reportPoll with a cancelled ctx printed %q, want nothing", got)
	}
}
