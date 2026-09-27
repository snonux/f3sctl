package coordination

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// countingFn returns an fn for WhileIdle and a pointer to how often it ran.
func countingFn() (func() error, *int) {
	n := 0
	return func() error { n++; return nil }, &n
}

// TestWhileIdleRunsFnWhenNoJobHasEverRun pins the laptop case: no state dir
// at all is idle, fn runs, and nothing is created -- WhileIdle may run as
// root and must not leave a root-owned state dir or job.lock behind.
func TestWhileIdleRunsFnWhenNoJobHasEverRun(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	m := NewManager(dir, defaultUnmuteTimeout, defaultOffWorstCase)
	fn, ran := countingFn()

	if err := m.WhileIdle(fn); err != nil {
		t.Fatalf("WhileIdle = %v, want fn run", err)
	}
	if *ran != 1 {
		t.Errorf("fn ran %d times, want once", *ran)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("state dir exists (stat err %v), want it never created", err)
	}
}

// TestWhileIdleRefusesWhileAJobRuns pins the guard itself: a running job
// refuses with ErrJobRunning naming the job, and fn never runs.
func TestWhileIdleRefusesWhileAJobRuns(t *testing.T) {
	m := newTestManager(t)
	seedJob(t, m, Job{ID: "cyc", Action: "all-cycle", State: JobRunning, Node: "pi1",
		Started: time.Now().UTC().Format(time.RFC3339)})
	fn, ran := countingFn()

	err := m.WhileIdle(fn)
	if !errors.Is(err, ErrJobRunning) {
		t.Fatalf("WhileIdle = %v, want ErrJobRunning", err)
	}
	for _, want := range []string{"all-cycle", "cyc", "pi1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to name %q", err, want)
		}
	}
	if *ran != 0 {
		t.Errorf("fn ran %d times while a job was running, want never", *ran)
	}
}

// TestWhileIdleRefusesARunningJobWithoutALockFile: a missing job.lock skips
// the locking, never the check. A running job.json still refuses.
func TestWhileIdleRefusesARunningJobWithoutALockFile(t *testing.T) {
	m := newTestManager(t)
	if err := m.write(Job{ID: "r", State: JobRunning, Started: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatalf("seeding job.json: %v", err)
	}
	fn, ran := countingFn()

	if err := m.WhileIdle(fn); !errors.Is(err, ErrJobRunning) {
		t.Fatalf("WhileIdle = %v, want ErrJobRunning", err)
	}
	if *ran != 0 {
		t.Errorf("fn ran %d times, want never", *ran)
	}
	assertNoLockFile(t, m)
}

// TestWhileIdleIgnoresFinishedAndStaleJobs pins that "running" means what
// Start means by it: a finished job, a failed one, and one claiming to run
// past the staleness ceiling (its process is gone) do not block.
func TestWhileIdleIgnoresFinishedAndStaleJobs(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	jobs := map[string]Job{
		"done":   {ID: "d", State: JobDone, Started: now},
		"failed": {ID: "f", State: JobFailed, Started: now},
	}
	for name, j := range jobs {
		t.Run(name, func(t *testing.T) {
			m := newTestManager(t)
			seedJob(t, m, j)
			fn, ran := countingFn()
			if err := m.WhileIdle(fn); err != nil || *ran != 1 {
				t.Fatalf("WhileIdle = %v, fn ran %d times; want nil and once", err, *ran)
			}
		})
	}
	t.Run("stale", func(t *testing.T) {
		m := newTestManager(t)
		old := time.Now().Add(-(m.StaleCeiling() + time.Minute)).UTC().Format(time.RFC3339)
		seedJob(t, m, Job{ID: "s", State: JobRunning, Started: old})
		fn, ran := countingFn()
		if err := m.WhileIdle(fn); err != nil || *ran != 1 {
			t.Fatalf("WhileIdle = %v, fn ran %d times; want nil and once", err, *ran)
		}
	})
}

// TestWhileIdleBlocksAStartDuringFn is the race the lock is held for: a job
// started -- here or by another process on this node -- while fn switches a
// plug is refused, not run under it. Once fn has returned, Start works again.
func TestWhileIdleBlocksAStartDuringFn(t *testing.T) {
	m := newTestManager(t)
	seedJob(t, m, Job{ID: "old", State: JobDone, Started: time.Now().UTC().Format(time.RFC3339)})

	// A second Manager on the same dir stands in for the CGI process.
	cgi := NewManager(m.dir, defaultUnmuteTimeout, defaultOffWorstCase)
	spawned := 0
	cgi.spawnFunc = func(string, []string) error { spawned++; return nil }

	var startErr error
	err := m.WhileIdle(func() error {
		// Run the Start concurrently, as a real one would arrive, and wait
		// for its answer while still inside fn.
		done := make(chan error, 1)
		go func() {
			_, err := cgi.Start("all-cycle", []string{"job-run", "power", "all", "cycle"})
			done <- err
		}()
		startErr = <-done
		return nil
	})
	if err != nil {
		t.Fatalf("WhileIdle = %v", err)
	}
	if !errors.Is(startErr, ErrJobRunning) {
		t.Fatalf("Start during fn = %v, want ErrJobRunning", startErr)
	}
	if spawned != 0 {
		t.Fatalf("a job was spawned while fn held the lock")
	}
	if got := m.Read(); got == nil || got.ID != "old" {
		t.Fatalf("Read() = %+v, want the old job untouched", got)
	}

	if _, err := cgi.Start("all-cycle", nil); err != nil {
		t.Fatalf("Start after fn = %v, want it to succeed", err)
	}
	if spawned != 1 {
		t.Errorf("spawned %d jobs after fn, want 1", spawned)
	}
}

// TestWhileIdleReportsAHeldLockAsARunningJob pins the bound: a lock held past
// idleLockWait is refused as a job in flight after that wait, not waited on
// for good, and fn does not run.
func TestWhileIdleReportsAHeldLockAsARunningJob(t *testing.T) {
	m := newTestManager(t)
	m.idleLockWait = 50 * time.Millisecond
	seedJob(t, m, Job{ID: "d", State: JobDone})
	holdLock(t, m)
	fn, ran := countingFn()

	start := time.Now()
	err := m.WhileIdle(fn)
	took := time.Since(start)

	if !errors.Is(err, ErrJobRunning) || !errors.Is(err, errLockHeld) {
		t.Fatalf("WhileIdle = %v, want ErrJobRunning wrapping errLockHeld", err)
	}
	if !strings.Contains(err.Error(), "another power operation or plug switch holds the job lock") {
		t.Errorf("err = %q, want it to say what may be holding the lock", err)
	}
	if took < m.idleLockWait || took > m.idleLockWait+time.Second {
		t.Errorf("WhileIdle gave up after %s, want about %s", took, m.idleLockWait)
	}
	if *ran != 0 {
		t.Errorf("fn ran %d times, want never", *ran)
	}
}

// TestWhileIdleWaitsOutABriefHolder: Start and a recorder hold the lock for
// milliseconds, so a caller arriving then waits rather than being refused.
func TestWhileIdleWaitsOutABriefHolder(t *testing.T) {
	m := newTestManager(t)
	seedJob(t, m, Job{ID: "d", State: JobDone})
	release := holdLock(t, m)
	time.AfterFunc(50*time.Millisecond, release)
	fn, ran := countingFn()

	if err := m.WhileIdle(fn); err != nil || *ran != 1 {
		t.Fatalf("WhileIdle = %v, fn ran %d times; want nil and once", err, *ran)
	}
}

// TestWhileIdleFailsClosedOnAnUnreadableStateDir: a state dir this user may
// not open is not an idle node. The error is reported as is -- not as a
// running job -- and fn does not run.
func TestWhileIdleFailsClosedOnAnUnreadableStateDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory regardless")
	}
	m := newTestManager(t)
	seedJob(t, m, Job{ID: "d", State: JobDone})
	if err := os.Chmod(m.dir, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(m.dir, 0o700) })
	fn, ran := countingFn()

	err := m.WhileIdle(fn)
	if err == nil || errors.Is(err, ErrJobRunning) || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("WhileIdle = %v, want a permission error that is not ErrJobRunning", err)
	}
	if *ran != 0 {
		t.Errorf("fn ran %d times, want never", *ran)
	}
}

