package httpapi

import "github.com/snonux/f3sctl/internal/httpapi/contract"

// serverActions is the contract.ActionRenderer a Server hands both domain
// surfaces at construction time, resolving the Server's Router lazily -- on
// every render, not when the surface is built.
//
// The laziness breaks a genuine cycle rather than papering over an ordering
// accident: the Router is built from the route table, and the route table is
// declared by the surfaces themselves (their handlers are closures over the
// Surface), so no Router can exist when a surface is constructed. Resolving
// through the Server means a surface is complete the moment its constructor
// returns -- there is no field to assign afterwards and forget -- and the one
// remaining ordering rule (Server.build must run before anything is served)
// fails loudly instead of rendering an empty actions list.
type serverActions struct{ s *Server }

// actionRenderer returns the contract.ActionRenderer this Server's surfaces
// must be constructed with, so actions they render come from the Router
// Server.build later hangs off s.
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
