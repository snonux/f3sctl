package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/snonux/f3sctl/internal"
	"github.com/snonux/f3sctl/internal/power"
)

// handleRoot renders the entry point: the only URL a client is allowed to know.
func (s *Server) handleRoot(state State, _ request) (Entity, int, error) {
	return Entity{
		Class: []string{"f3sctl"},
		Title: "f3s homelab control",
		Properties: map[string]any{
			// apiVersion changes only on a breaking change. A client that
			// does not recognise it must stop rather than guess.
			"apiVersion": 1,
			"version":    internal.Version,
			"node":       s.node,
		},
		Links:   s.links(),
		Actions: s.actions(state),
	}, http.StatusOK, nil
}

// handleStatus renders every host plus the fan state in one response, so a
// watchface needs a single request per refresh.
func (s *Server) handleStatus(state State, _ request) (Entity, int, error) {
	e := Entity{
		Class:      []string{"status"},
		Title:      "Host and rack status",
		Properties: map[string]any{"node": s.node},
		Links: []Link{
			{Rel: []string{"self"}, Href: s.href("/status")},
			{Rel: []string{"up"}, Href: s.href("/")},
		},
		Actions: s.actions(state),
	}

	for _, h := range state.Hosts {
		e.Entities = append(e.Entities, hostEntity(h))
	}
	e.Entities = append(e.Entities, s.fansEntity(state))

	if state.Job != nil {
		e.Entities = append(e.Entities, s.jobEntity(*state.Job))
	}
	return e, http.StatusOK, nil
}

// hostEntity renders one probed host.
//
// Both signals are reported rather than a single "up", because their
// combination is what distinguishes off from booting from wedged -- see
// power.HostStatus and docs/CLIENT.md.
func hostEntity(h power.HostStatus) Entity {
	return Entity{
		Class: []string{"host", h.Role},
		Rel:   []string{"item"},
		Properties: map[string]any{
			"name": h.Name,
			"ip":   h.IP,
			"ping": h.Ping,
			"ssh":  h.SSH,
			"ms":   h.MS,
		},
	}
}

func (s *Server) fansEntity(state State) Entity {
	props := map[string]any{"on": state.Fans.On, "ip": state.Fans.IP}
	if state.FansErr != nil {
		// Reported rather than swallowed: "the plug is unreachable" is a
		// different situation from "the plug is off", and a client showing
		// the latter for the former would be actively misleading.
		props["error"] = state.FansErr.Error()
	}
	return Entity{
		Class:      []string{"fans"},
		Rel:        []string{"item"},
		Properties: props,
		Links:      []Link{{Rel: []string{"self"}, Href: s.href("/fans")}},
	}
}

func (s *Server) handleFans(state State, _ request) (Entity, int, error) {
	e := s.fansEntity(state)
	e.Rel = nil
	e.Title = "Rack fan plug"
	e.Links = []Link{
		{Rel: []string{"self"}, Href: s.href("/fans")},
		{Rel: []string{"up"}, Href: s.href("/")},
	}
	e.Actions = s.actionsFor(state, "fans-on", "fans-off")
	return e, http.StatusOK, nil
}

func (s *Server) handleFansOn(state State, _ request) (Entity, int, error) {
	return s.setFans(state, true)
}

// handleFansOff switches the plug off, requiring explicit confirmation while
// any host is still drawing power.
//
// The check mirrors the `force` field the registry advertises, so a client
// that renders what it was given never hits this path.
func (s *Server) handleFansOff(state State, req request) (Entity, int, error) {
	if state.anyFHostUp() && !req.boolField("force") {
		return Entity{}, http.StatusConflict, fmt.Errorf(
			"hosts are still running and the rack fans cool them; " +
				"re-send with force=true if you really mean to switch the plug off")
	}
	return s.setFans(state, false)
}

func (s *Server) setFans(state State, on bool) (Entity, int, error) {
	fans, err := s.engine.FansSet(context.Background(), on)
	if err != nil {
		// A failure here is the plug's or the network's, not the client's.
		return Entity{}, http.StatusBadGateway, err
	}

	state.Fans, state.FansErr = fans, nil
	e, _, _ := s.handleFans(state, request{})
	return e, http.StatusOK, nil
}

