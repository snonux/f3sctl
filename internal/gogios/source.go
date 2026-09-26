package gogios

import (
	"context"
	"net/http"
	"time"

	"github.com/snonux/f3sctl/internal/config"
)

// Source is the production report source: the cached-or-fetched Gogios
// report for one configuration, read through its own HTTP client.
//
// It is what a consumer that wants the report as an injected dependency (the
// REST API's Gogios surface, via its ReportSource interface) holds, instead
// of calling the package-level Fetch/ClearCache with a whole config.Config.
// A Source is safe for concurrent use: it holds only immutable configuration
// and an *http.Client, and the on-disk cache it shares with other processes
// is written atomically (see writeCache).
type Source struct {
	cfg    config.Config
	client *http.Client
}

// NewSource returns a Source reading the report configured in cfg (its URL,
// cache TTL, fetch timeout and cache dir) through a dedicated HTTP client
// (see NewHTTPClient) bounded by cfg.GogiosFetchTimeout.
func NewSource(cfg config.Config) *Source {
	return &Source{cfg: cfg, client: NewHTTPClient(cfg.GogiosFetchTimeout.D())}
}

// NewHTTPClient returns the dedicated HTTP client Gogios fetches go through.
//
// Dedicated rather than http.DefaultClient, which has no timeout at all and
// is process-global state any other package may reconfigure: this one gets
// its own transport (a clone of the default one, so proxy settings and dial
// behaviour stay the standard library's) and an overall timeout covering the
// connect, the headers and the body read. The per-request context deadline
// fetch applies stays as well; whichever is shorter wins. A zero timeout
// means none, as for http.Client itself (and as fetch treats it).
func NewHTTPClient(timeout time.Duration) *http.Client {
	var transport *http.Transport
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = t.Clone()
	} else {
		// DefaultTransport was replaced by something that is not an
		// *http.Transport: still use a transport of our own, with the one
		// default setting that matters here, rather than share theirs.
		transport = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

// Fetch returns the Gogios alert report, serving the on-disk cache when it
// is fresher than the configured GogiosCacheTTL; otherwise it HTTP-GETs the
// configured GogiosURL, writes the body to the cache atomically, and returns
// the parsed report.
//
// The cache lives in the configured StateDir so it survives across CGI
// processes (the f3sctl API is a fresh process per request, so an in-memory
// cache would not persist). A fetch failure is an error: the cache is
// fresh-or-fetch, not stale-on-error, so an operator always knows whether
// they are looking at a current report or a failure.
func (s *Source) Fetch(ctx context.Context) (*Report, error) {
	p := cachePath(s.cfg)
	if r, ok := readCache(p, s.cfg.GogiosCacheTTL.D()); ok {
		return r, nil
	}

	raw, err := fetch(ctx, s.client, s.cfg)
	if err != nil {
		return nil, err
	}

	// Parse before caching: a 200 with a malformed body must not be written to
	// disk as if it were a good report, or the next read would fail to parse it
	// too and every call would re-fetch until the upstream body recovers.
	r, err := parse(raw)
	if err != nil {
		return nil, err
	}

	// Caching is best-effort: a write failure must not stop a successful fetch
	// from being returned, only stop the next call from being served from the
	// cache.
	_ = writeCache(p, raw)

	return r, nil
}

// Clear removes the cached report so the next Fetch re-fetches. See
// ClearCache.
func (s *Source) Clear() error {
	return ClearCache(s.cfg)
}
