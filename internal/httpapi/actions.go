package httpapi

import (
	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/httpapi/gogiosapi"
	"github.com/snonux/f3sctl/internal/httpapi/powerapi"
)

// powerSurfaceFunc and gogiosSurfaceFunc construct a domain surface bound to
// the given action renderer. Server.build takes these rather than finished
// surfaces so that it, not its caller, supplies the renderer -- see build.
type (
	powerSurfaceFunc  func(contract.ActionRenderer) *powerapi.Surface
	gogiosSurfaceFunc func(contract.ActionRenderer) *gogiosapi.Surface
)

// serverActions is the contract.ActionRenderer Server.build hands both domain
// surfaces at construction time, resolving the Server's Router lazily -- on
// every render, not when the surface is built.
//
// The laziness breaks a genuine cycle rather than papering over an ordering
// accident: the Router is built from the route table, and the route table is
// declared by the surfaces themselves (their handlers are closures over the
// Surface), so no Router can exist when a surface is constructed. Resolving
// through the Server means a surface is complete the moment its constructor
// returns -- there is no field to assign afterwards and forget -- and the one
// remaining ordering rule (the Router must exist before anything renders)
// fails loudly instead of rendering an empty actions list.
type serverActions struct{ s *Server }

// actionRenderer returns the contract.ActionRenderer that renders through the
// Router Server.build hangs off s. Only build hands it out.
func (s *Server) actionRenderer() contract.ActionRenderer { return serverActions{s: s} }

// router returns the Server's Router, panicking if Server.build has not run.
//
// A panic, not an empty list: rendering before the route table exists is a
// wiring bug in the composition root, never a runtime condition, and the
// silent alternative -- a resource that simply advertises no actions -- is
// exactly the failure this type exists to rule out.
func (a serverActions) router() *Router {
	if a.s.router == nil {
		panic("httpapi: action rendered before Server.build built the router")
	}
	return a.s.router
}

// Actions delegates to Router.Actions.
func (a serverActions) Actions(state contract.State) []contract.Action {
	return a.router().Actions(state)
}

// ActionsFor delegates to Router.ActionsFor.
func (a serverActions) ActionsFor(state contract.State, names ...string) []contract.Action {
	return a.router().ActionsFor(state, names...)
}

// SectionActions delegates to Router.SectionActions.
func (a serverActions) SectionActions(state contract.State, section string) []contract.Action {
	return a.router().SectionActions(state, section)
}
