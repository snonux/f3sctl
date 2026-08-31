package powerapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/power"
)

// handleStatus renders every host plus the fan state in one response, so a
// watchface needs a single request per refresh.
//
// /status makes its own single peer round trip -- via peerJob, the same
// helper handleJob uses -- rather than also relying on the composition root's
// enrichState PeerBusy check (StatusPath is excluded from that, see
// enrichState): this route needs the peer's *job*, not just whether it is
// running, to embed the merged job entity, so deriving PeerBusy from that
// same fetch avoids paying for a second, separate peer round trip against a
// peer that may be slow or unreachable.
func (sf *Surface) handleStatus(ctx context.Context, state contract.State, req contract.Request) (contract.Entity, int, error) {
	peer := sf.peerJob(ctx, req.APIKey)
	state.PeerBusy = peer != nil && peer.State == coordination.JobRunning

	e := contract.Entity{
		Class:      []string{"status"},
		Title:      "Host and rack status",
		Properties: map[string]any{"node": sf.Node},
		Links: []contract.Link{
			{Rel: []string{"self"}, Href: sf.Href(StatusPath)},
			{Rel: []string{"up"}, Href: sf.Href("/")},
		},
		Actions: sf.allActions(state),
	}

	for _, h := range state.Hosts {
		e.Entities = append(e.Entities, hostEntity(h))
	}
	e.Entities = append(e.Entities, sf.fansEntity(state))

	if job := coordination.NewestJob(state.Job, peer); job != nil {
		e.Entities = append(e.Entities, sf.jobEntity(*job))
	}
	return e, http.StatusOK, nil
}

// allActions and actionsFor render actions onto a resource, nil-safely: the
// table can be declared (and its predicates tested) without the composition
// root's Router attached, and a route declaration never needs to render an
// actions list -- only serving one does. The injected fields (see Surface)
// are what keep the Siren action shape in exactly one place, the composition
// root's Router, so a surface cannot grow its own action rendering that
// drifts from the rest of the API's.
func (sf *Surface) allActions(state contract.State) []contract.Action {
	if sf.Actions == nil {
		return nil
	}
	return sf.Actions(state)
}

