package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/snonux/f3sctl/internal"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power"
)

// handleOpenAPI renders an OpenAPI 3.1 description generated from the same
// route registry that serves the requests.
//
// This is the second half of "self-describing", aimed at a different reader.
// Siren tells a running client what it may do *at this moment*; OpenAPI tells
// a code generator, a test, or a person what the surface is in general. Both
// come from the same route registry (registry.go's buildRoutes), so neither
// can describe an endpoint that does not exist or miss one that does. The
// per-operation detail -- success status (sync 200 or job 202), error
// statuses, query parameters -- is generated from the same declarations
// (contract.Route's Response, Errors and Query), and openapi_test.go drives
// real handlers to pin that what they answer is what is documented.
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
	// inv is the inventory the routes are served with; the widest state
	// fields are judged against is built from its hosts (see widestState).
	inv inventory.Inventory
}

// NewOpenAPIBuilder returns a builder that resolves hrefs through router and
// judges state-dependent fields against inv, the served inventory.
func NewOpenAPIBuilder(router *Router, inv inventory.Inventory) *OpenAPIBuilder {
	return &OpenAPIBuilder{router: router, inv: inv}
}

// Build renders the complete OpenAPI document.
func (b *OpenAPIBuilder) Build() map[string]any {
	paths := map[string]any{}
	widest := widestState(b.inv)

	for _, r := range b.router.routes {
		if r.Path == openAPIPath {
			continue
		}

		key := b.router.Href(r.Path)
		entry, _ := paths[key].(map[string]any)
		if entry == nil {
			entry = map[string]any{}
		}
		entry[strings.ToLower(r.Method)] = operationFor(r, widest)
		paths[key] = entry
	}

	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   "f3sctl",
			"version": internal.Version,
			"description": "Power, status and Shelly-plug control for the f3s homelab. " +
				"Operations are grouped into sections: Power (host wake/shutdown, " +
				"status, jobs), AC (rack-fan and f-host mains Shelly plugs), and " +
				"Gogios (alerting -- the mute pair and the alert-report browse), " +
				"with API covering the entry point itself. Hypermedia (Siren): " +
				"fetch the root and follow what it offers rather than hard-coding " +
				"these paths. Every error is a Siren entity of class \"error\". " +
				"Every request, whatever its path, is authenticated first (401); " +
				"after that, a path not listed here (other than this document, " +
				"/openapi.json) answers 404, and a method not listed for a path " +
				"answers 405. An action's form fields may also be sent as query " +
				"parameters of the same name.",
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

// sections is the tag vocabulary: power, AC (Shelly plugs), and gogios as
// separate sections, plus the entry-point resources. Each description says
// what belongs there; powerapi declares both Power and AC, gogiosapi declares
// Gogios.
var sections = []section{
	{
		Name: contract.SectionAPI,
		Description: "The entry point and its OpenAPI description " +
			"-- the two URLs a client knows without following a link first.",
	},
	{
		Name: contract.SectionPower,
		Description: "Host power control: the status and job resources, and the " +
			"power pairs for the power group (the cluster), for every f-host, " +
			"and for each f-host on its own.",
	},
	{
		Name: contract.SectionAC,
		Description: "Shelly plug control: the rack-fan plug (shelly1) and the " +
			"f-host mains AC plug (shelly2), with their on/off actions.",
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
//
// Everything a reader needs to call the route correctly -- its parameters,
// its success status, the errors it can answer with -- is generated from the
// route's own declaration (Response, Query, Errors, Action, Method), never
// written out per path here. That is what keeps this document from drifting
// into describing, say, a synchronous plug switch as a 202 job.
func operationFor(r contract.Route, widest contract.State) map[string]any {
	op := map[string]any{
		"operationId": r.Name,
		"summary":     r.Title,
		// r.Section names which section this operation groups under; a route
		// without one renders untagged (Swagger UI's "default" group) rather
		// than guessing -- and TestOpenAPICoversEveryRoute fails on it. See
		// contract.Route.Section.
		"tags":      []any{r.Section},
		"responses": responsesFor(r),
	}
	if params := parametersFor(r); len(params) > 0 {
		op["parameters"] = params
	}

	if r.Action {
		op["description"] = availabilityNote(r) + completionNote(r.Response)

		if fields := describeFields(r, widest); len(fields) > 0 {
			op["requestBody"] = map[string]any{
				"required": false,
				// contract.Request.BoolField reads a field from the form body
				// or the query string alike; say so rather than hide it.
				"description": "Each field may also be sent as a query parameter of the same name.",
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

// availabilityNote says, in prose, when an action is offered.
//
// Availability is state-dependent and therefore cannot be expressed as data
// here; it is described so a reader of the static document is not misled
// into thinking every action is always callable -- or, for an action with no
// Available predicate (the cache clear), into expecting a 409 it never gets.
func availabilityNote(r contract.Route) string {
	if r.Available == nil {
		return "Always advertised in the parent entity's actions. "
	}
	return "Advertised in the parent entity's actions only when currently available. " +
		"A 409 means it was attempted when it was not. "
}

// completionNote says, in prose, how an action of kind k completes -- the
// half of contract.ResponseKind a status code alone does not convey.
func completionNote(k contract.ResponseKind) string {
	if k == contract.ResponseJob {
		return "Starts a detached power job and answers 202 at once; " +
			"poll the job resource until its state leaves \"running\"."
	}
	return "Performed before the response is written; answers 200 with the updated resource."
}

// responsesFor renders every status a route can answer with: its one success
// status (from r.Response), the errors the pipeline adds around every route
// or every action (pipelineErrors), and the ones its own handler declares
// (r.Errors). A status declared by both keeps both reasons, joined, since
// either can produce it.
func responsesFor(r contract.Route) map[string]any {
	reasons := map[int]string{}
	for _, e := range append(pipelineErrors(r), r.Errors...) {
		if prev, ok := reasons[e.Status]; ok {
			reasons[e.Status] = prev + "; or " + e.Description
			continue
		}
		reasons[e.Status] = e.Description
	}
	reasons[r.Response.Status()] = successDescription(r)

	out := make(map[string]any, len(reasons))
	for status, desc := range reasons {
		out[strconv.Itoa(status)] = map[string]any{"description": desc}
	}
	return out
}

// successDescription describes a route's success response body.
func successDescription(r contract.Route) string {
	switch {
	case r.Response == contract.ResponseJob:
		return "accepted: the started job, as a Siren entity; poll the job resource"
	case r.Action:
		return "done: the updated resource, as a Siren entity"
	default:
		return "Siren entity"
	}
}

// pipelineErrors is every error status that can answer a request for r
// without r's own handler deciding it: ServeCGI's malformed-body (400, POST
// only) and server-construction (500) failures, serve()'s auth check (401),
// its availability backstop for actions with an Available predicate (409),
// and, for a job route, the job manager's or the peer's refusal (409) or a
// failure to start the job (500). See conflictReasons. A route's Errors add to
// these, or add a reason to one of them.
//
// 404 and 405 are not here: they answer paths and methods no route declares,
// so there is no operation to hang them on; the info description says so.
func pipelineErrors(r contract.Route) []contract.ErrorResponse {
	job := r.Response == contract.ResponseJob
	fault := "server fault: this node is misconfigured"
	if job {
		fault += ", or the job could not be started"
	}
	out := []contract.ErrorResponse{
		{Status: http.StatusUnauthorized, Description: "missing or bad X-API-Key, or this node's key file cannot be read"},
		{Status: http.StatusInternalServerError, Description: fault},
	}
	if r.Method == http.MethodPost {
		out = append(out, contract.ErrorResponse{Status: http.StatusBadRequest, Description: "the request body could not be read"})
	}
	if c := conflictReasons(r); len(c) > 0 {
		out = append(out, contract.ErrorResponse{Status: http.StatusConflict, Description: strings.Join(c, "; or ")})
	}
	return out
}

// conflictReasons lists why the pipeline can answer r with 409: serve()'s
// availability backstop, which only an action with an Available predicate
// can trip, and, for a job route, the job manager or the peer node already
// running a job.
func conflictReasons(r contract.Route) []string {
	var out []string
	if r.Action && r.Available != nil {
		out = append(out, "not available now: re-fetch the parent resource and read its actions")
	}
	if r.Response == contract.ResponseJob {
		out = append(out, "a power job is already running on either API node")
	}
	return out
}

// parametersFor renders a route's declared query parameters (r.Query) as
// OpenAPI Parameter Objects. Every parameter is a plain string: the query
// string carries nothing else.
func parametersFor(r contract.Route) []any {
	out := make([]any, 0, len(r.Query))
	for _, q := range r.Query {
		out = append(out, map[string]any{
			"name":        q.Name,
			"in":          "query",
			"required":    q.Required,
			"description": q.Description,
			"schema":      map[string]any{"type": "string"},
		})
	}
	return out
}

// describeFields renders a route's parameters for the static document.
//
// Fields are state-dependent, so this evaluates them against the widest state:
// one in which every conditional field appears. A static description should
// list everything an action can ever accept, and say (as the operation
// description does) that availability is decided at runtime.
func describeFields(r contract.Route, widest contract.State) map[string]any {
	if r.Fields == nil {
		return nil
	}

	out := map[string]any{}
	for _, f := range r.FieldsFor(widest) {
		typ := "string"
		if f.Type == "checkbox" {
			typ = "boolean"
		}
		out[f.Name] = map[string]any{"type": typ, "description": f.Title}
	}
	return out
}

// widestState is a synthetic state chosen to make every conditional field
// appear: every f-host of inv known to be up (which is what adds the fans-off
// and ac-off confirmations) and no job running.
//
// The hosts come from the served inventory, not a fixed name: the plug guards
// judge a snapshot by the inventory's groups (power.RackActivityFrom), so a
// host the inventory does not have would read as a cold rack and drop the
// `force` field from the document. Every f-host covers both guards -- the fan
// guard's power group and the AC guard's full set.
func widestState(inv inventory.Inventory) contract.State {
	var s contract.State
	for _, h := range inv.EveryFHost() {
		s.Hosts = append(s.Hosts, power.HostStatus{
			Name: h.Name, Role: string(h.Role), Ping: true, PingKnown: true, SSH: true,
		})
	}
	return s
}
