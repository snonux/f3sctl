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
		peers:   coordination.NewPeerSet(cfg.PeerNodes, cfg.PeerJobPath),
		auth:    NewAuthenticator(cfg.APIKeyFile),
		router:  router,
		openapi: NewOpenAPIBuilder(router),
		siren:   NewSirenRenderer(),
		node:    node,
	}, nil
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
	state := s.enrichState(ctx, s.snapshot(ctx), req)

	// An action that is not currently available is refused here, before any
	// handler runs. A well-written client never reaches this: it was not
	// offered the action in the first place. This is the backstop for a
	// client racing another, or one that ignored the contract.
	if r.Action && !r.available(state) {
		return s.siren.WriteError(out, http.StatusConflict,
			fmt.Sprintf("%q is not available right now; re-fetch the resource and read its actions", r.Name))
	}

	entity, status, err := r.Handle(s, state, req)
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

// snapshot probes everything the availability predicates need, once.
func (s *Server) snapshot(ctx context.Context) State {
	st := State{
		Hosts: s.engine.ProbeAll(ctx),
		Job:   s.jobs.Read(),
	}
	st.Fans, st.FansErr = s.engine.FansStatus(ctx)
	return st
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
