package httpapi

import (
	"context"
	"net/http"

	"github.com/snonux/f3sctl/internal"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/httpapi/gogiosapi"
	"github.com/snonux/f3sctl/internal/httpapi/powerapi"
	"github.com/snonux/f3sctl/internal/inventory"
)

// buildRoutes is the complete API surface, assembled out of its three owners:
//
//   - the composition root's own resources (the entry point and the OpenAPI
//     description -- the two URLs a client knows without any other route
//     existing first),
//   - the power surface (internal/httpapi/powerapi): status, job, fans and
//     every power operation,
//   - the Gogios surface (internal/httpapi/gogiosapi): the mute concern and
//     the alert-report browse.
//
// Every surface declares its own routes next to the handlers that serve them,
// in its own package; this is only the meeting point, so the order here is
// the order the surfaces are appended in. The order is wire-visible (link and
// action lists follow route order) but is not part of the stability contract
// (docs/CLIENT.md §11), so a new surface appended last changes nothing a
// well-written client relies on.
//
// The table is built once per Server, from inv, the inventory this server was
// configured with, NOT from inventory.Default(): power.New, ProbeAll and
// every other site resolve the inventory through injected config.Config, so
// the route table must too. Reading the compiled-in global here would build
// the action routes from a different inventory than the one the engine acts
// on -- an operator who overrode "inventory" in f3sctl.json would get new
// hosts probed and shown in /status but no action route for them, and removed
// hosts would keep a dead route -- and powerapi.JobArgsFrom derives the
// detached child's argv from these same routes, so the CLI<->API<->job
// contract would miss the host as well.
func (s *Server) buildRoutes(inv inventory.Inventory, pw *powerapi.Surface, gg *gogiosapi.Surface) []contract.Route {
	out := s.resourceRoutes()
	out = append(out, pw.Routes()...)
	out = append(out, gg.Routes()...)
	return out
}

// resourceRoutes is every GET route the composition root owns: navigable
// resources rendered in "links", never in "actions"
// (see TestGETRoutesAreLinksNotActions). None of these takes a CLIVerb --
// they are followed by relation, not invoked by a CLI verb.
//
// The handlers are bound method values so they satisfy contract.Handle; the
// router they render through is read at request time, so building this table
// before s.router exists is fine.
func (s *Server) resourceRoutes() []contract.Route {
	return []contract.Route{
		{
			Name: "self", Title: "API root",
			Method: http.MethodGet, Path: "/",
			// The composition root's two resources are stamped inline rather
			// than through a surface-package stamp: they are two literals, not
			// a section of their own package. SectionAPI puts them under the
			// OpenAPI document's API section, separate from the Power and
			// Gogios surfaces the other routes are stamped with in Routes().
			Section: contract.SectionAPI,
			// SkipsProbe because handleRoot renders no state-derived data: the
			// actions list (the only probe-dependent part of the root) moved
			// to the per-section folders, so pointing a browser's menu at the
			// root costs no probe at all. The signature keeps State to stay
			// Handle-typed.
			SkipsProbe: true,
			Handle:     s.handleRoot,
		},
		{
			Name: "describedby", Title: "OpenAPI description",
			Method: http.MethodGet, Path: openAPIPath,
			// handleOpenAPI ignores State completely: OpenAPIBuilder renders
			// every route's Fields against a synthetic widestState(), not
			// the request's own state (see openapi.go).
			Section:    contract.SectionAPI,
			SkipsProbe: true,
			Handle:     s.handleOpenAPI,
		},
	}
}

// handleRoot renders the entry point: the only URL a client is allowed to know.
func (s *Server) handleRoot(_ context.Context, _ contract.State, _ contract.Request) (contract.Entity, int, error) {
	return contract.Entity{
		Class: []string{"f3sctl"},
		Title: "f3s homelab control",
		Properties: map[string]any{
			// apiVersion changes only on a breaking change. A client that
			// does not recognise it must stop rather than guess. This API
			// has never had a breaking change: the reorganisation into
			// per-domain surface packages (internal/httpapi/powerapi and
			// internal/httpapi/gogiosapi) moved Go code around, no wire
			// shape, so the version stays at v1 throughout. A brief v2
			// bump for that same no-op restructure was reverted for the
			// same reason (sy0): old REST clients kept working against the
			// restructured server because nothing they read ever moved.
			"apiVersion": 1,
			"version":    internal.Version,
			"node":       s.node,
		},
		// The root is a foldER index, deliberately carrying no actions of
		// its own: every operation lives in its section folder instead --
		// power operations on the /power folder, the Gogios mute pair and
		// the report cache clear on the /gogios one -- so a browser pointing
		// at the overview sees two folders and the read-only resources, not
		// a dozen operations (see docs/CLIENT.md §3). Only possible actions
		// are ever advertised, and none of these are, because the root
		// renders none at all -- which is also why this route is
		// SkipsProbe: nothing here depends on fleet state, so fetching
		// the entry point stops paying the ~3s probe every menu render
		// used to cost (see rz0 for the flag's discipline).
		Links: s.router.Links(),
	}, http.StatusOK, nil
}
