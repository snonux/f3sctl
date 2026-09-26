package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/httpapi/powerapi"
	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power"
)

// plugRecorder is a powerapi.Engine that records every plug write and every
// confirming probe, and reports an idle rack, so a handler that is reached
// goes straight through to the write. The writes tell "refused" from
// "performed"; the probes tell serve()'s refusal from the handler's own.
type plugRecorder struct {
	mu        sync.Mutex
	fans, acs []bool
	probes    int
}

func (p *plugRecorder) FansSet(_ context.Context, on bool) (power.FansState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fans = append(p.fans, on)
	return power.FansState{On: on}, nil
}

func (p *plugRecorder) ACSet(_ context.Context, on bool) (power.ACState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.acs = append(p.acs, on)
	return power.ACState{On: on}, nil
}

func (p *plugRecorder) RackActivity(context.Context) power.RackActivity { return p.probe() }
func (p *plugRecorder) ACActivity(context.Context) power.RackActivity   { return p.probe() }

func (p *plugRecorder) probe() power.RackActivity {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probes++
	return power.RackActivity{}
}

func (p *plugRecorder) writes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.fans) + len(p.acs)
}

func (p *plugRecorder) probeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.probes
}

// jobSource says where a running job lives for jobGateServer.
type jobSource string

const (
	noJob    jobSource = "no job"
	localJob jobSource = "local job"
	peerJob  jobSource = "peer job"
)

// jobGateServer is a Server over a cold fleet (every host silent, so no force
// field is ever needed) with both Shelly plugs reading plugsOn, a plug-writing
// fake engine, and a running power job placed according to src.
func jobGateServer(t *testing.T, plugsOn bool, src jobSource) (*Server, *plugRecorder) {
	t.Helper()

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "apikey")
	if err := os.WriteFile(keyFile, []byte("sekrit\n"), 0o600); err != nil {
		t.Fatalf("writing the API key file: %v", err)
	}
	running := coordination.Job{
		ID: "j1", Action: "all-cycle", State: coordination.JobRunning,
		Started: time.Now().UTC().Format(time.RFC3339), Node: "pi1",
	}
	if src == localJob {
		writeJobFile(t, dir, running)
	}
	peers := coordination.NewPeerSet(nil, "")
	if src == peerJob {
		peers = coordination.NewPeerSet([]string{fakePeer(t, running)}, "/job")
	}

	cfg := config.Default()
	jobs := coordination.NewManager(dir, cfg.UnmuteTimeout.D(), 0)
	eng := &plugRecorder{}
	inv := inventory.Default()
	surface := func(a contract.ActionRenderer) *powerapi.Surface {
		return powerapi.New("test", contract.Hrefs(""), inv, eng, jobs, peers, a)
	}
	srv := (&Server{
		cfg: cfg, jobs: jobs, peers: peers,
		auth: NewAuthenticator(keyFile), siren: NewSirenRenderer(), node: "test",
		probeHosts: func(context.Context) []power.HostStatus { return nil },
		fansStatus: func(context.Context) (power.FansState, error) {
			return power.FansState{On: plugsOn}, nil
		},
		acStatus: func(context.Context) (power.ACState, error) {
			return power.ACState{On: plugsOn}, nil
		},
	}).assemble(inv, surface, testGogiosSurface(), "")
	return srv, eng
}

// writeJobFile records j as this node's job, the way a spawned child would.
func writeJobFile(t *testing.T, dir string, j coordination.Job) {
	t.Helper()
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatalf("encoding job: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "job.json"), raw, 0o600); err != nil {
		t.Fatalf("writing job.json: %v", err)
	}
}

// fakePeer serves j as the other API node's /job and returns its host:port.
func fakePeer(t *testing.T, j coordination.Job) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"class": []string{"job"}, "properties": j})
	if err != nil {
		t.Fatalf("encoding peer job: %v", err)
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(peer.Close)
	return strings.TrimPrefix(peer.URL, "http://")
}

// postStatus serves one authenticated POST through the real pipeline and
// returns the CGI status code it wrote.
func postStatus(t *testing.T, srv *Server, path string) int {
	t.Helper()
	req := contract.Request{
		Method: http.MethodPost, Path: path, APIKey: "sekrit",
		Query: url.Values{}, Form: url.Values{},
	}
	var out bytes.Buffer
	if err := srv.serve(&out, req); err != nil {
		t.Fatalf("serve(POST %s): %v", path, err)
	}
	line, _, _ := bytes.Cut(out.Bytes(), []byte("\r\n"))
	fields := strings.Fields(strings.TrimPrefix(string(line), "Status: "))
	if len(fields) == 0 {
		t.Fatalf("POST %s wrote no status line:\n%s", path, out.String())
	}
	code, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("POST %s status line %q: %v", path, line, err)
	}
	return code
}

// TestPlugSwitchesRefusedWhileAJobRuns exercises serve()'s 409 backstop for
// the Shelly plugs: a client racing a job (or ignoring the advertised
// actions) must be refused before the handler runs, with no plug touched.
// This is what stops an ac-off during an all-cycle's silent standby wait
// from cutting mains under the wake half.
//
// The no-job rows are the control: the same request against the same fleet
// is performed, so a 409 in the job rows can only come from the job. Zero
// confirming probes in the job rows (against one per off in the control)
// proves it was serve() that refused, not the handler's own later re-check
// (jobStartedMeanwhile) -- that one would also 409 with no write, so without
// this the routes' !JobRunning could be dropped and nothing here would notice.
func TestPlugSwitchesRefusedWhileAJobRuns(t *testing.T) {
	paths := []string{"/fans/on", "/fans/off", "/ac/on", "/ac/off"}
	for _, src := range []jobSource{noJob, localJob, peerJob} {
		for _, path := range paths {
			t.Run(string(src)+" "+path, func(t *testing.T) {
				// Each switch is judged in the plug state that would offer it.
				off := strings.HasSuffix(path, "/off")
				srv, eng := jobGateServer(t, off, src)
				want, wantWrites, wantProbes := http.StatusConflict, 0, 0
				if src == noJob {
					want, wantWrites = http.StatusOK, 1
					if off {
						wantProbes = 1
					}
				}
				if got := postStatus(t, srv, path); got != want {
					t.Errorf("POST %s status = %d, want %d", path, got, want)
				}
				if got := eng.writes(); got != wantWrites {
					t.Errorf("POST %s made %d plug writes, want %d", path, got, wantWrites)
				}
				if got := eng.probeCount(); got != wantProbes {
					t.Errorf("POST %s ran %d confirming probes, want %d", path, got, wantProbes)
				}
			})
		}
	}
}

// TestACControlFolderWithholdsPlugActionsDuringAJob is the job-running
// counterpart of TestACControlFolderOffersThePlugActions: with both plugs on
// and a job in flight (here or on the peer), /ac-control offers no switch.
func TestACControlFolderWithholdsPlugActionsDuringAJob(t *testing.T) {
	for _, src := range []jobSource{localJob, peerJob} {
		t.Run(string(src), func(t *testing.T) {
			srv, _ := jobGateServer(t, true, src)
			e := getEntity(t, srv, "/ac-control")
			for _, name := range []string{"fans-on", "fans-off", "ac-on", "ac-off"} {
				if hasAction(e, name) {
					t.Errorf("ac-control actions = %v, want %s withheld while a job runs", actionNames(e), name)
				}
			}
			for _, rel := range []string{"fans", "ac"} {
				if !hasServedRel(e.Links, rel) {
					t.Errorf("ac-control links = %+v, missing rel %q: the plugs stay readable", e.Links, rel)
				}
			}
		})
	}
}
