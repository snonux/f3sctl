package coordination

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/snonux/f3sctl/internal/atomicfile"
)

// JobState is the lifecycle of an asynchronous power operation.
type JobState string

const (
	JobRunning JobState = "running"
	JobDone    JobState = "done"
	JobFailed  JobState = "failed"
)

// Job is one asynchronous power operation.
//
// Power operations cannot be served synchronously: `power off` stops guests on
// three hosts in sequence, each with a 240s bound, which is well past relayd's
// 300s session timeout and far past anything a watch app will wait for. So the
// request starts a detached child and returns immediately.
type Job struct {
	// ID uniquely identifies this job across both API nodes.
	//
	// It exists because relayd load-balances pi0 and pi1: a POST may start a
	// job on pi1 while the client's next poll lands on pi0, which holds an
	// entirely different job -- possibly an old *failed* one. Without an ID
	// the client cannot tell "not my job" from "my job finished", and will
	// report someone else's stale failure as its own result. Observed for
	// real on 2026-08-08: a shutdown that was running perfectly well was
	// reported as failed because the other node still held a failure from
	// twenty minutes earlier.
	ID string `json:"id"`
	// Action is the CLI action this job runs, e.g. "off".
	Action string   `json:"action"`
	State  JobState `json:"state"`
	// Started and Finished are RFC 3339 timestamps.
	Started  string `json:"started"`
	Finished string `json:"finished,omitempty"`
	// RC is the child's exit code once it has finished.
	RC *int `json:"rc"`
	// Node names the Pi that ran this job. relayd load-balances pi0 and pi1,
	// so a client may well be reading state from the node that did *not* run
	// the job -- see docs/CLIENT.md.
	Node string `json:"node"`
	// Error is the child's failure message, when it failed.
	Error string `json:"error,omitempty"`

	// Step is the stage the operation has reached, updated as it runs.
	//
	// A shutdown takes minutes. Without this a polling client can only see
	// "running" the whole time and cannot tell a healthy slow shutdown from a
	// wedged one.
	Step string `json:"step,omitempty"`
	// Updated is when Step or Hosts last changed (RFC 3339). A client can use
	// it to notice an operation that has stopped making progress.
	Updated string `json:"updated,omitempty"`
	// Hosts is per-host progress, keyed by host name.
	Hosts map[string]HostProgress `json:"hosts,omitempty"`
}

// HostProgress is where one host has got to within an operation.
type HostProgress struct {
	// Phase is pending, working, confirming, done or failed.
	Phase string `json:"phase"`
	// Detail explains the phase when there is something worth saying, such as
	// why a host failed.
	Detail string `json:"detail,omitempty"`
}

// NewestJob picks which of two jobs -- typically this node's own and its
// peer's, see powerapi's currentJob and PeerSet.FetchJob -- is the one to
// report for "current or last power job". Either argument may be nil,
// meaning that side has none.
//
// A running job always wins over one that has already stopped: PeerSet.Busy
// is asked before a job starts precisely so at most one node can ever be
// running one at a time, so a running job is never stale in the way a merely
// finished one can be. Otherwise the job with the later Started timestamp
// wins, since "last" means whichever action was actually taken more
// recently, not whichever node happens to answer a given request.
//
// Started strings compare correctly as plain strings because Manager.Start
// always formats them with time.RFC3339 in UTC: fixed-width, zero-padded
// fields sort identically whether compared as text or parsed and compared as
// time.Time, so there is no need to parse either side just to pick the later
// one.
func NewestJob(a, b *Job) *Job {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if aRunning, bRunning := a.State == JobRunning, b.State == JobRunning; aRunning != bRunning {
		if aRunning {
			return a
		}
		return b
	}
	if b.Started > a.Started {
		return b
	}
	return a
}

