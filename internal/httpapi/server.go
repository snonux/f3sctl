package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/debug"
	"strings"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/gogios"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/httpapi/gogiosapi"
	"github.com/snonux/f3sctl/internal/httpapi/powerapi"
	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power"
)

// Server answers one CGI request.
//
// It owns no coordination logic of its own, and little else besides the
// composition itself: whether a job may start, whether the peer node is busy,
// and the job's lifecycle all live in internal/coordination, injected here as
// jobs and peers; the API key check, route matching/href-building, response
// serialisation and the OpenAPI doc live in Authenticator, Router,
// SirenRenderer and OpenAPIBuilder; and the domain routes and handlers live in
// the two surface packages, internal/gogiosapi and internal/powerapi, each
// holding exactly one concern of the API. Server's own job is to compose
// these -- build the shared collaborators, assemble the route table out of the
// surfaces, parse the request, ask engine/jobs/peers/auth/router what is true,
// hand the answer to the route to render.
type Server struct {
	cfg    config.Config
	engine *power.Engine
	// jobs owns the on-disk lifecycle of a power job started by this node:
	// claiming the lock, spawning the detached child, and reading back its
	// progress. See internal/coordination.Manager.
	jobs *coordination.Manager
	// peers answers whether the *other* API node is currently running a job,
	// so a client is never offered an action that job would conflict with.
	// See internal/coordination.PeerSet.
	peers *coordination.PeerSet
	// auth checks a request's API key against the one configured for this
	// node. See Authenticator.
	auth *Authenticator
	// router matches a request to a route and builds the absolute hrefs
	// handed back to clients. See Router.
	router *Router
	// openapi generates the OpenAPI document served at /openapi.json, from
	// the same route declarations router serves. See OpenAPIBuilder.
	openapi *OpenAPIBuilder
	// siren writes every response -- Siren entity, plain JSON, or error --
	// in the CGI wire format. See SirenRenderer.
	siren SirenRenderer
	node  string

	// probeHosts probes every host worth reporting, feeding State.Hosts. Nil
	// means the engine's own probe (Engine.ProbeAll, ~3s of concurrent
	// ping+TCP dials); only tests substitute anything else -- to count calls,
	// or to avoid paying for real network probes when what is under test is
	// whether snapshot() ran them at all. See Server.probeHostsFn.
	probeHosts func(context.Context) []power.HostStatus

	// fansStatus reads the rack-fan Shelly plug, feeding State.Fans and
	// State.FansErr. Nil means the engine's own read (Engine.FansStatus, an
	// HTTP call bounded by a 5s timeout); same reasoning as probeHosts. See
	// Server.fansStatusFn.
	fansStatus func(context.Context) (power.FansState, error)

	// acStatus reads the f-host mains AC Shelly plug, feeding State.AC and
	// State.ACErr. Nil means Engine.ACStatus; same reasoning as fansStatus.
	acStatus func(context.Context) (power.ACState, error)

	// monitorStatus reads the Gogios mute marker from both gateways, feeding
	// State.Monitoring. Nil means the engine's own read
	// (Engine.MonitoringStatus, an SSH round trip to each gateway, several
	// seconds); same reasoning as probeHosts. Fetched only for the routes
	// that declare contract.NeedMonitoring (see enrichState). See
	// Server.monitorStatusFn.
	monitorStatus func(context.Context) []power.GatewayMute
}

// ServeCGI answers a single CGI request read from the process environment and
// stdin, writing the response to out.
func ServeCGI(cfg config.Config, out io.Writer) error {
	return serveCGI(cfg, os.Stdin, out, os.Stderr, newServer)
}

