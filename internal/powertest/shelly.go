// Package powertest holds test fixtures shared between internal/cli and
// internal/power for exercising the Shelly rack-fan plug's RPC surface.
//
// It exists as its own, non-test package because Go cannot import one
// package's _test.go files from another package: a fixture used from more
// than one package's tests has to live in an ordinary .go file, in a package
// of its own, imported by both.
package powertest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// FakeShelly stands in for the rack-fan Shelly plug.
//
// It answers the two RPCs the engine uses (Switch.Set and Switch.GetStatus)
// and records every requested state, which is what tests assert on: "did the
// fans actually get switched" is the question, not "what did the CLI print".
// Digest auth is deliberately not offered -- ShellyClient.RPC only performs
// the challenge-response when it gets a 401, so answering 200 straight away
// keeps the fixture to what is under test.
type FakeShelly struct {
	srv *httptest.Server

	mu   sync.Mutex
	on   bool
	sets []bool // the requested state of every Switch.Set, in order
}

// NewFakeShelly starts a fake Shelly plug reporting the given initial state
// and registers its shutdown with t.Cleanup.
func NewFakeShelly(t *testing.T, initiallyOn bool) *FakeShelly {
	t.Helper()
	s := &FakeShelly{on: initiallyOn}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *FakeShelly) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch r.URL.Path {
	case "/rpc/Switch.Set":
		on := r.URL.Query().Get("on") == "true"
		s.sets = append(s.sets, on)
		s.on = on
		_ = json.NewEncoder(w).Encode(map[string]bool{"was_on": !on})
	case "/rpc/Switch.GetStatus":
		_ = json.NewEncoder(w).Encode(map[string]bool{"output": s.on})
	default:
		http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
	}
}

// Unplug makes the fixture unreachable, the way a plug that has lost power or
// fallen off the wifi is. Closing twice is safe: t.Cleanup closes it again.
func (s *FakeShelly) Unplug() { s.srv.Close() }

// SetCalls returns the state requested by each Switch.Set so far.
func (s *FakeShelly) SetCalls() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.sets...)
}

// Addr is what goes into Inventory.ShellyIP: the engine builds its URL as
// "http://" + ShellyIP + path, so a host:port belongs there.
func (s *FakeShelly) Addr() string { return strings.TrimPrefix(s.srv.URL, "http://") }