// newJobID returns a random identifier for a job. Random rather than a
// counter: the two API nodes keep separate state and must never mint the same
// ID.
func newJobID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating a job id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// staleBuffer is the slack added on top of the job paths' worst cases to get
// the server's staleness ceiling (Manager.staleCeiling).
//
// It mirrors internal/client.jobWaitBuffer's reasoning (see that constant's
// doc comment, added by gy0), but -- unlike UnmuteTimeout alone -- it is
// layered on top of a ceiling that already accounts for BOTH paths' own
// worst cases (see staleCeilingFor), not just one of them:
//
//   - the wake path's real worst case is UnmuteTimeout itself
//     (waitForCluster's deadline) plus the fan/WoL prelude and the gateway
//     SSH round trips that follow it, neither bounded by UnmuteTimeout;
//   - the shutdown path's real worst case is power.ShutdownWorstCase(cfg) --
//     every host in the largest shutdown set at VMShutdownTimeout each, plus
//     one confirmation wait -- which has nothing to do with UnmuteTimeout at
//     all, and used to be badly underestimated here: an earlier version of
//     this comment claimed "three hosts at 240s, ~12m" fit inside this same
//     10-minute buffer, which is arithmetic nonsense (3*240s is already 12
//     minutes, bigger than the buffer alone) -- see kz0's follow-up fix.
//
// The buffer also absorbs the power cycle's own fixed cost between those two
// halves (power.CycleAll: the strict dark check, AC off dwell and NIC settle
// wait -- about a minute and a half), which is bounded and small next to it.
const staleBuffer = 10 * time.Minute

// staleCeilingFor derives the server's job-staleness ceiling from the two
// worst cases a running job may be experiencing: the wake path (bounded by
// unmuteTimeout) and the shutdown path (bounded by offWorstCase, see
// power.ShutdownWorstCase).
//
// It is their SUM, not their max. A plain `power off` or `power on` job is
// only ever on one of the two, but `power all cycle` (power.CycleAll) runs a
// whole shutdown and then a whole wake in the same job, and a ceiling that
// only cleared the larger half would declare a healthy cycle failed part way
// through its wake. Being generous costs a crashed job reading as running for
// longer; being tight kills healthy ones -- see kz0 for the history of that
// trade-off. With the defaults (20m + 18m + 10m) the ceiling is 48m.
func staleCeilingFor(unmuteTimeout, offWorstCase time.Duration) time.Duration {
	return unmuteTimeout + offWorstCase + staleBuffer
}

// JobRecorder is the narrow seam Manager exposes to the detached job child
// (internal/jobrun, the composition root): Progress to advance the job's
// state and Finish to close it. Defined here, on the leaf that owns job.json,
// so internal/jobrun depends on the behaviour it needs rather than on
// *Manager -- and so coordination stays import-free of cli.
//
// A JobRecorder is always bound to one job ID (see Manager.Recorder) and only
// ever writes job.json while that job is still the one recorded there.
type JobRecorder interface {
	Progress(step, host, phase, detail string)
	Finish(rc int, errMsg string) error
}

// Manager owns the on-disk lifecycle of one node's power job: claiming the
// lock, spawning the detached child that actually runs it, recording its
// progress, and reading the result back for a client to render.
//
// It knows nothing about HTTP: internal/httpapi renders what Manager reports,
// and the detached job child (internal/jobrun, the composition root) records
// progress and completion through the JobRecorder Manager.Recorder returns.
type Manager struct {
	// dir holds job.json, job.lock and job.log.
	dir string

	// staleCeiling is how long a job may claim to be running before Read
	// reclassifies it as failed -- see stale(). Derived at construction time
	// (NewManager, via staleCeilingFor) from both the configured UnmuteTimeout
	// and the shutdown path's own worst case, rather than a fixed constant or
	// UnmuteTimeout alone, so raising either one raises this ceiling too. See
	// kz0.
	staleCeiling time.Duration

	// spawnFunc starts the detached child that actually performs a job's
	// args for the job with the given id. Nil means the real re-exec of this
	// binary (see spawn); only tests substitute anything else, the same seam
	// as PeerSet.fetch, so Start's locking and bookkeeping can be verified
	// without spawning a real process.
	spawnFunc func(id string, args []string) error

	// mu serializes the read-modify-write Progress and Finish do on job.json
	// within this process. The parallel shutdown path
	// (power.shutdownTogether) runs one goroutine per host, and each calls
	// Progress (via the job Reporter) concurrently; without a lock, two
	// goroutines Read the same record, each add their own host, and the
	// second write overwrites the first -- a polling client sees an
	// incomplete hosts map. The lock is per-Manager (one node, one job) and
	// Progress is not hot, so a mutex is the right shape.
	//
	// It does not cross processes: that is the job.lock flock's job, which
	// update takes around its read-check-write so a new Start (in the CGI
	// process) and a stale-but-alive child cannot interleave -- see update.
	// The write-then-rename in write() is what keeps a concurrent reader from
	// seeing a half-written record across that boundary.
	mu sync.Mutex

	// progressLockWait and finishLockWait bound how long a recorder's
	// Progress and Finish wait for the job.lock flock before giving up (see
	// update). Fields rather than constants only so tests can shorten them;
	// NewManager sets the defaults.
	progressLockWait time.Duration
	finishLockWait   time.Duration
	// idleLockWait bounds how long WhileIdle waits for job.lock. A field for
	// the same reason as the two above.
	idleLockWait time.Duration
}

