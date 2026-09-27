package coordination

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/power"
)

// defaultUnmuteTimeout is config.Default().UnmuteTimeout.D(), used throughout
// this file so newTestManager's staleCeiling matches the shipped default
// (20m + defaultOffWorstCase's 18m + staleBuffer's 10m = 48m, summed because
// a power cycle runs both paths in one job) unless a test deliberately
// overrides either input to prove the ceiling tracks both.
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

	var gotID string
	var gotArgs []string
	m.spawnFunc = func(id string, args []string) error {
		gotID, gotArgs = id, args
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
	// The child must be told which job it runs, or its Recorder could never
	// match job.json and it would record nothing.
	if gotID == "" || gotID != job.ID {
		t.Errorf("spawnFunc got job id %q, want the started job's %q", gotID, job.ID)
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
	m.spawnFunc = func(string, []string) error { spawned = true; return nil }

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
	m.spawnFunc = func(string, []string) error { spawned = true; return nil }

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
// see why, per jobrun.Run's own doc comment about failures being written rather
// than only returned.
func TestManagerStartRecordsFailureWhenSpawnFails(t *testing.T) {
	m := newTestManager(t)
	spawnErr := errors.New("boom")
	m.spawnFunc = func(string, []string) error { return spawnErr }

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
	old := time.Now().Add(-(m.StaleCeiling() + time.Minute)).UTC().Format(time.RFC3339)
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

// TestStaleCeilingForCoversBothPathsInOneJob pins the exact formula
// (unmuteTimeout + offWorstCase + staleBuffer) rather than just its
// externally visible effect. The sum, not the max, is what a power cycle
// needs: it runs a whole shutdown and then a whole wake in the same job, so a
// ceiling that cleared only the larger half would fail it mid-wake.
func TestStaleCeilingForCoversBothPathsInOneJob(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		unmuteTimeout, offWorstCase time.Duration
		want                        time.Duration
	}{
		{"unmuteTimeout larger", 37 * time.Minute, 5 * time.Minute, 42*time.Minute + staleBuffer},
		{"offWorstCase larger", 2 * time.Minute, 18 * time.Minute, 20*time.Minute + staleBuffer},
		{"shipped defaults", defaultUnmuteTimeout, defaultOffWorstCase, 38*time.Minute + staleBuffer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := staleCeilingFor(tc.unmuteTimeout, tc.offWorstCase); got != tc.want {
				t.Errorf("staleCeilingFor(%s, %s) = %s, want %s",
					tc.unmuteTimeout, tc.offWorstCase, got, tc.want)
			}
		})
	}
}

// TestManagerReadSurvivesAFullPowerCycle is the regression test for the sum:
// a cycle job 35m in -- a full default shutdown (18m) followed by a wake still
// waiting on the k3s nodes -- is past what the old max(20m, 18m)+10m = 30m
// ceiling allowed, and must still read as running.
func TestManagerReadSurvivesAFullPowerCycle(t *testing.T) {
	m := newTestManager(t)
	old := time.Now().Add(-35 * time.Minute).UTC().Format(time.RFC3339)
	if err := m.write(Job{ID: "cycling", State: JobRunning, Started: old}); err != nil {
		t.Fatalf("seeding a running job: %v", err)
	}

	if got := m.Read(); got == nil || got.State != JobRunning {
		t.Errorf("Read() = %+v, want State=%q: a 35m-old cycle job is within "+
			"UnmuteTimeout + ShutdownWorstCase", got, JobRunning)
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
	m.spawnFunc = func(string, []string) error { spawned = true; return nil }

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
	seedJob(t, m, Job{ID: "p1", State: JobRunning})

	rec := m.Recorder("p1")
	rec.Progress("waking hosts", "", "", "")
	rec.Progress("", "f0", "working", "sending magic packet")

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
	m.Recorder("none").Progress("step", "host", "working", "detail") // must not panic
	if got := m.Read(); got != nil {
		t.Errorf("Read() = %+v, want still nil: Progress must not invent a job", got)
	}
	assertNoLockFile(t, m)
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
			seedJob(t, m, Job{ID: "f1", State: JobRunning,
				Started: time.Now().UTC().Format(time.RFC3339)})

			if err := m.Recorder("f1").Finish(tc.rc, tc.msg); err != nil {
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
	if err := m.Recorder("none").Finish(0, ""); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Finish with no job ever recorded = %v, want fs.ErrNotExist", err)
	}
	assertNoLockFile(t, m)
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

// TestManagerProgressIsSafeForConcurrentHostUpdates is the regression test for
// g51: Progress is a read-modify-write of job.json, and the parallel shutdown
// path (power.shutdownTogether) calls it from one goroutine per host. Without
// serialization, N goroutines each Read the same record, each add only their
// own host to j.Hosts, and the second write overwrites the first -- so a
// polling client sees fewer hosts than the batch is actually shutting down,
// and writers racing the single .tmp path can corrupt job.json. With the
// Manager's mutex, every host's update survives.
//
// Run with -race (mage test does): the lost-update is asserted directly here,
// and the race detector flags the unsynchronised file/map access a build
// without the lock would still have even when the count happened to come out
// right.
func TestManagerProgressIsSafeForConcurrentHostUpdates(t *testing.T) {
	m := newTestManager(t)
	seedJob(t, m, Job{ID: "g51", State: JobRunning})

	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m.Recorder("g51").Progress("", fmt.Sprintf("h%d", i), "working", "shutting down")
		}(i)
	}
	wg.Wait()

	got := m.Read()
	if got == nil {
		t.Fatal("Read() = nil after concurrent Progress; job.json was likely corrupted by racing writes")
	}
	if len(got.Hosts) != n {
		t.Fatalf("Hosts has %d entries, want all %d: the parallel Progress calls lost updates (g51)", len(got.Hosts), n)
	}
	for i := 0; i < n; i++ {
		hp, ok := got.Hosts[fmt.Sprintf("h%d", i)]
		if !ok {
			t.Errorf("Hosts[\"h%d\"] missing: its Progress update was lost to a concurrent write", i)
			continue
		}
		if hp.Phase != "working" {
			t.Errorf("Hosts[\"h%d\"].Phase = %q, want %q", i, hp.Phase, "working")
		}
	}
}

// TestManagerProgressAndFinishDoNotRaceTheTerminalState pins the other half of
// g51: a late Progress from one of the parallel-shutdown goroutines must not
// race Finish's read-then-write and overwrite the terminal state with a stale
// host map. Finish takes the same mutex as Progress, so a Finish that runs
// after the last Progress sees every host and writes the terminal record
// atomically with respect to them.
func TestManagerProgressAndFinishDoNotRaceTheTerminalState(t *testing.T) {
	m := newTestManager(t)
	seedJob(t, m, Job{ID: "g51f", State: JobRunning})

	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m.Recorder("g51f").Progress("", fmt.Sprintf("h%d", i), "working", "")
		}(i)
	}
	// A Finish concurrent with the last Progress updates: it must not lose a
	// host to a racing Progress, and the terminal state must read as done.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := m.Recorder("g51f").Finish(0, ""); err != nil {
			t.Errorf("Finish: %v", err)
		}
	}()
	wg.Wait()

	got := m.Read()
	if got == nil {
		t.Fatal("Read() = nil after concurrent Progress+Finish; job.json was likely corrupted")
	}
	if got.State != JobDone {
		t.Errorf("State = %q, want %q: a racing Progress overwrote the terminal state", got.State, JobDone)
	}
	if len(got.Hosts) != n {
		t.Errorf("Hosts has %d entries, want all %d: a late Progress raced Finish and lost a host", len(got.Hosts), n)
	}
}

