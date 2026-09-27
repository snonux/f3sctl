package gogiosapi

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/snonux/f3sctl/internal/gogios"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
)

// echoActions is a contract.ActionRenderer that renders one action per name
// it is asked for, so a test can tell from a resource's actions list exactly
// which actions its handler requested. It stands in for the composition
// root's Router, which this package cannot import; this surface only ever
// calls ActionsFor, so SectionActions renders a marker that would show up in
// any assertion if a handler started calling it.
type echoActions struct{}

func (echoActions) ActionsFor(_ contract.State, names ...string) []contract.Action {
	out := make([]contract.Action, 0, len(names))
	for _, n := range names {
		out = append(out, contract.Action{Name: n})
	}
	return out
}

func (echoActions) SectionActions(contract.State, string) []contract.Action {
	return []contract.Action{{Name: "unexpected:SectionActions"}}
}

// fakeMonitor is a Monitor whose mute calls always succeed and whose gateway
// read reports one muted gateway.
type fakeMonitor struct{}

func (fakeMonitor) MuteGogios(context.Context, io.Writer) error { return nil }
func (fakeMonitor) UnmuteNow(context.Context, io.Writer) error  { return nil }
func (fakeMonitor) MonitoringStatus(context.Context) []gogios.GatewayMute {
	return []gogios.GatewayMute{{Name: "gw", Muted: true}}
}

// actionNames lists an entity's action names in order.
func actionNames(e contract.Entity) []string {
	var out []string
	for _, a := range e.Actions {
		out = append(out, a.Name)
	}
	return out
}

// TestNewRejectsANilActionRenderer pins the constructor's guard: a Surface
// with no renderer would serve /monitoring and /gogios with no mute controls
// -- indistinguishable, to a client, from "nothing is possible right now" --
// so New refuses to build one at all.
func TestNewRejectsANilActionRenderer(t *testing.T) {
	defer wantPanic(t, "nil ActionRenderer")
	New("test", contract.Hrefs(""), &fakeReports{}, fakeMonitor{}, nil)
}

// TestNewRejectsANilMonitor pins the Monitor guard: the NeedMonitoring
// Provider reads the mute through it for GET /gogios and every route
// rendering the mute pair, so a Surface without one would crash the Gogios
// folder on its first request rather than only the mute routes.
func TestNewRejectsANilMonitor(t *testing.T) {
	defer wantPanic(t, "nil Monitor")
	New("test", contract.Hrefs(""), &fakeReports{}, nil, echoActions{})
}

// wantPanic fails t unless the deferred call recovers a panic naming what,
// so each constructor guard is pinned by its own refusal rather than by
// whichever guard happens to fire first.
func wantPanic(t *testing.T, what string) {
	t.Helper()
	p := recover()
	if msg, _ := p.(string); !strings.Contains(msg, what) {
		t.Errorf("New recovered %v, want a panic naming %q", p, what)
	}
}

// TestNewRejectsANilReportSource pins the other constructor guard: the
// report routes read through the surface's source, so a Surface without one
// would only fail later, on the first report request, with a nil
// dereference. A nil *gogios.Source -- the production type -- wrapped in the
// interface is not nil to a plain == check, and is rejected too.
func TestNewRejectsANilReportSource(t *testing.T) {
	for name, reports := range map[string]ReportSource{
		"nil interface":      nil,
		"nil *gogios.Source": (*gogios.Source)(nil),
	} {
		t.Run(name, func(t *testing.T) {
			defer wantPanic(t, "nil ReportSource")
			New("test", contract.Hrefs(""), reports, fakeMonitor{}, echoActions{})
		})
	}
}

// TestConstructedSurfaceRendersActionsThroughItsRenderer serves every
// resource with controls from a Surface built by New -- through the route
// table's own handlers, the actions included -- and checks each actions list
// is exactly what the renderer New was given produced for it.
func TestConstructedSurfaceRendersActionsThroughItsRenderer(t *testing.T) {
	mutePair := []string{"monitoring-mute", "monitoring-unmute"}
	want := map[string][]string{
		"GET /monitoring":         mutePair,
		"POST /monitoring/mute":   mutePair,
		"POST /monitoring/unmute": mutePair,
		"GET /gogios":             append([]string{"gogios-cache-clear"}, mutePair...),
	}
	sf := New("test", contract.Hrefs(""), &fakeReports{}, fakeMonitor{}, echoActions{})
	// An unreachable report: the folder still renders, controls included.
	state := WithReport(contract.State{}, nil, errFake{})

	for _, r := range sf.Routes() {
		key := r.Method + " " + r.Path
		names, ok := want[key]
		if !ok {
			continue
		}
		delete(want, key)
		e, status, err := r.Handle(context.Background(), state, contract.Request{})
		if err != nil || status != http.StatusOK {
			t.Errorf("%s = %d, %v; want 200", key, status, err)
			continue
		}
		if got := actionNames(e); !slices.Equal(got, names) {
			t.Errorf("%s actions = %v, want %v", key, got, names)
		}
	}
	for key := range want {
		t.Errorf("no route %s in the table", key)
	}
}