// TestWhileIdleReturnsFnsError pins that fn's own failure (the plug write)
// comes back unchanged, and releases the lock.
func TestWhileIdleReturnsFnsError(t *testing.T) {
	m := newTestManager(t)
	seedJob(t, m, Job{ID: "d", State: JobDone})
	boom := errors.New("boom")

	if err := m.WhileIdle(func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("WhileIdle = %v, want %v", err, boom)
	}
	m.spawnFunc = func(string, []string) error { return nil }
	if _, err := m.Start("off", nil); err != nil {
		t.Errorf("Start after a failed fn = %v, want the lock released", err)
	}
}

// TestWhileIdleFailsClosedOnAnUnreadableJobState: a job.json this user may
// not read, or one that does not parse, is not "no job" -- Start may read it
// that way, but a plug switch must not. Here without a job.lock too, so the
// missing-lock path cannot skip the check. fn does not run.
func TestWhileIdleFailsClosedOnAnUnreadableJobState(t *testing.T) {
	t.Run("unreadable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode-000 file regardless")
		}
		m := newTestManager(t)
		if err := m.write(Job{ID: "r", State: JobRunning, Started: time.Now().UTC().Format(time.RFC3339)}); err != nil {
			t.Fatalf("seeding job.json: %v", err)
		}
		if err := os.Chmod(m.statePath(), 0); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		fn, ran := countingFn()

		err := m.WhileIdle(fn)
		if !errors.Is(err, fs.ErrPermission) || errors.Is(err, ErrJobRunning) {
			t.Fatalf("WhileIdle = %v, want a permission error that is not ErrJobRunning", err)
		}
		if *ran != 0 {
			t.Errorf("fn ran %d times, want never", *ran)
		}
		assertNoLockFile(t, m)
	})
	t.Run("corrupt", func(t *testing.T) {
		m := newTestManager(t)
		if err := os.WriteFile(m.statePath(), []byte("{not json"), 0o600); err != nil {
			t.Fatalf("corrupting job.json: %v", err)
		}
		fn, ran := countingFn()

		if err := m.WhileIdle(fn); !errors.Is(err, ErrCorruptJobState) {
			t.Fatalf("WhileIdle = %v, want ErrCorruptJobState", err)
		}
		if *ran != 0 {
			t.Errorf("fn ran %d times, want never", *ran)
		}
	})
}

// TestRunningJobErrorNamesTheJob pins the refusal's wording, shared by this
// node's and a peer's job.
func TestRunningJobErrorNamesTheJob(t *testing.T) {
	err := RunningJobError(Job{ID: "c1", Action: "all-cycle", Node: "pi1", Started: "2026-09-27T08:00:00Z"})
	if !errors.Is(err, ErrJobRunning) {
		t.Fatalf("err = %v, want ErrJobRunning", err)
	}
	want := `another power operation is already running: "all-cycle" (job c1 on pi1, started 2026-09-27T08:00:00Z)`
	if err.Error() != want {
		t.Errorf("err = %q, want %q", err, want)
	}
}
