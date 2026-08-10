package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/power"
)

// probeCounter counts calls to the two reads jy0 made lazy: the fleet probe
// (Engine.ProbeAll) and the Shelly plug read (Engine.FansStatus).
type probeCounter struct {
	probes, fanReads int
}

// countingServer returns a Server driven entirely through serve(), whose
// probeHosts and fansStatus seams are counters rather than a real
// *power.Engine -- see Server.probeHosts/fansStatus and probeHostsFn/
// fansStatusFn. No *power.Engine is built at all: every route exercised below
// (job, openapi.json, status) never reaches s.engine, so leaving it nil is
// safe and keeps the test off the network entirely.
//
// jobs and peers point at an empty temp dir and an empty peer set, so
// Manager.Read returns nil (no job has ever run) and PeerSet.Busy returns
// false without a network call (there are no peers to ask) -- both cheap and
// deterministic, matching what a real idle node would report.
func countingServer(t *testing.T) (*Server, *probeCounter) {
	t.Helper()

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "apikey")
	if err := os.WriteFile(keyFile, []byte("sekrit\n"), 0o600); err != nil {
		t.Fatalf("writing the API key file: %v", err)
	}

	pc := &probeCounter{}
	router := NewRouter("")
	srv := &Server{
		cfg:     config.Default(),
		jobs:    coordination.NewManager(dir),
		peers:   coordination.NewPeerSet(nil, ""),
		auth:    NewAuthenticator(keyFile),
		router:  router,
		openapi: NewOpenAPIBuilder(router),
		siren:   NewSirenRenderer(),
		node:    "test",
		probeHosts: func(context.Context) []power.HostStatus {
			pc.probes++
			return nil
		},
		fansStatus: func(context.Context) (power.FansState, error) {
			pc.fanReads++
			return power.FansState{}, nil
		},
	}
	return srv, pc
}

// getRequest builds a GET request against path, authenticated the way
// countingServer's Authenticator expects.
func getRequest(path string) request {
	return request{
		Method: http.MethodGet,
		Path:   path,
		APIKey: "sekrit",
		Query:  url.Values{},
		Form:   url.Values{},
	}
}

// TestResolvePeerJobPathDerivesFromSCRIPTNAME is the regression test for uy0:
// with cfg.PeerJobPath left at its empty default, the peer job path must
// track this node's own mount point (router's base, built from SCRIPT_NAME)
// rather than a hardcoded literal, so remounting the CGI script needs one
// change, not a config edit on top of it.
func TestResolvePeerJobPathDerivesFromSCRIPTNAME(t *testing.T) {
	router := NewRouter("/cgi-bin/f3sctl")
	cfg := config.Default()
	cfg.PeerJobPath = ""

	want := "/cgi-bin/f3sctl" + jobPath
	if got := resolvePeerJobPath(cfg, router); got != want {
		t.Errorf("resolvePeerJobPath(empty override) = %q, want %q", got, want)
	}
}

// TestResolvePeerJobPathHonoursExplicitOverride covers the escape hatch: two
// peers that are *not* mounted the same way still need a way to point at the
// other node's real path, so an explicit config value must win over the
// SCRIPT_NAME-derived default.
func TestResolvePeerJobPathHonoursExplicitOverride(t *testing.T) {
	router := NewRouter("/cgi-bin/f3sctl")
	cfg := config.Default()
	cfg.PeerJobPath = "/somewhere/else/job"

	if got := resolvePeerJobPath(cfg, router); got != cfg.PeerJobPath {
		t.Errorf("resolvePeerJobPath(explicit override) = %q, want %q", got, cfg.PeerJobPath)
	}
}

// TestSnapshotSkipsTheProbeForRoutesThatNeverReadIt is the regression test
// for jy0: serve() used to call s.snapshot(ctx) unconditionally, which ran
// Engine.ProbeAll (7 concurrent ping+TCP probes, ~3s) and Engine.FansStatus
// (a Shelly HTTP call, up to 5s) on every request -- including /job, polled
// every 10s through a multi-minute shutdown, and /openapi.json, a static
// document. Neither handler renders state.Hosts or state.Fans (see
// handleJob and handleOpenAPI), so both reads were pure waste on these two
// routes. This pins that they are no longer made at all.
func TestSnapshotSkipsTheProbeForRoutesThatNeverReadIt(t *testing.T) {
	for _, path := range []string{jobPath, openAPIPath} {
		t.Run(path, func(t *testing.T) {
			srv, pc := countingServer(t)
			var out bytes.Buffer
			if err := srv.serve(&out, getRequest(path)); err != nil {
				t.Fatalf("serve(%s): %v", path, err)
			}
			if pc.probes != 0 {
				t.Errorf("ProbeAll called %d times serving %s, want 0", pc.probes, path)
			}
			if pc.fanReads != 0 {
				t.Errorf("FansStatus called %d times serving %s, want 0", pc.fanReads, path)
			}
		})
	}
}

// TestSnapshotStillProbesRoutesThatNeedIt is the control for the test above:
// /status renders state.Hosts and state.Fans directly (handleStatus), so the
// laziness in snapshot() must not have turned into "never probes anything" --
// it has to still pay for exactly one probe and one fan read here.
func TestSnapshotStillProbesRoutesThatNeedIt(t *testing.T) {
	srv, pc := countingServer(t)
	var out bytes.Buffer
	if err := srv.serve(&out, getRequest("/status")); err != nil {
		t.Fatalf("serve(/status): %v", err)
	}
	if pc.probes != 1 {
		t.Errorf("ProbeAll called %d times serving /status, want exactly 1", pc.probes)
	}
	if pc.fanReads != 1 {
		t.Errorf("FansStatus called %d times serving /status, want exactly 1", pc.fanReads)
	}
}

// TestSkipsProbeCoversTheMonitoringFamily pins that skipsProbe treats every
// /monitoring path -- the resource itself and its mute/unmute actions -- the
// same way, matching the "/monitoring" prefix check enrichState already uses
// to decide whether to fetch the Gogios mute. handleMonitoring, handleMute
// and handleUnmute all render only state.Monitoring; see handlers.go.
func TestSkipsProbeCoversTheMonitoringFamily(t *testing.T) {
	for _, path := range []string{"/monitoring", "/monitoring/mute", "/monitoring/unmute"} {
		if !skipsProbe(path) {
			t.Errorf("skipsProbe(%q) = false, want true: no monitoring handler reads state.Hosts or state.Fans", path)
		}
	}
	for _, path := range []string{"/", "/status", "/fans", "/fans/on", "/fans/off", "/power/off"} {
		if skipsProbe(path) {
			t.Errorf("skipsProbe(%q) = true, want false: this route's handler or Available predicates do read the probe", path)
		}
	}
}
