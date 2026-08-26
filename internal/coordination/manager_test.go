package coordination

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/power"
)

// defaultUnmuteTimeout is config.Default().UnmuteTimeout.D(), used throughout
// this file so newTestManager's staleCeiling matches the shipped default
// (max(20m, defaultOffWorstCase) + staleBuffer's 10m = the historical fixed
// 30m, since 20m > defaultOffWorstCase's 18m) unless a test deliberately
// overrides either input to prove the ceiling now tracks both.
const defaultUnmuteTimeout = 20 * time.Minute

// defaultOffWorstCase is power.ShutdownWorstCase(config.Default()): 4 hosts
// (f0-f3, the OffAll case) at the default 240s VMShutdownTimeout each, plus
// the default 2m confirmation wait -- 18m. Computed once here, the same way
// every production NewManager call site derives it, rather than hardcoded, so
// this file does not silently drift from power's own constants.
var defaultOffWorstCase = power.ShutdownWorstCase(config.Default())

// newTestManager returns a Manager rooted at a fresh temp dir, so tests never
// touch a real /var/db/f3sctl. Its staleCeiling matches config.Default(), the
// same UnmuteTimeout and VMShutdownTimeout every production NewManager call
// site is built from.
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	if got := config.Default().UnmuteTimeout.D(); got != defaultUnmuteTimeout {
		t.Fatalf("config.Default().UnmuteTimeout = %s, want %s: defaultUnmuteTimeout "+
			"must track it or the tests below silently stop meaning what they say", got, defaultUnmuteTimeout)
	}
	return NewManager(t.TempDir(), defaultUnmuteTimeout, defaultOffWorstCase)
}

// TestManagerReadReturnsNilBeforeAnyJob pins the "nothing has ever run" state
// a fresh node starts in: no job.json on disk, no error, just nil.
func TestManagerReadReturnsNilBeforeAnyJob(t *testing.T) {
	m := newTestManager(t)
	if got := m.Read(); got != nil {
		t.Errorf("Read() = %+v, want nil with no job ever recorded", got)
	}
}