// Default lock waits for a recorder (see Manager.progressLockWait).
//
// Progress must stay close to non-blocking: power.Engine.restoreAC records a
// step right before switching mains AC back on, and whatever holds job.lock
// (a wedged CGI Start, an operator's flock(1)) must not be able to hold that
// up. A dropped progress update costs a client one stale step. Finish is the
// one record a polling client cannot do without, so it waits longer, but is
// still bounded so a held lock cannot keep the child from exiting.
//
// WhileIdle waits a little: the lock's legitimate holders (Start, a
// recorder's update) let go within milliseconds, so a caller that merely
// arrived at the same moment should not be refused for it. Anything holding
// it longer is treated as a job in flight.
const (
	defaultProgressLockWait = 200 * time.Millisecond
	defaultFinishLockWait   = 5 * time.Second
	defaultIdleLockWait     = 2 * time.Second
	lockPollInterval        = 10 * time.Millisecond
)

// stateTempAge is how old a leftover job.json temp file must be before a
// write sweeps it (see atomicfile.RemoveStaleTemps). A job write takes
// milliseconds, so anything this old belongs to a writer that was killed.
const stateTempAge = 5 * time.Minute

// NewManager returns a Manager whose state lives under dir.
//
// unmuteTimeout and offWorstCase are, respectively, config.Config's
// UnmuteTimeout and power.ShutdownWorstCase(cfg) -- passed as plain
// time.Duration values rather than the whole config, the same minimal-surface
// pattern NewPeerSet uses for PeerNodes/jobPath. Both callers (httpapi's
// newServer, jobrun.Run) already have a full config.Config in hand,
// so computing offWorstCase is one call at the construction site rather than
// giving Manager its own opinion about inventory or VMShutdownTimeout. Used
// only to derive staleCeiling; see staleCeilingFor and staleBuffer's doc
// comments for why the ceiling must track both.
func NewManager(dir string, unmuteTimeout, offWorstCase time.Duration) *Manager {
	return &Manager{
		dir:              dir,
		staleCeiling:     staleCeilingFor(unmuteTimeout, offWorstCase),
		progressLockWait: defaultProgressLockWait,
		finishLockWait:   defaultFinishLockWait,
		idleLockWait:     defaultIdleLockWait,
	}
}

func (m *Manager) statePath() string { return filepath.Join(m.dir, "job.json") }
func (m *Manager) lockPath() string  { return filepath.Join(m.dir, "job.lock") }
func (m *Manager) logPath() string   { return filepath.Join(m.dir, "job.log") }

// Read returns the current or last job, or nil if none has ever run (or
// job.json is unreadable).
func (m *Manager) Read() *Job {
	jp, err := m.load()
	if err != nil {
		return nil
	}
	j := *jp

	// A job recorded as running whose process is gone (the node rebooted
	// mid-shutdown, say) would otherwise block every action forever. Treat a
	// stale record as failed rather than trusting it indefinitely.
	if j.State == JobRunning && m.stale(j) {
		j.State = JobFailed
		j.Error = "the process that owned this job is gone (node restarted?)"
	}
	return &j
}

// load returns job.json exactly as recorded, without Read's staleness
// reclassification: that is a view for clients, and must never be written
// back by a recorder of a slow-but-alive job. It returns an error wrapping
// fs.ErrNotExist when there is no job.json and one wrapping
// ErrCorruptJobState when it does not parse.
func (m *Manager) load() (*Job, error) {
	raw, err := os.ReadFile(m.statePath())
	if err != nil {
		return nil, err
	}
	var j Job
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorruptJobState, err)
	}
	return &j, nil
}

