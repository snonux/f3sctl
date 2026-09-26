package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
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
	get := func(name, path string) contract.Route {
		return contract.Route{Name: name, Method: http.MethodGet, Path: path}
	}

	for _, tc := range []struct {
		name, want string
		routes     []contract.Route
	}{
		{"host named fans", `duplicate route name "fans-on"`, declaredRoutes(fansHostInventory())},
		{"same name", `duplicate route name "a"`, []contract.Route{get("a", "/a"), get("a", "/b")}},
		{"same endpoint", `duplicate route GET /a ("b")`, []contract.Route{get("a", "/a"), get("b", "/a")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, err := NewRouter("", tc.routes)
			if !errors.Is(err, ErrAmbiguousRoutes) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("NewRouter err = %v, want ErrAmbiguousRoutes mentioning %q", err, tc.want)
			}
			if rt != nil {
				t.Error("NewRouter returned a router for an ambiguous table")
			}
		})
	}
}

// fansHostInventory is an inventory the loader would reject (an f-host named
// "fans"), built directly to reach the router's own guard.
func fansHostInventory() inventory.Inventory {
	return inventory.Inventory{Hosts: []inventory.Host{
		{Name: "f0", Role: inventory.RoleF},
		{Name: "fans", Role: inventory.RoleF},
	}}
}

// TestServeCGIReportsAnAmbiguousRouteTableAsA500 pins how the guard surfaces
// in production: the CGI builds its Server per request, so a refused route
// table must come back as a readable Siren error with status 500 -- like an
// unreadable SSH key -- rather than a panic that kills every request with a
// bare server error.
func TestServeCGIReportsAnAmbiguousRouteTableAsA500(t *testing.T) {
	cfg := serverTestConfig(t, "correct-key")
	cfg.Inventory.Hosts = fansHostInventory().Hosts
	cgiEnvForJob(t, "correct-key")

	var out bytes.Buffer
	if err := ServeCGI(cfg, &out); err != nil {
		t.Fatalf("ServeCGI: %v", err)
	}

	headers, body := splitCGIResponse(t, out.String())
	if headers["Status"] != "500 Internal Server Error" {
		t.Fatalf("Status header = %q, want 500\nbody: %s", headers["Status"], body)
	}
	var got contract.Entity
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", err, body)
	}
	if !slices.Contains(got.Class, "error") {
		t.Errorf("class = %v, want a Siren error entity", got.Class)
	}
	if msg, _ := got.Properties["message"].(string); !strings.Contains(msg, `duplicate route name "fans-on"`) {
		t.Errorf("properties.message = %q, want it to name the duplicate", msg)
	}
}

// TestFixedRouteWordsAreReservedHostNames ties inventory.reservedHostNames to
// the route table it protects. Host routes are named <host>-on/<host>-off and
// served at /power/<host>/<verb>; every fixed route of that shape -- power-on,
// fans-off, /power/all/cycle, ... -- claims a word no host may be called, or
// a host by that name would collide with it. So each such word must fail the
// inventory's host-name validation. A new fixed route of that shape fails
// here until its word is reserved.
func TestFixedRouteWordsAreReservedHostNames(t *testing.T) {
	inv := inventory.Default()
	hostRoute := map[string]bool{}
	for _, h := range inv.Hosts {
		hostRoute[h.Name+"-on"], hostRoute[h.Name+"-off"] = true, true
	}

	words := map[string]string{} // claimed word -> the route claiming it
	for _, r := range testRoutes(inv) {
		if hostRoute[r.Name] {
			continue
		}
		for _, suffix := range []string{"-on", "-off"} {
			if w, ok := strings.CutSuffix(r.Name, suffix); ok {
				words[w] = r.Name
			}
		}
		if rest, ok := strings.CutPrefix(r.Path, "/power/"); ok {
			if w, _, ok := strings.Cut(rest, "/"); ok {
				words[w] = r.Method + " " + r.Path
			}
		}
	}
	if len(words) == 0 {
		t.Fatal("found no fixed <word>-on/-off or /power/<word>/ routes; the scan is broken")
	}
	for w, route := range words {
		if inventory.ValidateHostName(w) == nil {
			t.Errorf("%q is claimed by %s but is a valid host name; reserve it in inventory", w, route)
		}
	}
}