func (sf *Surface) actionsFor(state contract.State, names ...string) []contract.Action {
	if sf.ActionsFor == nil {
		return nil
	}
	return sf.ActionsFor(state, names...)
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
func hostEntity(h power.HostStatus) contract.Entity {
	return contract.Entity{
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

func (sf *Surface) fansEntity(state contract.State) contract.Entity {
	props := map[string]any{"on": state.Fans.On, "ip": state.Fans.IP}
	if state.FansErr != nil {
		// Reported rather than swallowed: "the plug is unreachable" is a
		// different situation from "the plug is off", and a client showing
		// the latter for the former would be actively misleading.
		props["error"] = state.FansErr.Error()
	}
	return contract.Entity{
		Class:      []string{"fans"},
		Rel:        []string{"item"},
		Properties: props,
		Links:      []contract.Link{{Rel: []string{"self"}, Href: sf.Href("/fans")}},
	}
}

func (sf *Surface) handleFans(_ context.Context, state contract.State, _ contract.Request) (contract.Entity, int, error) {
	e := sf.fansEntity(state)
	e.Rel = nil
	e.Title = "Rack fan plug"
	e.Links = []contract.Link{
		{Rel: []string{"self"}, Href: sf.Href("/fans")},
		{Rel: []string{"up"}, Href: sf.Href("/")},
	}
	e.Actions = sf.actionsFor(state, "fans-on", "fans-off")
	return e, http.StatusOK, nil
}

func (sf *Surface) handleFansOn(ctx context.Context, state contract.State, req contract.Request) (contract.Entity, int, error) {
	return sf.setFans(ctx, state, req, true)
}

// handleFansOff switches the plug off, requiring explicit confirmation while
// anything in the rack may still be drawing power.
//
// The check mirrors the `force` field the registry advertises, so a client that
// renders what it was given normally never hits this path -- normally, because
// this one is the stricter of the two. See rackStillBusy.
func (sf *Surface) handleFansOff(ctx context.Context, state contract.State, req contract.Request) (contract.Entity, int, error) {
	if !req.BoolField("force") {
		if busy := sf.rackStillBusy(ctx, state); busy.Busy() {
			return contract.Entity{}, http.StatusConflict, fmt.Errorf(
				"the rack may still be drawing power (%s) and the rack fans cool it; "+
					"re-send with force=true if you really mean to switch the plug off",
				busy.Why())
		}
	}
	return sf.setFans(ctx, state, req, false)
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
func (sf *Surface) rackStillBusy(ctx context.Context, state contract.State) power.RackActivity {
	if busy := RackBusy(state); busy.Busy() {
		return busy
	}
	return sf.confirmRack(ctx)
}

// confirmRack runs the strict probe, falling back to the engine's.
//
// A seam for the same reason power.Engine.isUp is one: without it this path can
// only be tested by sending real ICMP to the real rack, so it would not be
// tested at all -- and it is the last thing standing between a remote client
// and the cooling.
func (sf *Surface) confirmRack(ctx context.Context) power.RackActivity {
	if sf.RackConfirm != nil {
		return sf.RackConfirm(ctx)
	}
	return sf.Engine.RackActivity(ctx)
}

func (sf *Surface) setFans(ctx context.Context, state contract.State, req contract.Request, on bool) (contract.Entity, int, error) {
	fans, err := sf.Engine.FansSet(ctx, on)
	if err != nil {
		// A failure here is the plug's or the network's, not the client's.
		return contract.Entity{}, http.StatusBadGateway, err
	}

	state.Fans, state.FansErr = fans, nil
	e, _, _ := sf.handleFans(ctx, state, req)
	return e, http.StatusOK, nil
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
func (sf *Surface) handleJob(ctx context.Context, state contract.State, req contract.Request) (contract.Entity, int, error) {
	job := state.Job
	if req.Query.Get(coordination.PeerQueryParam) == "" {
		job = sf.currentJob(ctx, state.Job, req.APIKey)
	}

	if job == nil {
		return contract.Entity{
			Class:      []string{"job"},
			Title:      "No power operation has run on either API node",
			Properties: map[string]any{"state": "none", "node": sf.Node},
			Links: []contract.Link{
				{Rel: []string{"self"}, Href: sf.Href(JobPath)},
				{Rel: []string{"up"}, Href: sf.Href("/")},
			},
		}, http.StatusOK, nil
	}

	e := sf.jobEntity(*job)
	e.Rel = nil
	e.Links = append(e.Links, contract.Link{Rel: []string{"up"}, Href: sf.Href("/")})
	return e, http.StatusOK, nil
}

// currentJob is what makes GET /job report the same job regardless of which
// of pi0/pi1 answered: it merges this node's own last job with its peer's,
// via coordination.NewestJob. See peerJob for the fallback when there is no
// peer to ask.
func (sf *Surface) currentJob(ctx context.Context, local *coordination.Job, apiKey string) *coordination.Job {
	return coordination.NewestJob(local, sf.peerJob(ctx, apiKey))
}

// peerJob asks this node's peer for its own current or last job, tolerating
// no PeerSet at all (nil-safe, matching every other injected seam in this
// package) or a peer that cannot be reached -- both report as nil here, the
// same "continue anyway" tolerance PeerSet.Busy already applies before
// starting a job. Shared by currentJob and handleStatus so a route that needs
// both the job and whether the peer is busy pays for one peer round trip, not
// two.
func (sf *Surface) peerJob(ctx context.Context, apiKey string) *coordination.Job {
	if sf.Peers == nil {
		return nil
	}
	return sf.Peers.FetchJob(ctx, sf.Node, apiKey)
}

func (sf *Surface) jobEntity(j coordination.Job) contract.Entity {
	props := map[string]any{
		"id":      j.ID,
		"action":  j.Action,
		"state":   string(j.State),
		"started": j.Started,
		"node":    j.Node,
		"rc":      j.RC,
		// staleAfterSeconds is the coordination Manager's staleness ceiling
		// (UnmuteTimeout + a buffer -- see kz0), in seconds. A remote client
		// has no access to this node's UnmuteTimeout config, so before
		// this it had nothing to derive its own poll deadline from and could
		// only hardcode a guess that silently went stale the moment an
		// operator raised UnmuteTimeout server-side (see lz0, and
		// docs/client-reference.js's waitForJob). Reading it here instead
		// keeps a client's patience and this node's own staleness judgment
		// from ever being able to decouple again.
		"staleAfterSeconds": int(sf.Jobs.StaleCeiling().Seconds()),
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
	return contract.Entity{
		Class:      []string{"job"},
		Rel:        []string{"item"},
		Title:      "Power operation",
		Properties: props,
		Links:      []contract.Link{{Rel: []string{"self"}, Href: sf.Href(JobPath)}},
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

// action returns a handler that starts a detached power job.
//
// The response is 202: the work has been accepted, not completed. A client
// follows the job link until its state leaves "running".
func (sf *Surface) action(action string) contract.Handle {
	// ctx bounds the peer-busy check (PeerSet.Busy now takes one): the
	// other API node is asked before the local flock is taken, and a
	// cancelled request -- the CGI client that went home -- stops waiting on
	// a peer that is neither idle nor answering. It is NOT threaded into
	// Manager.Start: Start spawns a detached child that re-execs and outlives
	// this CGI request by design -- see internal/jobrun -- so
	// even if Start grew a context parameter, the request's ctx would be the
	// wrong one to give it; the job must keep running after the response that
	// started it has been sent.
	return func(ctx context.Context, _ contract.State, req contract.Request) (contract.Entity, int, error) {
		// Ask the other API node first. The local flock only serialises
		// requests that reach THIS node, and relayd load-balances the two, so
		// without this two clicks seconds apart start two shutdowns against
		// the same hosts -- observed on 2026-08-08.
		if busy, node := sf.Peers.Busy(ctx, sf.Node, req.APIKey); busy {
			return contract.Entity{}, http.StatusConflict,
				fmt.Errorf("a power operation is already running on %s", node)
		}

		job, err := sf.Jobs.Start(action, sf.jobArgs(action))
		if errors.Is(err, coordination.ErrJobRunning) {
			return contract.Entity{}, http.StatusConflict, err
		}
		if err != nil {
			return contract.Entity{}, http.StatusInternalServerError, err
		}

		e := sf.jobEntity(job)
		e.Rel = nil
		e.Title = "Power operation accepted"
		return e, http.StatusAccepted, nil
	}
}

// jobArgs maps a job action identifier to the CLI invocation the detached
// child runs. It is JobArgsFrom bound to this surface's own route table; see
// that function for the derivation and the consistency tests (in the
// composition root) for what it guards against.
//
// The surface's own Routes() is read from the same inventory the engine acts
// on (sf.Inv), the one the composition root configured -- see Routes' doc
// comment.
func (sf *Surface) jobArgs(action string) []string {
	return JobArgsFrom(sf.Routes(), action)
}

// JobArgsFrom finds the route whose job action identifier (Route.JobAction)
// is action, and splits its declared CLIVerb into words to build the detached
// child's argv. A test can feed it a synthetic route table -- e.g. one route
// with no CLIVerb declared -- and check the fallback that never happens
// against the real registry.
//
// The child runs the very same code path as `f3sctl power off` typed at a
// shell, which is what keeps the CLI and the API from ever diverging in what
// they actually do. This used to be a hand-written switch keyed on the same
// strings the route registry, the client and the CLI each parsed
// independently -- see sy0's annotation for the drift that let a new action
// silently disagree between them. Deriving the argv from CLIVerb instead
// means the route declaration is the only place the words "power f1 on" are
// written down.
func JobArgsFrom(rs []contract.Route, action string) []string {
	for _, r := range rs {
		if r.Action && r.CLIVerb != "" && r.JobAction() == action {
			return append([]string{"job-run"}, strings.Fields(r.CLIVerb)...)
		}
	}
	return nil
}
