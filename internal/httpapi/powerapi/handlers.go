package powerapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
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
		Actions: sf.actions.Actions(state),
	}

	e.Entities = append(e.Entities, sf.hostEntities(state.Hosts)...)
	e.Entities = append(e.Entities, sf.fansEntity(state))
	e.Entities = append(e.Entities, sf.acEntity(state))

	if job := coordination.NewestJob(state.Job, peer); job != nil {
		e.Entities = append(e.Entities, sf.jobEntity(*job))
	}
	return e, http.StatusOK, nil
}

// handlePowerFolder renders the Power control section folder: host wake and
// shutdown. Its links are status and job; its actions are every host power
// operation the route table offers right now. Shelly plug switches live on
// the sibling AC control folder (/ac-control).
//
// It looks up nothing by hand: SectionActions narrows the route table's own
// offer by contract.Route.Section, so a power action added to the surface is
// in this folder (and in the OpenAPI document's Power section) with no second
// edit. The entity is judged on the SAME fleet snapshot the status route
// uses, which is why the route is not SkipsProbe despite rendering links
// alone.
func (sf *Surface) handlePowerFolder(_ context.Context, state contract.State, _ contract.Request) (contract.Entity, int, error) {
	return contract.Entity{
		Class:      []string{"power", "section"},
		Title:      "Power control",
		Properties: map[string]any{"node": sf.Node},
		Links: []contract.Link{
			{Rel: []string{"self"}, Href: sf.Href("/power")},
			{Rel: []string{"up"}, Href: sf.Href("/")},
			{Rel: []string{"status"}, Href: sf.Href(StatusPath)},
			{Rel: []string{"job"}, Href: sf.Href(JobPath)},
		},
		Actions: sf.actions.SectionActions(state, contract.SectionPower),
	}, http.StatusOK, nil
}

// handleACControlFolder renders the AC control section folder: both Shelly
// plugs (rack fans on shelly1, f-host mains on shelly2). Peer to Power
// control on the root; plug resources are NoRootLink and reached from here.
func (sf *Surface) handleACControlFolder(_ context.Context, state contract.State, _ contract.Request) (contract.Entity, int, error) {
	return contract.Entity{
		Class:      []string{"ac-control", "section"},
		Title:      "AC control",
		Properties: map[string]any{"node": sf.Node},
		Links: []contract.Link{
			{Rel: []string{"self"}, Href: sf.Href("/ac-control")},
			{Rel: []string{"up"}, Href: sf.Href("/")},
			{Rel: []string{"fans"}, Href: sf.Href("/fans")},
			{Rel: []string{"ac"}, Href: sf.Href("/ac")},
		},
		Actions: sf.actions.SectionActions(state, contract.SectionAC),
	}, http.StatusOK, nil
}

