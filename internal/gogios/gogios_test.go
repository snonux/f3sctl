package gogios

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
)

// reportJSON is a small, representative Gogios report: one CRITICAL (unhandled),
// one stale WARNING, and two OKs, plus an unknown JSON key (defensive parsing).
const reportJSON = `{
  "lastUpdated": "2026-08-27T08:58:18+02:00",
  "subject": "GOGIOS Report [C:1 W:1 U:0 S:1 SU:0 OK:2]",
  "summary": {"critical":1,"warning":1,"unknown":0,"stale":1,"suppressed":0,"ok":2},
  "futureField": "ignore me",
  "sections": {
    "statusChanged": [],
    "unhandled": [
      {"name":"Check Ping6 r1.wg0.wan.buetow.org","status":"CRITICAL","output":"timed out","epoch":1724744298}
    ],
    "stale": [
      {"name":"Check SWAP blowfish","status":"WARNING","output":"SWAP WARNING","epoch":1724000000,"lastCheckedAgeSeconds":99999}
    ],
    "suppressed": [],
    "ok": [
      {"name":"Check Ping4 master.buetow.org","status":"OK","output":"PING OK","epoch":1724744300},
      {"name":"Check HTTP IPv4 foo.zone","status":"OK","output":"HTTP OK","epoch":1724744301}
    ]
  }
}`

// testCfg builds a config whose cache lives in a temp dir and whose GogiosURL
// points at srv, with a generous fetch timeout and a 60s cache TTL unless
// overridden.
func testCfg(t *testing.T, srv *httptest.Server, opts ...func(*config.Config)) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.GogiosURL = srv.URL
	cfg.GogiosFetchTimeout = config.Duration(5 * time.Second)
	cfg.GogiosCacheTTL = config.Duration(60 * time.Second)
	for _, o := range opts {
		o(&cfg)
	}
	return cfg
}

// countingServer serves reportJSON and counts how many times it was hit, so a
// test can prove the cache served a read without a second HTTP request.
func countingServer(t *testing.T, body string, status int) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func hits(n *int32) int32 { return atomic.LoadInt32(n) }

