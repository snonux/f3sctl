package coordination

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

// newTestManager returns a Manager rooted at a fresh temp dir, so tests never
// touch a real /var/db/f3sctl.
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	return NewManager(t.TempDir())
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
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("taking the lock: %v", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

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
