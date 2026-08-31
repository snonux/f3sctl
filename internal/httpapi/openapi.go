package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/snonux/f3sctl/internal"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/power"
)

// handleOpenAPI renders an OpenAPI 3.1 description generated from the same
// route registry that serves the requests.
//
// This is the second half of "self-describing", aimed at a different reader.
// Siren tells a running client what it may do *at this moment*; OpenAPI tells
// a code generator, a test, or a person what the surface is in general. Both
// come from the same route registry (registry.go's buildRoutes), so neither
// can describe an endpoint that does not exist or miss one that does.
// It is the one route not wrapped in a Siren envelope -- an OpenAPI document
// has its own well-known shape, and burying it inside "properties" would make
// it useless to every tool that reads OpenAPI. serve() recognises it by path
// and writes Properties out as the whole body.
func (s *Server) handleOpenAPI(_ context.Context, _ contract.State, _ contract.Request) (contract.Entity, int, error) {
	return contract.Entity{Properties: s.openapi.Build()}, http.StatusOK, nil
}

// openAPIPath is the route whose body is emitted raw rather than as Siren.
const openAPIPath = "/openapi.json"

// OpenAPIBuilder generates the OpenAPI 3.1 document for the API surface.
//
// It is generated from the same route declarations (the Router's table,
// built by buildRoutes) that drive Router's Siren rendering, so the static
// document
// and what a client is actually offered at runtime can never describe two
// different APIs. It depends only on a Router for route lookup and href
// resolution, not on Server, so the whole document can be built and asserted
// on in a test without an engine, jobs or peers.
type OpenAPIBuilder struct {
	router *Router
}

// NewOpenAPIBuilder returns a builder that resolves hrefs through router.
func NewOpenAPIBuilder(router *Router) *OpenAPIBuilder {
	return &OpenAPIBuilder{router: router}
}

// Build renders the complete OpenAPI document.
func (b *OpenAPIBuilder) Build() map[string]any {
	paths := map[string]any{}

	for _, r := range b.router.routes {
		if r.Path == openAPIPath {
			continue
		}

		key := b.router.Href(r.Path)
		entry, _ := paths[key].(map[string]any)
		if entry == nil {
			entry = map[string]any{}
		}
		entry[strings.ToLower(r.Method)] = operationFor(r)
		paths[key] = entry
	}

	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   "f3sctl",
			"version": internal.Version,
			"description": "Power, status and rack-fan control for the f3s homelab. " +
				"Operations are grouped into two sections: Power (the rack -- status, " +
				"jobs, power operations, the fan plug) and Gogios (alerting -- the mute " +
				"pair and the alert-report browse), with API covering the entry point " +
				"itself. Hypermedia (Siren): fetch the root and follow what it offers " +
				"rather than hard-coding these paths.",
		},
		// The sections: one tag object per contract.Route.Section a route
		// declares, in the fixed order of the sections table below. This is
		// what makes a Swagger UI or generated reader show power operations
		// and Gogios operations as separate groups rather than one flat list
		// -- the machine-readable half of the surface split that
		// powerapi/gogiosapi express in Go. TestOpenAPICoversEveryRoute pins
		// that every operation carries exactly its route's section tag.
		"tags": tagList(),
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"apiKey": map[string]any{"type": "apiKey", "in": "header", "name": "X-API-Key"},
			},
		},
		"security": []any{map[string]any{"apiKey": []any{}}},
		"paths":    paths,
	}
}

// section is one entry of the OpenAPI tag vocabulary: a section of the API
// surface rendered as its own group in every tag-aware reader.
//
// The order here is the order the sections appear in, and follows the route
// table's own order (registry.go): the entry point first, then the two domain
// surfaces. A section with no route would advertise a group that never
// appears under any path -- TestEveryRouteDeclaresAKnownSection fails on
// that (and on the converse, a route naming a section absent from here).
type section struct {
	Name        string
	Description string
}

// sections is the tag vocabulary: power and gogios as separate sections,
// plus the entry-point resources. Each description says what belongs there,
// the same split the surface packages (powerapi, gogiosapi) draw in Go.
var sections = []section{
	{
		Name: contract.SectionAPI,
		Description: "The entry point and its OpenAPI description " +
			"-- the two URLs a client knows without following a link first.",
	},
	{
		Name: contract.SectionPower,
		Description: "Rack control: the status and job resources, the rack-fan plug, " +
			"the cluster-wide (f0/f1/f2), all-hosts (f0-f3) and per-host power pairs, " +
			"and the fans on/off actions.",
	},
	{
		Name: contract.SectionGogios,
		Description: "Gogios alerting: the monitoring mute/unmute pair and the " +
			"read-only alert-report browse (overview, per-status drill-downs, " +
			"per-check detail, cache clear).",
	},
}

// tagList renders the sections as the OpenAPI document's top-level tags
// array.
func tagList() []any {
	out := make([]any, 0, len(sections))
	for _, s := range sections {
		out = append(out, map[string]any{"name": s.Name, "description": s.Description})
	}
	return out
}

// operationFor renders one route's OpenAPI Operation Object.
func operationFor(r contract.Route) map[string]any {
	op := map[string]any{
		"operationId": r.Name,
		"summary":     r.Title,
		// r.Section names which section this operation groups under; a route
		// without one renders untagged (Swagger UI's "default" group) rather
		// than guessing -- and TestOpenAPICoversEveryRoute fails on it. See
		// contract.Route.Section.
		"tags": []any{r.Section},
		"responses": map[string]any{
			"200": map[string]any{"description": "Siren entity"},
			"401": map[string]any{"description": "missing or bad X-API-Key"},
		},
	}

	if r.Action {
		// Availability is state-dependent and therefore cannot be expressed
		// here; it is described in prose so a reader of the static document
		// is not misled into thinking every action is always callable.
		op["description"] = "Advertised in the parent entity's actions only when currently available. " +
			"A 409 means it was attempted when it was not."
		op["responses"].(map[string]any)["202"] = map[string]any{"description": "accepted; poll the job resource"}
		op["responses"].(map[string]any)["409"] = map[string]any{"description": "not available now, or another job is running"}

		if fields := describeFields(r); len(fields) > 0 {
			op["requestBody"] = map[string]any{
				"required": false,
				"content": map[string]any{
					"application/x-www-form-urlencoded": map[string]any{
						"schema": map[string]any{"type": "object", "properties": fields},
					},
				},
			}
		}
	}

	return op
}

// describeFields renders a route's parameters for the static document.
//
// Fields are state-dependent, so this evaluates them against the widest state:
// one in which every conditional field appears. A static description should
// list everything an action can ever accept, and say (as the operation
// description does) that availability is decided at runtime.
func describeFields(r contract.Route) map[string]any {
	if r.Fields == nil {
		return nil
	}

	out := map[string]any{}
	for _, f := range r.FieldsFor(widestState()) {
		typ := "string"
		if f.Type == "checkbox" {
			typ = "boolean"
		}
		out[f.Name] = map[string]any{"type": typ, "description": f.Title}
	}
	return out
}

// widestState is a synthetic state chosen to make every conditional field
// appear: an f-host known to be up (which is what adds the fans-off
// confirmation) and no job running.
func widestState() contract.State {
	return contract.State{
		Hosts: []power.HostStatus{
			{Name: "f0", Role: "f", Ping: true, PingKnown: true, SSH: true},
		},
	}
}
