package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

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
	inv := inventory.Default()
	pw := powerapi.New("test", contract.Hrefs(""), inv, o.eng, o.jobs, o.peers, actions)
	gg := gogiosapi.New("test", contract.Hrefs(""), unreachableReports(), o.monitor, actions)

	state := contract.State{
		Fans: power.FansState{On: true}, AC: power.ACState{On: true},
		Monitoring: []gogios.GatewayMute{{Name: "gw", Muted: true}},
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

// TestTestRouterSharesItsBaseWithTheSurfaces pins the helper the router tests
// lean on: a testRouter mounted at a non-empty base serves resources from
// both surfaces whose self links and advertised action hrefs are all built
// under that same base -- as newServer builds them -- so a test never mixes
// hrefs from two different mounts.
func TestTestRouterSharesItsBaseWithTheSurfaces(t *testing.T) {
	const base = "/cgi-bin/f3sctl"
	rt := testRouter(inventory.Default(), base)
	state := contract.State{
		Fans:       power.FansState{On: true},
		Monitoring: []gogios.GatewayMute{{Name: "gw", Muted: true}},
	}

	for _, path := range []string{"/fans", "/monitoring"} {
		r, ok := rt.Lookup(http.MethodGet, path)
		if !ok {
			t.Fatalf("no GET %s route", path)
		}
		e, _, err := r.Handle(context.Background(), state, contract.Request{})
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if len(e.Actions) == 0 {
			t.Errorf("GET %s advertises no actions; want at least one to check", path)
		}
		if self := selfHref(e); self != base+path {
			t.Errorf("GET %s self link = %q, want %q", path, self, base+path)
		}
		for _, a := range e.Actions {
			if !strings.HasPrefix(a.Href, base+"/") {
				t.Errorf("GET %s action %s href = %q, want it under %q", path, a.Name, a.Href, base)
			}
		}
	}
}

// selfHref returns the href of e's self link, or "" if it has none.
func selfHref(e contract.Entity) string {
	for _, l := range e.Links {
		if len(l.Rel) > 0 && l.Rel[0] == "self" {
			return l.Href
		}
	}
	return ""
}
