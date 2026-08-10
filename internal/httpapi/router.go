package httpapi

import (
	"net/http"
	"slices"
)

// Router owns the HTTP-facing side of the route registry: matching a request
// to a route, telling a 404 from a 405, and turning a route's Path into the
// absolute href a client is handed back.
//
// It knows nothing about *executing* a route -- that stays route.Handle and
// Server.serve -- only how the declarations in routes() map onto URLs. Split
// out of Server so hrefs, links and the actions list can be built and tested
// without a Server (or the engine, jobs and peers it drags in) at all.
type Router struct {
	// base is the URL prefix every href is built from -- bozohttpd's
	// SCRIPT_NAME, e.g. "/cgi-bin/f3sctl". Hrefs are absolute paths so a
	// client never has to know how the API is mounted.
	base string
}

// NewRouter returns a Router that builds hrefs under base.
func NewRouter(base string) *Router {
	return &Router{base: base}
}

// Href builds an absolute path for a route.
func (rt *Router) Href(path string) string {
	if path == "/" {
		return rt.base + "/"
	}
	return rt.base + path
}

// Lookup finds the route serving method and path.
func (rt *Router) Lookup(method, path string) (route, bool) {
	for _, r := range routes() {
		if r.Method == method && r.Path == path {
			return r, true
		}
	}
	return route{}, false
}

// PathExists reports whether any route serves this path, regardless of
// method. It is what separates a 404 from a 405.
func (rt *Router) PathExists(path string) bool {
	for _, r := range routes() {
		if r.Path == path {
			return true
		}
	}
	return false
}

// Links renders every resource as a navigable link.
func (rt *Router) Links() []Link {
	var out []Link
	for _, r := range routes() {
		if r.Action || r.Method != http.MethodGet {
			continue
		}
		out = append(out, Link{Rel: []string{r.Name}, Href: rt.Href(r.Path), Title: r.Title})
	}
	return out
}

// Actions renders every action that is possible right now.
//
// Actions that are not possible are omitted entirely rather than marked
// disabled. That is the core of the contract: a client renders what it is
// given, and never needs to encode a rule about when something is allowed.
func (rt *Router) Actions(state State) []Action {
	var out []Action
	for _, r := range routes() {
		if !r.Action || !r.available(state) {
			continue
		}
		out = append(out, rt.action(r, state))
	}
	return out
}

// ActionsFor is Actions narrowed to the named routes, for resources that
// should only advertise their own controls.
func (rt *Router) ActionsFor(state State, names ...string) []Action {
	var out []Action
	for _, r := range routes() {
		if !r.Action || !r.available(state) || !slices.Contains(names, r.Name) {
			continue
		}
		out = append(out, rt.action(r, state))
	}
	return out
}

func (rt *Router) action(r route, state State) Action {
	a := Action{
		Name:   r.Name,
		Title:  r.Title,
		Method: r.Method,
		Href:   rt.Href(r.Path),
		Fields: r.fields(state),
	}
	if len(a.Fields) > 0 {
		a.Type = "application/x-www-form-urlencoded"
	}
	return a
}