// TestFetchWritesAndReturnsTheReport pins the cold path: with no cache present,
// Fetch HTTP-GETs the report, returns it parsed, and writes the cache so the
// next call does not have to fetch.
func TestFetchWritesAndReturnsTheReport(t *testing.T) {
	srv, n := countingServer(t, reportJSON, http.StatusOK)
	cfg := testCfg(t, srv)

	got, err := Fetch(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if hits(n) != 1 {
		t.Errorf("server hits = %d, want 1 (cold cache must fetch)", hits(n))
	}
	if got.Subject != "GOGIOS Report [C:1 W:1 U:0 S:1 SU:0 OK:2]" {
		t.Errorf("Subject = %q", got.Subject)
	}
	if got.Summary.Critical != 1 || got.Summary.Warning != 1 || got.Summary.Stale != 1 || got.Summary.Ok != 2 {
		t.Errorf("Summary = %+v, want C:1 W:1 S:1 OK:2", got.Summary)
	}

	if _, err := os.Stat(filepath.Join(cfg.StateDir, "gogios-report.json")); err != nil {
		t.Errorf("cache file not written: %v", err)
	}
	// Unknown JSON keys were ignored, not an error.
	if got.LastUpdated != "2026-08-27T08:58:18+02:00" {
		t.Errorf("LastUpdated = %q", got.LastUpdated)
	}
}

// TestCacheHitServesWithoutFetching pins the 1-minute cache's whole point: a
// second read within the TTL serves the cached file and never reaches the
// server. A browse session is several reads in quick succession; without this
// each click would re-fetch.
func TestCacheHitServesWithoutFetching(t *testing.T) {
	srv, n := countingServer(t, reportJSON, http.StatusOK)
	cfg := testCfg(t, srv)

	if _, err := Fetch(context.Background(), cfg); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	if hits(n) != 1 {
		t.Fatalf("precondition: expected 1 hit, got %d", hits(n))
	}

	// Second read: served from the cache. The server must not be hit again.
	got, err := Fetch(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if hits(n) != 1 {
		t.Errorf("server hits = %d after a cached read, want still 1 (cache must serve within TTL)", hits(n))
	}
	if got.Subject == "" {
		t.Error("cached report parsed empty")
	}
}

// TestCacheMissAfterTTLRefetches pins the expiry: once the cached file is older
// than the TTL, the next read fetches again. The mtime is backdated rather
// than waiting out a real TTL, so the test is instant and deterministic.
func TestCacheMissAfterTTLRefetches(t *testing.T) {
	srv, n := countingServer(t, reportJSON, http.StatusOK)
	cfg := testCfg(t, srv)

	if _, err := Fetch(context.Background(), cfg); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}

	// Backdate the cache to well past the 60s TTL.
	past := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(filepath.Join(cfg.StateDir, "gogios-report.json"), past, past); err != nil {
		t.Fatalf("backdating cache mtime: %v", err)
	}

	if _, err := Fetch(context.Background(), cfg); err != nil {
		t.Fatalf("second Fetch after TTL: %v", err)
	}
	if hits(n) != 2 {
		t.Errorf("server hits = %d, want 2 (a cache older than TTL must re-fetch)", hits(n))
	}
}

// TestClearCacheRemovesTheFileAndForcesRefetch pins the cache-clear action: it
// deletes the cached report so the very next read re-fetches even within the
// TTL, and it is not an error when there is nothing to clear.
func TestClearCacheRemovesTheFileAndForcesRefetch(t *testing.T) {
	srv, n := countingServer(t, reportJSON, http.StatusOK)
	cfg := testCfg(t, srv)

	if _, err := Fetch(context.Background(), cfg); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	if hits(n) != 1 {
		t.Fatalf("precondition: expected 1 hit, got %d", hits(n))
	}

	if err := ClearCache(cfg); err != nil {
		t.Fatalf("ClearCache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "gogios-report.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("cache file still present after ClearCache: err=%v", err)
	}

	// Next read re-fetches despite the TTL not having elapsed.
	if _, err := Fetch(context.Background(), cfg); err != nil {
		t.Fatalf("Fetch after clear: %v", err)
	}
	if hits(n) != 2 {
		t.Errorf("server hits = %d, want 2 (ClearCache must force a re-fetch on the next read)", hits(n))
	}

	// Clearing an already-empty cache is not an error.
	if err := ClearCache(cfg); err != nil {
		t.Errorf("ClearCache with no cache: %v, want nil", err)
	}
}

// TestFetchErrorsOnANonOKResponse pins the non-200 path: a Gogios endpoint
// answering with an error is a fetch failure, not a silently empty report.
func TestFetchErrorsOnANonOkResponse(t *testing.T) {
	srv, n := countingServer(t, "server error", http.StatusInternalServerError)
	cfg := testCfg(t, srv)

	_, err := Fetch(context.Background(), cfg)
	if err == nil {
		t.Fatal("Fetch on a 500 succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %v, want it to mention the 500 status", err)
	}
	if hits(n) != 1 {
		t.Errorf("hits = %d, want 1", hits(n))
	}
	// A failed fetch must not leave a cache file behind, or the next call would
	// serve a half/empty body as if it were the report.
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "gogios-report.json")); err == nil {
		t.Error("a 500 left a cache file behind; failed fetches must not be cached")
	}
}

// TestFetchErrorsOnAnUnreachableServer pins the network-failure path: an
// unreachable Gogios endpoint is a fetch error, distinct from a non-200.
func TestFetchErrorsOnAnUnreachableServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close() // closed: the port refuses the connection

	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.GogiosURL = addr
	cfg.GogiosFetchTimeout = config.Duration(2 * time.Second)
	cfg.GogiosCacheTTL = config.Duration(60 * time.Second)

	_, err := Fetch(context.Background(), cfg)
	if err == nil {
		t.Fatal("Fetch on a closed server succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "fetching the Gogios report") {
		t.Errorf("error = %v, want it to say the report could not be fetched", err)
	}
}

