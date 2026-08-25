// Package client drives the f3sctl HTTP API as a hypermedia client.
//
// It is what `f3sctl --remote ...` uses, so this laptop can control the
// homelab from anywhere rather than only from the LAN. That matters for more
// than convenience: Wake-on-LAN magic packets are not routed, so a machine off
// the 192.168.1.0/24 broadcast domain physically cannot wake an f-host. Asking
// pi0/pi1 to send the packet is the only way to do it remotely.
//
// This package follows exactly the contract documented in docs/CLIENT.md and
// implemented by docs/client-reference.js: it knows the base URL and the API
// key, and discovers everything else. There are no literal API paths in this
// file, which is the property that lets the server move things around.
//
// One opportunistic exception: Entity.ActionForVerb also matches on
// Action.CLIVerb, an additive Siren field that docs/CLIENT.md does not
// document (the contract still only promises rel-based links and name-based
// actions). It is used only as a preferred shortcut when a server advertises
// it, with a fallback to the documented name-based lookup for one that
// doesn't -- see ActionForVerb and sy0.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/snonux/f3sctl/internal/config"
)

// Client talks to one f3sctl API endpoint.
type Client struct {
	base   string
	key    string
	http   *http.Client
	stdout io.Writer

	// cfg supplies settings the client and server must not disagree about --
	// currently just UnmuteTimeout, which jobWaitTimeout uses to derive how
	// long waitForJob polls before giving up. Carrying the whole config
	// rather than a single duration means a future shared setting has
	// somewhere to live without another constructor-signature change.
	cfg config.Config

	// trace, when set, receives a line per request showing what was called,
	// where, and which node answered. Nil disables tracing.
	trace io.Writer
}

// Verbose turns on request tracing, writing to w.
//
// It goes to stderr rather than stdout so the trace never contaminates output
// something else might parse. Worth having because this API is discovered
// rather than hard-coded: without a trace there is no way to see which URLs a
// run actually followed, and -- given relayd load-balances pi0 and pi1 --
// which node served each one.
func (c *Client) Verbose(w io.Writer) *Client {
	c.trace = w
	return c
}

// New returns a client for the API at base, authenticating with key.
//
// cfg is the same config.Config the caller resolved for everything else --
// passing it through (rather than just base and key) is what lets
// jobWaitTimeout derive the poll deadline from cfg.UnmuteTimeout instead of
// carrying an independent guess of the server's worst-case job runtime.
func New(base, key string, cfg config.Config, stdout io.Writer) (*Client, error) {
	if base == "" {
		return nil, fmt.Errorf("no API URL configured (set api_url in the config or F3SCTL_URL)")
	}
	if key == "" {
		return nil, fmt.Errorf("no API key available (set api_key_file in the config or F3SCTL_KEY)")
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return &Client{
		base: base,
		key:  key,
		cfg:  cfg,
		// Generous but bounded: the API answers a status request by probing
		// seven hosts, and a phone-tethered link is not fast.
		//
		// A minute rather than the obvious thirty seconds because of one route:
		// POST /fans/off confirms the rack is idle before it cuts the cooling,
		// and confirming a silence takes several probes spaced ten seconds
		// apart (power.confirmLiveness). Timing that request out client-side
		// would leave the operator with no idea whether the plug was switched --
		// the one outcome worth being sure about here.
		http:   &http.Client{Timeout: 60 * time.Second},
		stdout: stdout,
	}, nil
}

// Entity is the subset of Siren this client reads. It mirrors the server's
// representation without importing it: a client must not depend on the
// server's internals, only on the documented wire format.
type Entity struct {
	Class      []string       `json:"class"`
	Properties map[string]any `json:"properties"`
	Entities   []Entity       `json:"entities"`
	Links      []Link         `json:"links"`
	Actions    []Action       `json:"actions"`
}

type Link struct {
	Rel  []string `json:"rel"`
	Href string   `json:"href"`
}

type Action struct {
	Name   string `json:"name"`
	Title  string `json:"title"`
	Method string `json:"method"`
	Href   string `json:"href"`
	// CLIVerb is the exact f3sctl command that invokes this action, e.g.
	// "power f1 on", declared once by the server's route registry
	// (internal/httpapi/registry.go's route.CLIVerb) and carried here
	// unchanged. Entity.ActionForVerb matches against it, which is what lets
	// this client recognise a command without its own verb->name table --
	// see that method's doc comment and sy0's annotation.
	CLIVerb string  `json:"cliVerb,omitempty"`
	Fields  []Field `json:"fields"`
}

type Field struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	Required bool   `json:"required"`
}

// Link returns the href for a relation, resolved against the base URL.
func (e Entity) Link(rel string) (string, bool) {
	for _, l := range e.Links {
		for _, r := range l.Rel {
			if r == rel {
				return l.Href, true
			}
		}
	}
	return "", false
}

