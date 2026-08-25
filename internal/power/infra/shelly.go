package infra

// This file holds the rack-fan Shelly plug's HTTP RPC mechanism -- the raw
// http.Client, the hand-rolled HTTP digest auth, and the Switch.GetStatus /
// Switch.Set calls -- pulled off internal/power's Engine so that changing how
// the plug is reached (a different firmware, a different auth scheme, a
// second fan switch) is an edit here, not an edit to the Engine that also
// carries the shutdown and fan-guard policy.
//
// Nothing in this file is policy: there is no retry, no settle loop, no
// "switch the fans off only if the rack is idle". The settle read-back that
// Shelly's slow relay forces lives in the FansBackend adapter that holds one
// of these clients (internal/power/backends_exec.go's execFans), which is the
// layer that turns a bare RPC into the read-back-confirmed state the policy
// methods actually want. infra has no opinion on what a caller does with the
// answer; internal/power is this package's only importer.

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/snonux/f3sctl/internal/util"
)

// ShellyClient talks to one rack-fan Shelly plug over its authenticated HTTP
// RPC. It holds only the mechanism: the plug's address, the password it
// resolves lazily (digest auth is mandatory, so the password is not read
// until the first call that needs it -- status probing on a machine with no
// f3sctl key file still works for everything but the fans), and the
// per-request timeout. Status and Set are the two calls the fan policy
// makes; RPC is the lower-level seam they are built on and the one a test or
// a second delivery channel would reach for to run an RPC this client does
// not name.
type ShellyClient struct {
	// IP is the address the URL is built from, as "host:port" -- the engine
	// builds its URL as "http://" + IP + path, so a port belongs here when
	// the plug is not on 80. Held by value rather than read back from a
	// config on every call so the client is self-contained: the adapter
	// that holds it does not also need to hold the config.
	IP string
	// Password resolves the plug's admin password on demand. A function
	// rather than a string so the file is read at most once per call and
	// not at all until a call needs it -- mirroring config.ResolveShellyPassword,
	// which is what production wires here.
	Password func() (string, error)
	// Timeout bounds a single HTTP round trip to the plug. A field rather
	// than a hard-coded literal so a test or a slow plug can widen it
	// without touching the policy layer.
	Timeout time.Duration
}

// shellyStatus is the subset of Switch.GetStatus the fan policy reads.
type shellyStatus struct {
	Output bool `json:"output"`
}

// Status reads the plug's current switch state.
func (c *ShellyClient) Status(ctx context.Context) (bool, error) {
	body, err := c.RPC(ctx, "/rpc/Switch.GetStatus?id=0")
	if err != nil {
		return false, err
	}
	var st shellyStatus
	if err := json.Unmarshal(body, &st); err != nil {
		return false, fmt.Errorf("parsing Shelly status: %w", err)
	}
	return st.Output, nil
}

// Set sends the Switch.Set RPC for switch id=0. It returns as soon as the
// plug accepts the command; it does NOT wait for the relay to flip, which is
// why the adapter above this calls Status afterwards (see execFans.Set's
// settle loop in internal/power/backends_exec.go).
func (c *ShellyClient) Set(ctx context.Context, on bool) error {
	_, err := c.RPC(ctx, fmt.Sprintf("/rpc/Switch.Set?id=0&on=%t", on))
	return err
}

// RPC performs one authenticated Shelly RPC call.
//
// The plug uses HTTP digest auth (user "admin"), which net/http does not
// implement, so the challenge-response is done by hand below.
func (c *ShellyClient) RPC(ctx context.Context, path string) ([]byte, error) {
	password, err := c.Password()
	if err != nil {
		return nil, fmt.Errorf("cannot control the rack fans: %w", err)
	}

	url := "http://" + c.IP + path
	client := &http.Client{Timeout: c.Timeout}

	// First request draws the digest challenge.
	resp, err := c.do(ctx, client, url, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		defer resp.Body.Close()
		return readShellyBody(resp)
	}

	challenge := resp.Header.Get("WWW-Authenticate")
	resp.Body.Close()

	auth, err := digestAuth(challenge, "admin", password, "GET", path)
	if err != nil {
		return nil, err
	}

	resp, err = c.do(ctx, client, url, auth)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return readShellyBody(resp)
}

func (c *ShellyClient) do(ctx context.Context, cl *http.Client, url, auth string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching the Shelly plug at %s: %w", c.IP, err)
	}
	return resp, nil
}

func readShellyBody(resp *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, fmt.Errorf("reading Shelly response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("shelly plug returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// digestAuth builds an RFC 2617 digest Authorization header for the given
// challenge. The Shelly firmware uses qop="auth" with SHA-256 or MD5; both are
// handled here.
func digestAuth(challenge, user, password, method, uri string) (string, error) {
	if !strings.HasPrefix(strings.ToLower(challenge), "digest ") {
		return "", fmt.Errorf("shelly plug did not offer digest auth (got %q)", challenge)
	}
	p := parseChallenge(challenge[len("Digest "):])

	realm, nonce := p["realm"], p["nonce"]
	if realm == "" || nonce == "" {
		return "", fmt.Errorf("shelly digest challenge missing realm or nonce")
	}

	hash := pickHash(p["algorithm"])
	cnonce, err := randomHex(8)
	if err != nil {
		return "", err
	}
	const nc = "00000001"

	ha1 := hash(user + ":" + realm + ":" + password)
	ha2 := hash(method + ":" + uri)
	response := hash(strings.Join([]string{ha1, nonce, nc, cnonce, "auth", ha2}, ":"))

	return fmt.Sprintf(
		`Digest username="%s", realm="%s", nonce="%s", uri="%s", algorithm=%s, qop=auth, nc=%s, cnonce="%s", response="%s"`,
		user, realm, nonce, uri, util.OrDefault(p["algorithm"], "MD5"), nc, cnonce, response), nil
}

// pickHash returns the digest function named by the challenge's algorithm
// parameter, defaulting to MD5 as RFC 2617 requires.
func pickHash(algorithm string) func(string) string {
	if strings.HasPrefix(strings.ToUpper(algorithm), "SHA-256") {
		return sha256Hex
	}
	return md5Hex
}

func parseChallenge(s string) map[string]string {
	out := map[string]string{}
	for _, part := range splitChallenge(s) {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return out
}

// splitChallenge splits on commas that are not inside a quoted string, since
// a realm or nonce may legitimately contain one.
func splitChallenge(s string) []string {
	var parts []string
	var cur strings.Builder
	inQuotes := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			cur.WriteRune(r)
		case r == ',' && !inQuotes:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	return parts
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// sha256Hex is the SHA-256 variant of the digest hash function, used when the
// Shelly plug's challenge advertises algorithm=SHA-256.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating cnonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}