// TestManagerStartClaimsTheLockAndRunsSpawn pins the happy path: Start writes
// a running job, calls the (substituted) spawn, and leaves the job running
// because spawn succeeded.
func TestManagerStartClaimsTheLockAndRunsSpawn(t *testing.T) {
	m := newTestManager(t)

	var gotArgs []string
	m.spawnFunc = func(args []string) error {
		gotArgs = args
		return nil
	}

	job, err := m.Start("off", []string{"job-run", "power", "off"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if job.State != JobRunning {
		t.Errorf("job.State = %q, want %q", job.State, JobRunning)
	}
	if job.Action != "off" {
		t.Errorf("job.Action = %q, want %q", job.Action, "off")
	}
	if len(gotArgs) != 3 || gotArgs[0] != "job-run" {
		t.Errorf("spawnFunc got args %v, want the job-run invocation", gotArgs)
	}

	// Read must agree with what Start returned: a client polling
	// immediately after 202 has to see the same job.
	read := m.Read()
	if read == nil || read.ID != job.ID || read.State != JobRunning {
		t.Errorf("Read() after Start = %+v, want the just-started job", read)
	}
}

// TestManagerStartFailsWhenAJobIsAlreadyRecordedRunning is the negative test
// for the state-based half of the conflict check: a job.json that already
// says "running" (and is not stale) must refuse a second Start before ever
// touching the lock's own semantics.
func TestManagerStartFailsWhenAJobIsAlreadyRecordedRunning(t *testing.T) {
	m := newTestManager(t)
	if err := m.write(Job{
		ID: "already-running", State: JobRunning,
		Started: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seeding a running job: %v", err)
	}

	spawned := false
	m.spawnFunc = func([]string) error { spawned = true; return nil }

	_, err := m.Start("off", nil)
	if !errors.Is(err, ErrJobRunning) {
		t.Fatalf("Start error = %v, want ErrJobRunning", err)
	}
	if spawned {
		t.Error("spawn ran despite a job already being recorded as running")
	}
}

// TestManagerStartFailsWhileTheLockIsHeld is the negative test for the flock
// half: something else holding the lock must refuse Start even if job.json
// itself is silent (e.g. the very first claim, before the holder has written
// anything yet).
//
// This is the exact mechanism that serialises two requests landing on the
// same node -- see peer.go for the complementary mechanism across nodes.
func TestManagerStartFailsWhileTheLockIsHeld(t *testing.T) {
	m := newTestManager(t)

	lock, err := os.OpenFile(m.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("opening the lock file: %v", err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("taking the lock: %v", err)
	}
	// Unlock on exit; the error is not actionable (the fd closes and releases the
	// lock anyway), so it is explicitly discarded rather than ignored --
	// keeping errcheck able to flag a future ignored Flock *acquire*.
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	spawned := false
	m.spawnFunc = func([]string) error { spawned = true; return nil }

	_, err = m.Start("off", nil)
	if !errors.Is(err, ErrJobRunning) {
		t.Fatalf("Start error = %v, want ErrJobRunning while the lock is held", err)
	}
	if spawned {
		t.Error("spawn ran despite the lock being held by someone else")
	}
}

// TestManagerStartRecordsFailureWhenSpawnFails pins that a spawn failure is
// not silently dropped: the job is recorded failed so a polling client can
// see why, per RunJob's own doc comment about failures being written rather
// than only returned.
func TestManagerStartRecordsFailureWhenSpawnFails(t *testing.T) {
	m := newTestManager(t)
	spawnErr := errors.New("boom")
	m.spawnFunc = func([]string) error { return spawnErr }

	job, err := m.Start("off", nil)
	if !errors.Is(err, spawnErr) {
		t.Fatalf("Start error = %v, want %v", err, spawnErr)
	}
	if job.State != JobFailed {
		t.Errorf("job.State = %q, want %q", job.State, JobFailed)
	}
	if job.Error != spawnErr.Error() {
		t.Errorf("job.Error = %q, want %q", job.Error, spawnErr.Error())
	}

	// And it must be readable back, not just returned: this is what lets a
	// polling client discover the failure after the CGI process has exited.
	read := m.Read()
	if read == nil || read.State != JobFailed {
		t.Errorf("Read() after a failed spawn = %+v, want a failed job", read)
	}
}

// TestManagerReadTreatsAnOldRunningJobAsStale pins the guard against a job
// record left behind by a process that is simply gone (the node rebooted
// mid-shutdown): without it, a stale "running" record would block every
// power action forever.
func TestManagerReadTreatsAnOldRunningJobAsStale(t *testing.T) {
	m := newTestManager(t)
	old := time.Now().Add(-31 * time.Minute).UTC().Format(time.RFC3339)
	if err := m.write(Job{ID: "stale", State: JobRunning, Started: old}); err != nil {
		t.Fatalf("seeding a stale job: %v", err)
	}

	got := m.Read()
	if got == nil {
		t.Fatal("Read() = nil, want the stale job reclassified as failed")
	}
	if got.State != JobFailed {
		t.Errorf("State = %q, want %q for a job outliving any plausible runtime", got.State, JobFailed)
	}
	if got.Error == "" {
		t.Error("a stale job must explain itself, or a client sees a bare failure with no cause")
	}
}

// TestManagerReadDoesNotFlagARecentRunningJobAsStale is the companion to the
// staleness test above: a job that is genuinely still within its plausible
// runtime must not be reclassified out from under a real shutdown in
// progress.
func TestManagerReadDoesNotFlagARecentRunningJobAsStale(t *testing.T) {
	m := newTestManager(t)
	recent := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	if err := m.write(Job{ID: "fresh", State: JobRunning, Started: recent}); err != nil {
		t.Fatalf("seeding a running job: %v", err)
	}

	got := m.Read()
	if got == nil || got.State != JobRunning {
		t.Fatalf("Read() = %+v, want the job to still read as running", got)
	}
}

// TestManagerReadTracksAConfiguredUnmuteTimeout is the regression test for
// kz0: a job started under a raised UnmuteTimeout (37m, matching the
// 2026-08-09 600s -> 1200s change plus headroom, and the same value gy0's
// TestJobWaitTimeoutTracksConfiguredUnmuteTimeout pins on the client side)
// must not be marked stale at the fixed 30m the old code used. It sits at
// 35m -- past the old fixed ceiling, but comfortably within
// 37m+staleBuffer -- so this only passes if stale() actually reads
// m.staleCeiling instead of a hardcoded 30*time.Minute. offWorstCase is the
// default here: UnmuteTimeout alone already dominates it, so this test is
// purely about the wake-path half of the formula.
func TestManagerReadTracksAConfiguredUnmuteTimeout(t *testing.T) {
	m := NewManager(t.TempDir(), 37*time.Minute, defaultOffWorstCase)
	old := time.Now().Add(-35 * time.Minute).UTC().Format(time.RFC3339)
	if err := m.write(Job{ID: "still-going", State: JobRunning, Started: old}); err != nil {
		t.Fatalf("seeding a running job: %v", err)
	}

	got := m.Read()
	if got == nil || got.State != JobRunning {
		t.Errorf("Read() = %+v, want State=%q: a 35m-old job must still read as running "+
			"when UnmuteTimeout=37m, the same false-negative class gy0 fixed on the client, "+
			"just on the server's own staleness ceiling", got, JobRunning)
	}
}

// TestManagerReadStillFlagsStaleBeyondTheConfiguredCeiling is
// TestManagerReadTracksAConfiguredUnmuteTimeout's companion: raising the
// ceiling must not turn the check into a no-op. A job far beyond even a
// generous 37m UnmuteTimeout's derived ceiling is still a crashed process,
// not a slow one, and must still be reclassified failed -- preserving the
// safety direction the staleness check exists for.
func TestManagerReadStillFlagsStaleBeyondTheConfiguredCeiling(t *testing.T) {
	m := NewManager(t.TempDir(), 37*time.Minute, defaultOffWorstCase)
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	if err := m.write(Job{ID: "long-gone", State: JobRunning, Started: old}); err != nil {
		t.Fatalf("seeding a stale job: %v", err)
	}

	got := m.Read()
	if got == nil || got.State != JobFailed {
		t.Errorf("Read() = %+v, want State=%q: a 2h-old job is a crashed process "+
			"regardless of how generous UnmuteTimeout is configured", got, JobFailed)
	}
}

// TestManagerReadSurvivesAnOffPathWorstCaseEvenWithASmallUnmuteTimeout is the
// regression test for the gap a reviewer found in kz0's first fix: deriving
// staleCeiling from UnmuteTimeout alone reopens the exact false-failure bug
// kz0 exists to close, just for the Off job type, the moment UnmuteTimeout is
// configured smaller than the shutdown path's own worst case. UnmuteTimeout
// bounds an unrelated wake-path wait, so there is nothing stopping an operator
// from lowering it for reasons that have nothing to do with shutdown timing.
//
// UnmuteTimeout here is 2m -- small enough that UnmuteTimeout+staleBuffer
// (12m) is comfortably below defaultOffWorstCase (18m: 4 hosts * 240s + the
// 2m confirmation wait). The job sits at 17m: past what the old,
// UnmuteTimeout-only formula would allow, but still within the real Off-path
// worst case, so it must still read as running.
func TestManagerReadSurvivesAnOffPathWorstCaseEvenWithASmallUnmuteTimeout(t *testing.T) {
	m := NewManager(t.TempDir(), 2*time.Minute, defaultOffWorstCase)
	old := time.Now().Add(-17 * time.Minute).UTC().Format(time.RFC3339)
	if err := m.write(Job{ID: "shutting-down", State: JobRunning, Started: old}); err != nil {
		t.Fatalf("seeding a running job: %v", err)
	}

	got := m.Read()
	if got == nil || got.State != JobRunning {
		t.Errorf("Read() = %+v, want State=%q: a 17m-old Off job must still read as running "+
			"when UnmuteTimeout=2m but the shutdown path's own worst case "+
			"(power.ShutdownWorstCase, %s here) is ~18m", got, JobRunning, defaultOffWorstCase)
	}
}

// TestManagerReadIsCloseToTheBoundaryEitherWay is an end-to-end sanity check
// that Read() actually reclassifies near m.staleCeiling rather than at some
// other threshold entirely: a job a few seconds inside it must still read as
// running, and one a few seconds past it must read as failed. The margin
// (3s) is not incidental -- j.Started round-trips through RFC3339, which only
// has one-second resolution, so anything tighter risks the truncation itself
// crossing the boundary and making the test flaky regardless of stale()'s
// comparison operator. That imprecision is exactly why this test cannot, by
// itself, catch a ">=" vs ">" off-by-one at the exact boundary -- see
// TestJobIsStaleBoundaryIsExclusive for that.
func TestManagerReadIsCloseToTheBoundaryEitherWay(t *testing.T) {
	m := NewManager(t.TempDir(), defaultUnmuteTimeout, defaultOffWorstCase)
	ceiling := m.StaleCeiling()
	const margin = 3 * time.Second

	within := time.Now().Add(-(ceiling - margin)).UTC().Format(time.RFC3339)
	if err := m.write(Job{ID: "within", State: JobRunning, Started: within}); err != nil {
		t.Fatalf("seeding a running job: %v", err)
	}
	if got := m.Read(); got == nil || got.State != JobRunning {
		t.Errorf("Read() %s inside staleCeiling = %+v, want State=%q", margin, got, JobRunning)
	}

	past := time.Now().Add(-(ceiling + margin)).UTC().Format(time.RFC3339)
	if err := m.write(Job{ID: "past", State: JobRunning, Started: past}); err != nil {
		t.Fatalf("seeding a running job: %v", err)
	}
	if got := m.Read(); got == nil || got.State != JobFailed {
		t.Errorf("Read() %s past staleCeiling = %+v, want State=%q", margin, got, JobFailed)
	}
}

// TestJobIsStaleBoundaryIsExclusive pins the exact boundary jobIsStale (and
// therefore stale()) draws: age == ceiling must NOT count as stale, only
// age > ceiling. Comparing durations directly, rather than going through a
// Job's RFC3339 Started timestamp, is what makes hitting the boundary exactly
// possible at all -- see jobIsStale's doc comment for why the timestamp path
// cannot. This is the test that actually catches a ">=" vs ">" regression;
// TestManagerReadIsCloseToTheBoundaryEitherWay only catches a threshold that
// has drifted by more than a few seconds.
func TestJobIsStaleBoundaryIsExclusive(t *testing.T) {
	const ceiling = 30 * time.Minute

	if jobIsStale(ceiling, ceiling) {
		t.Error("jobIsStale(ceiling, ceiling) = true, want false: exactly at the ceiling " +
			"is not yet stale (the comparison must be \">\", not \">=\")")
	}
	if !jobIsStale(ceiling+time.Nanosecond, ceiling) {
		t.Error("jobIsStale(ceiling+1ns, ceiling) = false, want true: any age past the ceiling is stale")
	}
}

// TestStaleCeilingForTakesTheLargerWorstCase pins the exact formula
// (max(unmuteTimeout, offWorstCase) + staleBuffer) rather than just its
// externally visible effect, exercising both directions -- UnmuteTimeout
// dominant and offWorstCase dominant -- so a future change to either input's
// handling is caught here first.
func TestStaleCeilingForTakesTheLargerWorstCase(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		unmuteTimeout, offWorstCase time.Duration
		want                        time.Duration
	}{
		{"unmuteTimeout dominates", 37 * time.Minute, 5 * time.Minute, 37*time.Minute + staleBuffer},
		{"offWorstCase dominates", 2 * time.Minute, 18 * time.Minute, 18*time.Minute + staleBuffer},
		{"equal", 10 * time.Minute, 10 * time.Minute, 10*time.Minute + staleBuffer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := staleCeilingFor(tc.unmuteTimeout, tc.offWorstCase); got != tc.want {
				t.Errorf("staleCeilingFor(%s, %s) = %s, want %s",
					tc.unmuteTimeout, tc.offWorstCase, got, tc.want)
			}
		})
	}
}

// TestManagerStartSucceedsAfterAStaleJobIsReclaimed pins that Start's
// under-lock re-check uses the same staleness rule as Read: a stale
// "running" record must not permanently wedge the API.
func TestManagerStartSucceedsAfterAStaleJobIsReclaimed(t *testing.T) {
	m := newTestManager(t)
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	if err := m.write(Job{ID: "long-gone", State: JobRunning, Started: old}); err != nil {
		t.Fatalf("seeding a stale job: %v", err)
	}

	spawned := false
	m.spawnFunc = func([]string) error { spawned = true; return nil }

	job, err := m.Start("on", nil)
	if err != nil {
		t.Fatalf("Start after a stale job: %v", err)
	}
	if !spawned {
		t.Error("spawn did not run; a stale job should not block a new one")
	}
	if job.State != JobRunning {
		t.Errorf("job.State = %q, want %q", job.State, JobRunning)
	}
}

// TestManagerProgressUpdatesStepAndHostState pins that Progress is a
// read-modify-write against the same job.json Read/Finish use, which is what
// lets a detached child (the only writer while it runs) and the CGI process
// serving reads agree on the operation's state.
func TestManagerProgressUpdatesStepAndHostState(t *testing.T) {
	m := newTestManager(t)
	if err := m.write(Job{ID: "p1", State: JobRunning}); err != nil {
		t.Fatalf("seeding a job: %v", err)
	}

	m.Progress("waking hosts", "", "", "")
	m.Progress("", "f0", "working", "sending magic packet")

	got := m.Read()
	if got == nil {
		t.Fatal("Read() = nil after Progress")
	}
	if got.Step != "waking hosts" {
		t.Errorf("Step = %q, want %q", got.Step, "waking hosts")
	}
	hp, ok := got.Hosts["f0"]
	if !ok {
		t.Fatal("Hosts[\"f0\"] missing after a host-scoped Progress call")
	}
	if hp.Phase != "working" || hp.Detail != "sending magic packet" {
		t.Errorf("Hosts[\"f0\"] = %+v, want phase=working detail set", hp)
	}
}

// TestManagerProgressIsBestEffortWhenNoJobExists pins that Progress never
// panics or errors when called with nothing recorded yet -- a detached child
// racing its own first write must not be able to crash on a progress update.
func TestManagerProgressIsBestEffortWhenNoJobExists(t *testing.T) {
	m := newTestManager(t)
	m.Progress("step", "host", "working", "detail") // must not panic
	if got := m.Read(); got != nil {
		t.Errorf("Read() = %+v, want still nil: Progress must not invent a job", got)
	}
}

// TestManagerFinishRecordsSuccessAndFailure pins both outcomes Finish must
// distinguish: rc 0 is done, anything else is failed, and the message and
// timestamp travel with it either way.
func TestManagerFinishRecordsSuccessAndFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		rc   int
		msg  string
		want JobState
	}{
		{"success", 0, "", JobDone},
		{"failure", 1, "ssh: connection refused", JobFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t)
			if err := m.write(Job{ID: "f1", State: JobRunning,
				Started: time.Now().UTC().Format(time.RFC3339)}); err != nil {
				t.Fatalf("seeding a job: %v", err)
			}

			if err := m.Finish(tc.rc, tc.msg); err != nil {
				t.Fatalf("Finish: %v", err)
			}

			got := m.Read()
			if got == nil {
				t.Fatal("Read() = nil after Finish")
			}
			if got.State != tc.want {
				t.Errorf("State = %q, want %q", got.State, tc.want)
			}
			if got.RC == nil || *got.RC != tc.rc {
				t.Errorf("RC = %v, want %d", got.RC, tc.rc)
			}
			if got.Error != tc.msg {
				t.Errorf("Error = %q, want %q", got.Error, tc.msg)
			}
			if got.Finished == "" {
				t.Error("Finished timestamp not set")
			}
		})
	}
}

