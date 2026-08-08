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
package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one f3sctl API endpoint.
type Client struct {
	base   string
	key    string
	http   *http.Client
	stdout io.Writer
}

// New returns a client for the API at base, authenticating with key.
func New(base, key string, stdout io.Writer) (*Client, error) {
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
		// Generous but bounded: the API answers a status request by probing
		// seven hosts, and a phone-tethered link is not fast.
		http:   &http.Client{Timeout: 30 * time.Second},
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
	Name   string  `json:"name"`
	Title  string  `json:"title"`
	Method string  `json:"method"`
	Href   string  `json:"href"`
	Fields []Field `json:"fields"`
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
func (c *Client) do(href, method string, form url.Values) (Entity, error) {
	var e Entity

	abs, err := c.resolve(href)
	if err != nil {
		return e, err
	}

	var body io.Reader
	if len(form) > 0 {
		body = bytes.NewBufferString(form.Encode())
	}

	req, err := http.NewRequest(method, abs, body)
	if err != nil {
		return e, err
	}
	req.Header.Set("X-API-Key", c.key)
	if len(form) > 0 {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Distinguish "could not ask" from "the answer was no". A failure
		// here says nothing about the state of the homelab.
		return e, fmt.Errorf("cannot reach the f3sctl API at %s: %w", c.base, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return e, fmt.Errorf("server returned something that is not an entity (HTTP %d)", resp.StatusCode)
	}

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

// Root fetches the entry point and checks the API version.
func (c *Client) Root() (Entity, error) {
	e, err := c.do("", http.MethodGet, nil)
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
func (c *Client) Follow(from Entity, rel string) (Entity, error) {
	href, ok := from.Link(rel)
	if !ok {
		return Entity{}, fmt.Errorf("the API offers no %q resource", rel)
	}
	return c.do(href, http.MethodGet, nil)
}

// Perform invokes an action, filling any fields it declares.
//
// Nothing here knows what a particular field means: a required checkbox is
// satisfied by confirm, and its own title is what gets shown when it is not.
func (c *Client) Perform(a Action, confirm bool) (Entity, error) {
	form := url.Values{}
	for _, f := range a.Fields {
		if f.Type != "checkbox" {
			continue
		}
		if f.Required && !confirm {
			return Entity{}, fmt.Errorf("%s needs confirmation: %s (pass --force)", a.Name, f.Title)
		}
		form.Set(f.Name, fmt.Sprintf("%t", confirm))
	}
	return c.do(a.Href, a.Method, form)
}