// startJob starts a job on m with a substituted spawn and returns its ID, the
// one the detached child would be handed through JobIDEnv.
func startJob(t *testing.T, m *Manager, action string) string {
	t.Helper()
	var spawnedID string
	m.spawnFunc = func(id string, _ []string) error { spawnedID = id; return nil }
	job, err := m.Start(action, nil)
	if err != nil {
		t.Fatalf("Start(%q): %v", action, err)
	}
	if spawnedID != job.ID {
		t.Fatalf("spawned with job id %q, want %q", spawnedID, job.ID)
	}
	return job.ID
}

// TestRecorderWritesItsOwnJob is the normal path through Start: the child's
// recorder, bound to the ID Start handed it, records both progress and the
// outcome.
func TestRecorderWritesItsOwnJob(t *testing.T) {
	m := newTestManager(t)
	id := startJob(t, m, "off")
	rec := m.Recorder(id)

	rec.Progress("stopping guests", "f0", "working", "")
	if err := rec.Finish(0, ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	got := m.Read()
	if got == nil || got.ID != id {
		t.Fatalf("Read() = %+v, want job %q", got, id)
	}
	if got.State != JobDone || got.Step != "stopping guests" || got.Hosts["f0"].Phase != "working" {
		t.Errorf("job = %+v, want done with the recorded step and host", got)
	}
}

// TestRecorderOfASupersededJobCannotOverwriteTheNewerJob is the regression
// test for na: a job-run child that hangs past the staleness ceiling while
// still alive has its job reported as failed by Read, which frees Start to
// record a newer job. When the old child finally reports progress or
// finishes, it must not overwrite the newer job's record.
func TestRecorderOfASupersededJobCannotOverwriteTheNewerJob(t *testing.T) {
	m := newTestManager(t)
	old := time.Now().Add(-(m.StaleCeiling() + time.Minute)).UTC().Format(time.RFC3339)
	if err := m.write(Job{ID: "hung", Action: "off", State: JobRunning, Started: old}); err != nil {
		t.Fatalf("seeding the hung job: %v", err)
	}
	hung := m.Recorder("hung")

	newID := startJob(t, m, "on")

	hung.Progress("still stopping guests", "f1", "failed", "late")
	err := hung.Finish(1, "finally timed out")
	if !errors.Is(err, ErrJobSuperseded) {
		t.Fatalf("stale Finish error = %v, want ErrJobSuperseded", err)
	}

	got := m.Read()
	if got == nil || got.ID != newID {
		t.Fatalf("Read() = %+v, want the newer job %q", got, newID)
	}
	if got.State != JobRunning || got.Action != "on" {
		t.Errorf("newer job = %+v, want it still running action on", got)
	}
	if got.RC != nil || got.Error != "" || got.Finished != "" {
		t.Errorf("newer job = %+v, want no outcome: the stale Finish leaked into it", got)
	}
	if got.Step != "" || len(got.Hosts) != 0 {
		t.Errorf("newer job = %+v, want no progress: the stale Progress leaked into it", got)
	}

	// And the newer job's own child still records normally.
	if err := m.Recorder(newID).Finish(0, ""); err != nil {
		t.Fatalf("newer job's Finish: %v", err)
	}
	if got := m.Read(); got == nil || got.State != JobDone {
		t.Errorf("Read() after the newer job's Finish = %+v, want done", got)
	}
}

// TestRecorderWaitsForTheJobLock pins that a recorder's read-check-write runs
// under the job.lock flock Start claims. Without it a stale child could check
// its own ID, then have Start record a newer job, then rename its stale record
// over the newer one; the ID check alone would not stop that.
func TestRecorderWaitsForTheJobLock(t *testing.T) {
	m := newTestManager(t)
	seedJob(t, m, Job{ID: "j", State: JobRunning,
		Started: time.Now().UTC().Format(time.RFC3339)})

	unlock := holdLock(t, m)

	done := make(chan error, 1)
	go func() { done <- m.Recorder("j").Finish(0, "") }()

	select {
	case err := <-done:
		unlock()
		t.Fatalf("Finish returned (%v) while the job lock was held, want it to wait", err)
	case <-time.After(100 * time.Millisecond):
	}
	if got := m.Read(); got == nil || got.State != JobRunning {
		t.Fatalf("Read() = %+v, want the job untouched while the lock is held", got)
	}

	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Finish after the lock was released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Finish still blocked after the lock was released")
	}
	if got := m.Read(); got == nil || got.State != JobDone {
		t.Errorf("Read() = %+v, want done once the lock was released", got)
	}
}