// stale reports whether a job claiming to run has outlived any plausible
// runtime, judged against m.staleCeiling (UnmuteTimeout + ShutdownWorstCase +
// staleBuffer -- see NewManager, staleCeilingFor and
// staleBuffer's doc comments). Before kz0 this was a fixed 30 minutes, which
// quietly stopped being generous enough the moment an operator raised
// UnmuteTimeout past ~25m (as happened 2026-08-09, 600s -> 1200s, to survive a
// slow ntpd_sync_on_start): the server would call a perfectly healthy job
// "failed" while the client was still patiently waiting on the same, larger,
// UnmuteTimeout-derived deadline (internal/client.jobWaitTimeout, gy0). A
// first fix derived the ceiling from UnmuteTimeout alone, which quietly
// reopened the same bug from the other side: it covered the wake path but not
// the shutdown path's own, independent worst case (power.ShutdownWorstCase),
// so lowering UnmuteTimeout while leaving VMShutdownTimeout at its default (or
// raising it) could again call a healthy job "failed" mid-shutdown. The
// ceiling now covers both paths, summed, because a power cycle runs both in
// one job (see staleCeilingFor).
func (m *Manager) stale(j Job) bool {
	started, err := time.Parse(time.RFC3339, j.Started)
	if err != nil {
		return true
	}
	return jobIsStale(time.Since(started), m.staleCeiling)
}

// jobIsStale is stale()'s actual comparison, pulled out to a pure function so
// a test can pin the exact boundary deterministically. A test driving stale()
// itself cannot: j.Started round-trips through RFC3339, which only has
// one-second resolution, so the age Read() later computes from it always
// carries up to ~1s of truncation noise relative to whatever instant a test
// intended -- there is no Started value that reliably reproduces
// age == ceiling to the nanosecond. Comparing durations directly sidesteps
// that entirely.
//
// age must be strictly greater than ceiling to count as stale -- exactly at
// the ceiling is still within the budget staleCeilingFor computed, not past
// it -- so an accidental ">=" here would fail a job the instant it reaches
// its ceiling instead of the instant it exceeds it.
func jobIsStale(age, ceiling time.Duration) bool {
	return age > ceiling
}

// StaleCeiling returns how long a job may run before Read reclassifies it as
// failed. Exposed so httpapi can advertise it on the job resource
// (jobEntity's "staleAfterSeconds"), letting a remote client -- which has no
// access to this node's UnmuteTimeout config -- derive its own poll deadline
// from the server's actual effective value instead of a second, independently
// hardcoded guess. See lz0.
func (m *Manager) StaleCeiling() time.Duration { return m.staleCeiling }

// write atomically replaces job.json with j (see atomicfile.Write: a unique,
// fsynced temp file renamed into place), so a reader never sees a
// half-written record and two writers never share one temp path. Callers
// other than tests hold the job.lock flock (Start, update), so writers are
// also serialized.
func (m *Manager) write(j Job) error {
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(m.statePath(), append(raw, '\n'), "job state", stateTempAge)
}

// errLockHeld wraps a failure to get the job.lock flock within the allowed
// wait, so Start can tell "somebody else holds the lock" from "the lock file
// could not be opened".
var errLockHeld = errors.New("the job lock is held")

// lock opens job.lock with the extra open flag (os.O_CREATE for Start, none
// for a recorder, which must never create state -- see update) and flocks it
// exclusively, retrying for up to wait. A zero wait is a single non-blocking
// attempt. It returns the function that releases the lock.
func (m *Manager) lock(flag int, wait time.Duration) (unlock func(), err error) {
	f, err := os.OpenFile(m.lockPath(), os.O_RDWR|flag, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the job lock: %w", err)
	}
	if err := flockWithin(int(f.Fd()), wait); err != nil {
		// The lock file's close error is not actionable; it is explicitly
		// discarded so errcheck keeps flagging write-path os.File closes
		// (see .golangci.yml).
		_ = f.Close()
		return nil, err
	}
	return func() {
		// Unlocking and closing errors are not actionable (closing the fd
		// releases the lock anyway), so they are explicitly discarded rather
		// than ignored -- keeping errcheck able to flag a future ignored
		// Flock *acquire*.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// flockWithin takes an exclusive flock on fd, polling a non-blocking attempt
// every lockPollInterval until wait has passed. A plain blocking flock could
// not be bounded. Contention past the wait is reported wrapping errLockHeld;
// any other flock error is returned as is, straight away.
func flockWithin(fd int, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("locking the job lock: %w", err)
		}
		left := time.Until(deadline)
		if left <= 0 {
			return fmt.Errorf("%w (waited %s)", errLockHeld, wait)
		}
		time.Sleep(min(lockPollInterval, left))
	}
}

