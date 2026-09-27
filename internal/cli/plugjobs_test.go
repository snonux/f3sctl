package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/power"
	"github.com/snonux/f3sctl/internal/powertest"
)

// The plug off guard's job check, end to end: the real coordination Manager
// over a job.json in the test's own state dir, and the real PeerSet against
// an httptest peer. See plugs_test.go for plugOff against a scripted guard.

// seedJobState records j in dir as Manager.Start leaves it: job.json plus the
// job.lock the guard locks (it never creates one).
func seedJobState(t *testing.T, dir string, j coordination.Job) {
	t.Helper()
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatalf("encoding the job: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "job.json"), raw, 0o600); err != nil {
		t.Fatalf("writing job.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "job.lock"), nil, 0o600); err != nil {
		t.Fatalf("creating job.lock: %v", err)
	}
}

func runningJob(started time.Time) coordination.Job {
	return coordination.Job{ID: "cyc1", Action: "all-cycle", State: coordination.JobRunning,
		Node: "pi0", Started: started.UTC().Format(time.RFC3339)}
}

// TestPlugOffRefusesWhileALocalJobRuns is the bug this guard exists for: a
// `power all cycle` running on this node, and `ac off` / `fans off` typed
// into a shell on it. Both are refused without switching anything or even
// probing; --force still switches, and on -- the recovery path for a job
// whose process died -- is not gated at all.
func TestPlugOffRefusesWhileALocalJobRuns(t *testing.T) {
	for _, noun := range []string{"fans", "ac"} {
		t.Run(noun, func(t *testing.T) {
			shelly := powertest.NewFakeShelly(t, true)
			cfg := testConfig(t, shelly)
			seedJobState(t, cfg.StateDir, runningJob(time.Now()))
			live := hostsUp()

			out, _, err := runCLI(t, cfg, live, noun, "off")
			if !errors.Is(err, coordination.ErrJobRunning) {
				t.Fatalf("%s off = %v, want a wrapped coordination.ErrJobRunning", noun, err)
			}
			if !strings.Contains(err.Error(), "all-cycle") || !strings.Contains(err.Error(), "--force") {
				t.Errorf("err = %q, want it to name the job and offer --force", err)
			}
			if got := shelly.SetCalls(); len(got) != 0 || out != "" || live.calls != 0 {
				t.Fatalf("Switch.Set calls = %v, output = %q, liveness calls = %d; want none",
					got, out, live.calls)
			}

			if _, _, err := runCLI(t, cfg, live, noun, "off", "--force"); err != nil {
				t.Fatalf("%s off --force: %v", noun, err)
			}
			if _, _, err := runCLI(t, cfg, live, noun, "on"); err != nil {
				t.Fatalf("%s on while a job runs: %v", noun, err)
			}
			if got := shelly.SetCalls(); len(got) != 2 || got[0] || !got[1] {
				t.Errorf("Switch.Set calls = %v, want [false true]: forced off, then on", got)
			}
		})
	}
}

// TestPlugOffIgnoresFinishedAndStaleJobs: only a job that is really running
// blocks. A finished or failed one does not, and neither does one still
// recorded as running past the staleness ceiling -- its process is gone, and
// the plugs must not stay locked out until someone edits job.json.
func TestPlugOffIgnoresFinishedAndStaleJobs(t *testing.T) {
	now := time.Now()
	finished := func(state coordination.JobState) coordination.Job {
		j := runningJob(now)
		j.State = state
		return j
	}
	jobs := map[string]coordination.Job{
		"done":   finished(coordination.JobDone),
		"failed": finished(coordination.JobFailed),
		"stale":  runningJob(now.Add(-48 * time.Hour)),
	}
	for name, j := range jobs {
		t.Run(name, func(t *testing.T) {
			shelly := powertest.NewFakeShelly(t, true)
			cfg := testConfig(t, shelly)
			seedJobState(t, cfg.StateDir, j)

			if _, _, err := runCLI(t, cfg, hostsUp(), "ac", "off"); err != nil {
				t.Fatalf("ac off with a %s job: %v", name, err)
			}
			if got := shelly.SetCalls(); len(got) != 1 || got[0] {
				t.Errorf("Switch.Set calls = %v, want exactly one with on=false", got)
			}
		})
	}
}

// fakePeer serves GET /job as a peer API node does, answering with job (nil
// for "none") or, when status is not 200, with that status. It counts hits
// and records the API key each presented.
type fakePeer struct {
	srv  *httptest.Server
	hits atomic.Int32
	key  atomic.Value
}

func newFakePeer(t *testing.T, status int, job *coordination.Job) *fakePeer {
	t.Helper()
	p := &fakePeer{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.hits.Add(1)
		p.key.Store(r.Header.Get("X-API-Key"))
		if r.URL.Path != "/cgi-bin/f3sctl/job" || r.URL.Query().Get(coordination.PeerQueryParam) == "" {
			http.NotFound(w, r)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		props := any(map[string]string{"state": "none"})
		if job != nil {
			props = job
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"properties": props})
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakePeer) addr() string { return strings.TrimPrefix(p.srv.URL, "http://") }

// peerConfig is testConfig asking one fake peer, with an API key in the
// environment. PeerJobPath is left to httpapi.PeerJobPath's derivation, so
// the fake peer's path check pins that too.
func peerConfig(t *testing.T, shelly *powertest.FakeShelly, peer *fakePeer) config.Config {
	t.Helper()
	cfg := testConfig(t, shelly)
	cfg.PeerNodes = []string{peer.addr()}
	t.Setenv("F3SCTL_KEY", "k3y")
	return cfg
}

// TestPlugOffRefusesWhileAPeerRunsAJob: relayd may have handed the job to
// the other Pi, and a laptop has no job state of its own at all. The guard
// asks the API nodes as the API's own plug routes do, and refuses when one
// is mid-job.
func TestPlugOffRefusesWhileAPeerRunsAJob(t *testing.T) {
	shelly := powertest.NewFakeShelly(t, true)
	job := runningJob(time.Now())
	job.Node = "pi1"
	peer := newFakePeer(t, http.StatusOK, &job)
	cfg := peerConfig(t, shelly, peer)

	_, _, err := runCLI(t, cfg, hostsUp(), "fans", "off")
	if !errors.Is(err, coordination.ErrJobRunning) || !strings.Contains(err.Error(), "pi1") {
		t.Fatalf("fans off = %v, want ErrJobRunning naming pi1", err)
	}
	if got := shelly.SetCalls(); len(got) != 0 {
		t.Errorf("Switch.Set calls = %v, want none", got)
	}
	if got, _ := peer.key.Load().(string); got != "k3y" {
		t.Errorf("peer was sent API key %q, want the CLI's", got)
	}
}

// TestPlugOffTreatsAnIdleOrUnreachablePeerAsIdle: a peer with no running job
// lets the switch through after being asked twice (before and after the
// probe); one that fails to answer is treated as idle, the fail-open the API
// applies too -- a dead Pi must not lock the plugs.
func TestPlugOffTreatsAnIdleOrUnreachablePeerAsIdle(t *testing.T) {
	done := runningJob(time.Now())
	done.State = coordination.JobDone
	peers := map[string]func(t *testing.T) *fakePeer{
		"no job":   func(t *testing.T) *fakePeer { return newFakePeer(t, http.StatusOK, nil) },
		"finished": func(t *testing.T) *fakePeer { return newFakePeer(t, http.StatusOK, &done) },
		"failing":  func(t *testing.T) *fakePeer { return newFakePeer(t, http.StatusInternalServerError, nil) },
	}
	for name, newPeer := range peers {
		t.Run(name, func(t *testing.T) {
			shelly := powertest.NewFakeShelly(t, true)
			peer := newPeer(t)
			cfg := peerConfig(t, shelly, peer)

			if _, _, err := runCLI(t, cfg, hostsUp(), "ac", "off"); err != nil {
				t.Fatalf("ac off: %v", err)
			}
			if got := shelly.SetCalls(); len(got) != 1 || got[0] {
				t.Errorf("Switch.Set calls = %v, want exactly one with on=false", got)
			}
			if n := peer.hits.Load(); n != 2 {
				t.Errorf("peer asked %d times, want 2 (before and after the probe)", n)
			}
		})
	}
}

// TestPlugOffWithoutAnAPIKeyChecksThisHostOnly: the peers cannot be asked
// without a key. That is said on stderr, the peer is not contacted, and this
// host's own job state still guards.
func TestPlugOffWithoutAnAPIKeyChecksThisHostOnly(t *testing.T) {
	shelly := powertest.NewFakeShelly(t, true)
	peer := newFakePeer(t, http.StatusOK, nil)
	cfg := peerConfig(t, shelly, peer)
	t.Setenv("F3SCTL_KEY", "")
	cfg.APIKeyFile = filepath.Join(t.TempDir(), "missing")

	_, errOut, err := runCLI(t, cfg, hostsUp(), "fans", "off")
	if err != nil {
		t.Fatalf("fans off: %v", err)
	}
	if !strings.Contains(errOut, "checking this host only") {
		t.Errorf("stderr = %q, want it to say the peers were not asked", errOut)
	}
	if n := peer.hits.Load(); n != 0 {
		t.Errorf("peer asked %d times without a key, want never", n)
	}

	seedJobState(t, cfg.StateDir, runningJob(time.Now()))
	if _, _, err := runCLI(t, cfg, hostsUp(), "fans", "off"); !errors.Is(err, coordination.ErrJobRunning) {
		t.Fatalf("fans off with a local job = %v, want ErrJobRunning", err)
	}
	if got := shelly.SetCalls(); len(got) != 1 {
		t.Errorf("Switch.Set calls = %v, want only the first, idle, switch", got)
	}
}

// TestPlugOffBlocksAJobStartedDuringTheSwitch is the race the job lock is
// held for: a job starting on this node -- the CGI's Manager.Start, in its
// own process -- while the local plug write is in flight must be refused,
// not run a cycle under a plug being switched.
//
// The Start is real. Should the guard fail to hold the lock it would spawn
// this test binary, so its argv is one that runs no tests and exits.
func TestPlugOffBlocksAJobStartedDuringTheSwitch(t *testing.T) {
	for _, tc := range plugCases() {
		t.Run(tc.plug.noun, func(t *testing.T) {
			cfg := testConfig(t, powertest.NewFakeShelly(t, true))
			finished := runningJob(time.Now())
			finished.State = coordination.JobDone
			seedJobState(t, cfg.StateDir, finished)
			cgi := coordination.NewManager(cfg.StateDir, cfg.UnmuteTimeout.D(), power.ShutdownWorstCase(cfg))

			var startErr error
			eng := &fakePlugEngine{on: true, onSet: func() {
				_, startErr = cgi.Start("all-cycle", []string{"-test.run=^$"})
			}}
			var errOut bytes.Buffer
			jobs := newJobGuard(cfg, &errOut)

			if err := plugOff(context.Background(), eng, tc.plug, false, hostsUp().hosts, jobs, &bytes.Buffer{}); err != nil {
				t.Fatalf("plugOff: %v", err)
			}
			if !errors.Is(startErr, coordination.ErrJobRunning) {
				t.Fatalf("Start during the switch = %v, want ErrJobRunning", startErr)
			}
			if got := cgi.Read(); got == nil || got.State != coordination.JobDone {
				t.Errorf("job state = %+v, want the finished job untouched", got)
			}
			if want := []string{fmt.Sprintf(tc.setCallFmt, false)}; len(eng.calls) != 1 || eng.calls[0] != want[0] {
				t.Errorf("calls = %v, want %v", eng.calls, want)
			}
		})
	}
}