// TestRecordersInSeparateManagersDoNotLoseUpdates pins that the job.lock
// flock, not only the per-Manager mutex, serializes recorders: two Managers
// on the same directory stand in for two processes writing one job.json.
func TestRecordersInSeparateManagersDoNotLoseUpdates(t *testing.T) {
	a := newTestManager(t)
	b := NewManager(a.dir, defaultUnmuteTimeout, defaultOffWorstCase)
	// This pins serialization, not Progress's lock bound: give the two
	// Managers' cross-process contention (each write fsyncs) ample time so a
	// slow disk cannot drop an update and flake the count.
	a.progressLockWait, b.progressLockWait = 10*time.Second, 10*time.Second
	seedJob(t, a, Job{ID: "x", State: JobRunning})

	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		m := a
		if i%2 == 1 {
			m = b
		}
		wg.Add(1)
		go func(m *Manager, i int) {
			defer wg.Done()
			m.Recorder("x").Progress("", fmt.Sprintf("h%d", i), "working", "")
		}(m, i)
	}
	wg.Wait()

	got := a.Read()
	if got == nil {
		t.Fatal("Read() = nil; job.json was likely corrupted by racing writes")
	}
	if len(got.Hosts) != n {
		t.Errorf("Hosts has %d entries, want all %d: cross-process writers lost updates", len(got.Hosts), n)
	}
}

