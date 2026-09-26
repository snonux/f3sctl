package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/inventory"
)

// TestRouteTablesAreUnambiguous pins that the served route table has unique
// route names and unique method+path pairs, for the compiled-in inventory and
// for a renamed one: host routes are generated from the inventory, so their
// names must never collide with the fixed ones.
func TestRouteTablesAreUnambiguous(t *testing.T) {
	renamed := inventory.Inventory{Hosts: []inventory.Host{
		{Name: "g0", Role: inventory.RoleF},
		{Name: "g1", Role: inventory.RoleF},
		{Name: "g9", Role: inventory.RoleF, Standalone: true},
		{Name: "q0", Role: inventory.RoleCluster},
	}}
	for name, inv := range map[string]inventory.Inventory{"default": inventory.Default(), "renamed": renamed} {
		if err := checkRoutes(testRoutes(inv)); err != nil {
			t.Errorf("%s inventory: %v", name, err)
		}
	}
}

// TestNewRouterRefusesAmbiguousTables pins the structural guard behind the
// reserved host names: whatever lets a duplicate through, the router refuses
// it at construction instead of serving the first match under another
// route's name. The first case is the collision that prompted it -- an f-host
// called "fans", built directly so the inventory's own validation is bypassed,
// whose shutdown would have been offered as "fans-off".
func TestNewRouterRefusesAmbiguousTables(t *testing.T) {
	fansHost := inventory.Inventory{Hosts: []inventory.Host{
		{Name: "f0", Role: inventory.RoleF},
		{Name: "fans", Role: inventory.RoleF},
	}}
	get := func(name, path string) contract.Route {
		return contract.Route{Name: name, Method: http.MethodGet, Path: path}
	}

	for _, tc := range []struct {
		name, want string
		routes     []contract.Route
	}{
		{"host named fans", `duplicate route name "fans-on"`, testRoutes(fansHost)},
		{"same name", `duplicate route name "a"`, []contract.Route{get("a", "/a"), get("a", "/b")}},
		{"same endpoint", `duplicate route GET /a ("b")`, []contract.Route{get("a", "/a"), get("b", "/a")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if got := fmt.Sprint(recover()); !strings.Contains(got, tc.want) {
					t.Errorf("NewRouter panic = %q, want it to mention %q", got, tc.want)
				}
			}()
			NewRouter("", tc.routes)
		})
	}
}
