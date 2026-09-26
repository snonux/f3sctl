package httpapi

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power"
)

// routeAt finds the route serving method and path in rs.
func routeAt(t *testing.T, rs []contract.Route, method, path string) contract.Route {
	t.Helper()
	for _, r := range rs {
		if r.Method == method && r.Path == path {
			return r
		}
	}
	t.Fatalf("no route %s %s", method, path)
	return contract.Route{}
}

// TestSurfaceRendersTheBuiltRoutersActions pins the lazy resolution: a
// surface constructed with a Server's actionRenderer before that Server has a
// Router renders, once Server.build has run, exactly the actions that Router
// judges possible -- and a non-empty list where one is possible, so the
// surface is never on a silent no-actions path.
func TestSurfaceRendersTheBuiltRoutersActions(t *testing.T) {
	inv := inventory.Default()
	srv := &Server{}
	pw := testPowerSurface(inv, srv.actionRenderer())
	srv.assemble(inv, pw, testGogiosSurface(srv.actionRenderer()), "/cgi-bin/f3sctl")

	state := contract.State{Fans: power.FansState{On: true}}
	e, _, err := routeAt(t, pw.Routes(), http.MethodGet, "/fans").Handle(context.Background(), state, contract.Request{})
	if err != nil {
		t.Fatalf("GET /fans: %v", err)
	}
	want := srv.router.ActionsFor(state, "fans-on", "fans-off")
	if len(want) == 0 {
		t.Fatal("the Router offers no fan action with the fans on; the test state is wrong")
	}
	if !reflect.DeepEqual(e.Actions, want) {
		t.Errorf("GET /fans actions = %+v, want the Router's %+v", e.Actions, want)
	}
	if !strings.HasPrefix(e.Actions[0].Href, "/cgi-bin/f3sctl/") {
		t.Errorf("action href %q is not under the built Router's base", e.Actions[0].Href)
	}
}

// TestRenderingBeforeBuildPanics is the negative case: serving a surface
// whose Server was never built must fail loudly, not answer with an empty
// actions list a client would read as "nothing is possible right now".
func TestRenderingBeforeBuildPanics(t *testing.T) {
	srv := &Server{}
	pw := testPowerSurface(inventory.Default(), srv.actionRenderer())
	fans := routeAt(t, pw.Routes(), http.MethodGet, "/fans")

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("GET /fans rendered before Server.build; want a panic")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "before Server.build") {
			t.Errorf("panic = %v, want it to name the missing Server.build", r)
		}
	}()
	_, _, _ = fans.Handle(context.Background(), contract.State{}, contract.Request{})
}
