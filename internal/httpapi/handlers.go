package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/snonux/f3sctl/internal"
	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/power"
)

// handleRoot renders the entry point: the only URL a client is allowed to know.
func (s *Server) handleRoot(_ context.Context, state State, _ request) (Entity, int, error) {
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
		Links:   s.router.Links(),
		Actions: s.router.Actions(state),
	}, http.StatusOK, nil
}

// handleStatus renders every host plus the fan state in one response, so a
// watchface needs a single request per refresh.
//
// /status makes its own single peer round trip -- via s.peerJob, the same
// helper currentJob uses -- rather than also relying on enrichState's
// PeerBusy (statusPath is excluded from that, see enrichState): this route
// needs the peer's *job*, not just whether it is running, to embed the
// merged job entity, so deriving PeerBusy from that same fetch avoids paying
// for a second, separate peer round trip against a peer that may be slow or
// unreachable.
func (s *Server) handleStatus(ctx context.Context, state State, req request) (Entity, int, error) {
	peer := s.peerJob(ctx, req.APIKey)
	state.PeerBusy = peer != nil && peer.State == coordination.JobRunning

	e := Entity{
		Class:      []string{"status"},
		Title:      "Host and rack status",
		Properties: map[string]any{"node": s.node},
		Links: []Link{
			{Rel: []string{"self"}, Href: s.router.Href("/status")},
			{Rel: []string{"up"}, Href: s.router.Href("/")},
		},
		Actions: s.router.Actions(state),
	}

	for _, h := range state.Hosts {
		e.Entities = append(e.Entities, hostEntity(h))
	}
	e.Entities = append(e.Entities, s.fansEntity(state))

	if job := coordination.NewestJob(state.Job, peer); job != nil {
		e.Entities = append(e.Entities, s.jobEntity(*job))
	}
	return e, http.StatusOK, nil
}