// serveCGI is ServeCGI with its process-level dependencies passed in: the
// request body, the log stream a panic is reported to, and the Server
// constructor -- the seam that lets a test serve through a Server whose
// handler panics.
//
// A panic on this request's own goroutine -- a violated invariant such as
// serverActions' unbuilt Router, or a bug in a handler or a snapshot hook --
// is recovered here: its value and stack go to logw (the web server's error
// log, under CGI), and the client gets a Siren 500 like any other server
// fault rather than a truncated or empty response. The panic's text is
// deliberately not sent to the client. A panic on any other goroutine --
// the engine's concurrent per-host probes, say -- cannot be recovered here
// and still crashes the process, as Go gives no way to catch it from outside
// that goroutine.
func serveCGI(cfg config.Config, stdin io.Reader, out, logw io.Writer, newSrv func(config.Config) (*Server, error)) (err error) {
	// SirenRenderer is stateless, so it is safe to use ahead of a Server --
	// the error paths below can fire before one exists at all (a malformed
	// request, a Server that failed to construct, or a panic while
	// constructing one).
	siren := NewSirenRenderer()
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintf(logw, "f3sctl: panic serving CGI request: %v\n%s", p, debug.Stack())
			err = siren.WriteError(out, http.StatusInternalServerError, "internal server error")
		}
	}()

	req, err := parseCGIRequest(stdin)
	if err != nil {
		return siren.WriteError(out, http.StatusBadRequest, err.Error())
	}

	srv, err := newSrv(cfg)
	if err != nil {
		// A misconfigured server (unreadable SSH key, say) is a server fault,
		// not the client's. Report it as one so a client does not retry.
		return siren.WriteError(out, http.StatusInternalServerError, err.Error())
	}

	return srv.serve(out, req)
}

func newServer(cfg config.Config) (*Server, error) {
	eng, err := power.New(cfg)
	if err != nil {
		return nil, err
	}
	node, _ := os.Hostname()

	base := strings.TrimSuffix(os.Getenv("SCRIPT_NAME"), "/")
	href := contract.Hrefs(base)
	jobs := coordination.NewManager(cfg.StateDir, cfg.UnmuteTimeout.D(), power.ShutdownWorstCase(cfg))
	peers := coordination.NewPeerSet(cfg.PeerNodes, resolvePeerJobPath(cfg, base))

	srv := &Server{
		cfg:    cfg,
		engine: eng,
		jobs:   jobs,
		peers:  peers,
		auth:   NewAuthenticator(cfg.APIKeyFile),
		siren:  NewSirenRenderer(),
		node:   node,
	}

	// The two domain surfaces, each bound to exactly the collaborators its
	// handlers need and sharing this node's href builder. build constructs
	// them, handing both its own action renderer.
	newPower := func(actions contract.ActionRenderer) *powerapi.Surface {
		return powerapi.New(node, href, cfg.Inventory, eng, jobs, peers, actions)
	}
	newGogios := func(actions contract.ActionRenderer) *gogiosapi.Surface {
		return gogiosapi.New(node, href, cfg, eng, actions)
	}
	return srv.build(cfg.Inventory, newPower, newGogios, base)
}

// build constructs both domain surfaces, builds this Server's route table
// from them, and hangs a Router (and the OpenAPI builder over it) off the
// Server -- the wiring that makes the Server servable.
//
// build owns the surfaces' construction, taking factories rather than
// finished surfaces, so the renderer is always build's to supply: each
// factory is handed this Server's own (see serverActions), which resolves the
// Router built here, and must pass it through to its surface's New. It is
// its own step so tests can construct a Server literal, supply the surfaces
// they want, and go through exactly the same path production takes (their
// assemble helper wraps this one).
//
// It fails only on an ambiguous route table (see NewRouter).
func (s *Server) build(inv inventory.Inventory, newPower powerSurfaceFunc, newGogios gogiosSurfaceFunc, base string) (*Server, error) {
	actions := s.actionRenderer()
	router, err := NewRouter(base, s.buildRoutes(inv, newPower(actions), newGogios(actions)))
	if err != nil {
		return nil, err
	}
	s.router = router
	s.openapi = NewOpenAPIBuilder(router, inv)
	return s, nil
}

// resolvePeerJobPath returns the URL path this node asks a peer for its
// current job.
//
// An explicit cfg.PeerJobPath always wins, for the rare case where the two
// peers are not mounted the same way. Otherwise (the default) it is derived
// from this node's own mount -- the identical mechanism every link and action
// handed back to a client already goes through -- on the assumption that pi0
// and pi1 are symmetric peers sharing one CGI mount. That keeps a SCRIPT_NAME
// remount a one-place change instead of two: without this, an operator who
// moves the mount point but forgets the separate peer_job_path config value
// gets a peer check that silently reads back as idle forever, which is the
// dangerous failure mode -- two jobs can start.
//
// The one case that derivation must NOT be trusted for: base itself being
// empty. That happens whenever this node's own SCRIPT_NAME was empty or
// missing when the base was read (bozohttpd not setting it, a proxy that
// strips the header, ServeCGI invoked outside its normal CGI harness) -- and
// an empty SCRIPT_NAME is far more likely to be a broken environment than a
// deliberate "the API is mounted at the filesystem root". Deriving anyway
// would silently hand PeerSet a bare "/job", which almost certainly 404s on
// the peer; fetchPeerJob then errors, and PeerSet.Busy treats every fetch
// error as "peer not busy" -- an unreachable-reads-as-idle failure with
// nothing to distinguish "the peer is genuinely down" from "this node
// mis-derived the URL it asked at". Falling back to this project's own
// documented CGI mount convention (defaultCGIMount, the literal that was
// hardcoded here before this derivation existed) is a safer bet than trusting
// an empty base at face value, and matches what every real deployment of this
// project actually uses.
func resolvePeerJobPath(cfg config.Config, base string) string {
	if cfg.PeerJobPath != "" {
		return cfg.PeerJobPath
	}
	if base == "" {
		return defaultCGIMount + powerapi.JobPath
	}
	return contract.Href(base, powerapi.JobPath)
}