// ErrJobRunning is returned when another power operation already holds the
// lock. Actions are never queued: two shutdowns interleaving would be far
// worse than the second caller being told to wait.
var ErrJobRunning = errors.New("another power operation is already running")

// ErrJobSuperseded is returned by a JobRecorder's Finish when job.json no
// longer records the recorder's job: a newer job has replaced it.
//
// This happens when a job-run child hangs past the staleness ceiling while
// still alive. Read then reports its job as failed, a new Start is free to
// claim the slot and write a new job.json, and the old child's eventual
// Progress or Finish must not overwrite the newer job's record. See
// Manager.update.
var ErrJobSuperseded = errors.New("job.json records a newer job")

// ErrNoJobID is returned by the Finish of a recorder with an empty job ID
// (Manager.Recorder("")): a job-run child started without JobIDEnv -- by
// hand, or by a CGI binary older than JobIDEnv -- does not know which job it
// is running, so it records nothing and touches no state on disk.
var ErrNoJobID = errors.New("no job ID to record against (" + JobIDEnv + " unset)")

// ErrCorruptJobState is returned by a recorder's Finish when job.json exists
// but does not parse as a job record. It is distinct from fs.ErrNotExist (no
// job.json at all) so a corrupt record is not mistaken for a missing one.
var ErrCorruptJobState = errors.New("job.json is not a valid job record")

// Environment variables through which Manager.spawn hands the detached
// job-run child its state directory and the ID of the job it is running.
// jobrun.Run reads them back; they are named here, next to spawn, so the two
// ends of that contract cannot drift apart.
const (
	JobDirEnv = "F3SCTL_JOB_DIR"
	JobIDEnv  = "F3SCTL_JOB_ID"
)

// Start launches action as a detached child and records it as running.
//
// The lock is held only long enough to claim the slot; the child runs
// independently of this CGI process, which exits as soon as it has replied.
func (m *Manager) Start(action string, args []string) (Job, error) {
	// A single non-blocking attempt: actions are never queued. A running
	// child's Progress or Finish also holds this lock, for the few
	// milliseconds of its read-check-write (see update); a Start landing in
	// that window is told a job is running, which is true in every case but a
	// stale-but-alive child's, and there a retry succeeds.
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return Job{}, err
	}
	unlock, err := m.lock(os.O_CREATE, 0)
	if errors.Is(err, errLockHeld) {
		return Job{}, ErrJobRunning
	}
	if err != nil {
		return Job{}, err
	}
	defer unlock()

	// Re-check under the lock: the previous holder may have finished between
	// our state read and acquiring it.
	if cur := m.Read(); cur != nil && cur.State == JobRunning {
		return Job{}, ErrJobRunning
	}

	node, _ := os.Hostname()
	id, err := newJobID()
	if err != nil {
		return Job{}, err
	}
	job := Job{
		ID:      id,
		Action:  action,
		State:   JobRunning,
		Started: time.Now().UTC().Format(time.RFC3339),
		Node:    node,
	}
	if err := m.write(job); err != nil {
		return Job{}, err
	}

	if err := m.doSpawn(id, args); err != nil {
		job.State = JobFailed
		job.Error = err.Error()
		job.Finished = time.Now().UTC().Format(time.RFC3339)
		_ = m.write(job)
		return job, err
	}
	return job, nil
}