// hostEntity renders one probed host.
//
// Both signals are reported rather than a single "up", because their
// combination is what distinguishes off from booting from wedged -- see
// power.HostStatus and docs/CLIENT.md.
//
// pingKnown is the third bit, and it is here because the server acts on it: a
// host whose probe could not be carried out keeps the rack fans on, so a client
// shown ping=false with no way to tell "silent" from "not measured" would see
// the fans-off confirmation appear over what looks to it like a cold rack. It
// is also the honest answer to "is that host off?", which is what the rest of
// the response is for.
func hostEntity(h power.HostStatus) Entity {
	return Entity{
		Class: []string{"host", h.Role},
		Rel:   []string{"item"},
		Properties: map[string]any{
			"name":      h.Name,
			"ip":        h.IP,
			"ping":      h.Ping,
			"pingKnown": h.PingKnown,
			"ssh":       h.SSH,
			"ms":        h.MS,
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
		Links:      []Link{{Rel: []string{"self"}, Href: s.router.Href("/fans")}},
	}
}

func (s *Server) handleFans(_ context.Context, state State, _ request) (Entity, int, error) {
	e := s.fansEntity(state)
	e.Rel = nil
	e.Title = "Rack fan plug"
	e.Links = []Link{
		{Rel: []string{"self"}, Href: s.router.Href("/fans")},
		{Rel: []string{"up"}, Href: s.router.Href("/")},
	}
	e.Actions = s.router.ActionsFor(state, "fans-on", "fans-off")
	return e, http.StatusOK, nil
}

func (s *Server) handleFansOn(ctx context.Context, state State, _ request) (Entity, int, error) {
	return s.setFans(ctx, state, true)
}

// handleFansOff switches the plug off, requiring explicit confirmation while
// anything in the rack may still be drawing power.
//
// The check mirrors the `force` field the registry advertises, so a client that
// renders what it was given normally never hits this path -- normally, because
// this one is the stricter of the two. See rackStillBusy.
func (s *Server) handleFansOff(ctx context.Context, state State, req request) (Entity, int, error) {
	if !req.boolField("force") {
		if busy := s.rackStillBusy(ctx, state); busy.Busy() {
			return Entity{}, http.StatusConflict, fmt.Errorf(
				"the rack may still be drawing power (%s) and the rack fans cool it; "+
					"re-send with force=true if you really mean to switch the plug off",
				busy.Why())
		}
	}
	return s.setFans(ctx, state, false)
}

// rackStillBusy is the enforcement half of the fan guard: the same question the
// registry asks to decide whether to advertise `force`, answered on the best
// evidence available rather than on the cheapest.
//
// The snapshot is consulted first because it is already taken and because it
// can only say "busy" for a reason the strict probe would also find. Only when
// it says the rack is cold -- the one answer that would actually cut cooling --
// is the confirming probe worth its cost, and that cost is real: it wants
// several consecutive silences per host, so a rack that really is idle takes
// the better part of a minute to prove it. That is precisely what
// `f3sctl fans off` pays locally, and paying it here too is the point. Before
// this, the local command refused while the same command with --remote went
// ahead.
func (s *Server) rackStillBusy(ctx context.Context, state State) power.RackActivity {
	if busy := state.rackBusy(); busy.Busy() {
		return busy
	}
	return s.confirmRack(ctx)
}

// confirmRack runs the strict probe, falling back to the engine's.
//
// A seam for the same reason power.Engine.isUp is one: without it this path can
// only be tested by sending real ICMP to the real rack, so it would not be
// tested at all -- and it is the last thing standing between a remote client
// and the cooling.
func (s *Server) confirmRack(ctx context.Context) power.RackActivity {
	if s.rackConfirm != nil {
		return s.rackConfirm(ctx)
	}
	return s.engine.RackActivity(ctx)
}

func (s *Server) setFans(ctx context.Context, state State, on bool) (Entity, int, error) {
	fans, err := s.engine.FansSet(ctx, on)
	if err != nil {
		// A failure here is the plug's or the network's, not the client's.
		return Entity{}, http.StatusBadGateway, err
	}

	state.Fans, state.FansErr = fans, nil
	e, _, _ := s.handleFans(ctx, state, request{})
	return e, http.StatusOK, nil
}

// handleMonitoring renders whether Gogios alerting is muted, per gateway.
//
// The mute is a marker file on each gateway, set while the cluster is taken
// down on purpose so a deliberate outage does not page. Showing it as a
// resource is what makes a *stranded* mute visible: nothing else in the API
// would reveal that alerting is off.
func (s *Server) handleMonitoring(_ context.Context, state State, _ request) (Entity, int, error) {
	gateways := make([]Entity, 0, len(state.Monitoring))
	for _, gw := range state.Monitoring {
		props := map[string]any{"name": gw.Name}
		if gw.Err != nil {
			// Unreachable is not the same as un-muted, and reporting it as
			// "alerting is fine" would be the more dangerous lie of the two.
			props["error"] = gw.Err.Error()
		} else {
			props["muted"] = gw.Muted
		}
		gateways = append(gateways, Entity{
			Class:      []string{"gateway"},
			Rel:        []string{"item"},
			Properties: props,
		})
	}

	return Entity{
		Class: []string{"monitoring"},
		Title: "Gogios alerting mute",
		Properties: map[string]any{
			"muted": state.monitoringMuted(),
			"node":  s.node,
		},
		Entities: gateways,
		Links: []Link{
			{Rel: []string{"self"}, Href: s.router.Href("/monitoring")},
			{Rel: []string{"up"}, Href: s.router.Href("/")},
		},
		Actions: s.router.ActionsFor(state, "monitoring-mute", "monitoring-unmute"),
	}, http.StatusOK, nil
}

func (s *Server) handleUnmute(ctx context.Context, state State, _ request) (Entity, int, error) {
	return s.setMute(ctx, state, false)
}

func (s *Server) handleMute(ctx context.Context, state State, _ request) (Entity, int, error) {
	return s.setMute(ctx, state, true)
}

// setMute changes the marker on both gateways and re-reads it.
//
// Re-reading rather than assuming: these are two independent gateways reached
// over SSH, and a partial success -- one muted, one not -- is a real outcome
// that the caller needs to see rather than infer from a 200. ctx comes from
// serve() rather than a fresh context.Background(), so the two SSH round
// trips this makes are bounded by the request's own deadline like every other
// backend call a handler makes.
func (s *Server) setMute(ctx context.Context, state State, mute bool) (Entity, int, error) {
	var err error
	if mute {
		err = s.engine.MuteGogios(ctx, io.Discard)
	} else {
		err = s.engine.UnmuteNow(ctx, io.Discard)
	}
	if err != nil {
		return Entity{}, http.StatusBadGateway, err
	}

	state.Monitoring = s.engine.MonitoringStatus(ctx)
	return s.handleMonitoring(ctx, state, request{})
}

// handleJob renders the current or last power operation.
//
// A GET carrying PeerQueryParam is another node's own peer check (Busy or
// FetchJob asking this node for its job), not a client -- and must get this
// node's own job back, unmerged, or the two nodes would ask each other
// forever. Only a request without that marker gets currentJob's merge, so an
// ordinary client sees the same job regardless of which of pi0/pi1 it landed
// on -- see currentJob and coordination.PeerQueryParam for the rest of this
// contract.
func (s *Server) handleJob(ctx context.Context, state State, req request) (Entity, int, error) {
	job := state.Job
	if req.Query.Get(coordination.PeerQueryParam) == "" {
		job = s.currentJob(ctx, state.Job, req.APIKey)
	}

	if job == nil {
		return Entity{
			Class:      []string{"job"},
			Title:      "No power operation has run on either API node",
			Properties: map[string]any{"state": "none", "node": s.node},
			Links: []Link{
				{Rel: []string{"self"}, Href: s.router.Href("/job")},
				{Rel: []string{"up"}, Href: s.router.Href("/")},
			},
		}, http.StatusOK, nil
	}

	e := s.jobEntity(*job)
	e.Rel = nil
	e.Links = append(e.Links, Link{Rel: []string{"up"}, Href: s.router.Href("/")})
	return e, http.StatusOK, nil
}

// currentJob is what makes GET /job report the same job regardless of which
// of pi0/pi1 answered: it merges this node's own last job with its peer's,
// via coordination.NewestJob. See peerJob for the fallback when there is no
// peer to ask.
func (s *Server) currentJob(ctx context.Context, local *coordination.Job, apiKey string) *coordination.Job {
	return coordination.NewestJob(local, s.peerJob(ctx, apiKey))
}

// peerJob asks this node's peer for its own current or last job, tolerating
// no PeerSet at all (nil-safe, matching every other s.peers seam in this
// package) or a peer that cannot be reached -- both report as nil here, the
// same "continue anyway" tolerance PeerSet.Busy already applies before
// starting a job. Shared by currentJob and handleStatus so a route that needs
// both the job and whether the peer is busy pays for one peer round trip, not
// two.
func (s *Server) peerJob(ctx context.Context, apiKey string) *coordination.Job {
	if s.peers == nil {
		return nil
	}
	return s.peers.FetchJob(ctx, s.node, apiKey)
}

func (s *Server) jobEntity(j coordination.Job) Entity {
	props := map[string]any{
		"id":      j.ID,
		"action":  j.Action,
		"state":   string(j.State),
		"started": j.Started,
		"node":    j.Node,
		"rc":      j.RC,
		// staleAfterSeconds is this node's own coordination.Manager.stale
		// ceiling (UnmuteTimeout + a buffer -- see kz0), in seconds. A remote
		// client has no access to this node's UnmuteTimeout config, so before
		// this it had nothing to derive its own poll deadline from and could
		// only hardcode a guess that silently went stale the moment an
		// operator raised UnmuteTimeout server-side (see lz0, and
		// docs/client-reference.js's waitForJob). Reading it here instead
		// keeps a client's patience and this node's own staleness judgment
		// from ever being able to decouple again.
		"staleAfterSeconds": int(s.jobs.StaleCeiling().Seconds()),
	}
	if j.Finished != "" {
		props["finished"] = j.Finished
	}
	if j.Error != "" {
		props["error"] = j.Error
	}
	if j.Step != "" {
		props["step"] = j.Step
	}
	if j.Updated != "" {
		props["updated"] = j.Updated
	}
	if hosts := jobHostProps(j.Hosts); hosts != nil {
		props["hosts"] = hosts
	}
	return Entity{
		Class:      []string{"job"},
		Rel:        []string{"item"},
		Title:      "Power operation",
		Properties: props,
		Links:      []Link{{Rel: []string{"self"}, Href: s.router.Href("/job")}},
	}
}

// jobHostProps renders a job's per-host progress as wire properties, or nil
// when there is none yet. Split out of jobEntity to keep that function within
// this repo's function-length guideline.
func jobHostProps(hosts map[string]coordination.HostProgress) map[string]any {
	if len(hosts) == 0 {
		return nil
	}
	out := map[string]any{}
	for name, hp := range hosts {
		entry := map[string]any{"phase": hp.Phase}
		if hp.Detail != "" {
			entry["detail"] = hp.Detail
		}
		out[name] = entry
	}
	return out
}

// handleAction returns a handler that starts a detached power job.
//
// The response is 202: the work has been accepted, not completed. A client
// follows the job link until its state leaves "running".
func handleAction(action string) func(*Server, context.Context, State, request) (Entity, int, error) {
	// ctx bounds the peer-busy check (PeerSet.Busy now takes one): the
	// other API node is asked before the local flock is taken, and a
	// cancelled request -- the CGI client that went home -- stops waiting on
	// a peer that is neither idle nor answering. It is NOT threaded into
	// Manager.Start: Start spawns a detached child that re-execs and outlives
	// this CGI request by design -- see internal/coordination/run.go -- so
	// even if Start grew a context parameter, the request's ctx would be the
	// wrong one to give it; the job must keep running after the response that
	// started it has been sent.
	return func(s *Server, ctx context.Context, _ State, req request) (Entity, int, error) {
		// Ask the other API node first. The local flock only serialises
		// requests that reach THIS node, and relayd load-balances the two, so
		// without this two clicks seconds apart start two shutdowns against
		// the same hosts -- observed on 2026-08-08.
		if busy, node := s.peers.Busy(ctx, s.node, req.APIKey); busy {
			return Entity{}, http.StatusConflict,
				fmt.Errorf("a power operation is already running on %s", node)
		}

		job, err := s.jobs.Start(action, s.jobArgs(action))
		if errors.Is(err, coordination.ErrJobRunning) {
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

// jobArgs maps a job action identifier to the CLI invocation the detached
// child runs. It is jobArgsFrom bound to the real route table; see that
// function for the derivation and consistency_test.go's consistency tests for
// what it guards against.
//
// A Server method rather than a free function so the route table is read
// from this server's configured inventory (s.router.inv), the same one the
// engine acts on -- see routes' doc comment in registry.go.
func (s *Server) jobArgs(action string) []string {
	return jobArgsFrom(routes(s.router.inv), action)
}

// jobArgsFrom finds the route whose job action identifier (route.jobAction,
// see registry.go) is action, and splits its declared CLIVerb into words to
// build the detached child's argv. Split out of jobArgs so a test can feed it
// a synthetic route table -- e.g. one route with no CLIVerb declared -- and
// check the fallback that never happens against the real registry.
//
// The child runs the very same code path as `f3sctl power off` typed at a
// shell, which is what keeps the CLI and the API from ever diverging in what
// they actually do. This used to be a hand-written switch keyed on the same
// strings the route registry, the client and the CLI each parsed
// independently -- see sy0's annotation for the drift that let a new action
// silently disagree between them. Deriving the argv from CLIVerb instead
// means the route declaration is the only place the words "power f1 on" are
// written down.
func jobArgsFrom(rs []route, action string) []string {
	for _, r := range rs {
		if r.Action && r.CLIVerb != "" && r.jobAction() == action {
			return append([]string{"job-run"}, strings.Fields(r.CLIVerb)...)
		}
	}
	return nil
}

// links, actions and actionsFor -- rendering every resource as a navigable
// link, and every currently-possible action -- now live on Router, since they
// depend only on the route registry and a base href, not on anything else
// Server carries. See Router.Links, Router.Actions, Router.ActionsFor.
