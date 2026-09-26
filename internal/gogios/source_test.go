package gogios

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
)

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestNewSourceUsesADedicatedClient pins that the production source does not
// share http.DefaultClient (no timeout, process-global) or its transport, and
// that its client is bounded by the configured fetch timeout.
func TestNewSourceUsesADedicatedClient(t *testing.T) {
	cfg := config.Default()
	cfg.GogiosFetchTimeout = config.Duration(7 * time.Second)
	s := NewSource(cfg)

	if s.client == nil || s.client == http.DefaultClient {
		t.Fatalf("client = %p, want a dedicated client, not http.DefaultClient (%p)", s.client, http.DefaultClient)
	}
	if s.client.Timeout != 7*time.Second {
		t.Errorf("client timeout = %v, want the configured 7s", s.client.Timeout)
	}
	if s.client.Transport == nil || s.client.Transport == http.DefaultTransport {
		t.Errorf("client transport = %v, want its own, not http.DefaultTransport", s.client.Transport)
	}
}

// TestSourceFetchGoesThroughItsClient pins that the report is fetched through
// the source's own client -- the one NewSource dedicates -- and that the
// fetched report is cached for the next Fetch like the package-level Fetch.
func TestSourceFetchGoesThroughItsClient(t *testing.T) {
	srv, n := countingServer(t, reportJSON, http.StatusOK)
	s := NewSource(testCfg(t, srv))
	var trips atomic.Int32
	base := s.client.Transport
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		trips.Add(1)
		return base.RoundTrip(r)
	})

	for range 2 {
		r, err := s.Fetch(context.Background())
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if r.Subject == "" {
			t.Fatal("Fetch returned an empty report")
		}
	}
	if got := trips.Load(); got != 1 {
		t.Errorf("round trips through the source's client = %d, want 1 (the second Fetch is a cache hit)", got)
	}
	if hits(n) != 1 {
		t.Errorf("server hits = %d, want 1", hits(n))
	}
}

// TestSourceFetchReportsAClientError pins the negative path: a transport
// failure is a fetch error naming the report, and nothing is cached.
func TestSourceFetchReportsAClientError(t *testing.T) {
	srv, n := countingServer(t, reportJSON, http.StatusOK)
	cfg := testCfg(t, srv)
	s := NewSource(cfg)
	boom := errors.New("boom")
	s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, boom })

	if _, err := s.Fetch(context.Background()); !errors.Is(err, boom) {
		t.Errorf("Fetch error = %v, want it to wrap the transport's %v", err, boom)
	}
	if hits(n) != 0 {
		t.Errorf("server hits = %d, want 0: the transport failed first", hits(n))
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "gogios-report.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a failed fetch left a cache file behind: err=%v", err)
	}
}

// TestSourceClearForcesARefetch pins Source.Clear: the next Fetch re-fetches
// despite a fresh cache, and clearing nothing is not an error.
func TestSourceClearForcesARefetch(t *testing.T) {
	srv, n := countingServer(t, reportJSON, http.StatusOK)
	s := NewSource(testCfg(t, srv))

	if _, err := s.Fetch(context.Background()); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	if err := s.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, err := s.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch after Clear: %v", err)
	}
	if hits(n) != 2 {
		t.Errorf("server hits = %d, want 2 (Clear must force a re-fetch)", hits(n))
	}
	if err := s.Clear(); err != nil {
		t.Fatalf("second Clear: %v", err)
	}
	if err := s.Clear(); err != nil {
		t.Errorf("Clear with nothing cached: %v, want nil", err)
	}
}

// TestSourceClearReportsAnUnremovableCache is Clear's negative path: a cache
// that exists but cannot be removed is an error, not a silent no-op.
func TestSourceClearReportsAnUnremovableCache(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	// A non-empty directory where the cache file belongs: os.Remove fails
	// with something other than "not exist", as an unremovable cache would.
	p := filepath.Join(cfg.StateDir, "gogios-report.json")
	if err := os.MkdirAll(filepath.Join(p, "child"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := NewSource(cfg).Clear(); err == nil {
		t.Error("Clear of an unremovable cache succeeded, want an error")
	}
}