// TestWriteLeavesNoTempFiles pins the CreateTemp+rename write: the unique temp
// file is renamed away on success and removed on failure, so nothing but
// job.json (and job.lock) accumulates in the state directory.
func TestWriteLeavesNoTempFiles(t *testing.T) {
	m := newTestManager(t)
	for i := 0; i < 3; i++ {
		if err := m.write(Job{ID: fmt.Sprintf("w%d", i), State: JobRunning}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	assertNoTempFiles(t, m.dir)
	if got := m.Read(); got == nil || got.ID != "w2" {
		t.Errorf("Read() = %+v, want the last write", got)
	}

	// A rename that cannot succeed: job.json is a non-empty directory.
	m2 := newTestManager(t)
	if err := os.MkdirAll(filepath.Join(m2.statePath(), "blocker"), 0o700); err != nil {
		t.Fatalf("making job.json a directory: %v", err)
	}
	if err := m2.write(Job{ID: "doomed"}); err == nil {
		t.Fatal("write succeeded over a directory, want an error")
	}
	assertNoTempFiles(t, m2.dir)
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil {
		t.Fatalf("globbing temp files: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("temp files left behind: %v", matches)
	}
}

// seedJob records j as Start leaves the state dir: job.json plus the job.lock
// file a recorder opens (without creating it).
func seedJob(t *testing.T, m *Manager, j Job) {
	t.Helper()
	if err := m.write(j); err != nil {
		t.Fatalf("seeding job %q: %v", j.ID, err)
	}
	f, err := os.OpenFile(m.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("creating job.lock: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing job.lock: %v", err)
	}
}

// holdLock takes job.lock as another process would -- through a second
// Manager, so m's in-process mutex plays no part -- and returns the release.
// The release is also registered as a cleanup, and is safe to call twice.
func holdLock(t *testing.T, m *Manager) func() {
	t.Helper()
	unlock, err := NewManager(m.dir, defaultUnmuteTimeout, defaultOffWorstCase).lock(0, 0)
	if err != nil {
		t.Fatalf("taking the lock: %v", err)
	}
	var once sync.Once
	release := func() { once.Do(unlock) }
	t.Cleanup(release)
	return release
}

func assertNoLockFile(t *testing.T, m *Manager) {
	t.Helper()
	if _, err := os.Stat(m.lockPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("job.lock exists (stat err %v): a recorder must never create state", err)
	}
}

// readRaw returns job.json's bytes, for asserting a file was left untouched.
func readRaw(t *testing.T, m *Manager) string {
	t.Helper()
	raw, err := os.ReadFile(m.statePath())
	if err != nil {
		t.Fatalf("reading job.json: %v", err)
	}
	return string(raw)
}

// TestProgressReturnsPromptlyWhileTheLockIsHeld pins that Progress stays close
// to non-blocking: power.Engine.restoreAC records a step right before it
// switches mains AC back on, so a held job.lock (a wedged Start, an operator's
// flock) must cost at most progressLockWait and a dropped update.
func TestProgressReturnsPromptlyWhileTheLockIsHeld(t *testing.T) {
	m := newTestManager(t)
	seedJob(t, m, Job{ID: "p", State: JobRunning})
	before := readRaw(t, m)
	holdLock(t, m)

	start := time.Now()
	m.Recorder("p").Progress("restoring f-host mains AC", "", "", "")
	took := time.Since(start)

	if took > m.progressLockWait+time.Second {
		t.Errorf("Progress took %s with the lock held, want about %s at most", took, m.progressLockWait)
	}
	if got := readRaw(t, m); got != before {
		t.Errorf("job.json changed without the lock:\n%s", got)
	}
}

// TestFinishGivesUpAfterTheLockWait pins Finish's bound: a lock held for good
// makes it return errLockHeld after finishLockWait instead of hanging the
// exiting child, and job.json is left alone.
func TestFinishGivesUpAfterTheLockWait(t *testing.T) {
	m := newTestManager(t)
	m.finishLockWait = 50 * time.Millisecond
	seedJob(t, m, Job{ID: "f", State: JobRunning})
	before := readRaw(t, m)
	holdLock(t, m)

	start := time.Now()
	err := m.Recorder("f").Finish(0, "")
	took := time.Since(start)

	if !errors.Is(err, errLockHeld) {
		t.Fatalf("Finish with the lock held = %v, want errLockHeld", err)
	}
	if took < m.finishLockWait {
		t.Errorf("Finish gave up after %s, want it to wait %s first", took, m.finishLockWait)
	}
	if took > m.finishLockWait+time.Second {
		t.Errorf("Finish took %s, want about %s", took, m.finishLockWait)
	}
	if got := readRaw(t, m); got != before {
		t.Errorf("job.json changed without the lock:\n%s", got)
	}
}

// TestStartReportsRunningWhileARecorderHoldsTheLock pins the other side of the
// shared lock: while a recorder is mid-update, Start says a job is running
// rather than queuing behind it or writing past it.
func TestStartReportsRunningWhileARecorderHoldsTheLock(t *testing.T) {
	m := newTestManager(t)
	seedJob(t, m, Job{ID: "done", State: JobDone, Started: time.Now().UTC().Format(time.RFC3339)})
	// The recorder's own lock path: the existing lock file, no O_CREATE.
	unlock, err := m.lock(0, 0)
	if err != nil {
		t.Fatalf("taking the lock as a recorder: %v", err)
	}
	defer unlock()

	spawned := false
	m.spawnFunc = func(string, []string) error { spawned = true; return nil }
	if _, err := m.Start("off", nil); !errors.Is(err, ErrJobRunning) {
		t.Fatalf("Start = %v, want ErrJobRunning while a recorder holds job.lock", err)
	}
	if spawned {
		t.Error("spawn ran while a recorder held job.lock")
	}
	if got := m.Read(); got == nil || got.ID != "done" {
		t.Errorf("Read() = %+v, want the old job untouched", got)
	}
}

// TestProgressOfASlowJobDoesNotPersistStaleness is the regression test for a
// recorder writing Read's client-facing view back: a job past the staleness
// ceiling reads as failed, but its still-alive child's own Progress must keep
// it running on disk.
func TestProgressOfASlowJobDoesNotPersistStaleness(t *testing.T) {
	m := newTestManager(t)
	old := time.Now().Add(-(m.StaleCeiling() + time.Minute)).UTC().Format(time.RFC3339)
	seedJob(t, m, Job{ID: "slow", State: JobRunning, Started: old})

	m.Recorder("slow").Progress("still going", "", "", "")

	j, err := m.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if j.State != JobRunning || j.Error != "" || j.Step != "still going" {
		t.Errorf("job.json = %+v, want still running with the step and no error", j)
	}
}

// TestRecorderWithoutAJobIDTouchesNothing pins Recorder("") -- a job-run
// started without JobIDEnv -- as inert: it neither matches a legacy job.json
// that has no id nor creates any state.
func TestRecorderWithoutAJobIDTouchesNothing(t *testing.T) {
	t.Run("legacy job.json without an id", func(t *testing.T) {
		m := newTestManager(t)
		seedJob(t, m, Job{State: JobRunning})
		before := readRaw(t, m)

		rec := m.Recorder("")
		rec.Progress("step", "f0", "working", "")
		if err := rec.Finish(0, ""); !errors.Is(err, ErrNoJobID) {
			t.Fatalf("Finish = %v, want ErrNoJobID", err)
		}
		if got := readRaw(t, m); got != before {
			t.Errorf("job.json changed:\n%s", got)
		}
	})
	t.Run("no state dir", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "missing")
		m := NewManager(dir, defaultUnmuteTimeout, defaultOffWorstCase)
		if err := m.Recorder("").Finish(0, ""); !errors.Is(err, ErrNoJobID) {
			t.Fatalf("Finish = %v, want ErrNoJobID", err)
		}
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("state dir exists (stat err %v), want it never created", err)
		}
	})
}

