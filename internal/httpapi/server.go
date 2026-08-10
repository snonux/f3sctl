package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/power"
)

// Server answers one CGI request.
//
// It owns no coordination logic of its own, and increasingly little else:
// whether a job may start, whether the peer node is busy, and the job's
// lifecycle all live in internal/coordination, injected here as jobs and
// peers; the API key check, route matching/href-building, response
// serialisation and the OpenAPI doc live in Authenticator, Router,
// SirenRenderer and OpenAPIBuilder, respectively. Server's own job is to
// compose these -- parse the request, ask engine/jobs/peers/auth/router what
// is true, hand the answer to siren to render.
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
	// the same route declarations router uses. See OpenAPIBuilder.
	openapi *OpenAPIBuilder
	// siren writes every response -- Siren entity, plain JSON, or error --
	// in the CGI wire format. See SirenRenderer.
	siren SirenRenderer
	node  string

	// rackConfirm re-probes what is running in the rack, with the consecutive
	// -silence evidence the fan guard demands before anything cuts cooling.
	// Nil means the engine's own probe; only tests substitute anything else.
	// See Server.confirmRack.
	rackConfirm func(context.Context) power.RackActivity

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
}

// request is the parsed CGI request.
type request struct {
	Method string
	Path   string
	Query  url.Values
	Form   url.Values
	APIKey string
}

// boolField reads a checkbox field from the query string or the form body.
func (r request) boolField(name string) bool {
	for _, v := range []string{r.Form.Get(name), r.Query.Get(name)} {
		switch strings.ToLower(v) {
		case "true", "1", "on", "yes":
			return true
		}
	}
	return false
}

// ServeCGI answers a single CGI request read from the process environment and
// stdin, writing the response to out.
func ServeCGI(cfg config.Config, out io.Writer) error {
	// SirenRenderer is stateless, so it is safe to use ahead of a Server --
	// the two error paths below can fire before one exists at all (a
	// malformed request, or a Server that failed to construct).
	siren := NewSirenRenderer()

	req, err := parseCGIRequest(os.Stdin)
	if err != nil {
		return siren.WriteError(out, http.StatusBadRequest, err.Error())
	}

	srv, err := newServer(cfg)
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
	router := NewRouter(strings.TrimSuffix(os.Getenv("SCRIPT_NAME"), "/"))
	return &Server{
		cfg:     cfg,
		engine:  eng,
		jobs:    coordination.NewManager(cfg.StateDir),
		peers:   coordination.NewPeerSet(cfg.PeerNodes, resolvePeerJobPath(cfg, router)),
		auth:    NewAuthenticator(cfg.APIKeyFile),
		router:  router,
		openapi: NewOpenAPIBuilder(router),
		siren:   NewSirenRenderer(),
		node:    node,
	}, nil
}

// defaultCGIMount is this project's own documented CGI mount convention (see
// README.md's example config, and config.Default() before uy0). It is the
// last-resort fallback resolvePeerJobPath uses when this node's own router
// has no base to derive anything from -- see that function's doc comment for
// why an empty base cannot be trusted as "mounted at the root".
const defaultCGIMount = "/cgi-bin/f3sctl"

// resolvePeerJobPath returns the URL path this node asks a peer for its
// current job.
//
// An explicit cfg.PeerJobPath always wins, for the rare case where the two
// peers are not mounted the same way. Otherwise (the default) it is derived
// from this node's own mount via router.Href -- the identical mechanism
// every link and action handed back to a client already goes through -- on
// the assumption that pi0 and pi1 are symmetric peers sharing one CGI mount.
// That keeps a SCRIPT_NAME remount a one-place change instead of two: without
// this, an operator who moves the mount point but forgets the separate
// peer_job_path config value gets a peer check that silently reads back as
// idle forever, which is the dangerous failure mode -- two jobs can start.
//
// The one case that derivation must NOT be trusted for: router.base itself
// being empty. That happens whenever this node's own SCRIPT_NAME was empty or
// missing when the Router was built (bozohttpd not setting it, a proxy that
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
func resolvePeerJobPath(cfg config.Config, router *Router) string {
	if cfg.PeerJobPath != "" {
		return cfg.PeerJobPath
	}
	if router.base == "" {
		return defaultCGIMount + jobPath
	}
	return router.Href(jobPath)
}

