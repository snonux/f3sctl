package httpapi

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/gogios"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/httpapi/gogiosapi"
	"github.com/snonux/f3sctl/internal/httpapi/powerapi"
	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power"
)

// TestRenderingBeforeBuildPanics is the negative case of serverActions: a
// renderer whose Server has no Router yet must fail loudly, not answer with
// an empty actions list a client would read as "nothing is possible right
// now". build never lets this happen -- it builds the Router before anything
// can serve -- so this pins the backstop, not a reachable path.
func TestRenderingBeforeBuildPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("rendered actions with no Router built; want a panic")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "before Server.build") {
			t.Errorf("panic = %v, want it to name the missing Server.build", r)
		}
	}()
	(&Server{}).actionRenderer().ActionsFor(contract.State{}, "fans-on")
}

// recordingActions is a contract.ActionRenderer that renders nothing and
// records every action name and section a handler asks it for.
type recordingActions struct {
	names, sections map[string]bool
}

func newRecordingActions() *recordingActions {
	return &recordingActions{names: map[string]bool{}, sections: map[string]bool{}}
}

// Actions renders the whole table, so it names nothing that could be wrong.
func (*recordingActions) Actions(contract.State) []contract.Action { return nil }

func (r *recordingActions) ActionsFor(_ contract.State, names ...string) []contract.Action {
	for _, n := range names {
		r.names[n] = true
	}
	return nil
}

func (r *recordingActions) SectionActions(_ contract.State, section string) []contract.Action {
	r.sections[section] = true
	return nil
}

// TestHandlersRequestOnlyDeclaredActions runs every handler of both surfaces
// with a recording renderer and requires every action name a handler asks
// for to be an action route in the real default route table, and every
// section it asks for to hold at least one. A typo ("ac-onn") in a handler's
// hand-written name list would otherwise just drop that action from the
// resource, silently -- the renderer skips names it does not know.
func TestHandlersRequestOnlyDeclaredActions(t *testing.T) {
	rec := newRecordingActions()
	runEveryHandler(t, rec)
	if len(rec.names) == 0 || len(rec.sections) == 0 {
		t.Fatalf("recorded names %v, sections %v; want both non-empty", rec.names, rec.sections)
	}

	names, sections := map[string]bool{}, map[string]bool{}
	for _, r := range testRoutes(inventory.Default()) {
		if r.Action {
			names[r.Name] = true
			sections[r.Section] = true
		}
	}
	for n := range rec.names {
		if !names[n] {
			t.Errorf("a handler requests action %q, which is no action route", n)
		}
	}
	for s := range rec.sections {
		if !sections[s] {
			t.Errorf("a handler requests section %q, which holds no action route", s)
		}
	}
}

// runEveryHandler serves every route of both surfaces, built on well-behaved
// fakes and rendering through actions, against a state in which every
// handler reaches its rendering. A handler that panics fails the test.
func runEveryHandler(t *testing.T, actions contract.ActionRenderer) {
	t.Helper()
	o := docOpts{}.withDefaults()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.GogiosURL = "http://127.0.0.1:1" // refused instantly: no network in tests
	inv := inventory.Default()
	pw := powerapi.New("test", contract.Hrefs(""), inv, o.eng, o.jobs, o.peers, actions)
	gg := gogiosapi.New("test", contract.Hrefs(""), cfg, o.monitor, actions)

	state := contract.State{
		Fans: power.FansState{On: true}, AC: power.ACState{On: true},
		Monitoring: []power.GatewayMute{{Name: "gw", Muted: true}},
		Gogios:     &gogios.Report{},
	}
	req := contract.Request{Query: url.Values{"name": {"x"}}, Form: url.Values{"force": {"true"}}}
	for _, r := range append(pw.Routes(), gg.Routes()...) {
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("%s %s panicked: %v", r.Method, r.Path, p)
				}
			}()
			_, _, _ = r.Handle(context.Background(), state, req)
		}()
	}
}