// TestFetchErrorsOnAMalformedBodyAndDoesNotCacheIt pins the parse-before-cache
// ordering: a 200 response with a body that fails to parse must not be
// written to the cache, or the next read would fall back to a fetch anyway
// (readCache also fails to parse it) while leaving a bogus file on disk.
func TestFetchErrorsOnAMalformedBodyAndDoesNotCacheIt(t *testing.T) {
	srv, n := countingServer(t, "not json", http.StatusOK)
	cfg := testCfg(t, srv)

	_, err := Fetch(context.Background(), cfg)
	if err == nil {
		t.Fatal("Fetch on a malformed body succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "parsing the Gogios report") {
		t.Errorf("error = %v, want it to say the report could not be parsed", err)
	}
	if hits(n) != 1 {
		t.Errorf("hits = %d, want 1", hits(n))
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "gogios-report.json")); err == nil {
		t.Error("a malformed body left a cache file behind; unparseable bodies must not be cached")
	}
}

// TestFetchFallsBackToFetchingOnACorruptCache pins the read-side of the same
// contract: a cache file that exists, is fresh (within TTL), but does not
// parse (e.g. truncated by an unrelated process) must not be served as-is --
// Fetch must fall back to a real fetch rather than returning garbage or
// erroring out.
func TestFetchFallsBackToFetchingOnACorruptCache(t *testing.T) {
	srv, n := countingServer(t, reportJSON, http.StatusOK)
	cfg := testCfg(t, srv)

	cachePath := filepath.Join(cfg.StateDir, "gogios-report.json")
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Fresh mtime (default from WriteFile is "now"), but unparseable content.
	if err := os.WriteFile(cachePath, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("seeding a corrupt cache: %v", err)
	}

	got, err := Fetch(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Fetch with a corrupt cache: %v", err)
	}
	if hits(n) != 1 {
		t.Errorf("server hits = %d, want 1 (a corrupt cache must trigger a fetch)", hits(n))
	}
	if got.Subject == "" {
		t.Error("Fetch after a corrupt cache returned an empty report")
	}
}

// TestFetchRespectsTheFetchTimeout pins the timeout composition: a Gogios
// endpoint slower than cfg.GogiosFetchTimeout must fail the fetch rather than
// hang, since the CGI process fetch() runs from has its own outer deadline.
func TestFetchRespectsTheFetchTimeout(t *testing.T) {
	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-unblock:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.GogiosURL = srv.URL
	cfg.GogiosFetchTimeout = config.Duration(50 * time.Millisecond)
	cfg.GogiosCacheTTL = config.Duration(60 * time.Second)

	_, err := Fetch(context.Background(), cfg)
	if err == nil {
		t.Fatal("Fetch against a server slower than GogiosFetchTimeout succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "fetching the Gogios report") {
		t.Errorf("error = %v, want it to say the report could not be fetched", err)
	}
}

// TestConcurrentFetchesDoNotCorruptTheCache pins writeCache's serialization:
// several goroutines racing Fetch against a cold cache must not interleave
// their writes to the shared path+".tmp" file. Before writeCacheMu was added,
// concurrent os.WriteFile calls on that same tmp path could corrupt each
// other's bytes ahead of either rename -- the same class of bug fixed in
// internal/coordination.Manager for job.json.
func TestConcurrentFetchesDoNotCorruptTheCache(t *testing.T) {
	srv, _ := countingServer(t, reportJSON, http.StatusOK)
	cfg := testCfg(t, srv)

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = Fetch(context.Background(), cfg)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: Fetch: %v", i, err)
		}
	}

	// The cache file on disk must still be a valid, uncorrupted report.
	raw, err := os.ReadFile(filepath.Join(cfg.StateDir, "gogios-report.json"))
	if err != nil {
		t.Fatalf("reading the cache file: %v", err)
	}
	if _, err := parse(raw); err != nil {
		t.Errorf("cache file corrupted by concurrent writers: %v", err)
	}
}