func (s *Server) serve(out io.Writer, req request) error {
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

	ctx := context.Background()
	state := s.enrichState(ctx, s.snapshot(ctx, req), req)

	// An action that is not currently available is refused here, before any
	// handler runs. A well-written client never reaches this: it was not
	// offered the action in the first place. This is the backstop for a
	// client racing another, or one that ignored the contract.
	if r.Action && !r.available(state) {
		return s.siren.WriteError(out, http.StatusConflict,
			fmt.Sprintf("%q is not available right now; re-fetch the resource and read its actions", r.Name))
	}

	entity, status, err := r.Handle(s, ctx, state, req)
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
// unconditionally. Hosts and Fans are not: Hosts costs Engine.ProbeAll, 7
// concurrent ping+TCP probes bounded by ProbeTimeout+1s each (~3s total);
// Fans costs Engine.FansStatus, an HTTP round trip to the Shelly plug bounded
// by a 5s timeout. Every Available predicate and every handler that reads
// either lives in routes whose Path does NOT satisfy skipsProbe -- see that
// function's doc for the routes excluded and why each one's handler and
// predicates provably never look at state.Hosts or state.Fans. Paying for
// both on every request used to mean /job, polled every 10s through a
// multi-minute shutdown, waited out a full fleet probe and a plug read for
// data it discards.
func (s *Server) snapshot(ctx context.Context, req request) State {
	st := State{Job: s.jobs.Read()}

	if !skipsProbe(req.Path) {
		st.Hosts = s.probeHostsFn()(ctx)
		st.Fans, st.FansErr = s.fansStatusFn()(ctx)
	}
	return st
}

// skipsProbe reports whether a route never reads State.Hosts or State.Fans,
// in its handler or in any Available/Fields predicate the router evaluates
// while rendering it -- so snapshot() can skip the fleet probe and the Shelly
// read for it entirely.
//
// The three cases, verified against the current handlers and registry.go
// rather than assumed:
//   - jobPath (handleJob) renders only state.Job.
//   - the "/monitoring" family (handleMonitoring, handleMute, handleUnmute,
//     and the monitoring-mute/monitoring-unmute Available predicates) reads
//     only state.Monitoring, populated separately by enrichState.
//   - openAPIPath (handleOpenAPI) ignores State completely: OpenAPIBuilder
//     renders every route's Fields against a synthetic widestState(), not the
//     request's own state (see openapi.go).
//
// Router.Actions/ActionsFor, called from handleRoot/handleStatus/handleFans/
// handleMonitoring, evaluate every action route's Available predicate before
// filtering by name -- including ones that do read Hosts/Fans -- but a
// skipped route's own handler never renders that unfiltered result, so a
// zero-value Hosts/Fans reaching those predicates only produces answers that
// get discarded, never ones a client sees.
func skipsProbe(path string) bool {
	return path == jobPath || path == openAPIPath || strings.HasPrefix(path, "/monitoring")
}

// probeHostsFn returns the fleet probe, falling back to the engine's real
// one. Same nil-safety pattern as Server.confirmRack.
func (s *Server) probeHostsFn() func(context.Context) []power.HostStatus {
	if s.probeHosts != nil {
		return s.probeHosts
	}
	return s.engine.ProbeAll
}

// fansStatusFn returns the Shelly plug read, falling back to the engine's
// real one. Same nil-safety pattern as Server.confirmRack.
func (s *Server) fansStatusFn() func(context.Context) (power.FansState, error) {
	if s.fansStatus != nil {
		return s.fansStatus
	}
	return s.engine.FansStatus
}

// enrichState adds the request-scoped facts that cost more than the local
// probes in snapshot() to gather -- the peer node's job state, and, only for
// /monitoring routes, the Gogios mute -- so serve() pays for them only when a
// route actually needs them.
func (s *Server) enrichState(ctx context.Context, state State, req request) State {
	// Ask the other node whether it is mid-job, so actions this node advertises
	// account for a job running over there.
	//
	// Two routes are excluded, and /job is excluded for a reason worth stating:
	// the peer check *is* a GET of the peer's /job. Letting /job trigger one
	// makes each node's answer depend on the other's, so a single question
	// bounces between them until the 3s client timeout fires and the peer is
	// misread as idle -- which is precisely how this arrived broken the first
	// time (pi0 answering in 5.3s and still offering power actions mid-job).
	// /job renders no actions, so it has no use for the answer anyway.
	// openapi.json describes the surface rather than the moment.
	//
	// An unreachable peer counts as idle, for the same reason PeerSet.Busy
	// gives: if one node is down the other must still be able to power the
	// cluster on.
	if req.Path != openAPIPath && req.Path != jobPath {
		state.PeerBusy, _ = s.peers.Busy(s.node, req.APIKey)
	}

	// The Gogios mute lives on the two OpenBSD gateways and costs an SSH round
	// trip each to read, so it is fetched only for the routes that actually
	// render or change it. Every other response would pay ~2s for a value it
	// never shows. This runs before the availability check in serve() because
	// monitoring-mute/unmute are judged against exactly this state.
	if strings.HasPrefix(req.Path, "/monitoring") {
		state.Monitoring = s.engine.MonitoringStatus(ctx)
	}

	return state
}
