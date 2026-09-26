package jobrun

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/powertest"
)

// jobTestConfig points the AC plug at a fake Shelly and F3SCTL_JOB_DIR at a
// fresh directory holding a running job, as the API leaves it for the child.
func jobTestConfig(t *testing.T) (config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	pwFile := filepath.Join(dir, "shelly_plug")
	if err := os.WriteFile(pwFile, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatalf("writing the Shelly password file: %v", err)
	}
	shelly := powertest.NewFakeShelly(t, true)
	cfg := config.Default()
	cfg.ShellyPasswordFile = []string{pwFile}
	cfg.Inventory = inventory.Inventory{ShellyIP: shelly.Addr(), ShellyACIP: shelly.Addr()}

	job := coordination.Job{
		ID:      "test",
		Action:  "ac status",
		State:   coordination.JobRunning,
		Started: time.Now().UTC().Format(time.RFC3339),
	}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("encoding the job: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "job.json"), raw, 0o600); err != nil {
		t.Fatalf("writing job.json: %v", err)
	}
	t.Setenv("F3SCTL_JOB_DIR", dir)
	return cfg, dir
}

// readJob reads back what Run recorded.
func readJob(t *testing.T, cfg config.Config, dir string) *coordination.Job {
	t.Helper()
	j := coordination.NewManager(dir, cfg.UnmuteTimeout.D(), time.Hour).Read()
	if j == nil {
		t.Fatal("job.json unreadable after Run")
	}
	return j
}

// TestRunRecordsACancelledJobAsFailed pins what a SIGTERM to the detached
// child leaves behind: main cancels the context instead of the process dying,
// so the child still reaches Finish and a polling client sees a failed job
// saying why, rather than a "running" job that only goes stale much later.
func TestRunRecordsACancelledJobAsFailed(t *testing.T) {
	cfg, dir := jobTestConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Run(ctx, cfg, []string{"ac", "status"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	j := readJob(t, cfg, dir)
	if j.State != coordination.JobFailed || j.RC == nil || *j.RC != 1 {
		t.Fatalf("job = %+v, want state failed with rc 1", j)
	}
	if !strings.Contains(j.Error, context.Canceled.Error()) {
		t.Errorf("job error = %q, want the cancellation", j.Error)
	}
}

// TestRunRecordsASuccessfulJobAsDone is the negative: the same job on a live
// context completes and says so.
func TestRunRecordsASuccessfulJobAsDone(t *testing.T) {
	cfg, dir := jobTestConfig(t)

	if err := Run(context.Background(), cfg, []string{"ac", "status"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	j := readJob(t, cfg, dir)
	if j.State != coordination.JobDone || j.RC == nil || *j.RC != 0 {
		t.Fatalf("job = %+v, want state done with rc 0", j)
	}
}
