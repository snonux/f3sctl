package client

import "testing"

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