// defaultCGIMount is this project's own documented CGI mount convention (see
// README.md's example config, and config.Default() before uy0). It is the
// last-resort fallback resolvePeerJobPath uses when this node's own router
// has no base to derive anything from -- see that function's doc comment for
// why an empty base cannot be trusted as "mounted at the root".
const defaultCGIMount = "/cgi-bin/f3sctl"

func (s *Server) serve(out io.Writer, req contract.Request) error {
	if err := s.auth.Check(req.APIKey); err != nil {
		// Deliberately identical for a missing and a wrong key: telling an
		// attacker which of the two they got is free information.
		return s.siren.WriteError(out, http.StatusUnauthorized, "unauthorized")
	}

	r, ok := s.router.Lookup(req.Method, req.Path)
	if !ok {
		if s.router.PathExists(req.Path) {
			return s.siren.WriteError(out, http.StatusMethodNotAllowed,
				fmt.Sprintf("%s is not allowed on %s", req.Method, req.Path))
		}
		return s.siren.WriteError(out, http.StatusNotFound, "no such resource: "+req.Path)
	}

	// Bound the request itself. The detached power job is spawned by the
	// request but deliberately outlives it (the power surface's action handler
	// passes no context to jobs.Start), so this never cancels a running job --
	// it bounds only what this request does synchronously: the fleet probe,
	// the Shelly read, the peer job round trip and the fan-guard re-confirm a
	// `fans off` runs. A request wedged on a slow or dead backend aborts here
	// cleanly rather than holding the CGI process open indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.CGITimeout.D())
	defer cancel()
	state := s.enrichState(ctx, s.snapshot(ctx, req), r, req)

	// An action that is not currently available is refused here, before any
	// handler runs. A well-written client never reaches this: it was not
	// offered the action in the first place. This is the backstop for a
	// client racing another, or one that ignored the contract.
	if r.Action && !r.IsAvailable(state) {
		return s.siren.WriteError(out, http.StatusConflict, contract.NotAvailableError(r.Name).Error())
	}

	entity, status, err := r.Handle(ctx, state, req)
	if err != nil {
		return s.siren.WriteError(out, status, err.Error())
	}

	// The OpenAPI document is served as itself rather than wrapped in Siren:
	// a tool that reads OpenAPI expects the document at the top level.
	if req.Path == openAPIPath {
		return s.siren.WriteJSON(out, status, entity.Properties)
	}
	return s.siren.WriteEntity(out, status, entity)
}

// snapshot probes what the requested route actually needs, once.
//
// Job is a local disk read (coordination.Manager.Read), cheap enough to take
// unconditionally. Hosts, Fans and AC are not: Hosts costs Engine.ProbeAll, 7
// concurrent ping+TCP probes bounded by ProbeTimeout+1s each (~3s total);
// Fans and AC each cost an HTTP round trip to a Shelly plug bounded by a 5s
// timeout. Every Available predicate and every handler that reads any of
// them lives in routes whose SkipsProbe flag is false -- see that field's
// doc comment in contract. Paying for all three on every request used to mean
// /job, polled every 10s through a multi-minute shutdown, waited out a full
// fleet probe and plug reads for data it discards.
func (s *Server) snapshot(ctx context.Context, req contract.Request) contract.State {
	st := contract.State{Job: s.jobs.Read()}

	if !skipsProbe(s.router.routes, req.Path) {
		st.Hosts = s.probeHostsFn()(ctx)
		st.Fans, st.FansErr = s.fansStatusFn()(ctx)
		st.AC, st.ACErr = s.acStatusFn()(ctx)
	}
	return st
}

