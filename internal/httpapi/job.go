package httpapi

import (
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
}

// jobStore persists job state and serialises access to it.
type jobStore struct {
	dir string
}

func (js jobStore) statePath() string { return filepath.Join(js.dir, "job.json") }
func (js jobStore) lockPath() string  { return filepath.Join(js.dir, "job.lock") }
func (js jobStore) logPath() string   { return filepath.Join(js.dir, "job.log") }

// read returns the current or last job, or nil if none has ever run.
func (js jobStore) read() *Job {
	raw, err := os.ReadFile(js.statePath())
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
	if j.State == JobRunning && js.stale(j) {
		j.State = JobFailed
		j.Error = "the process that owned this job is gone (node restarted?)"
	}
	return &j
}

// stale reports whether a job claiming to run has outlived any plausible
// runtime. The bound is generous: three hosts at 240s each, plus the Gogios
// un-mute wait, plus slack.
func (js jobStore) stale(j Job) bool {
	started, err := time.Parse(time.RFC3339, j.Started)
	if err != nil {
		return true
	}
	return time.Since(started) > 30*time.Minute
}

func (js jobStore) write(j Job) error {
	if err := os.MkdirAll(js.dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}

	// Write-then-rename so a reader never sees a half-written record.
	tmp := js.statePath() + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, js.statePath())
}

// errJobRunning is returned when another power operation already holds the
// lock. Actions are never queued: two shutdowns interleaving would be far
// worse than the second caller being told to wait.
var errJobRunning = errors.New("another power operation is already running")

// start launches action as a detached child and records it as running.
//
// The lock is held only long enough to claim the slot; the child runs
// independently of this CGI process, which exits as soon as it has replied.
func (js jobStore) start(action string, args []string) (Job, error) {
	if err := os.MkdirAll(js.dir, 0o700); err != nil {
		return Job{}, err
	}

	lock, err := os.OpenFile(js.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return Job{}, fmt.Errorf("opening the job lock: %w", err)
	}
	defer lock.Close()

	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return Job{}, errJobRunning
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	// Re-check under the lock: the previous holder may have finished between
	// our state read and acquiring it.
	if cur := js.read(); cur != nil && cur.State == JobRunning {
		return Job{}, errJobRunning
	}

	node, _ := os.Hostname()
	job := Job{
		Action:  action,
		State:   JobRunning,
		Started: time.Now().UTC().Format(time.RFC3339),
		Node:    node,
	}
	if err := js.write(job); err != nil {
		return Job{}, err
	}

	if err := js.spawn(args); err != nil {
		job.State = JobFailed
		job.Error = err.Error()
		job.Finished = time.Now().UTC().Format(time.RFC3339)
		_ = js.write(job)
		return job, err
	}
	return job, nil
}

// spawn starts this same binary in CLI mode, detached from the CGI process.
//
// Setsid puts the child in its own session so bozohttpd tearing down the CGI's
// process group cannot take the shutdown with it, and Release lets this
// process exit without reaping. The child records its own completion via
// `f3sctl job-run`.
func (js jobStore) spawn(args []string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating this binary: %w", err)
	}

	logFile, err := os.OpenFile(js.logPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
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
	cmd.Env = append(os.Environ(), "GATEWAY_INTERFACE=", "F3SCTL_JOB_DIR="+js.dir)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the job: %w", err)
	}
	return cmd.Process.Release()
}

// finish records a completed job. Called by the detached child on its way out.
func (js jobStore) finish(rc int, errMsg string) error {
	j := js.read()
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
	return js.write(*j)
}
