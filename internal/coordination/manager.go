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
	"syscall"
	"time"
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

// Manager owns the on-disk lifecycle of one node's power job: claiming the
// lock, spawning the detached child that actually runs it, recording its
// progress, and reading the result back for a client to render.
//
// It knows nothing about HTTP: internal/httpapi renders what Manager reports,
// and RunJob (in run.go) is what the detached child calls into on its way out.
type Manager struct {
	// dir holds job.json, job.lock and job.log.
	dir string

	// spawnFunc starts the detached child that actually performs a job's
	// args. Nil means the real re-exec of this binary (see spawn); only
	// tests substitute anything else, the same seam as PeerSet.fetch, so
	// Start's locking and bookkeeping can be verified without spawning a real
	// process.
	spawnFunc func(args []string) error
}

// NewManager returns a Manager whose state lives under dir.
func NewManager(dir string) *Manager { return &Manager{dir: dir} }

func (m *Manager) statePath() string { return filepath.Join(m.dir, "job.json") }
func (m *Manager) lockPath() string  { return filepath.Join(m.dir, "job.lock") }
func (m *Manager) logPath() string   { return filepath.Join(m.dir, "job.log") }

// Read returns the current or last job, or nil if none has ever run.
func (m *Manager) Read() *Job {
	raw, err := os.ReadFile(m.statePath())
	if err != nil {
		return nil
	}
	var j Job
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil
	}

	// A job recorded as running whose process is gone (the node rebooted
	// mid-shutdown, say) would otherwise block every action forever. Treat a
	// stale record as failed rather than trusting it indefinitely.
	if j.State == JobRunning && m.stale(j) {
		j.State = JobFailed
		j.Error = "the process that owned this job is gone (node restarted?)"
	}
	return &j
}

// stale reports whether a job claiming to run has outlived any plausible
// runtime. The bound is generous: three hosts at 240s each, plus the Gogios
// un-mute wait, plus slack.
func (m *Manager) stale(j Job) bool {
	started, err := time.Parse(time.RFC3339, j.Started)
	if err != nil {
		return true
	}
	return time.Since(started) > 30*time.Minute
}

func (m *Manager) write(j Job) error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}

	// Write-then-rename so a reader never sees a half-written record.
	tmp := m.statePath() + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.statePath())
}

// ErrJobRunning is returned when another power operation already holds the
// lock. Actions are never queued: two shutdowns interleaving would be far
// worse than the second caller being told to wait.
var ErrJobRunning = errors.New("another power operation is already running")

// Start launches action as a detached child and records it as running.
//
// The lock is held only long enough to claim the slot; the child runs
// independently of this CGI process, which exits as soon as it has replied.
func (m *Manager) Start(action string, args []string) (Job, error) {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return Job{}, err
	}

	lock, err := os.OpenFile(m.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return Job{}, fmt.Errorf("opening the job lock: %w", err)
	}
	defer lock.Close()

	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return Job{}, ErrJobRunning
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

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

	if err := m.doSpawn(args); err != nil {
		job.State = JobFailed
		job.Error = err.Error()
		job.Finished = time.Now().UTC().Format(time.RFC3339)
		_ = m.write(job)
		return job, err
	}
	return job, nil
}

// doSpawn starts the job's detached child, through spawnFunc when a test has
// substituted one.
func (m *Manager) doSpawn(args []string) error {
	if m.spawnFunc != nil {
		return m.spawnFunc(args)
	}
	return m.spawn(args)
}

// spawn starts this same binary in CLI mode, detached from the CGI process.
//
// Setsid puts the child in its own session so bozohttpd tearing down the CGI's
// process group cannot take the shutdown with it, and Release lets this
// process exit without reaping. The child records its own completion via
// `f3sctl job-run`, which calls RunJob.
func (m *Manager) spawn(args []string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating this binary: %w", err)
	}

	logFile, err := os.OpenFile(m.logPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("opening the job log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(self, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// The child must not inherit GATEWAY_INTERFACE, or it would start in CGI
	// mode and try to answer an HTTP request that does not exist.
	cmd.Env = append(os.Environ(), "GATEWAY_INTERFACE=", "F3SCTL_JOB_DIR="+m.dir)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the job: %w", err)
	}
	return cmd.Process.Release()
}

// Progress records a step and/or a host update on the running job.
//
// Each update is a read-modify-write of job.json. That is fine here: updates
// arrive a few times a minute at most, and the alternative -- holding state in
// memory -- would not survive the detached child being the only writer while a
// separate CGI process serves the reads.
func (m *Manager) Progress(step string, host string, phase, detail string) {
	j := m.Read()
	if j == nil {
		return
	}

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

	// Best effort: losing a progress update must never derail the operation
	// the client actually asked for.
	_ = m.write(*j)
}

// Finish records a completed job. Called by the detached child (via RunJob)
// on its way out.
func (m *Manager) Finish(rc int, errMsg string) error {
	j := m.Read()
	if j == nil {
		return fs.ErrNotExist
	}
	j.State = JobDone
	if rc != 0 {
		j.State = JobFailed
	}
	j.RC = &rc
	j.Error = errMsg
	j.Finished = time.Now().UTC().Format(time.RFC3339)
	return m.write(*j)
}