// WhileIdle runs fn while this node has no power job running, holding
// job.lock for fn's whole duration so no job can start under it.
//
// It is how an action that is not itself a job -- the local `f3sctl fans off`
// and `ac off` (internal/cli) -- stays out of a job's way: the rack-wide jobs
// switch those plugs themselves, and `power all cycle` cuts and restores AC,
// so a manual switch mid-job races them. "Running" is judged exactly as
// Start judges it (Read, so a job past the staleness ceiling or one already
// finished does not count), and a job running here is refused with an error
// wrapping ErrJobRunning. Holding the lock is what closes the other half:
// Start takes it with a single non-blocking attempt, so a job started while
// fn runs is refused with ErrJobRunning rather than switching the rack under
// it. fn should therefore be short -- one plug round trip, not a probe.
//
// The lock is waited for at most idleLockWait; a holder that keeps it longer
// is reported as a job in flight (ErrJobRunning), the same answer Start gives.
//
// WhileIdle never creates state, for the reason update gives: it may run as
// root (`doas f3sctl ac off`), and a root-owned job.lock would lock the
// unprivileged CGI out of Start for good. Without a job.lock -- no state dir,
// as on a laptop, or a node that has never run a job -- nothing is locked:
// Start creates the lock before recording its first job, so there is no job
// to wait for, and the one race left is with that very first Start. Any other
// failure to take the lock (a state dir this user may not open, say) is
// returned as is and fn does not run: not knowing is not the same as idle.
func (m *Manager) WhileIdle(fn func() error) error {
	unlock, err := m.lock(0, m.idleLockWait)
	switch {
	case errors.Is(err, errLockHeld):
		return fmt.Errorf("%w: %w", ErrJobRunning, err)
	case errors.Is(err, fs.ErrNotExist):
		unlock = func() {}
	case err != nil:
		return err
	}
	defer unlock()

	if cur := m.Read(); cur != nil && cur.State == JobRunning {
		return fmt.Errorf("%w: %q (job %s on %s, started %s)",
			ErrJobRunning, cur.Action, cur.ID, cur.Node, cur.Started)
	}
	return fn()
}

// doSpawn starts the job's detached child, through spawnFunc when a test has
// substituted one.
func (m *Manager) doSpawn(id string, args []string) error {
	if m.spawnFunc != nil {
		return m.spawnFunc(id, args)
	}
	return m.spawn(id, args)
}

// spawn starts this same binary in CLI mode, detached from the CGI process.
//
// Setsid puts the child in its own session so bozohttpd tearing down the CGI's
// process group cannot take the shutdown with it, and Release lets this
// process exit without reaping. The child records its own completion via
// `f3sctl job-run`, which calls jobrun.Run; id travels in JobIDEnv so the
// child only ever writes to its own job (see Manager.Recorder).
func (m *Manager) spawn(id string, args []string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating this binary: %w", err)
	}

	logFile, err := os.OpenFile(m.logPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("opening the job log: %w", err)
	}
	// The job log is the only record of a detached shutdown, so a failure to
	// close it is surfaced to stderr (captured into bozohttpd's error log, the
	// one out-of-band channel a CGI child has) rather than discarded -- a
	// swallowed close error here is the one write-path errcheck finding this
	// codebase actually cares about. The child holds its own copy of the fd
	// (cmd.Stdout below), so this close releases only the parent's; the
	// child's writes are not affected.
	defer func() {
		if err := logFile.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "f3sctl: closing the job log: %v\n", err)
		}
	}()

	cmd := exec.Command(self, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// The child must not inherit GATEWAY_INTERFACE, or it would start in CGI
	// mode and try to answer an HTTP request that does not exist.
	cmd.Env = append(os.Environ(), "GATEWAY_INTERFACE=", JobDirEnv+"="+m.dir, JobIDEnv+"="+id)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the job: %w", err)
	}
	return cmd.Process.Release()
}

// Recorder returns the JobRecorder through which the detached child running
// job id records its progress and outcome.
//
// Binding the recorder to an ID is what stops a stale-but-alive child from
// clobbering a newer job: its writes are checked against the ID on disk, under
// the job.lock flock, and dropped once a newer job has replaced its own. See
// update.
func (m *Manager) Recorder(id string) JobRecorder {
	return jobRecorder{m: m, id: id}
}

// jobRecorder is the JobRecorder Manager.Recorder returns.
type jobRecorder struct {
	m  *Manager
	id string
}