// Action returns an offered action by name.
//
// Absent means "not possible right now" -- not an error to work around. The
// server withholds actions precisely so a client does not have to know the
// rules.
func (e Entity) Action(name string) (Action, bool) {
	for _, a := range e.Actions {
		if a.Name == name {
			return a, true
		}
	}
	return Action{}, false
}

// ActionForVerb returns the offered action whose declared CLIVerb equals
// verb -- the CLI words a command was typed with, e.g. "power f3 on".
//
// Primary path: matches against Action.CLIVerb, the server's single source
// for the verb-to-action-name mapping (route.CLIVerb in
// internal/httpapi/registry.go). Matching against what the server just
// advertised replaces a client-local table that had to be kept in step by
// hand -- see sy0.
//
// Fallback path: CLIVerb is an additive Siren field (json:"cliVerb,omitempty"),
// so a server built before sy0 (09a5262-6dea943) never sends it, and it
// decodes as "" on every action -- the primary match can then never succeed,
// which used to make every remote command report "not available right now"
// against an un-upgraded server. That is not a rare edge case here: this
// project has no unified deploy step (mage cross/publish only pushes
// packages; upgrading the CGI on pi0/pi1 is a separate manual step), so a
// freshly built client talking to an older server is routine. When no action's
// CLIVerb matches, legacyActionName recovers the pre-sy0 derivation (the
// actionFor/actionName logic this file used to carry) and looks the result up
// by Action.Name -- the one lookup docs/CLIENT.md actually documents.
//
// Absent (both paths exhausted) still means what it always meant for Action:
// either verb names nothing the server knows, or it names something currently
// withheld -- the two are indistinguishable here on purpose, since only
// possible actions are ever advertised (see httpapi.Router.Actions).
func (e Entity) ActionForVerb(verb string) (Action, bool) {
	for _, a := range e.Actions {
		if a.CLIVerb != "" && a.CLIVerb == verb {
			return a, true
		}
	}
	if name, ok := legacyActionName(verb); ok {
		return e.Action(name)
	}
	return Action{}, false
}

// legacyActionFor maps a CLI verb onto the action name a pre-sy0 server
// advertised it under. It exists only as ActionForVerb's compatibility
// fallback: a server that declares CLIVerb makes this table redundant for
// that route, so a newly added route needs a CLIVerb on it
// (internal/httpapi/registry.go), not a new entry here.
var legacyActionFor = map[string]string{
	"power on":          "power-on",
	"power off":         "power-off",
	"fans on":           "fans-on",
	"fans off":          "fans-off",
	"monitoring mute":   "monitoring-mute",
	"monitoring unmute": "monitoring-unmute",
}

// legacyActionName reconstructs the action name a command used to resolve to
// before sy0 introduced Action.CLIVerb, for ActionForVerb's fallback path.
// Per-host commands ("power f1 off") are derived rather than listed, so the
// fallback keeps working for hosts added to the inventory after this code was
// last touched.
func legacyActionName(cmd string) (string, bool) {
	if name, ok := legacyActionFor[cmd]; ok {
		return name, true
	}

	fields := strings.Fields(cmd)
	if len(fields) == 3 && fields[0] == "power" && (fields[2] == "on" || fields[2] == "off") {
		return fields[1] + "-" + fields[2], true
	}
	return "", false
}

// resolve turns a root-relative href from a response into an absolute URL.
//
// The server returns paths, not full URLs, because behind relayd it cannot
// know the scheme and hostname the client used. Resolving them is the client's
// job; the path still came from the server.
func (c *Client) resolve(href string) (string, error) {
	baseURL, err := url.Parse(c.base)
	if err != nil {
		return "", fmt.Errorf("invalid API URL %q: %w", c.base, err)
	}
	ref, err := url.Parse(href)
	if err != nil {
		return "", fmt.Errorf("server returned an unparseable href %q: %w", href, err)
	}
	return baseURL.ResolveReference(ref).String(), nil
}

