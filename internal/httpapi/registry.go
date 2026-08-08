package httpapi

import (
	"net/http"

	"github.com/snonux/f3sctl/internal/power"
)

// State is a snapshot of the world, taken once per request before anything is
// rendered.
//
// Availability predicates read only from here, never from live probes of their
// own, so every action in a single response is judged against the same instant.
type State struct {
	Hosts   []power.HostStatus
	Fans    power.FansState
	FansErr error
	Job     *Job
}

// jobRunning reports whether a power operation is in flight. While one is,
// every power action is withheld: they are not queued, and offering a button
// that can only fail is exactly what the self-describing design exists to
// avoid.
func (s State) jobRunning() bool { return s.Job != nil && s.Job.State == JobRunning }

// host returns the named host's status.
func (s State) host(name string) (power.HostStatus, bool) {
	for _, h := range s.Hosts {
		if h.Name == name {
			return h, true
		}
	}
	return power.HostStatus{}, false
}

// clusterHostsUp reports how many of f0/f1/f2 answer ICMP.
func (s State) clusterHostsUp() (up, total int) {
	for _, h := range s.Hosts {
		if h.Role != "f" || h.Name == "f3" {
			continue
		}
		total++
		if h.Ping {
			up++
		}
	}
	return up, total
}

// anyFHostUp reports whether anything in the rack is drawing power. This is
// what gates switching the fans off.
func (s State) anyFHostUp() bool {
	for _, h := range s.Hosts {
		if h.Role == "f" && h.Ping {
			return true
		}
	}
	return false
}

// route is one entry in the API surface.
//
// Router, Siren rendering and the OpenAPI document are all generated from this
// one declaration. That is deliberate: three hand-maintained lists of the same
// endpoints would drift, and a self-describing API that lies is worse than one
// that never claimed to describe itself.
type route struct {
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
	// Available reports whether this route may be used given the current
	// state. Resources are always available; actions are advertised only when
	// they would succeed.
	Available func(State) bool
	// Fields describes the action's parameters for the current state.
	Fields func(State) []Field
	// Handle performs the route.
	Handle func(*Server, State, request) (Entity, int, error)
}

// routes is the complete API surface.
func routes() []route {
	return []route{
		{
			Name: "self", Title: "API root",
			Method: http.MethodGet, Path: "/",
			Handle: (*Server).handleRoot,
		},
		{
			Name: "status", Title: "Host and rack status",
			Method: http.MethodGet, Path: "/status",
			Handle: (*Server).handleStatus,
		},
		{
			Name: "fans", Title: "Rack fan plug",
			Method: http.MethodGet, Path: "/fans",
			Handle: (*Server).handleFans,
		},
		{
			Name: "job", Title: "Current or last power job",
			Method: http.MethodGet, Path: "/job",
			Handle: (*Server).handleJob,
		},
		{
			Name: "describedby", Title: "OpenAPI description",
			Method: http.MethodGet, Path: "/openapi.json",
			Handle: (*Server).handleOpenAPI,
		},

		{
			Name: "power-on", Title: "Power on f0/f1/f2",
			Method: http.MethodPost, Path: "/power/on", Action: true,
			// Offered only when something is actually off. When the whole
			// group already answers, waking it again is a no-op that would
			// still cost the caller a job slot.
			Available: func(s State) bool {
				up, total := s.clusterHostsUp()
				return !s.jobRunning() && up < total
			},
			Handle: handleAction("on"),
		},
		{
			Name: "power-off", Title: "Power off f0/f1/f2",
			Method: http.MethodPost, Path: "/power/off", Action: true,
			Available: func(s State) bool {
				up, _ := s.clusterHostsUp()
				return !s.jobRunning() && up > 0
			},
			Handle: handleAction("off"),
		},
		{
			Name: "f3-on", Title: "Power on f3",
			Method: http.MethodPost, Path: "/power/f3/on", Action: true,
			Available: func(s State) bool {
				h, ok := s.host("f3")
				return ok && !s.jobRunning() && !h.Ping
			},
			Handle: handleAction("f3-on"),
		},
		{
			Name: "f3-off", Title: "Power off f3",
			Method: http.MethodPost, Path: "/power/f3/off", Action: true,
			Available: func(s State) bool {
				h, ok := s.host("f3")
				return ok && !s.jobRunning() && h.Ping
			},
			Handle: handleAction("f3-off"),
		},

		{
			Name: "fans-on", Title: "Switch the rack fans on",
			Method: http.MethodPost, Path: "/fans/on", Action: true,
			// Unavailable when the plug cannot be read: without a read-back
			// there is no way to report truthfully whether it worked.
			Available: func(s State) bool { return s.FansErr == nil && !s.Fans.On },
			Handle:    (*Server).handleFansOn,
		},
		{
			Name: "fans-off", Title: "Switch the rack fans off",
			Method: http.MethodPost, Path: "/fans/off", Action: true,
			Available: func(s State) bool { return s.FansErr == nil && s.Fans.On },
			// The guard is expressed as a field rather than documented as a
			// rule: while a host is up the client is handed a confirmation
			// toggle with the reason in its title, and when the rack is cold
			// the field simply is not there.
			Fields: func(s State) []Field {
				if !s.anyFHostUp() {
					return nil
				}
				return []Field{{
					Name:     "force",
					Type:     "checkbox",
					Value:    false,
					Required: true,
					Title: "Hosts are still running: the rack fans keep them cool, " +
						"so switching the plug off now risks overheating. Confirm to proceed.",
				}}
			},
			Handle: (*Server).handleFansOff,
		},
	}
}

// lookup finds the route serving method and path.
func lookup(method, path string) (route, bool) {
	for _, r := range routes() {
		if r.Method == method && r.Path == path {
			return r, true
		}
	}
	return route{}, false
}

// pathExists reports whether any route serves this path, regardless of method.
// It is what separates a 404 from a 405.
func pathExists(path string) bool {
	for _, r := range routes() {
		if r.Path == path {
			return true
		}
	}
	return false
}

// available reports whether a route may be used now.
func (r route) available(s State) bool {
	if r.Available == nil {
		return true
	}
	return r.Available(s)
}

// fields returns the route's parameters for the current state.
func (r route) fields(s State) []Field {
	if r.Fields == nil {
		return nil
	}
	return r.Fields(s)
}
