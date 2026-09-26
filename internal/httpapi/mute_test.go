package httpapi

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/httpapi/gogiosapi"
	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power"
)

// gatewayRecorder is a fake gogiosapi.Monitor over scripted gateway states:
// a mute or un-mute sets every gateway it can reach, and each call is counted
// so a test can tell serve()'s 409 backstop from the handler running.
type gatewayRecorder struct {
	mu    sync.Mutex
	gws   []power.GatewayMute
	calls int
}

func (g *gatewayRecorder) MuteGogios(context.Context, io.Writer) error { return g.set(true) }
func (g *gatewayRecorder) UnmuteNow(context.Context, io.Writer) error  { return g.set(false) }

func (g *gatewayRecorder) set(muted bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	for i := range g.gws {
		if g.gws[i].Err == nil {
			g.gws[i].Muted = muted
		}
	}
	return nil
}

func (g *gatewayRecorder) MonitoringStatus(context.Context) []power.GatewayMute {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]power.GatewayMute(nil), g.gws...)
}

func (g *gatewayRecorder) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// muteServer is a Server whose Gogios surface drives gw, and whose enrichState
// reads the mute from gw too -- so availability and the handler see the same
// gateways, as they do in production through the engine.
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
		cfg:           cfg,
		jobs:          coordination.NewManager(t.TempDir(), cfg.UnmuteTimeout.D(), 0),
		peers:         coordination.NewPeerSet(nil, ""),
		auth:          NewAuthenticator(keyFile),
		siren:         NewSirenRenderer(),
		node:          "test",
		monitorStatus: gw.MonitoringStatus,
	}).assemble(inv, testPowerSurface(inv), gogiosapi.New("test", contract.Hrefs(""), cfg, gw), "")
}

// TestPostMonitoringMuteFinishesAPartialMute drives POST /monitoring/mute
// through the real pipeline (task ka). A partial mute -- one gateway muted,
// the other alerting or unreadable -- is performed, and a fleet known muted
// on every gateway is refused with 409 before the handler runs. Before the
// fix the partial rows were refused too: the mute keyed on "nothing muted",
// so a half-done mute could not be finished through the API.
func TestPostMonitoringMuteFinishesAPartialMute(t *testing.T) {
	unreadable := errFake{}
	for _, tc := range []struct {
		name      string
		gws       []power.GatewayMute
		wantCode  int
		wantCalls int
	}{
		{"partial, alerting", []power.GatewayMute{{Name: "blowfish", Muted: true}, {Name: "fishfinger"}}, http.StatusOK, 1},
		{"partial, unreadable", []power.GatewayMute{{Name: "blowfish", Muted: true}, {Name: "fishfinger", Err: unreadable}}, http.StatusOK, 1},
		{"all known muted", []power.GatewayMute{{Name: "blowfish", Muted: true}, {Name: "fishfinger", Muted: true}}, http.StatusConflict, 0},
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
	gw := &gatewayRecorder{gws: []power.GatewayMute{{Name: "blowfish", Muted: true}, {Name: "fishfinger"}}}
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
