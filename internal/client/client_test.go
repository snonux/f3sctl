package client

import (
	"io"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
)

// TestNewThreadsCfgThrough is the regression coverage for mz0: gy0 added a
// cfg parameter to New so jobWaitTimeout could derive its deadline from
// cfg.UnmuteTimeout, but every existing test built &Client{cfg: cfg}
// directly, bypassing New entirely -- a build that forgot "cfg: cfg" in the
// struct literal New returns would have left the whole suite green. This
// calls New itself and checks cfg actually reached the returned *Client, via
// the one behaviour that depends on it (jobWaitTimeout), rather than reaching
// into an unexported field.
func TestNewThreadsCfgThrough(t *testing.T) {
	cfg := config.Default()
	cfg.UnmuteTimeout = config.Duration(37 * time.Minute)

	c, err := New("http://example.invalid/", "some-key", cfg, io.Discard)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got := c.jobWaitTimeout()
	want := 37*time.Minute + jobWaitBuffer
	if got != want {
		t.Errorf("jobWaitTimeout() via New(cfg) = %s, want %s: cfg was not threaded through", got, want)
	}
}

// TestNewWithZeroValueConfigDoesNotSilentlyShortenTheDeadline is the specific
// case mz0's annotation calls out: a caller that passes a zero-value
// config.Config (cfg.UnmuteTimeout == 0, as opposed to config.Default()'s
// resolved default) must not silently produce a deadline that is only
// jobWaitBuffer long -- that would reintroduce, in a new disguise, the very
// "gave up moments before the job finished" bug jobWaitTimeout exists to
// prevent. This pins the current, honest behaviour: jobWaitTimeout uses
// exactly what cfg carries, so a caller that skips config.Default() gets a
// visibly too-short timeout (5m) rather than a comfortable one -- a caller
// wanting the safe default must go through config.Default(), not New.
func TestNewWithZeroValueConfigDoesNotSilentlyShortenTheDeadline(t *testing.T) {
	c, err := New("http://example.invalid/", "some-key", config.Config{}, io.Discard)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got := c.jobWaitTimeout()
	if got != jobWaitBuffer {
		t.Errorf("jobWaitTimeout() with a zero-value config.Config = %s, want exactly jobWaitBuffer (%s): "+
			"UnmuteTimeout.D() of a zero Duration must be 0, not a hidden default", got, jobWaitBuffer)
	}
}

// TestNewRequiresBaseAndKey pins New's own validation: an empty base or key
// must fail fast with a message naming what to configure, rather than
// producing a *Client that only fails much later, deep inside the first HTTP
// call, with a confusing "cannot reach the API" for what was actually a
// configuration problem.
func TestNewRequiresBaseAndKey(t *testing.T) {
	cfg := config.Default()

	if _, err := New("", "some-key", cfg, io.Discard); err == nil {
		t.Error("New with an empty base URL succeeded")
	}
	if _, err := New("http://example.invalid/", "", cfg, io.Discard); err == nil {
		t.Error("New with an empty API key succeeded")
	}
}

// TestNewAppendsATrailingSlashToTheBase pins the URL-joining contract
// Client.resolve relies on for the one relative-href case that actually
// occurs in production: Root's own call, c.do("", ...) (see Root, below in
// this file's package). Every other href a real server sends is
// host-absolute (a leading "/", built by httpapi.Router.Href), and for those
// url.ResolveReference drops the base's own path segment regardless of
// whether the base ends in "/" -- appending a trailing slash would not
// rescue that case (verify: ResolveReference("http://h/api", "/status") and
// ResolveReference("http://h/api/", "/status") both give "http://h/status").
// What the trailing slash actually buys is href=""'s round trip: resolving
// an empty reference against a base returns the base unchanged, so without
// normalising, a base of ".../api" and ".../api/" would resolve Root() to
// two different URLs depending only on whether the operator's configured
// api_url happened to end in a slash. New removes that dependency.
func TestNewAppendsATrailingSlashToTheBase(t *testing.T) {
	c, err := New("http://example.invalid/api", "some-key", config.Default(), io.Discard)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := c.resolve("") // the href Root() actually resolves
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := "http://example.invalid/api/"; got != want {
		t.Errorf("resolve(%q) = %q, want %q: New must append a trailing slash to a base without one",
			"", got, want)
	}
}

