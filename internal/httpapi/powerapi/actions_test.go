package powerapi

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/inventory"
)

// echoActions is a contract.ActionRenderer that renders an action for exactly
// what it was asked, so a test can tell from a resource's actions list which
// renderer method its handler called and with what: ActionsFor yields one
// action per requested name, and SectionActions "section:<name>". It stands in for the composition root's Router, which
// this package cannot import.
type echoActions struct{}

func (echoActions) ActionsFor(_ contract.State, names ...string) []contract.Action {
	out := make([]contract.Action, 0, len(names))
	for _, n := range names {
		out = append(out, contract.Action{Name: n})
	}
	return out
}

func (echoActions) SectionActions(_ contract.State, section string) []contract.Action {
	return []contract.Action{{Name: "section:" + section}}
}

// testNew is New with inert collaborators and the echo renderer: enough to
// declare the table and serve every resource that reads only state.
func testNew(inv inventory.Inventory) *Surface {
	return New("test", contract.Hrefs(""), inv, nil, nil, nil, echoActions{})
}

// TestNewRejectsANilActionRenderer pins the constructor's guard: a Surface
// with no renderer would serve every resource with an empty actions list --
// indistinguishable, to a client, from "nothing is possible right now" -- so
// New refuses to build one at all.
func TestNewRejectsANilActionRenderer(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("New with a nil ActionRenderer returned a Surface; want a panic")
		}
	}()
	New("test", contract.Hrefs(""), inventory.Default(), nil, nil, nil, nil)
}

// TestConstructedSurfaceRendersActionsThroughItsRenderer serves every
// resource with controls from a Surface built by New, through the route
// table's own handlers, and checks each actions list is exactly what the
// renderer New was given produced for that resource -- no resource renders
// its own actions, and none silently renders none.
func TestConstructedSurfaceRendersActionsThroughItsRenderer(t *testing.T) {
	want := map[string][]string{
		StatusPath:    {"section:" + contract.SectionPower, "section:" + contract.SectionAC},
		"/power":      {"section:" + contract.SectionPower},
		"/ac-control": {"section:" + contract.SectionAC},
		"/fans":       {"fans-on", "fans-off"},
		"/ac":         {"ac-on", "ac-off"},
	}
	for _, r := range testNew(inventory.Default()).Routes() {
		names, ok := want[r.Path]
		if !ok || r.Method != http.MethodGet {
			continue
		}
		delete(want, r.Path)
		e, _, err := r.Handle(context.Background(), contract.State{}, contract.Request{})
		if err != nil {
			t.Errorf("GET %s: %v", r.Path, err)
			continue
		}
		var got []string
		for _, a := range e.Actions {
			got = append(got, a.Name)
		}
		if !slices.Equal(got, names) {
			t.Errorf("GET %s actions = %v, want %v", r.Path, got, names)
		}
	}
	for path := range want {
		t.Errorf("no GET route for %s in the table", path)
	}
}
