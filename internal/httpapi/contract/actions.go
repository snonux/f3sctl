package contract

// ActionRenderer renders the Siren actions list a resource advertises,
// judged against the current state.
//
// It is the one place the Siren action shape (name, title, method, href,
// cliVerb, fields) is built: the composition root's Router implements it over
// the whole route table, and both domain surfaces (powerapi, gogiosapi) take
// one in their constructors, so a surface handler never renders an action of
// its own that could drift from the rest of the API's. Every method omits an
// action that is not possible right now rather than marking it disabled --
// see Entity.Actions.
type ActionRenderer interface {
	// ActionsFor renders only the named actions that are possible right now,
	// for a resource that advertises just its own controls.
	ActionsFor(state State, names ...string) []Action
	// SectionActions renders every action of one API section (Route.Section)
	// that is possible right now -- what a section folder offers.
	SectionActions(state State, section string) []Action
}