// handleJob renders the current or last power operation.
func (s *Server) handleJob(state State, _ request) (Entity, int, error) {
	if state.Job == nil {
		return Entity{
			Class:      []string{"job"},
			Title:      "No power operation has run on this node",
			Properties: map[string]any{"state": "none", "node": s.node},
			Links: []Link{
				{Rel: []string{"self"}, Href: s.href("/job")},
				{Rel: []string{"up"}, Href: s.href("/")},
			},
		}, http.StatusOK, nil
	}

	e := s.jobEntity(*state.Job)
	e.Rel = nil
	e.Links = append(e.Links, Link{Rel: []string{"up"}, Href: s.href("/")})
	return e, http.StatusOK, nil
}

func (s *Server) jobEntity(j Job) Entity {
	props := map[string]any{
		"action":  j.Action,
		"state":   string(j.State),
		"started": j.Started,
		"node":    j.Node,
		"rc":      j.RC,
	}
	if j.Finished != "" {
		props["finished"] = j.Finished
	}
	if j.Error != "" {
		props["error"] = j.Error
	}
	return Entity{
		Class:      []string{"job"},
		Rel:        []string{"item"},
		Title:      "Power operation",
		Properties: props,
		Links:      []Link{{Rel: []string{"self"}, Href: s.href("/job")}},
	}
}

// handleAction returns a handler that starts a detached power job.
//
// The response is 202: the work has been accepted, not completed. A client
// follows the job link until its state leaves "running".
func handleAction(action string) func(*Server, State, request) (Entity, int, error) {
	return func(s *Server, _ State, _ request) (Entity, int, error) {
		job, err := s.jobs.start(action, jobArgs(action))
		if errors.Is(err, errJobRunning) {
			return Entity{}, http.StatusConflict, err
		}
		if err != nil {
			return Entity{}, http.StatusInternalServerError, err
		}

		e := s.jobEntity(job)
		e.Rel = nil
		e.Title = "Power operation accepted"
		return e, http.StatusAccepted, nil
	}
}

// jobArgs maps an action name to the CLI invocation the detached child runs.
//
// The child runs the very same code path as `f3sctl power off` typed at a
// shell, which is what keeps the CLI and the API from ever diverging in what
// they actually do.
func jobArgs(action string) []string {
	switch action {
	case "on":
		return []string{"job-run", "power", "on"}
	case "off":
		return []string{"job-run", "power", "off"}
	case "f3-on":
		return []string{"job-run", "power", "f3", "on"}
	case "f3-off":
		return []string{"job-run", "power", "f3", "off"}
	}
	return nil
}

// links renders every resource as a navigable link.
func (s *Server) links() []Link {
	var out []Link
	for _, r := range routes() {
		if r.Action || r.Method != http.MethodGet {
			continue
		}
		out = append(out, Link{Rel: []string{r.Name}, Href: s.href(r.Path), Title: r.Title})
	}
	return out
}

// actions renders every action that is possible right now.
//
// Actions that are not possible are omitted entirely rather than marked
// disabled. That is the core of the contract: a client renders what it is
// given, and never needs to encode a rule about when something is allowed.
func (s *Server) actions(state State) []Action {
	var out []Action
	for _, r := range routes() {
		if !r.Action || !r.available(state) {
			continue
		}
		out = append(out, s.action(r, state))
	}
	return out
}

// actionsFor is actions() narrowed to the named routes, for resources that
// should only advertise their own controls.
func (s *Server) actionsFor(state State, names ...string) []Action {
	var out []Action
	for _, r := range routes() {
		if !r.Action || !r.available(state) || !contains(names, r.Name) {
			continue
		}
		out = append(out, s.action(r, state))
	}
	return out
}

func (s *Server) action(r route, state State) Action {
	a := Action{
		Name:   r.Name,
		Title:  r.Title,
		Method: r.Method,
		Href:   s.href(r.Path),
		Fields: r.fields(state),
	}
	if len(a.Fields) > 0 {
		a.Type = "application/x-www-form-urlencoded"
	}
	return a
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
