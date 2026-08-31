package httpapi

import (
	"net/http"
	"slices"

	"github.com/snonux/f3sctl/internal/httpapi/contract"
)

// Router owns the HTTP-facing side of the route registry: matching a request
// to a route, telling a 404 from a 405, and turning a route's Path into the
// absolute href a client is handed back.
//
// It knows nothing about *executing* a route -- that stays route.Handle and
// Server.serve -- only how the declared route table maps onto URLs. The table
// is built once by Server (from the two domain surfaces plus the root's own
// resources, see registry.go) and injected here, so hrefs, links and the
// actions list can be built and tested without a Server -- or the engine,
// jobs and peers it drags in -- at all.
type Router struct {
	// base is the URL prefix every href is built from -- bozohttpd's
	// SCRIPT_NAME, e.g. "/cgi-bin/f3sctl". Hrefs are absolute paths so a
	// client never has to know how the API is mounted.
	base string
	// routes is the declared API surface this router serves. It is the same
	// slice Server.serve dispatches over, so the document of the API (Links,
	// Actions) and its behaviour can never disagree with each other.
	routes []contract.Route
}

// NewRouter returns a Router that builds hrefs under base and serves the
// given route table.
func NewRouter(base string, rs []contract.Route) *Router {
	return &Router{base: base, routes: rs}
}

// Href builds an absolute path for a route. The shape is contract.Href's, so
// the router and every surface handler that builds a link share one
// implementation.
func (rt *Router) Href(path string) string {
	return contract.Href(rt.base, path)
}

// Lookup finds the route serving method and path.
func (rt *Router) Lookup(method, path string) (contract.Route, bool) {
	for _, r := range rt.routes {
		if r.Method == method && r.Path == path {
			return r, true
		}
	}
	return contract.Route{}, false
}

// PathExists reports whether any route serves this path, regardless of
// method. It is what separates a 404 from a 405.
func (rt *Router) PathExists(path string) bool {
	for _, r := range rt.routes {
		if r.Path == path {
			return true
		}
	}
	return false
}

// Links renders every resource as a navigable link.
//
// A route may opt out via NoRootLink when its bare href (no query string) is
// not a meaningful thing to follow -- see that field's doc comment.
func (rt *Router) Links() []contract.Link {
	var out []contract.Link
	for _, r := range rt.routes {
		if r.Action || r.Method != http.MethodGet || r.NoRootLink {
			continue
		}
		out = append(out, contract.Link{Rel: []string{r.Name}, Href: rt.Href(r.Path), Title: r.Title})
	}
	return out
}

// Actions renders every action that is possible right now.
//
// Actions that are not possible are omitted entirely rather than marked
// disabled. That is the core of the contract: a client renders what it is
// given, and never needs to encode a rule about when something is allowed.
func (rt *Router) actions(state contract.State) []contract.Action {
	var out []contract.Action
	for _, r := range rt.routes {
		if !r.Action || !r.IsAvailable(state) {
			continue
		}
		out = append(out, rt.action(r, state))
	}
	return out
}

// ActionsFor is Actions narrowed to the named routes, for resources that
// should only advertise their own controls.
func (rt *Router) actionsFor(state contract.State, names ...string) []contract.Action {
	var out []contract.Action
	for _, r := range rt.routes {
		if !r.Action || !r.IsAvailable(state) || !slices.Contains(names, r.Name) {
			continue
		}
		out = append(out, rt.action(r, state))
	}
	return out
}

// SectionActions renders every action of one section that is possible right
// now. It is what a section folder offers: the same state-dependent judgement
// every other rendering makes (only possible actions are advertised, actions
// that are not are omitted entirely), narrowed to the folder's own domain by
// the Route.Section stamps -- so the power folder carries exactly the power
// operations, never the Gogios ones, with no separate list to keep in step.
// See contract.Route.Section and powerapi's /power folder.
func (rt *Router) SectionActions(state contract.State, section string) []contract.Action {
	var out []contract.Action
	for _, r := range rt.routes {
		if !r.Action || r.Section != section || !r.IsAvailable(state) {
			continue
		}
		out = append(out, rt.action(r, state))
	}
	return out
}

// actions and actionsFor are injected into both domain surfaces (see
// powerapi.Surface and gogiosapi.Surface), so resources rendered inside a
// surface handler advertise exactly the actions the route table says are
// possible right now.

func (rt *Router) action(r contract.Route, state contract.State) contract.Action {
	a := contract.Action{
		Name:    r.Name,
		Title:   r.Title,
		Method:  r.Method,
		Href:    rt.Href(r.Path),
		CLIVerb: r.CLIVerb,
		Fields:  r.FieldsFor(state),
	}
	if len(a.Fields) > 0 {
		a.Type = "application/x-www-form-urlencoded"
	}
	return a
}
