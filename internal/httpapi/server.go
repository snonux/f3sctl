package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/power"
)

// Server answers one CGI request.
type Server struct {
	cfg    config.Config
	engine *power.Engine
	jobs   jobStore
	// base is the URL prefix every href is built from -- bozohttpd's
	// SCRIPT_NAME, e.g. "/cgi-bin/f3sctl". Hrefs are absolute paths so a
	// client never has to know how the API is mounted.
	base string
	node string
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
	req, err := parseCGIRequest(os.Stdin)
	if err != nil {
		return writeError(out, http.StatusBadRequest, err.Error())
	}

	srv, err := newServer(cfg)
	if err != nil {
		// A misconfigured server (unreadable SSH key, say) is a server fault,
		// not the client's. Report it as one so a client does not retry.
		return writeError(out, http.StatusInternalServerError, err.Error())
	}

	return srv.serve(out, req)
}

func newServer(cfg config.Config) (*Server, error) {
	eng, err := power.New(cfg)
	if err != nil {
		return nil, err
	}
	node, _ := os.Hostname()
	return &Server{
		cfg:    cfg,
		engine: eng,
		jobs:   jobStore{dir: cfg.StateDir},
		base:   strings.TrimSuffix(os.Getenv("SCRIPT_NAME"), "/"),
		node:   node,
	}, nil
}

func (s *Server) serve(out io.Writer, req request) error {
	if err := s.authenticate(req); err != nil {
		// Deliberately identical for a missing and a wrong key: telling an
		// attacker which of the two they got is free information.
		return writeError(out, http.StatusUnauthorized, "unauthorized")
	}

	r, ok := lookup(req.Method, req.Path)
	if !ok {
		if pathExists(req.Path) {
			return writeError(out, http.StatusMethodNotAllowed,
				fmt.Sprintf("%s is not allowed on %s", req.Method, req.Path))
		}
		return writeError(out, http.StatusNotFound, "no such resource: "+req.Path)
	}

	ctx := context.Background()
	state := s.snapshot(ctx)

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
	// An unreachable peer counts as idle, for the same reason peerJobRunning
	// gives: if one node is down the other must still be able to power the
	// cluster on.
	if req.Path != openAPIPath && req.Path != jobPath {
		state.PeerBusy, _ = s.peerJobRunning(req.APIKey)
	}

	// The Gogios mute lives on the two OpenBSD gateways and costs an SSH round
	// trip each to read, so it is fetched only for the routes that actually
	// render or change it. Every other response would pay ~2s for a value it
	// never shows. This runs before the availability check below because
	// monitoring-mute/unmute are judged against exactly this state.
	if strings.HasPrefix(req.Path, "/monitoring") {
		state.Monitoring = s.engine.MonitoringStatus(ctx)
	}

	// An action that is not currently available is refused here, before any
	// handler runs. A well-written client never reaches this: it was not
	// offered the action in the first place. This is the backstop for a
	// client racing another, or one that ignored the contract.
	if r.Action && !r.available(state) {
		return writeError(out, http.StatusConflict,
			fmt.Sprintf("%q is not available right now; re-fetch the resource and read its actions", r.Name))
	}

	entity, status, err := r.Handle(s, state, req)
	if err != nil {
		return writeError(out, status, err.Error())
	}

	// The OpenAPI document is served as itself rather than wrapped in Siren:
	// a tool that reads OpenAPI expects the document at the top level.
	if req.Path == openAPIPath {
		return writeJSON(out, status, entity.Properties)
	}
	return writeEntity(out, status, entity)
}

// authenticate checks the X-API-Key header against the configured key.
//
// The key is never accepted in the query string: bozohttpd logs request URIs
// to syslog and relayd logs connections, so a key in a URL would end up in two
// logs on three machines.
func (s *Server) authenticate(req request) error {
	want, err := os.ReadFile(s.cfg.APIKeyFile)
	if err != nil {
		return fmt.Errorf("reading the API key file: %w", err)
	}

	wantTrimmed := strings.TrimSpace(string(want))
	if wantTrimmed == "" {
		return fmt.Errorf("the API key file is empty")
	}

	// Constant-time so the comparison cannot be turned into an oracle for
	// guessing the key one byte at a time.
	if subtle.ConstantTimeCompare([]byte(req.APIKey), []byte(wantTrimmed)) != 1 {
		return fmt.Errorf("bad API key")
	}
	return nil
}

// snapshot probes everything the availability predicates need, once.
func (s *Server) snapshot(ctx context.Context) State {
	st := State{
		Hosts: s.engine.ProbeAll(ctx),
		Job:   s.jobs.read(),
	}
	st.Fans, st.FansErr = s.engine.FansStatus(ctx)
	return st
}

// href builds an absolute path for a route.
func (s *Server) href(path string) string {
	if path == "/" {
		return s.base + "/"
	}
	return s.base + path
}

// writeEntity emits a Siren entity as a CGI response.
func writeEntity(out io.Writer, status int, e Entity) error {
	body, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return writeResponse(out, status, sirenMediaType, append(body, '\n'))
}

// writeJSON emits a plain JSON document, for the one route that is not Siren.
func writeJSON(out io.Writer, status int, doc any) error {
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeResponse(out, status, "application/json", append(body, '\n'))
}

// writeError emits a problem response. It carries the same shape as any other
// entity so a client needs only one parser.
func writeError(out io.Writer, status int, msg string) error {
	e := Entity{
		Class:      []string{"error"},
		Properties: map[string]any{"status": status, "message": msg},
	}
	body, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return writeResponse(out, status, sirenMediaType, append(body, '\n'))
}

func writeResponse(out io.Writer, status int, contentType string, body []byte) error {
	// X-F3sctl-Node names the node that served this response, on EVERY reply
	// including errors. relayd load-balances pi0 and pi1, so "which node
	// answered" explains a great deal -- a job the other node knows nothing
	// about, most of all -- and it cannot be read off the URL. Putting it in a
	// header rather than only in the entity means it is present even when the
	// body is an error, which has no node property to carry it.
	node, _ := os.Hostname()

	// CGI headers use CRLF and a blank line before the body. bozohttpd turns
	// the Status header into the HTTP status line.
	_, err := fmt.Fprintf(out,
		"Status: %d %s\r\nContent-Type: %s\r\nCache-Control: no-store\r\nX-F3sctl-Node: %s\r\n\r\n%s",
		status, http.StatusText(status), contentType, node, body)
	return err
}
