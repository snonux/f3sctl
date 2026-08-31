package contract

import (
	"context"
)

// Section is the part of the API surface a route belongs to: the value a
// Route.Section carries. The names are the OpenAPI tag names too, so a
// generated reader shows them verbatim as its section headings.
const (
	// SectionAPI is the entry point and its OpenAPI description -- the two
	// URLs a client knows without following any other link first. Routes of
	// the composition root's resourceRoutes declare it inline.
	SectionAPI = "API"
	// SectionPower is everything the rack concerns: powerapi.
	SectionPower = "Power"
	// SectionGogios is alerting: gogiosapi.
	SectionGogios = "Gogios"
)

// Handle performs one route: it turns a request and the request's State
// snapshot into the entity to render, the HTTP status to report it under, and
// an error for the non-2xx cases.
//
// It is a plain function type rather than a method on the composition root's
// Server on purpose: the route table is declared by the per-domain surface
// packages (gogiosapi, powerapi), which cannot name the root's Server without
// an import cycle. A surface binds its own collaborators (engine slice, job
// manager, peer set) into Handle closures when it declares its routes.
//
// ctx bounds the request itself and every synchronously-performed backend
// call a handler makes; it must not be threaded into anything that is meant
// to outlive the request (the detached job child -- see internal/jobrun).
type Handle func(ctx context.Context, state State, req Request) (Entity, int, error)

// Route is one entry in the API surface.
//
// Siren rendering (the composition root's Router), the OpenAPI document and
// dispatch are all generated from these declarations. That is deliberate:
// hand-maintained parallel lists of the same endpoints would drift, and a
// self-describing API that lies is worse than one that never claimed to
// describe itself.
type Route struct {
	// Name is the stable identifier clients match on.
	Name string
	// Title is human-readable text for the action or link.
	Title string
	// Method is the HTTP method.
	Method string
	// Path is the exact PATH_INFO this route answers on.
	Path string
	// Action marks a state change (rendered in "actions"). Routes without it
	// are resources, rendered in "links".
	Action bool
	// Section names which part of the API surface this route belongs to: one
	// of the contract.Section* constants below. It is the single declaration
	// behind the OpenAPI document's tag grouping (openapi.go's sections), so
	// a generated reader shows power operations and Gogios operations as
	// separate sections instead of one flat list. It is purely additive on
	// the wire -- Siren entities never carry it -- which is why it needed no
	// apiVersion bump (see §11 in docs/CLIENT.md: the version moves only on a
	// breaking change).
	//
	// A whole surface package stamps its section once in Routes() rather than
	// on every literal: the package split (powerapi, gogiosapi) IS the
	// section split, so one stamp per Routes cannot forget a route, while a
	// per-literal field can. The composition root's own resources declare
	// Section inline -- they are two literals, not a surface package.
	// TestEveryRouteDeclaresAKnownSection keeps both halves honest: every
	// route must carry a section from the tag vocabulary, and the vocabulary
	// must not name a section no route uses.
	Section string
	// NoRootLink excludes a GET resource from the root entity's own link
	// list. Every other GET route is safe to follow exactly as its bare href
	// appears there (docs/CLIENT.md tells clients they may always do that) --
	// but gogios-check's Path carries no room for the check name (it is a
	// mandatory query parameter, ?name=...; see the Gogios surface's route
	// declarations for why it cannot be a path segment), so its bare href
	// resolves to nothing and following it as given 404s every time. Zero
	// value is false: a new GET route is linked from root by default, and
	// must opt out explicitly, the same "opt-in, not silently inherited"
	// discipline SkipsProbe uses.
	NoRootLink bool
	// SkipsProbe declares that this route's own Handle, and every
	// Available/Fields predicate the router evaluates while rendering it,
	// provably never read State.Hosts or State.Fans -- so the composition
	// root's snapshot can skip the ~3s fleet probe and the Shelly plug read
	// entirely when serving it. Zero value is false: a new route needs the
	// probe by default and must opt out explicitly and correctly, rather than
	// silently inherit an exemption because its path happened to match a
	// hardcoded prefix meant for someone else's routes. That is what went
	// wrong before this field existed -- see rz0.
	SkipsProbe bool
	// CLIVerb is the exact CLI words that invoke this action, e.g. "power on"
	// or "power f1 on". It is the single declaration of the "power off" <->
	// action-name <-> job-run-argv contract: the detached child's argv is
	// split from it (see powerapi's JobArgsFrom), and the Siren action
	// carries it over the wire so the remote client (internal/client/run.go)
	// can match a typed command to an action by reading what the server just
	// advertised, rather than the two of them independently guessing a name
	// from the same string. Left empty on GET/link routes, which are not
	// invoked by a CLI verb at all. See sy0's annotation for the drift this
	// replaced -- three hand-written maps of the same strings that could
	// silently disagree the moment a new action was added to only one of them.
	CLIVerb string
	// JobActionName is the identifier the detached child's argv derivation and
	// coordination.Manager's Job.Action match this route's job against, when
	// it differs from Name. It differs only for power-on/power-off: their job
	// identifier ("on"/"off") predates the route registry and is part of the
	// documented job wire contract (docs/CLIENT.md's `"action": "off"`), so it
	// is kept rather than renamed to match Name ("power-on"/"power-off").
	// Empty means "use Name" -- true for every other action, where the two
	// have always been the same string.
	JobActionName string
	// Available reports whether this route may be used given the current
	// state. Resources are always available; actions are advertised only when
	// they would succeed.
	Available func(State) bool
	// Fields describes the action's parameters for the current state.
	Fields func(State) []Field
	// Handle performs the route. ctx is the request's context, taken from
	// serve() and carried through to every Shelly/SSH/Gogios call a handler
	// makes, so a slow backend is bounded by the request's own deadline
	// rather than by a fresh context.Background() with no deadline at all.
	// It is not threaded into the detached job child spawn -- that job
	// deliberately outlives the CGI request that started it, so it must not
	// be cancelled when this context is.
	Handle Handle
}

// JobAction returns the identifier the detached child's argv derivation and
// the started job's Action field match this route against: JobActionName when
// the route declares one (power-on/power-off only -- see its doc comment),
// Name otherwise.
func (r Route) JobAction() string {
	if r.JobActionName != "" {
		return r.JobActionName
	}
	return r.Name
}

// IsAvailable reports whether a route may be used now.
func (r Route) IsAvailable(s State) bool {
	if r.Available == nil {
		return true
	}
	return r.Available(s)
}

// FieldsFor returns the route's parameters for the current state.
func (r Route) FieldsFor(s State) []Field {
	if r.Fields == nil {
		return nil
	}
	return r.Fields(s)
}

// Href builds the absolute href for a path under a CGI mount base.
//
// base is the URL prefix -- bozohttpd's SCRIPT_NAME, e.g. "/cgi-bin/f3sctl".
// Hrefs are absolute paths so a client never has to know how the API is
// mounted. Path "/" maps to base + "/" (the mount root) rather than base
// itself, so the entry point's href always carries the trailing slash a
// client resolves the rest against.
//
// One implementation for everyone: the composition root's Router, and every
// surface handler that builds a link, go through this, so they cannot drift
// into handing out differently-shaped hrefs for the same route.
func Href(base, path string) string {
	if path == "/" {
		return base + "/"
	}
	return base + path
}

// Hrefs returns a path-to-href builder bound to base, the shape surface
// packages carry instead of a Router dependency.
func Hrefs(base string) func(string) string {
	return func(path string) string { return Href(base, path) }
}
