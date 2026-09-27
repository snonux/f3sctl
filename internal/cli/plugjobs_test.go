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
// environment. PeerJobPath is left to coordination.ResolvePeerJobPath, so
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
	if !errors.Is(err, coordination.ErrJobRunning) {
		t.Fatalf("fans off = %v, want ErrJobRunning", err)
	}
	for _, want := range []string{"all-cycle", "cyc1", "pi1", "power status", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
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
// probe); one that cannot be reached at all is treated as idle, the
// fail-open the API applies too -- a dead Pi must not lock the plugs.
func TestPlugOffTreatsAnIdleOrUnreachablePeerAsIdle(t *testing.T) {
	done := runningJob(time.Now())
	done.State = coordination.JobDone
	for name, job := range map[string]*coordination.Job{"no job": nil, "finished": &done} {
		t.Run(name, func(t *testing.T) {
			shelly := powertest.NewFakeShelly(t, true)
			peer := newFakePeer(t, http.StatusOK, job)
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
	t.Run("unreachable", func(t *testing.T) {
		shelly := powertest.NewFakeShelly(t, true)
		peer := newFakePeer(t, http.StatusOK, nil)
		cfg := peerConfig(t, shelly, peer)
		peer.srv.Close()

		if _, _, err := runCLI(t, cfg, hostsUp(), "ac", "off"); err != nil {
			t.Fatalf("ac off with the peer down: %v", err)
		}
		if got := shelly.SetCalls(); len(got) != 1 || got[0] {
			t.Errorf("Switch.Set calls = %v, want exactly one with on=false", got)
		}
	})
}

// TestPlugOffRefusesWhenAPeerAnswersWithoutAJob: a peer that answered, but
// not with a job, has not said it is idle. A 401 is a wrong or rotated API
// key, a 404 a wrong peer_job_path, a 5xx a broken node: each refuses with
// what to check, and switches nothing.
func TestPlugOffRefusesWhenAPeerAnswersWithoutAJob(t *testing.T) {
	cases := map[string]struct {
		status  int
		jobPath string // peer_job_path; "" derives the path the fake peer serves
	}{
		"401 bad key":  {status: http.StatusUnauthorized},
		"404 bad path": {status: http.StatusOK, jobPath: "/wrong/job"},
		"500":          {status: http.StatusInternalServerError},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			shelly := powertest.NewFakeShelly(t, true)
			peer := newFakePeer(t, tc.status, nil)
			cfg := peerConfig(t, shelly, peer)
			cfg.PeerJobPath = tc.jobPath
			live := hostsUp()

			_, _, err := runCLI(t, cfg, live, "fans", "off")
			if !errors.Is(err, coordination.ErrPeerJobUnknown) || errors.Is(err, coordination.ErrJobRunning) {
				t.Fatalf("fans off = %v, want ErrPeerJobUnknown", err)
			}
			for _, want := range []string{"cannot tell whether a power job is running", "API key", "--force"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want it to mention %q", err, want)
				}
			}
			if got := shelly.SetCalls(); len(got) != 0 || live.calls != 0 {
				t.Errorf("Switch.Set calls = %v, liveness calls = %d; want none", got, live.calls)
			}
		})
	}
}

// TestPlugOffWithoutAnAPIKeyRefuses: with peers to ask and no key to ask
// them with there is no job check at all on a laptop, which keeps no job
// state of its own. Not knowing is not idle: refuse, pointing at the key
// and --force, without contacting the peer.
func TestPlugOffWithoutAnAPIKeyRefuses(t *testing.T) {
	shelly := powertest.NewFakeShelly(t, true)
	peer := newFakePeer(t, http.StatusOK, nil)
	cfg := peerConfig(t, shelly, peer)
	t.Setenv("F3SCTL_KEY", "")
	cfg.APIKeyFile = filepath.Join(t.TempDir(), "missing")

	_, errOut, err := runCLI(t, cfg, hostsUp(), "fans", "off")
	if err == nil || errors.Is(err, coordination.ErrJobRunning) {
		t.Fatalf("fans off without an API key = %v, want a refusal", err)
	}
	for _, want := range []string{"no API key", "F3SCTL_KEY", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
	if got := shelly.SetCalls(); len(got) != 0 {
		t.Errorf("Switch.Set calls = %v, want none", got)
	}
	if n := peer.hits.Load(); n != 0 || errOut != "" {
		t.Errorf("peer asked %d times, stderr %q; want neither", n, errOut)
	}
}

// TestPlugOffWithNoPeersConfiguredChecksThisHostOnly: an empty peer_nodes is
// a deliberate "there are no API nodes to ask", not a missing key, so only
// this host's job state guards.
func TestPlugOffWithNoPeersConfiguredChecksThisHostOnly(t *testing.T) {
	shelly := powertest.NewFakeShelly(t, true)
	cfg := testConfig(t, shelly)
	t.Setenv("F3SCTL_KEY", "")
	cfg.APIKeyFile = filepath.Join(t.TempDir(), "missing")

	if _, _, err := runCLI(t, cfg, hostsUp(), "fans", "off"); err != nil {
		t.Fatalf("fans off: %v", err)
	}
	seedJobState(t, cfg.StateDir, runningJob(time.Now()))
	if _, _, err := runCLI(t, cfg, hostsUp(), "fans", "off"); !errors.Is(err, coordination.ErrJobRunning) {
		t.Fatalf("fans off with a local job = %v, want ErrJobRunning", err)
	}
	if got := shelly.SetCalls(); len(got) != 1 {
		t.Errorf("Switch.Set calls = %v, want only the first, idle, switch", got)
	}
}

// TestPlugForceAndOnNeverAskAboutJobs: --force skips the job guard and on is
// never gated, so neither contacts a peer -- here one that would refuse --
// nor reads the API key, nor prints anything about either.
func TestPlugForceAndOnNeverAskAboutJobs(t *testing.T) {
	for _, withKey := range []bool{true, false} {
		for _, args := range [][]string{{"fans", "off", "--force"}, {"ac", "off", "-f"}, {"fans", "on"}, {"ac", "on"}} {
			t.Run(fmt.Sprintf("key=%t/%s", withKey, strings.Join(args, " ")), func(t *testing.T) {
				shelly := powertest.NewFakeShelly(t, args[1] == "off")
				job := runningJob(time.Now())
				peer := newFakePeer(t, http.StatusOK, &job)
				cfg := peerConfig(t, shelly, peer)
				seedJobState(t, cfg.StateDir, job)
				if !withKey {
					t.Setenv("F3SCTL_KEY", "")
					cfg.APIKeyFile = filepath.Join(t.TempDir(), "missing")
				}

				_, errOut, err := runCLI(t, cfg, hostsUp("f0"), args...)
				if err != nil {
					t.Fatalf("%s: %v", strings.Join(args, " "), err)
				}
				if got := shelly.SetCalls(); len(got) != 1 {
					t.Errorf("Switch.Set calls = %v, want exactly one", got)
				}
				if n := peer.hits.Load(); n != 0 || errOut != "" {
					t.Errorf("peer asked %d times, stderr %q; want neither", n, errOut)
				}
			})
		}
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
			jobs := newJobGuard(cfg)

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

// TestPlugOffRefusesOnAnUnreadableStateDir: this host's job state that this
// user may not read is not an idle host. The refusal names the state dir and
// what to do, without assuming which host this is.
func TestPlugOffRefusesOnAnUnreadableStateDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory regardless")
	}
	shelly := powertest.NewFakeShelly(t, true)
	cfg := testConfig(t, shelly)
	finished := runningJob(time.Now())
	finished.State = coordination.JobDone
	seedJobState(t, cfg.StateDir, finished)
	if err := os.Chmod(cfg.StateDir, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfg.StateDir, 0o700) })

	_, _, err := runCLI(t, cfg, hostsUp(), "ac", "off")
	if err == nil || errors.Is(err, coordination.ErrJobRunning) {
		t.Fatalf("ac off = %v, want a refusal that is not ErrJobRunning", err)
	}
	for _, want := range []string{cfg.StateDir, "run as a user that can read it", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "pi0") {
		t.Errorf("err = %q, must not assume this host is a Pi", err)
	}
	if got := shelly.SetCalls(); len(got) != 0 {
		t.Errorf("Switch.Set calls = %v, want none", got)
	}
}
