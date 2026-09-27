package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/gogios"
	"github.com/snonux/f3sctl/internal/inventory"
)

// gatewayRecorder is a fake gogiosapi.Monitor over scripted gateway states:
// a mute or un-mute sets every gateway it can reach and, like the engine's
// eachGateway, fails naming every one it could not. Each call is counted so a
// test can tell serve()'s 409 backstop from the handler running.
type gatewayRecorder struct {
	mu    sync.Mutex
	gws   []gogios.GatewayMute
	calls int
}

func (g *gatewayRecorder) MuteGogios(context.Context, io.Writer) error {
	return g.set(true, "gogios-mute")
}

func (g *gatewayRecorder) UnmuteNow(context.Context, io.Writer) error {
	return g.set(false, "gogios-unmute")
}

func (g *gatewayRecorder) set(muted bool, verb string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	var failed []string
	for i := range g.gws {
		if g.gws[i].Err != nil {
			failed = append(failed, g.gws[i].Name)
			continue
		}
		g.gws[i].Muted = muted
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not %s Gogios on: %v", verb, failed)
	}
	return nil
}

func (g *gatewayRecorder) MonitoringStatus(context.Context) []gogios.GatewayMute {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]gogios.GatewayMute(nil), g.gws...)
}

func (g *gatewayRecorder) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// muteServer is a Server whose Gogios surface drives gw, and whose
// NeedMonitoring Provider (run by enrichState) reads the mute from the same
// gw -- so availability and the handler see the same gateways, as they do in
// production through the engine.
func muteServer(t *testing.T, gw *gatewayRecorder) *Server {
	t.Helper()

	keyFile := filepath.Join(t.TempDir(), "apikey")
	if err := os.WriteFile(keyFile, []byte("sekrit\n"), 0o600); err != nil {
		t.Fatalf("writing the API key file: %v", err)
	}
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	inv := inventory.Default()
	return (&Server{
		cfg:   cfg,
		jobs:  coordination.NewManager(t.TempDir(), cfg.UnmuteTimeout.D(), 0),
		peers: coordination.NewPeerSet(nil, ""),
		auth:  NewAuthenticator(keyFile),
		siren: NewSirenRenderer(),
		node:  "test",
	}).assemble(inv, testPowerSurface(inv, ""), gogiosSurfaceOver("", unreachableReports(), gw), "")
}

// TestPostMonitoringMuteFinishesAPartialMute drives POST /monitoring/mute
// through the real pipeline (task ka). A partial mute -- one gateway muted,
// the other alerting or unreadable -- is attempted, and a fleet known muted
// on every gateway is refused with 409 before the handler runs. Before the
// fix the partial rows were refused too: the mute keyed on "nothing muted",
// so a half-done mute could not be finished through the API.
//
// The unreadable row still answers 502: the mute runs (wantCalls 1, not a
// 409 refusal) but cannot reach that gateway, and the engine reports it the
// way eachGateway does -- the point is that it is tried.
func TestPostMonitoringMuteFinishesAPartialMute(t *testing.T) {
	unreadable := errFake{}
	for _, tc := range []struct {
		name      string
		gws       []gogios.GatewayMute
		wantCode  int
		wantCalls int
	}{
		{"partial, alerting", []gogios.GatewayMute{{Name: "blowfish", Muted: true}, {Name: "fishfinger"}}, http.StatusOK, 1},
		{"partial, unreadable", []gogios.GatewayMute{{Name: "blowfish", Muted: true}, {Name: "fishfinger", Err: unreadable}}, http.StatusBadGateway, 1},
		{"all known muted", []gogios.GatewayMute{{Name: "blowfish", Muted: true}, {Name: "fishfinger", Muted: true}}, http.StatusConflict, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := &gatewayRecorder{gws: tc.gws}
			srv := muteServer(t, gw)

			if code := postStatus(t, srv, "/monitoring/mute"); code != tc.wantCode {
				t.Errorf("POST /monitoring/mute = %d, want %d", code, tc.wantCode)
			}
			if got := gw.callCount(); got != tc.wantCalls {
				t.Errorf("mute calls = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

// TestPostMonitoringMuteReportsTheResultingState pins what a finished partial
// mute answers with: the re-read monitoring resource, both gateways muted, and
// the mute no longer offered while the un-mute is.
func TestPostMonitoringMuteReportsTheResultingState(t *testing.T) {
	gw := &gatewayRecorder{gws: []gogios.GatewayMute{{Name: "blowfish", Muted: true}, {Name: "fishfinger"}}}
	e := postEntity(t, muteServer(t, gw), "/monitoring/mute")

	if muted, _ := e.Properties["muted"].(bool); !muted {
		t.Errorf("muted = %v, want true", e.Properties["muted"])
	}
	for _, st := range gw.MonitoringStatus(context.Background()) {
		if !st.Muted {
			t.Errorf("%s left alerting after the mute", st.Name)
		}
	}
	if hasAction(e, "monitoring-mute") || !hasAction(e, "monitoring-unmute") {
		t.Errorf("actions after mute = %v, want only monitoring-unmute", actionNames(e))
	}
}

// Negative: a mute that cannot reach a gateway answers 502 naming it, with no
// gateway state in the body -- the client must re-read /monitoring for that.
// The reachable gateway is muted all the same.
func TestPostMonitoringMuteNamesAnUnreachableGateway(t *testing.T) {
	gw := &gatewayRecorder{gws: []gogios.GatewayMute{{Name: "blowfish"}, {Name: "fishfinger", Err: errFake{}}}}
	e := postEntity(t, muteServer(t, gw), "/monitoring/mute")

	if msg, _ := e.Properties["message"].(string); msg != "could not gogios-mute Gogios on: [fishfinger]" {
		t.Errorf("message = %q, want fishfinger named", e.Properties["message"])
	}
	if _, ok := e.Properties["muted"]; ok {
		t.Errorf("properties = %v, want no mute state on a failed mute", e.Properties)
	}
	if st := gw.MonitoringStatus(context.Background()); !st[0].Muted {
		t.Errorf("blowfish = %+v, want it muted despite fishfinger failing", st[0])
	}
}