// TestParseErrorsOnMalformedJSON pins parse's own error path: syntactically
// invalid JSON is a wrapped error, not a panic or a silently empty report.
func TestParseErrorsOnMalformedJSON(t *testing.T) {
	_, err := parse([]byte("{not valid json"))
	if err == nil {
		t.Fatal("parse on malformed JSON succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "parsing the Gogios report") {
		t.Errorf("error = %v, want it to say the report could not be parsed", err)
	}
}

// TestByStatusGroupsAcrossSections pins the drill-down re-index: Gogios groups
// checks by lifecycle (unhandled/stale/suppressed/ok), but a "show me every
// CRITICAL" view wants them grouped by status. A CRITICAL that is stale and a
// CRITICAL that is unhandled both land under CRITICAL here.
func TestByStatusGroupsAcrossSections(t *testing.T) {
	r := &Report{
		Sections: Sections{
			Unhandled: []Check{
				{Name: "c1", Status: "CRITICAL"},
				{Name: "w1", Status: "WARNING"},
			},
			Stale: []Check{
				{Name: "c2", Status: "CRITICAL"}, // stale AND critical
			},
			Suppressed: []Check{
				{Name: "u1", Status: "UNKNOWN"},
			},
			Ok: []Check{
				{Name: "o1", Status: "OK"},
				{Name: "o2", Status: "OK"},
			},
		},
	}

	by := r.ByStatus()
	if len(by["CRITICAL"]) != 2 {
		t.Errorf("CRITICAL = %d checks, want 2 (one unhandled, one stale): %v", len(by["CRITICAL"]), by["CRITICAL"])
	}
	if len(by["WARNING"]) != 1 || by["WARNING"][0].Name != "w1" {
		t.Errorf("WARNING = %+v, want [w1]", by["WARNING"])
	}
	if len(by["UNKNOWN"]) != 1 {
		t.Errorf("UNKNOWN = %d, want 1", len(by["UNKNOWN"]))
	}
	if len(by["OK"]) != 2 {
		t.Errorf("OK = %d, want 2", len(by["OK"]))
	}
}

// TestCheckByNameFindsAcrossSections pins the per-check detail lookup: a name
// is found regardless of which section it lives in, and a name that does not
// exist reports ok=false.
func TestCheckByNameFindsAcrossSections(t *testing.T) {
	r := &Report{Sections: Sections{
		Unhandled: []Check{{Name: "Check Ping6 r1.wg0.wan.buetow.org", Status: "CRITICAL", Output: "timed out"}},
		Ok:        []Check{{Name: "Check HTTP IPv4 foo.zone", Status: "OK"}},
	}}

	got, ok := r.Check("Check Ping6 r1.wg0.wan.buetow.org")
	if !ok || got.Output != "timed out" {
		t.Errorf("Check(unhandled name) = %+v ok=%v, want the CRITICAL check", got, ok)
	}
	if got, ok := r.Check("Check HTTP IPv4 foo.zone"); !ok || got.Status != "OK" {
		t.Errorf("Check(ok name) = %+v ok=%v", got, ok)
	}
	if _, ok := r.Check("no such check"); ok {
		t.Error("Check(unknown) found, want not found")
	}
}

// TestParseIgnoresUnknownFields pins the defensive-parse contract: a Gogios
// release that adds a field (or a federated peer that adds one) must not break
// this reader. parse only reads the fields it knows.
func TestParseIgnoresUnknownFields(t *testing.T) {
	// The body carries an unknown top-level key and an unknown per-check key;
	// parse must succeed and read only the fields it knows.
	r, err := parse([]byte(`{"subject":"S","summary":{"critical":7},"futureKey":42,"sections":{"ok":[{"name":"n","status":"OK","futureCheckKey":true}]}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.Subject != "S" {
		t.Errorf("Subject = %q", r.Subject)
	}
	if r.Summary.Critical != 7 {
		t.Errorf("Critical = %d, want 7", r.Summary.Critical)
	}
	if len(r.Sections.Ok) != 1 || r.Sections.Ok[0].Name != "n" {
		t.Errorf("Ok = %+v, want one check named n", r.Sections.Ok)
	}
	// Also confirm the representative reportJSON round-trips through json.
	var rep Report
	if err := json.Unmarshal([]byte(reportJSON), &rep); err != nil {
		t.Fatalf("unmarshal reportJSON: %v", err)
	}
	if rep.Summary.Ok != 2 {
		t.Errorf("reportJSON Summary.Ok = %d, want 2", rep.Summary.Ok)
	}
}