// skipsProbe reports whether the route serving path never reads
// State.Hosts or State.Fans -- in its Handle, or in any Available/Fields
// predicate the router evaluates while rendering it -- so snapshot() can
// skip the fleet probe and the Shelly read for it entirely.
//
// The route table is read from the router's table, built once from the two
// domain surfaces plus the composition root's own resources -- so the
// exemption set is the same inventory the engine acts on, not the
// compiled-in inventory.Default(); see buildRoutes' doc comment in registry.go.
//
// Looked up by path alone, ignoring method, the same as Router.PathExists:
// every route in the registry has a unique Path (TestRoutesAreUnique), so
// this never has to disambiguate two routes sharing one. A path matching no
// route returns false -- the safe default of "run the probe" -- but that
// case cannot actually reach here: serve() only calls snapshot() after
// Router.Lookup has already found a route for this exact path.
//
// A resource route's own SkipsProbe therefore has to account for every
// action its handler renders, not just its own Handle: the /monitoring
// route being SkipsProbe:true is only correct because its mute/unmute
// actions are SkipsProbe:true as well -- a fact
// TestSkipsProbeRoutesDontDependOnHostsOrFans checks directly, since which
// actions a resource's handler chooses to render is still a human's call to
// get right when wiring up that handler, not something this function can
// derive. Router.Actions/ActionsFor, called from the status and root
// resources, by contrast, evaluate every action route's Available predicate
// including ones that do read Hosts/Fans -- but neither of those resources is
// SkipsProbe, so that is never an issue for them.
func skipsProbe(rs []contract.Route, path string) bool {
	for _, r := range rs {
		if r.Path == path {
			return r.SkipsProbe
		}
	}
	return false
}

// probeHostsFn returns the fleet probe, falling back to the engine's real
// one. Same nil-safety pattern as the power surface's confirmRack.
func (s *Server) probeHostsFn() func(context.Context) []power.HostStatus {
	if s.probeHosts != nil {
		return s.probeHosts
	}
	return s.engine.ProbeAll
}

// fansStatusFn returns the rack-fan Shelly plug read, falling back to the
// engine's real one. Same nil-safety pattern as the power surface's confirmRack.
func (s *Server) fansStatusFn() func(context.Context) (power.FansState, error) {
	if s.fansStatus != nil {
		return s.fansStatus
	}
	return s.engine.FansStatus
}

// acStatusFn returns the f-host AC Shelly plug read, falling back to the
// engine's real one.
func (s *Server) acStatusFn() func(context.Context) (power.ACState, error) {
	if s.acStatus != nil {
		return s.acStatus
	}
	return s.engine.ACStatus
}

// monitorStatusFn returns the gateway mute read, falling back to the
// engine's real one. Same nil-safety pattern as the power surface's
// confirmRack.
func (s *Server) monitorStatusFn() func(context.Context) []power.GatewayMute {
	if s.monitorStatus != nil {
		return s.monitorStatus
	}
	return s.engine.MonitoringStatus
}

// enrichState adds the request-scoped facts r declares it reads (see
// contract.Route.Needs) that cost more than the local probes in snapshot() to
// gather -- the peer node's job state, the Gogios mute and the alert report
// -- so serve() pays for each only when the route serving this request
// actually reads it. It runs before the availability check in serve(),
// because actions like monitoring-mute/unmute and every power action are
// judged against exactly this state.
//
// The route is the matched one (method and path), not a path prefix: what to
// fetch is part of each route's own declaration, so a new route cannot
// silently inherit, or miss, a fetch meant for its neighbours.
func (s *Server) enrichState(ctx context.Context, state contract.State, r contract.Route, req contract.Request) contract.State {
	if r.Needs.Has(contract.NeedPeerBusy) {
		// An unreachable peer counts as idle, for the same reason PeerSet.Busy
		// gives: if one node is down the other must still be able to power the
		// cluster on.
		state.PeerBusy, _ = s.peers.Busy(ctx, s.node, req.APIKey)
	}
	if r.Needs.Has(contract.NeedMonitoring) {
		state.Monitoring = s.monitorStatusFn()(ctx)
	}
	if r.Needs.Has(contract.NeedReport) {
		state.Gogios, state.GogiosErr = gogios.Fetch(ctx, s.cfg)
	}
	return state
}