// TestActionForVerbPrimaryPathPrefersCLIVerb pins that, when both the
// CLIVerb match and the legacy name-based fallback would resolve a verb (the
// case every current server actually produces, since routes with a CLIVerb
// keep the historical action name too), ActionForVerb picks the CLIVerb
// match. This is the "single source of truth" property sy0 exists for: were
// the two ever to disagree, the registry-derived CLIVerb -- not the
// compatibility fallback -- must win.
func TestActionForVerbPrimaryPathPrefersCLIVerb(t *testing.T) {
	holder := Entity{Actions: []Action{
		// Deliberately disagrees with legacyActionFor's "power-on": if the
		// fallback ever won here by mistake, this test would resolve to the
		// wrong action.
		{Name: "power-on-via-cliverb", CLIVerb: "power on"},
		{Name: "power-on"},
	}}

	got, ok := holder.ActionForVerb("power on")
	if !ok {
		t.Fatalf("ActionForVerb(%q) = not found, want a match", "power on")
	}
	if got.Name != "power-on-via-cliverb" {
		t.Errorf("ActionForVerb(%q).Name = %q, want %q (the CLIVerb match, not the legacy-name fallback)",
			"power on", got.Name, "power-on-via-cliverb")
	}
}

// TestActionForVerbFallsBackToLegacyNameWhenCLIVerbAbsent is the regression
// test for the review finding on sy0: a server built before 09a5262 never
// emits Action.CLIVerb, so it decodes as "" on every action and the primary
// match can never succeed. Without a fallback, ActionForVerb would fail to
// resolve any command against such a server, and runAction would report every
// single one as "not available right now" -- indistinguishable from a
// legitimately withheld action. This pins that the pre-sy0 name derivation
// (legacyActionName) still resolves each command correctly when CLIVerb is
// empty.
func TestActionForVerbFallsBackToLegacyNameWhenCLIVerbAbsent(t *testing.T) {
	holder := Entity{Actions: []Action{
		{Name: "power-on"}, // CLIVerb absent, as a pre-sy0 server sends it
		{Name: "power-off"},
		{Name: "fans-on"},
		{Name: "fans-off"},
		{Name: "monitoring-mute"},
		{Name: "monitoring-unmute"},
		{Name: "f1-on"}, // per-host action, derived rather than listed
		{Name: "f1-off"},
		{Name: "all-on"}, // "all" is a host-shaped token, not a real host
		{Name: "all-off"},
	}}

	cases := map[string]string{
		"power on":          "power-on",
		"power off":         "power-off",
		"fans on":           "fans-on",
		"fans off":          "fans-off",
		"monitoring mute":   "monitoring-mute",
		"monitoring unmute": "monitoring-unmute",
		"power f1 on":       "f1-on",
		"power f1 off":      "f1-off",
		"power all on":      "all-on",
		"power all off":     "all-off",
	}

	for verb, wantName := range cases {
		got, ok := holder.ActionForVerb(verb)
		if !ok {
			t.Errorf("ActionForVerb(%q) against a CLIVerb-less server = not found, want %q via fallback", verb, wantName)
			continue
		}
		if got.Name != wantName {
			t.Errorf("ActionForVerb(%q) = %q, want %q", verb, got.Name, wantName)
		}
	}
}

// TestActionForVerbUnknownVerbFindsNothingEitherWay confirms a verb that
// names nothing -- neither a CLIVerb nor a legacy name -- still resolves to
// "not found" through both paths, rather than the fallback inventing a match.
func TestActionForVerbUnknownVerbFindsNothingEitherWay(t *testing.T) {
	holder := Entity{Actions: []Action{{Name: "power-on"}}}

	if _, ok := holder.ActionForVerb("fans toggle"); ok {
		t.Errorf("ActionForVerb(%q) = found, want not found (names nothing)", "fans toggle")
	}
}