// TestManagerFinishErrorsWhenNoJobExists pins that Finish (called by the
// detached child on its way out) cannot silently succeed against a job.json
// that was never written -- that would hide a real bug in Start.
func TestManagerFinishErrorsWhenNoJobExists(t *testing.T) {
	m := newTestManager(t)
	if err := m.Finish(0, ""); err == nil {
		t.Error("Finish succeeded with no job ever recorded, want an error")
	}
}

// TestNewestJobPrefersEitherNilSide pins the two base cases: with only one
// side present, that side is the answer regardless of which argument
// position it is in.
func TestNewestJobPrefersEitherNilSide(t *testing.T) {
	j := &Job{ID: "x", State: JobDone, Started: "2026-08-16T10:00:00Z"}

	if got := NewestJob(j, nil); got != j {
		t.Errorf("NewestJob(j, nil) = %v, want j", got)
	}
	if got := NewestJob(nil, j); got != j {
		t.Errorf("NewestJob(nil, j) = %v, want j", got)
	}
	if got := NewestJob(nil, nil); got != nil {
		t.Errorf("NewestJob(nil, nil) = %v, want nil", got)
	}
}

// TestNewestJobPrefersARunningJobOverAFinishedOne pins the core reason this
// exists: PeerSet.Busy already guarantees at most one node can be running a
// job at a time, so a running job is never stale in the way a merely
// finished one on the other node can be -- even if that finished job started
// more recently (e.g. a quick single-host action on one node while a slower
// cluster-wide job is still going on the other).
func TestNewestJobPrefersARunningJobOverAFinishedOne(t *testing.T) {
	running := &Job{ID: "running", State: JobRunning, Started: "2026-08-16T09:00:00Z"}
	laterButDone := &Job{ID: "done", State: JobDone, Started: "2026-08-16T10:00:00Z"}

	if got := NewestJob(running, laterButDone); got != running {
		t.Errorf("NewestJob(running, laterButDone) = %v, want the running job", got)
	}
	if got := NewestJob(laterButDone, running); got != running {
		t.Errorf("NewestJob(laterButDone, running) = %v, want the running job", got)
	}
}

// TestNewestJobPrefersTheLaterStartedJobWhenNeitherIsRunning is the ordinary
// case once both jobs have stopped: whichever action was actually taken more
// recently on either node is "the last job", not whichever node a request
// happened to land on.
func TestNewestJobPrefersTheLaterStartedJobWhenNeitherIsRunning(t *testing.T) {
	earlier := &Job{ID: "earlier", State: JobDone, Started: "2026-08-16T09:00:00Z"}
	later := &Job{ID: "later", State: JobFailed, Started: "2026-08-16T10:00:00Z"}

	if got := NewestJob(earlier, later); got != later {
		t.Errorf("NewestJob(earlier, later) = %v, want the later job", got)
	}
	if got := NewestJob(later, earlier); got != later {
		t.Errorf("NewestJob(later, earlier) = %v, want the later job", got)
	}
}
