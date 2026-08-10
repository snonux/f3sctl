package power

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/snonux/f3sctl/internal/util"
)

// FansState is the rack-fan plug's reported state.
type FansState struct {
	On bool   `json:"on"`
	IP string `json:"ip"`
}

// shellyStatus is the subset of Switch.GetStatus we care about.
type shellyStatus struct {
	Output bool `json:"output"`
}

// FansStatus reads the rack-fan Shelly plug's current state.
func (e *Engine) FansStatus(ctx context.Context) (FansState, error) {
	st, err := e.shellyGetStatus(ctx)
	if err != nil {
		return FansState{IP: e.cfg.Inventory.ShellyIP}, err
	}
	return FansState{On: st.Output, IP: e.cfg.Inventory.ShellyIP}, nil
}

// FansSet switches the rack-fan plug and returns its state read back from the
// device.
//
// The read-back is not belt-and-braces: Shelly's RPC reports some failures in
// a 200 response body, and a digest auth failure returns a 401 with a body
// too. Trusting the status code alone would report success while the plug sat
// untouched — which, on the "off" path, means the fans keep running after a
// shutdown, and on the "on" path means the rack heats up under load.
// The read-back is retried rather than taken once, because the relay does not
// flip instantly: Switch.Set returns as soon as the command is accepted, and a
// GetStatus issued immediately after can still report the previous state. A
// single eager read turned a perfectly good switch-on into a failed job on
// 2026-08-09 -- "asked for on=true, it reports on=false" -- while the plug was
// in fact on, and the whole rack was left powered down as a result.
//
// This does not weaken the check: a plug that genuinely never changes still
// fails, just after a bounded wait instead of instantly.
const (
	fansSettleAttempts = 6
	fansSettleInterval = 500 * time.Millisecond
)

func (e *Engine) FansSet(ctx context.Context, on bool) (FansState, error) {
	q := fmt.Sprintf("/rpc/Switch.Set?id=0&on=%t", on)
	if _, err := e.shellyRPC(ctx, q); err != nil {
		return FansState{IP: e.cfg.Inventory.ShellyIP}, err
	}

	var st shellyStatus
	var err error
	for attempt := 0; attempt < fansSettleAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return FansState{IP: e.cfg.Inventory.ShellyIP}, ctx.Err()
			case <-time.After(fansSettleInterval):
			}
		}
		st, err = e.shellyGetStatus(ctx)
		if err != nil {
			return FansState{IP: e.cfg.Inventory.ShellyIP}, err
		}
		if st.Output == on {
			break
		}
	}
	if st.Output != on {
		return FansState{On: st.Output, IP: e.cfg.Inventory.ShellyIP},
			fmt.Errorf("shelly plug did not change state: asked for on=%t, "+
				"it still reports on=%t after %s", on, st.Output,
				time.Duration(fansSettleAttempts-1)*fansSettleInterval)
	}
	return FansState{On: st.Output, IP: e.cfg.Inventory.ShellyIP}, nil
}

func (e *Engine) shellyGetStatus(ctx context.Context) (shellyStatus, error) {
	var st shellyStatus
	body, err := e.shellyRPC(ctx, "/rpc/Switch.GetStatus?id=0")
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return st, fmt.Errorf("parsing Shelly status: %w", err)
	}
	return st, nil
}

// shellyRPC performs one authenticated Shelly RPC call.
//
// The plug uses HTTP digest auth (user "admin"), which net/http does not
// implement, so the challenge-response is done by hand below.
func (e *Engine) shellyRPC(ctx context.Context, path string) ([]byte, error) {
	password, err := e.cfg.ResolveShellyPassword()
	if err != nil {
		return nil, fmt.Errorf("cannot control the rack fans: %w", err)
	}

	url := "http://" + e.cfg.Inventory.ShellyIP + path
	client := &http.Client{Timeout: 5 * time.Second}

	// First request draws the digest challenge.
	resp, err := e.shellyDo(ctx, client, url, "")
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

	resp, err = e.shellyDo(ctx, client, url, auth)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return readShellyBody(resp)
}

func (e *Engine) shellyDo(ctx context.Context, c *http.Client, url, auth string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching the Shelly plug at %s: %w", e.cfg.Inventory.ShellyIP, err)
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

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating cnonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}
