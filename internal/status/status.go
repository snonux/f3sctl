// Package status holds the plain value types that describe what the rack
// looks like right now: one host's probe result, and the two Shelly plugs'
// states.
//
// They are the shared vocabulary of every surface that shows or transports a
// status -- the engine that produces them (internal/power), the API that
// serves them (internal/httpapi/powerapi), the table renderer
// (internal/presenter) and the hypermedia client that rebuilds them from a
// /status response (internal/client). They used to live in internal/power,
// which dragged the whole engine -- SSH, Wake-on-LAN, the Shelly RPC, the
// Gogios mute -- into the dependency graph of the client and the presenter,
// neither of which ever drives it. A leaf package with no imports is the
// dependency-inversion fix: producers and consumers both depend on the data,
// not on each other.
//
// Keep it that way: no behaviour here beyond what the values themselves
// mean, and no imports from this module. Deciding things from a status (the
// fan guards, the power-group selectors) stays in internal/power.
//
// The JSON tags use the same names as the host, fans and AC entity
// properties of the /status resource (docs/CLIENT.md). The API builds those
// entities field by field rather than marshalling these structs, but the
// tags are kept identical to what they were in internal/power and pinned by
// TestJSONShape, so anything that does marshal one sees no change.
package status

// HostStatus is one host's observed state.
//
// Two independent signals are reported rather than a single "up", because the
// difference between them is operationally meaningful:
//
//	Ping && SSH    the host is up and serving
//	Ping && !SSH   the host is booting (or sshd is wedged)
//	!Ping && !SSH  the host is off -- or hung in single-user after a failed
//	               shutdown, in which case it is powered on, has no network,
//	               and Wake-on-LAN will not wake it. Worth knowing before
//	               pressing "on" again and concluding the button is broken.
type HostStatus struct {
	Name string `json:"name"`
	Role string `json:"role"`
	IP   string `json:"ip"`
	Ping bool   `json:"ping"`
	// PingKnown says whether the ICMP probe reached a conclusion at all. False
	// means it never ran -- no ping(8), a binary that could not be started, a
	// context cut short -- which is NOT the same as a host that said nothing.
	//
	// It is a separate field rather than a third state of Ping because Ping is
	// what gets displayed, and a display wants two columns. Anything that
	// *decides* something must read both (power's livenessOf does): the zero
	// value of this struct is therefore "silent, and we do not know why",
	// which is the safe reading for a HostStatus that some other package built
	// by hand.
	PingKnown bool    `json:"pingKnown"`
	SSH       bool    `json:"ssh"`
	MS        float64 `json:"ms"`
}

// FansState is the rack-fan plug's reported state.
type FansState struct {
	On bool   `json:"on"`
	IP string `json:"ip"`
}

// ACState is the f-host mains AC plug's reported state (shelly2).
type ACState struct {
	On bool   `json:"on"`
	IP string `json:"ip"`
}
