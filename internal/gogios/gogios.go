// Package gogios fetches and caches Gogios's JSON alert report so f3sctl can
// browse the current monitoring state without re-implementing Gogios's own
// federation.
//
// Gogios (~/git/gogios) runs on the two OpenBSD frontends and writes a JSON
// twin of its HTML report next to the HTML status file; the relayd-fronted
// https://gogios.buetow.org/index.json serves the federated merge (each
// frontend peers with the other). f3sctl reads that one URL, caches it on
// disk for a configurable TTL (default a minute), and re-serves the cached
// copy so a browse session -- overview, drill-down by status, per-check
// detail -- does not re-fetch on every click.
//
// Nothing here is policy: this package has no opinion on which alerts matter.
// It is the read-side sibling of internal/power's Monitor, which owns the
// mute-marker concern; the alert-browse concern lives here, off the Engine.
package gogios

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/snonux/f3sctl/internal/config"
)

// Report is the Gogios JSON report, mirroring the shape
// ~/git/gogios/internal/json_report.go persists. Fields are decoded
// defensively: unknown JSON keys are ignored, so a Gogios release that adds a
// field does not break this reader.
type Report struct {
	LastUpdated string   `json:"lastUpdated"`
	Subject     string   `json:"subject"`
	Summary     Summary  `json:"summary"`
	Sections    Sections `json:"sections"`
}

// Summary is the headline count: the [C:.. W:.. U:.. S:.. SU:.. OK:..] line
// from the email subject, as numbers.
type Summary struct {
	Critical   int `json:"critical"`
	Warning    int `json:"warning"`
	Unknown    int `json:"unknown"`
	Stale      int `json:"stale"`
	Suppressed int `json:"suppressed"`
	Ok         int `json:"ok"`
}

// Sections mirrors the report's grouped sections. Every slice is a list of
// checks; a non-OK check lives in exactly one of Unhandled, Stale or
// Suppressed, and an OK check lives in Ok. StatusChanged is the transient
// "changed since the last notification" view. ByStatus rebuilds a flat
// per-status index from the union of these.
type Sections struct {
	StatusChanged []Check `json:"statusChanged"`
	Unhandled     []Check `json:"unhandled"`
	Stale         []Check `json:"stale"`
	Suppressed    []Check `json:"suppressed"`
	Ok            []Check `json:"ok"`
}

// Check is one Gogios check result.
type Check struct {
	Name                  string `json:"name"`
	Status                string `json:"status"`
	PrevStatus            string `json:"prevStatus,omitempty"`
	Output                string `json:"output"`
	FederatedFrom         string `json:"federatedFrom,omitempty"`
	Epoch                 int64  `json:"epoch"`
	LastCheckedAgeSeconds int64  `json:"lastCheckedAgeSeconds,omitempty"`
}

// Fetch returns the Gogios alert report, serving the on-disk cache when it
// is fresher than cfg.GogiosCacheTTL; otherwise it HTTP-GETs cfg.GogiosURL,
// writes the body to the cache atomically, and returns the parsed report.
//
// The cache lives in cfg.StateDir so it survives across CGI processes (the
// f3sctl API is a fresh process per request, so an in-memory cache would not
// persist). A fetch failure is an error: the cache is fresh-or-fetch, not
// stale-on-error, so an operator always knows whether they are looking at a
// current report or a failure.
func Fetch(ctx context.Context, cfg config.Config) (*Report, error) {
	p := cachePath(cfg)
	if r, ok := readCache(p, cfg.GogiosCacheTTL.D()); ok {
		return r, nil
	}

	raw, err := fetch(ctx, cfg)
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

// ClearCache removes the cached report so the next Fetch call re-fetches. It
// is not an error if there is nothing to clear.
func ClearCache(cfg config.Config) error {
	if err := os.Remove(cachePath(cfg)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ByStatus indexes the report into a per-status map for drill-down: every
// check, grouped by its Status (CRITICAL/WARNING/UNKNOWN/OK as Gogios spells
// them), from the union of all sections. A "show me every CRITICAL" view is
// the union of CRITICALs across Unhandled, Stale and Suppressed -- Gogios
// groups by lifecycle, the caller wants them grouped by status, so this
// re-indexes.
func (r *Report) ByStatus() map[string][]Check {
	out := map[string][]Check{}
	for _, cs := range [][]Check{
		r.Sections.StatusChanged,
		r.Sections.Unhandled,
		r.Sections.Stale,
		r.Sections.Suppressed,
		r.Sections.Ok,
	} {
		for _, c := range cs {
			out[c.Status] = append(out[c.Status], c)
		}
	}
	return out
}

// Check finds one check by name across the whole report. ok is false when no
// check has that name. Names are unique across the report.
func (r *Report) Check(name string) (Check, bool) {
	for _, cs := range [][]Check{
		r.Sections.StatusChanged,
		r.Sections.Unhandled,
		r.Sections.Stale,
		r.Sections.Suppressed,
		r.Sections.Ok,
	} {
		for _, c := range cs {
			if c.Name == name {
				return c, true
			}
		}
	}
	return Check{}, false
}

// fetch HTTP-GETs the report at cfg.GogiosURL, bounded by cfg.GogiosFetchTimeout
// and the caller's context.
func fetch(ctx context.Context, cfg config.Config) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.GogiosFetchTimeout.D())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.GogiosURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building the Gogios request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching the Gogios report from %s: %w", cfg.GogiosURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status from Gogios at %s: %s", cfg.GogiosURL, resp.Status)
	}

	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// readCache returns the parsed cached report when the cache file exists and is
// fresher than ttl. A missing, stale, or unparseable cache reports "not fresh"
// (false), so the caller falls back to a fetch.
func readCache(path string, ttl time.Duration) (*Report, bool) {
	info, err := os.Stat(path)
	if err != nil || time.Since(info.ModTime()) > ttl {
		return nil, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	r, err := parse(raw)
	if err != nil {
		return nil, false
	}
	return r, true
}

// writeCacheMu serializes writeCache calls within this process. Two
// concurrent Fetch calls racing on a cold cache would otherwise both write to
// the same path+".tmp" file -- not just a "last rename wins" race, but two
// unsynchronized os.WriteFile calls able to interleave and corrupt the tmp
// file's bytes before either rename runs. internal/coordination.Manager hit
// this exact bug (fixed by a mutex around its own write-then-rename); this
// mirrors that fix.
var writeCacheMu sync.Mutex

// writeCache writes the report body atomically (write-then-rename), so a
// concurrent reader never sees a half-written cache. It mirrors the pattern
// internal/coordination.Manager uses for job.json, including serializing
// writers -- see writeCacheMu.
func writeCache(path string, raw []byte) error {
	writeCacheMu.Lock()
	defer writeCacheMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func parse(raw []byte) (*Report, error) {
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parsing the Gogios report: %w", err)
	}
	return &r, nil
}

func cachePath(cfg config.Config) string {
	return filepath.Join(cfg.StateDir, "gogios-report.json")
}
