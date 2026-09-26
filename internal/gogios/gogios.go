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
	"slices"
	"strings"
	"time"

	"github.com/snonux/f3sctl/internal/config"
)

// maxReportBytes caps how much of the Gogios response body is read. The
// federated report is a few tens of KiB; anything past this is a
// misconfigured or hostile endpoint, not a report.
const maxReportBytes = 1 << 20

// staleTempAge is how old a leftover cache temp file must be before a writer
// removes it. A temp file only lives for the write+sync+rename of an already
// fetched body (the fetch itself happens before it is created), so a live
// writer's file is seconds old at most; anything older belongs to a CGI
// process that was killed mid-write.
const staleTempAge = 5 * time.Minute

// ErrReportTooLarge is returned by Fetch when the Gogios response body
// exceeds maxReportBytes. It is reported explicitly rather than silently
// truncating the body into a confusing JSON parse error.
var ErrReportTooLarge = errors.New("gogios report exceeds the size limit")

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
// "changed since the last notification" view: a second copy (with
// PrevStatus) of checks already listed in Unhandled or Ok, not a section of
// its own. ByStatus rebuilds a flat per-status index from the sections
// behind the summary counts (Unhandled, Stale, Ok).
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

// ClearCache removes the cached report so the next Fetch call re-fetches, and
// sweeps stale temp files a killed writer left behind. It is not an error if
// there is nothing to clear.
func ClearCache(cfg config.Config) error {
	p := cachePath(cfg)
	removeStaleTemps(p, staleTempAge)
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// statuses is the fixed set of drill-down categories, in display order,
// matching the six counts in the report's own subject headline
// ("[C:.. W:.. U:.. S:.. SU:.. OK:..]"). An array rather than a slice, and
// unexported, so no caller can reorder or grow it: Statuses hands out copies.
//
// Four are severities (a check's own Status, lower-cased) and two --
// "stale" and "suppressed" -- are lifecycle groupings (Sections.Stale and
// Sections.Suppressed): a stale or suppressed check keeps its own
// CRITICAL/WARNING/UNKNOWN/OK status. ChecksFor owns that split.
var statuses = [...]string{"critical", "warning", "unknown", "stale", "suppressed", "ok"}

// Statuses returns the drill-down categories in display order. It is the
// single source for the API's /gogios/<status> routes and overview links, the
// local CLI's `gogios <status>` grammar, and the remote client's grammar and
// detail search order. Each call returns a fresh copy, so a caller may keep or
// modify the result without affecting anyone else.
func Statuses() []string {
	return slices.Clone(statuses[:])
}

// ChecksFor selects the checks in one Statuses category.
//
// "critical"/"warning"/"unknown"/"ok" are severities: the ByStatus entry for
// the upper-cased status, i.e. every check Gogios counts in its summary with
// that Status (suppressed ones excluded, changed ones listed once with their
// PrevStatus). "stale"/"suppressed" are lifecycle groupings and read
// Sections.Stale/Suppressed directly -- filtering ByStatus instead would
// either double-count a stale check under its severity and "stale", or need a
// Status value Gogios itself never writes.
//
// A status outside Statuses (misspelled, upper-cased, or a raw Gogios Status
// such as "CRITICAL") selects nothing and returns nil. The result never
// aliases the report's own sections.
func (r *Report) ChecksFor(status string) []Check {
	switch status {
	case "stale":
		return slices.Clone(r.Sections.Stale)
	case "suppressed":
		return slices.Clone(r.Sections.Suppressed)
	}
	if !slices.Contains(statuses[:], status) {
		return nil
	}
	return r.ByStatus()[strings.ToUpper(status)]
}

// ByStatus indexes the report into a per-status map for the severity
// drill-downs: every check Gogios counts in its summary, grouped by its Status
// (CRITICAL/WARNING/UNKNOWN/OK as Gogios spells them). A "show me every
// CRITICAL" view is the union of CRITICALs across Unhandled and Stale --
// Gogios groups by lifecycle, the caller wants them grouped by status, so
// this re-indexes. Suppressed checks are left out, as Gogios leaves them out
// of its summary counts; they have their own drill-down (Sections.Suppressed).
//
// Each check appears once: StatusChanged is not unioned in (see
// lifecycleSections), only its PrevStatus is carried over. A StatusChanged
// entry with no twin in any lifecycle section is still listed (see
// orphanedChanges), so a producer quirk never silently hides a check.
func (r *Report) ByStatus() map[string][]Check {
	prev := r.prevStatuses()
	out := map[string][]Check{}
	for _, cs := range r.severitySections() {
		for _, c := range cs {
			c = withPrevStatus(c, prev)
			out[c.Status] = append(out[c.Status], c)
		}
	}
	for _, c := range r.orphanedChanges() {
		out[c.Status] = append(out[c.Status], c)
	}
	return out
}

// Check finds one check by name across the lifecycle sections, suppressed
// ones included, with its PrevStatus filled in when it changed since the last
// notification; failing that, it falls back to an orphaned StatusChanged
// entry. ok is false when no check has that name. Names are unique across
// the lifecycle sections.
func (r *Report) Check(name string) (Check, bool) {
	for _, cs := range r.lifecycleSections() {
		for _, c := range cs {
			if c.Name == name {
				return withPrevStatus(c, r.prevStatuses()), true
			}
		}
	}
	// Not in any lifecycle section, so any StatusChanged match is an orphan.
	for _, c := range r.Sections.StatusChanged {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

// severitySections returns the sections behind Gogios's summary counts:
// Unhandled, Stale and Ok. Suppressed is absent because Gogios excludes
// suppressed checks from those counts.
func (r *Report) severitySections() [][]Check {
	return [][]Check{
		r.Sections.Unhandled,
		r.Sections.Stale,
		r.Sections.Ok,
	}
}

// lifecycleSections returns the sections that partition the report: every
// check lives in exactly one of them. StatusChanged is deliberately absent --
// Gogios writes a changed check into StatusChanged AND into its lifecycle
// section (Unhandled or Ok; a stale or suppressed check is never listed as
// changed), so unioning it in would list every changed check twice.
func (r *Report) lifecycleSections() [][]Check {
	return append(r.severitySections(), r.Sections.Suppressed)
}

// orphanedChanges returns the StatusChanged entries whose name appears in no
// lifecycle section. Gogios never writes one, but should a producer (or a
// federated merge) ever do so, the check is listed rather than dropped.
func (r *Report) orphanedChanges() []Check {
	listed := map[string]bool{}
	for _, cs := range r.lifecycleSections() {
		for _, c := range cs {
			listed[c.Name] = true
		}
	}
	var out []Check
	for _, c := range r.Sections.StatusChanged {
		if !listed[c.Name] {
			out = append(out, c)
		}
	}
	return out
}

// prevStatuses maps each StatusChanged check's name to its PrevStatus. Only
// the StatusChanged copy of a changed check carries PrevStatus; its lifecycle
// twin omits it.
func (r *Report) prevStatuses() map[string]string {
	prev := make(map[string]string, len(r.Sections.StatusChanged))
	for _, c := range r.Sections.StatusChanged {
		if c.PrevStatus != "" {
			prev[c.Name] = c.PrevStatus
		}
	}
	return prev
}

// withPrevStatus returns c with PrevStatus taken from prev when c does not
// carry one itself. c is a copy, so the report's own sections are untouched.
func withPrevStatus(c Check, prev map[string]string) Check {
	if c.PrevStatus == "" {
		c.PrevStatus = prev[c.Name]
	}
	return c
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

	return readBody(resp.Body, maxReportBytes)
}

// readBody reads at most limit bytes from r. It reads one byte past limit so
// a body of exactly limit bytes is accepted while a larger one is reported as
// ErrReportTooLarge instead of being truncated.
func readBody(r io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading the Gogios report: %w", err)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%w of %d bytes", ErrReportTooLarge, limit)
	}
	return raw, nil
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

// writeCache writes the report body atomically: it writes a uniquely named
// temp file in the cache's directory, fsyncs it, and renames it over the
// cache. Each writer gets its own temp file, so concurrent writers --
// goroutines in one process or, as in production, separate CGI processes --
// never share bytes; the last rename wins and a reader only ever sees a
// complete file. The fsync keeps a power loss from leaving a renamed but torn
// file; the rename itself may still be lost, which only means an older (or
// no) cache and a re-fetch. Temp files orphaned by killed writers are swept
// on each write.
func writeCache(path string, raw []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating the Gogios cache dir: %w", err)
	}
	removeStaleTemps(path, staleTempAge)

	// os.CreateTemp creates the file with mode 0600.
	f, err := os.CreateTemp(dir, filepath.Base(path)+tempInfix+"*"+tempSuffix)
	if err != nil {
		return fmt.Errorf("creating the Gogios cache temp file: %w", err)
	}
	tmp := f.Name()
	// Remove the temp file on any failure; after a successful rename it no
	// longer exists under this name.
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()

	if err := writeAndSync(f, raw); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("renaming the Gogios cache into place: %w", err)
	}
	return nil
}

// syncWriteCloser is the slice of *os.File that writeAndSync needs; it is an
// interface so a test can make each step fail.
type syncWriteCloser interface {
	io.WriteCloser
	Sync() error
}

// writeAndSync writes raw to f, fsyncs it, and closes it. f is closed on
// every path.
func writeAndSync(f syncWriteCloser, raw []byte) error {
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing the Gogios cache temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("syncing the Gogios cache temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing the Gogios cache temp file: %w", err)
	}
	return nil
}

// tempInfix and tempSuffix frame the random part of a cache temp file's name:
// <cache base>.<random>.tmp.
const (
	tempInfix  = "."
	tempSuffix = ".tmp"
)

// removeStaleTemps deletes cache temp siblings of path older than maxAge.
// It is best-effort: a CGI process killed between CreateTemp and Rename
// leaves its uniquely named temp file behind, and without this sweep those
// would accumulate forever. Fresh temp files are left alone, since they may
// belong to a writer that is still running. Errors are ignored; the next
// write retries.
func removeStaleTemps(path string, maxAge time.Duration) {
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := filepath.Base(path) + tempInfix
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, tempSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) <= maxAge {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
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
