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
			Handle: s.handleRoot,
		},
		{
			Name: "describedby", Title: "OpenAPI description",
			Method: http.MethodGet, Path: openAPIPath,
			// handleOpenAPI ignores State completely: OpenAPIBuilder renders
			// every route's Fields against a synthetic widestState(), not
			// the request's own state (see openapi.go).
			SkipsProbe: true,
			Handle:     s.handleOpenAPI,
		},
	}
}

// handleRoot renders the entry point: the only URL a client is allowed to know.
func (s *Server) handleRoot(_ context.Context, state contract.State, _ contract.Request) (contract.Entity, int, error) {
	return contract.Entity{
		Class: []string{"f3sctl"},
		Title: "f3s homelab control",
		Properties: map[string]any{
			// apiVersion changes only on a breaking change. A client that
			// does not recognise it must stop rather than guess. v2 was
			// raised for this API's restructure into per-domain surface
			// packages (internal/httpapi/powerapi and
			// internal/httpapi/gogiosapi): no response shape changed, but the
			// bump forces the client and server halves to upgrade together,
			// which is safer than letting a v1-era client run against a
			// restructured server with no signal that anything moved.
			"apiVersion": 2,
			"version":    internal.Version,
			"node":       s.node,
		},
		Links:   s.router.Links(),
		Actions: s.router.actions(state),
	}, http.StatusOK, nil
}
