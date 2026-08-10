package httpapi

import (
	"strings"
	"testing"

	"github.com/snonux/f3sctl/internal/client"
)

// This file is sy0's single-source-of-truth guard: it walks the real
// routes() table and checks that jobArgs (the server's job-run argv) and the
// remote client's action resolution both stay consistent with whatever a
// route declares, so a future route addition is checked automatically rather
// than relying on someone remembering to update three places by hand -- see
// registry.go's route.CLIVerb doc comment for the drift this replaced.

// TestJobArgsDerivedFromEveryRoutesCLIVerb pins that jobArgs, for every
// action route in the real table, reproduces exactly "job-run" followed by
// the route's own CLIVerb split on whitespace. This is true by construction
// (jobArgsFrom does nothing but that split), but pinning it here means a
// route whose CLIVerb does not actually match its job's real argv -- e.g. a
// typo, or a CLIVerb with extra whitespace -- fails a test instead of
// shipping a job-run invocation nobody asked for.
func TestJobArgsDerivedFromEveryRoutesCLIVerb(t *testing.T) {
	for _, r := range routes() {
		if !r.Action {
			continue
		}
		if r.CLIVerb == "" {
			t.Errorf("action route %q declares no CLIVerb", r.Name)
			continue
		}

		want := append([]string{"job-run"}, strings.Fields(r.CLIVerb)...)
		got := jobArgs(r.jobAction())
		if !equalArgs(got, want) {
			t.Errorf("jobArgs(%q) = %v, want %v (derived from CLIVerb %q)",
				r.jobAction(), got, want, r.CLIVerb)
		}
	}
}

// TestEveryActionsCLIVerbIsUnique catches two routes claiming the same CLI
// command, which would make the client's ActionForVerb match whichever one
// happened to be advertised first.
func TestEveryActionsCLIVerbIsUnique(t *testing.T) {
	seen := map[string]string{}
	for _, r := range routes() {
		if !r.Action || r.CLIVerb == "" {
			continue
		}
		if prev, dup := seen[r.CLIVerb]; dup {
			t.Errorf("CLIVerb %q is claimed by both %q and %q", r.CLIVerb, prev, r.Name)
		}
		seen[r.CLIVerb] = r.Name
	}
}

// TestClientResolvesEveryRoutesCLIVerbToItsName is the cross-package half of
// the guard: it builds the client.Action list exactly as Router.action would
// render it for every route in the real table, then checks that
// client.Entity.ActionForVerb -- the function internal/client/run.go actually
// calls -- resolves each route's own CLIVerb back to that route's Name.
//
// This is what "the route registry is the single source of truth" means in
// practice: add a route with a new CLIVerb here and, so long as it is
// distinct (see TestEveryActionsCLIVerbIsUnique above), the client
// automatically knows how to invoke it -- no separate table to update, and
// this test would fail if the wiring between route.CLIVerb and
// Action.CLIVerb (router.go's Router.action) ever broke.
func TestClientResolvesEveryRoutesCLIVerbToItsName(t *testing.T) {
	var actions []client.Action
	for _, r := range routes() {
		if !r.Action || r.CLIVerb == "" {
			continue
		}
		actions = append(actions, client.Action{Name: r.Name, CLIVerb: r.CLIVerb})
	}
	holder := client.Entity{Actions: actions}

	for _, r := range routes() {
		if !r.Action || r.CLIVerb == "" {
			continue
		}
		got, ok := holder.ActionForVerb(r.CLIVerb)
		if !ok {
			t.Errorf("client could not resolve CLIVerb %q (route %q)", r.CLIVerb, r.Name)
			continue
		}
		if got.Name != r.Name {
			t.Errorf("client resolved CLIVerb %q to action %q, want %q", r.CLIVerb, got.Name, r.Name)
		}
	}
}

// TestJobArgsFromRouteWithoutCLIVerb is the negative case for
// TestJobArgsDerivedFromEveryRoutesCLIVerb: a route that is an action but
// declares no CLIVerb (a mistake nothing in the real table currently makes --
// see TestJobArgsDerivedFromEveryRoutesCLIVerb, which would catch it there
// too) must not make jobArgsFrom invent an argv. It has to say "I don't know
// how to run this" -- nil -- rather than guess.
func TestJobArgsFromRouteWithoutCLIVerb(t *testing.T) {
	synthetic := []route{
		{Name: "mystery", Action: true}, // no CLIVerb
	}
	if got := jobArgsFrom(synthetic, "mystery"); got != nil {
		t.Errorf("jobArgsFrom(route with no CLIVerb) = %v, want nil", got)
	}
}

// TestJobArgsUnrecognizedAction is the other negative case: an action
// identifier that names no route at all (a typo, or an action retired from
// the registry) must not silently return an empty-but-non-nil slice or
// someone else's argv.
func TestJobArgsUnrecognizedAction(t *testing.T) {
	if got := jobArgs("totally-not-a-real-action"); got != nil {
		t.Errorf("jobArgs(unrecognized) = %v, want nil", got)
	}
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
