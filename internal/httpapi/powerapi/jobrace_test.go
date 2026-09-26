package powerapi

import (
	"context"
	"encoding/json"
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
	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/power"
)

// switchablePeer is the other API node's /job, idle until running is set --
// so a test can have the peer start a job at a chosen moment.
func switchablePeer(t *testing.T) (*coordination.PeerSet, *atomic.Bool) {
	t.Helper()
	running := &atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if running.Load() {
			fmt.Fprint(w, peerRunningJobBody)
			return
		}
		fmt.Fprint(w, `{"properties":{"state":"none"}}`)
	}))
	t.Cleanup(srv.Close)
	return &coordination.PeerSet{Nodes: []string{srv.Listener.Addr().String()}, JobPath: "/job"}, running
}

// startLocalJob records a running job in dir's job.json, the way a spawned
// child does the moment it starts.
func startLocalJob(t *testing.T, dir string) {
	t.Helper()
	raw, err := json.Marshal(coordination.Job{
		ID: "local-job", Action: "all-cycle", State: coordination.JobRunning,
		Started: time.Now().UTC().Format(time.RFC3339), Node: "test",
	})
	if err != nil {
		t.Fatalf("encoding job: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "job.json"), raw, 0o600); err != nil {
		t.Fatalf("writing job.json: %v", err)
	}
}

// TestPlugOffRefusedWhenAJobStartsDuringTheProbe pins the re-check between
// the confirming probe and the plug write. serve() judged the request with no
// job running; the probe then takes up to a minute, and a job started in that
// window -- here, or on the peer -- must not have its plug switched under it.
//
// The "no job" rows are the control: the same probe with nothing started
// still switches the plug, so the refusals can only come from the job.
func TestPlugOffRefusedWhenAJobStartsDuringTheProbe(t *testing.T) {
	handlers := map[string]func(*Surface) contract.Handle{
		"fans-off": func(sf *Surface) contract.Handle { return sf.handleFansOff },
		"ac-off":   func(sf *Surface) contract.Handle { return sf.handleACOff },
	}
	for action, handler := range handlers {
		for _, src := range []string{"no job", "local job", "peer job"} {
			t.Run(action+" "+src, func(t *testing.T) {
				plug := newFakePlug(t)
				dir := t.TempDir()
				peers, peerRunning := switchablePeer(t)
				sf := testSurface(t, plug, func(context.Context) power.RackActivity {
					switch src {
					case "local job":
						startLocalJob(t, dir)
					case "peer job":
						peerRunning.Store(true)
					}
					return power.RackActivity{} // the rack is confirmed idle
				})
				sf.Jobs = coordination.NewManager(dir, config.Default().UnmuteTimeout.D(), 0)
				sf.Peers = peers

				_, status, err := handler(sf)(context.Background(), coldSnapshot(), contract.Request{})
				if src == "no job" {
					if err != nil || status != http.StatusOK {
						t.Fatalf("status = %d, err = %v, want 200: nothing started", status, err)
					}
					if got := plug.setCalls(); len(got) != 1 || got[0] {
						t.Fatalf("Switch.Set calls = %v, want exactly one with on=false", got)
					}
					return
				}
				if status != http.StatusConflict {
					t.Fatalf("status = %d, want %d: a job started during the probe", status, http.StatusConflict)
				}
				if err == nil || !strings.Contains(err.Error(), "not available right now") {
					t.Errorf("error = %v, want serve()'s not-available refusal", err)
				}
				if got := plug.setCalls(); len(got) != 0 {
					t.Fatalf("Switch.Set calls = %v, want none", got)
				}
			})
		}
	}
}

// TestPlugOffReCheckTreatsADownPeerAsIdle pins the re-check's fail-open: a
// peer that cannot be reached counts as idle, as everywhere else PeerSet.Busy
// is asked, so one node being down never stops the other switching the plugs.
func TestPlugOffReCheckTreatsADownPeerAsIdle(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	addr := down.Listener.Addr().String()
	down.Close() // connection refused from here on

	plug := newFakePlug(t)
	sf := testSurface(t, plug, func(context.Context) power.RackActivity { return power.RackActivity{} })
	sf.Peers = &coordination.PeerSet{Nodes: []string{addr}, JobPath: "/job"}

	_, status, err := sf.handleACOff(context.Background(), coldSnapshot(), contract.Request{})
	if err != nil || status != http.StatusOK {
		t.Fatalf("status = %d, err = %v, want 200: an unreachable peer is idle", status, err)
	}
	if got := plug.setCalls(); len(got) != 1 || got[0] {
		t.Fatalf("Switch.Set calls = %v, want exactly one with on=false", got)
	}
}

// TestJobStartedDuringTheProbeOutranksTheProbesVerdict pins the order of the
// two refusals. A job that starts during the probe usually wakes the hosts, so
// the probe hears them and says "busy"; answering that with the probe's
// "re-send with force=true" would invite a client to force a plug switch
// under a running job. The job's not-available refusal must win.
func TestJobStartedDuringTheProbeOutranksTheProbesVerdict(t *testing.T) {
	handlers := map[string]func(*Surface) contract.Handle{
		"fans-off": func(sf *Surface) contract.Handle { return sf.handleFansOff },
		"ac-off":   func(sf *Surface) contract.Handle { return sf.handleACOff },
	}
	for action, handler := range handlers {
		for _, src := range []string{"local job", "peer job"} {
			t.Run(action+" "+src, func(t *testing.T) {
				plug := newFakePlug(t)
				dir := t.TempDir()
				peers, peerRunning := switchablePeer(t)
				sf := testSurface(t, plug, func(context.Context) power.RackActivity {
					if src == "local job" {
						startLocalJob(t, dir)
					} else {
						peerRunning.Store(true)
					}
					// The job's wake is under way: the probe hears f1.
					return power.RackActivityFrom([]power.HostStatus{fState("f1", true, true)})
				})
				sf.Jobs = coordination.NewManager(dir, config.Default().UnmuteTimeout.D(), 0)
				sf.Peers = peers

				_, status, err := handler(sf)(context.Background(), coldSnapshot(), contract.Request{})
				if status != http.StatusConflict {
					t.Fatalf("status = %d, want %d", status, http.StatusConflict)
				}
				if err == nil || !strings.Contains(err.Error(), "not available right now") {
					t.Errorf("error = %v, want the not-available refusal, not the probe's force advice", err)
				}
				if got := plug.setCalls(); len(got) != 0 {
					t.Fatalf("Switch.Set calls = %v, want none", got)
				}
			})
		}
	}
}