// do performs one request and decodes the entity.
//
// ctx bounds the round trip: it is plumbed into http.NewRequestWithContext so a
// cancelled caller -- the CGI request that went home, or a `--remote` job
// poll interrupted with Ctrl-C (see waitForJob) -- stops the in-flight HTTP
// call rather than running the client's own 60s timeout out first. The
// client's per-request Timeout still applies as a backstop for a caller that
// hands over an unbounded context.
func (c *Client) do(ctx context.Context, href, method string, form url.Values) (Entity, error) {
	var e Entity

	abs, err := c.resolve(href)
	if err != nil {
		return e, err
	}

	var body io.Reader
	if len(form) > 0 {
		body = bytes.NewBufferString(form.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, method, abs, body)
	if err != nil {
		return e, err
	}
	req.Header.Set("X-API-Key", c.key)
	if len(form) > 0 {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	started := time.Now()
	c.tracef("→ %s %s%s", method, abs, traceFields(form))

	resp, err := c.http.Do(req)
	if err != nil {
		// Distinguish "could not ask" from "the answer was no". A failure
		// here says nothing about the state of the homelab.
		c.tracef("✗ %s (%s)", err, time.Since(started).Round(time.Millisecond))
		return e, fmt.Errorf("cannot reach the f3sctl API at %s: %w", c.base, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		c.tracef("← %d, unparseable body (%s)", resp.StatusCode, time.Since(started).Round(time.Millisecond))
		return e, fmt.Errorf("server returned something that is not an entity (HTTP %d)", resp.StatusCode)
	}

	// Report which node answered. relayd load-balances pi0 and pi1, so this is
	// not decoration: it is what explains a job the other node knows nothing
	// about, and it is invisible from the URL alone.
	c.traceResponse(resp.StatusCode, resp.Header.Get("X-F3sctl-Node"), e, time.Since(started))

	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted:
		return e, nil
	case http.StatusUnauthorized:
		return e, fmt.Errorf("the API rejected the key")
	default:
		msg, _ := e.Properties["message"].(string)
		if msg == "" {
			msg = resp.Status
		}
		return e, fmt.Errorf("%s", msg)
	}
}

// tracef writes one trace line, when tracing is on.
func (c *Client) tracef(format string, args ...any) {
	if c.trace == nil {
		return
	}
	fmt.Fprintf(c.trace, "  "+format+"\n", args...)
}

// traceResponse reports the status, the answering node and the timing.
//
// The node comes from the X-F3sctl-Node header, which every response carries,
// falling back to the entity property for a server too old to send it.
func (c *Client) traceResponse(status int, headerNode string, e Entity, took time.Duration) {
	if c.trace == nil {
		return
	}

	node := headerNode
	if node == "" {
		node, _ = e.Properties["node"].(string)
	}
	if node == "" {
		node = "?"
	}

	detail := ""
	if msg, ok := e.Properties["message"].(string); ok && msg != "" {
		detail = " — " + msg
	}
	c.tracef("← %d from %s (%s)%s", status, node, took.Round(time.Millisecond), detail)
}

// traceFields renders the form fields being sent, so a trace shows not just
// which action was invoked but with what.
func traceFields(form url.Values) string {
	if len(form) == 0 {
		return ""
	}
	return " [" + form.Encode() + "]"
}

// Root fetches the entry point and checks the API version.
func (c *Client) Root(ctx context.Context) (Entity, error) {
	e, err := c.do(ctx, "", http.MethodGet, nil)
	if err != nil {
		return e, err
	}

	// Refuse rather than guess: a version bump means something this client
	// relies on has changed shape.
	if v, ok := e.Properties["apiVersion"].(float64); ok && int(v) != SupportedAPIVersion {
		return e, fmt.Errorf("the API speaks version %d, this f3sctl understands %d; upgrade f3sctl",
			int(v), SupportedAPIVersion)
	}
	return e, nil
}

// SupportedAPIVersion is the one apiVersion this client is written against.
const SupportedAPIVersion = 1

// Follow fetches a linked resource by relation.
func (c *Client) Follow(ctx context.Context, from Entity, rel string) (Entity, error) {
	href, ok := from.Link(rel)
	if !ok {
		return Entity{}, fmt.Errorf("the API offers no %q resource", rel)
	}
	return c.do(ctx, href, http.MethodGet, nil)
}

// Perform invokes an action, filling any fields it declares.
//
// Nothing here knows what a particular field means: a required checkbox is
// satisfied by confirm, and its own title is what gets shown when it is not.
func (c *Client) Perform(ctx context.Context, a Action, confirm bool) (Entity, error) {
	form := url.Values{}
	sawForce := false
	for _, f := range a.Fields {
		if f.Type != "checkbox" {
			continue
		}
		if f.Name == "force" {
			sawForce = true
		}
		if f.Required && !confirm {
			return Entity{}, fmt.Errorf("%s needs confirmation: %s (pass --force)", a.Name, f.Title)
		}
		form.Set(f.Name, fmt.Sprintf("%t", confirm))
	}

	// gz0: fans-off's "force" checkbox is gated by the server's cheap,
	// single-probe snapshot (registry.go's rackBusy), while the plug switch
	// itself is guarded by a stricter multi-probe confirmation inside the
	// engine (handlers.go's rackStillBusy). When the snapshot reads the rack
	// as cold, the advertisement omits the field entirely -- so the loop
	// above never runs for it -- even though the confirming probe can still
	// find a host up and 409. A caller who explicitly passed --force meant it
	// regardless of which snapshot the advertisement happened to be built
	// from, so send it anyway: the server's own confirmation is the actual
	// safety gate, and an unsolicited-but-true force field cannot make an
	// action less safe, it can only save the round trip that a spurious 409
	// and a retry would otherwise force on the user.
	if confirm && !sawForce {
		form.Set("force", "true")
	}

	return c.do(ctx, a.Href, a.Method, form)
}