// Compile-time assertion that jobRecorder is the JobRecorder the detached
// child (internal/jobrun) records progress and completion through. A
// signature drift on Progress or Finish surfaces here, at the seam, rather
// than only at the jobrun call site.
var _ JobRecorder = jobRecorder{}

// Progress records a step and/or a host update on the running job.
//
// Each update is a read-modify-write of job.json. That is fine here: updates
// arrive a few times a minute at most, and the alternative -- holding state in
// memory -- would not survive the detached child being the only writer while a
// separate CGI process serves the reads.
//
// The read-modify-write is serialized (see update): the parallel shutdown path
// (power.shutdownTogether) calls Progress from one goroutine per host, and
// without that those goroutines would Read the same record, each add only
// their own host, and the second write would overwrite the first -- losing
// the other hosts' updates. See the mu field's doc comment.
//
// It is best effort and close to non-blocking: it waits at most
// progressLockWait for job.lock and otherwise drops the update, because
// power.Engine.restoreAC records a step right before switching mains AC back
// on. A dropped update, including one for a superseded job, must never derail
// the operation the client actually asked for.
func (r jobRecorder) Progress(step string, host string, phase, detail string) {
	_ = r.m.update(r.id, r.m.progressLockWait, func(j *Job) {
		if step != "" {
			j.Step = step
		}
		if host != "" {
			if j.Hosts == nil {
				j.Hosts = map[string]HostProgress{}
			}
			j.Hosts[host] = HostProgress{Phase: phase, Detail: detail}
		}
		j.Updated = time.Now().UTC().Format(time.RFC3339)
	})
}

// Finish records a completed job. Called by the detached child (via jobrun.Run)
// on its way out. It goes through the same update as Progress so a late
// Progress from one of the parallel-shutdown goroutines cannot race Finish's
// read-then-write and overwrite the terminal state with a stale host map.
//
// It waits at most finishLockWait for job.lock. Its errors (see update)
// wrap ErrNoJobID for a recorder without a job ID, fs.ErrNotExist when there
// is no job state, ErrCorruptJobState when job.json does not parse,
// ErrJobSuperseded when job.json records another job, or errLockHeld when the
// lock stayed held past the wait.
func (r jobRecorder) Finish(rc int, errMsg string) error {
	return r.m.update(r.id, r.m.finishLockWait, func(j *Job) {
		j.State = JobDone
		if rc != 0 {
			j.State = JobFailed
		}
		j.RC = &rc
		j.Error = errMsg
		j.Finished = time.Now().UTC().Format(time.RFC3339)
	})
}

// update applies mutate to job.json, but only while job.json still records
// job id.
//
// The whole read-check-write runs under mu (goroutines of this process) and
// the job.lock flock (other processes). The flock is what closes the
// stale-but-alive race: Read reports a hung child's job as failed once it
// outlives the staleness ceiling, so a new Start may claim job.lock and write
// a newer job.json while the old child is still running. Start writes only
// under the flock, so with the check here also under it, the old child either
// writes before the new job exists or sees the new ID and backs off -- it can
// never check the old ID, lose the CPU to Start, and then rename its stale
// record over the new one. The flock is waited for at most wait.
//
// update never creates state: an empty id returns ErrNoJobID before touching
// disk, and job.lock is opened without O_CREATE, so a missing state dir or
// lock is fs.ErrNotExist. (Start creates both before it records a job.)
// Otherwise a root-run `f3sctl job-run` could leave a root-owned state dir or
// lock behind that the unprivileged CGI's Start then cannot open.
//
// It reads job.json with load, not Read: Read's staleness reclassification is
// a client-facing view, and persisting it here would have a slow-but-alive
// job's own progress update mark it failed.
func (m *Manager) update(id string, wait time.Duration, mutate func(*Job)) error {
	if id == "" {
		return ErrNoJobID
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	unlock, err := m.lock(0, wait)
	if err != nil {
		return err
	}
	defer unlock()

	j, err := m.load()
	if err != nil {
		return err
	}
	if j.ID != id {
		return fmt.Errorf("recording job %q: %w (job.json records job %q)", id, ErrJobSuperseded, j.ID)
	}
	mutate(j)
	return m.write(*j)
}