// hostEntities renders the snapshot's hosts, marking which are in the power
// group -- the hosts power-on/power-off act on and the fan guard judges.
//
// The membership is decided here, from the configured inventory through the
// same selector the availability predicates use (power.PowerGroupStatuses),
// so a client never needs its own copy of the rule (docs/CLIENT.md §4).
func (sf *Surface) hostEntities(hosts []power.HostStatus) []contract.Entity {
	group := power.PowerGroupStatuses(sf.Inv, hosts)
	out := make([]contract.Entity, 0, len(hosts))
	for _, h := range hosts {
		member := slices.ContainsFunc(group, func(g power.HostStatus) bool { return g.Name == h.Name })
		out = append(out, hostEntity(h, member))
	}
	return out
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
//
// powerGroup says whether the host is one power-on/power-off act on (see
// hostEntities); the host's role is its second class.
func hostEntity(h power.HostStatus, powerGroup bool) contract.Entity {
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
			// Derived from the inventory, never from the name: a client
			// must not re-derive the power group itself.
			"powerGroup": powerGroup,
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
		// Nested under AC control, not the root overview (NoRootLink).
		{Rel: []string{"up"}, Href: sf.Href("/ac-control")},
	}
	e.Actions = sf.actions.ActionsFor(state, "fans-on", "fans-off")
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
//
// The guard may have spent up to a minute probing, and a job may have started
// on either node in that time; see jobStartedMeanwhile. That is checked
// before the probe's own verdict, whatever it was: a job waking the hosts
// makes the probe hear them, and "re-send with force=true" would then be
// exactly the wrong advice.
func (sf *Surface) handleFansOff(ctx context.Context, state contract.State, req contract.Request) (contract.Entity, int, error) {
	if !req.BoolField("force") {
		busy := sf.rackStillBusy(ctx, state)
		if sf.jobStartedMeanwhile(ctx, req.APIKey) {
			return contract.Entity{}, http.StatusConflict, contract.NotAvailableError("fans-off")
		}
		if busy.Busy() {
			return contract.Entity{}, http.StatusConflict, fmt.Errorf(
				"the rack may still be drawing power (%s) and the rack fans cool it; "+
					"re-send with force=true if you really mean to switch the plug off",
				busy.Why())
		}
	}
	return sf.setFans(ctx, state, req, false)
}

// jobStartedMeanwhile re-reads the peer's and this node's job state, fresh
// rather than from the request's snapshot, after an off handler's confirming
// probe and right before its plug write.
//
// serve() refuses a plug switch while a job runs, but it judges that once, up
// front, and the off handlers then probe for the better part of a minute. A
// power-on or all-cycle started on either node inside that window would have
// its plug flipped under it. This shrinks the window rather than closing it:
// it is a re-check, not a lock -- the plug has no lock to take, and
// Manager.Start's flock only serialises jobs. The switches without a probe
// (on, and off with force) have no such window beyond serve()'s own check, so
// they are not re-checked.
//
// The peer is asked first and the local job file read last, so the local gap
// left is only FansSet/ACSet's digest-authenticated Shelly round trip. The
// peer's answer is older by the local read plus that round trip, and by up to
// the peer client's 3s timeout on top when the peer is slow: that one extra
// round trip is the price of the check. A peer that is down or does not
// answer in time counts as idle, the same fail-open PeerSet.Busy applies
// everywhere else: if one node is down the other must still be able to switch
// the plugs.
func (sf *Surface) jobStartedMeanwhile(ctx context.Context, apiKey string) bool {
	if busy, _ := sf.Peers.Busy(ctx, sf.Node, apiKey); busy {
		return true
	}
	j := sf.Jobs.Read()
	return j != nil && j.State == coordination.JobRunning
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
	if busy := sf.rackBusy(state); busy.Busy() {
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

func (sf *Surface) acEntity(state contract.State) contract.Entity {
	props := map[string]any{"on": state.AC.On, "ip": state.AC.IP}
	if state.ACErr != nil {
		props["error"] = state.ACErr.Error()
	}
	return contract.Entity{
		Class:      []string{"ac"},
		Rel:        []string{"item"},
		Properties: props,
		Links:      []contract.Link{{Rel: []string{"self"}, Href: sf.Href("/ac")}},
	}
}

func (sf *Surface) handleAC(_ context.Context, state contract.State, _ contract.Request) (contract.Entity, int, error) {
	e := sf.acEntity(state)
	e.Rel = nil
	e.Title = "F-host mains AC plug"
	e.Links = []contract.Link{
		{Rel: []string{"self"}, Href: sf.Href("/ac")},
		// Nested under AC control, not the root overview (NoRootLink).
		{Rel: []string{"up"}, Href: sf.Href("/ac-control")},
	}
	e.Actions = sf.actions.ActionsFor(state, "ac-on", "ac-off")
	return e, http.StatusOK, nil
}

func (sf *Surface) handleACOn(ctx context.Context, state contract.State, req contract.Request) (contract.Entity, int, error) {
	return sf.setAC(ctx, state, req, true)
}

// handleACOff switches the AC plug off, requiring explicit confirmation while
// any f-host may still be drawing power. Independent of power off: cutting
// AC is never an automatic side-effect of a graceful shutdown.
//
// As for fans-off, a job that started during the confirming probe is caught
// by jobStartedMeanwhile before the plug is touched, and reported ahead of
// whatever the probe found.
func (sf *Surface) handleACOff(ctx context.Context, state contract.State, req contract.Request) (contract.Entity, int, error) {
	if !req.BoolField("force") {
		busy := sf.acStillBusy(ctx, state)
		if sf.jobStartedMeanwhile(ctx, req.APIKey) {
			return contract.Entity{}, http.StatusConflict, contract.NotAvailableError("ac-off")
		}
		if busy.Busy() {
			return contract.Entity{}, http.StatusConflict, fmt.Errorf(
				"hosts may still be drawing power (%s); cutting mains AC hard-powers them off "+
					"and risks ZFS / bhyve damage; re-send with force=true if you really mean it",
				busy.Why())
		}
	}
	return sf.setAC(ctx, state, req, false)
}

func (sf *Surface) acStillBusy(ctx context.Context, state contract.State) power.RackActivity {
	if busy := sf.acBusy(state); busy.Busy() {
		return busy
	}
	return sf.confirmAC(ctx)
}

func (sf *Surface) confirmAC(ctx context.Context) power.RackActivity {
	if sf.ACConfirm != nil {
		return sf.ACConfirm(ctx)
	}
	return sf.Engine.ACActivity(ctx)
}

func (sf *Surface) setAC(ctx context.Context, state contract.State, req contract.Request, on bool) (contract.Entity, int, error) {
	ac, err := sf.Engine.ACSet(ctx, on)
	if err != nil {
		return contract.Entity{}, http.StatusBadGateway, err
	}

	state.AC, state.ACErr = ac, nil
	e, _, _ := sf.handleAC(ctx, state, req)
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