// TestRecorderInAMissingDirCreatesNothing pins that a recorder never creates
// the state dir or job.lock, even with a job ID: a root-run job-run would
// otherwise leave root-owned state the CGI's Start cannot open.
func TestRecorderInAMissingDirCreatesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	m := NewManager(dir, defaultUnmuteTimeout, defaultOffWorstCase)

	rec := m.Recorder("x")
	rec.Progress("step", "", "", "")
	if err := rec.Finish(1, "boom"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Finish = %v, want fs.ErrNotExist", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("state dir exists (stat err %v), want it never created", err)
	}
}

// TestFinishReportsACorruptJobState pins that an unparseable job.json is its
// own error, not mistaken for a missing one, and is left as found.
func TestFinishReportsACorruptJobState(t *testing.T) {
	m := newTestManager(t)
	seedJob(t, m, Job{ID: "c"})
	if err := os.WriteFile(m.statePath(), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupting job.json: %v", err)
	}

	err := m.Recorder("c").Finish(0, "")
	if !errors.Is(err, ErrCorruptJobState) {
		t.Fatalf("Finish = %v, want ErrCorruptJobState", err)
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Finish = %v, must not read as fs.ErrNotExist", err)
	}
	if got := readRaw(t, m); got != "{not json" {
		t.Errorf("job.json = %q, want it left as found", got)
	}
}
