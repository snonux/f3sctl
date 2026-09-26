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
	"testing/iotest"
	"time"

	"github.com/snonux/f3sctl/internal/config"
)

// reportJSON is a small, representative Gogios report: one CRITICAL (unhandled),
// one stale WARNING, and two OKs, plus an unknown JSON key (defensive parsing).
// The CRITICAL and one OK changed since the last notification, so -- exactly as
// Gogios writes it -- each is listed twice: once in statusChanged (with
// prevStatus) and once in its lifecycle section (without). A suppressed
// CRITICAL is left out of the summary counts, as Gogios's countBy does.
const reportJSON = `{
  "lastUpdated": "2026-08-27T08:58:18+02:00",
  "subject": "GOGIOS Report [C:1 W:1 U:0 S:1 SU:1 OK:2]",
  "summary": {"critical":1,"warning":1,"unknown":0,"stale":1,"suppressed":1,"ok":2},
  "futureField": "ignore me",
  "sections": {
    "statusChanged": [
      {"name":"Check Ping6 r1.wg0.wan.buetow.org","status":"CRITICAL","prevStatus":"OK","output":"timed out","epoch":1724744298},
      {"name":"Check HTTP IPv4 foo.zone","status":"OK","prevStatus":"WARNING","output":"HTTP OK","epoch":1724744301}
    ],
    "unhandled": [
      {"name":"Check Ping6 r1.wg0.wan.buetow.org","status":"CRITICAL","output":"timed out","epoch":1724744298}
    ],
    "stale": [
      {"name":"Check SWAP blowfish","status":"WARNING","output":"SWAP WARNING","epoch":1724000000,"lastCheckedAgeSeconds":99999}
    ],
    "suppressed": [
      {"name":"Check Load r2.wg0.wan.buetow.org","status":"CRITICAL","output":"load 42","epoch":1724744302}
    ],
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
	if got.Subject != "GOGIOS Report [C:1 W:1 U:0 S:1 SU:1 OK:2]" {
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

// TestConcurrentFetchesDoNotCorruptTheCache pins writeCache's concurrency
// safety: several goroutines racing Fetch against a cold cache must leave a
// valid cache and no stray temp files. There is no in-process lock; each
// writer uses its own temp file, which is what also makes this safe across
// the separate CGI processes the API runs as.
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
	assertNoTempFiles(t, cfg.StateDir)
}

// assertNoTempFiles fails the test if dir holds anything besides the cache
// file itself: a leftover temp file means a writer did not clean up.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.Name() != "gogios-report.json" {
			t.Errorf("unexpected file %q left in the cache dir", e.Name())
		}
	}
}

// TestConcurrentWriteCacheDistinctBodies pins the "last rename wins, never a
// mix" guarantee: writers racing with different bodies (as two CGI processes
// fetching at slightly different times would) leave exactly one of the
// bodies on disk, byte for byte, never an interleaving of them.
func TestConcurrentWriteCacheDistinctBodies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gogios-report.json")

	const n = 16
	bodies := make(map[string]bool, n)
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		// Large, distinct bodies make an interleaved write detectable.
		body := strings.Repeat(fmt.Sprintf("%02d", i), 64<<10)
		bodies[body] = true
		wg.Add(1)
		go func(i int, body string) {
			defer wg.Done()
			errs[i] = writeCache(path, []byte(body))
		}(i, body)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("writer %d: %v", i, err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the cache: %v", err)
	}
	if !bodies[string(got)] {
		t.Errorf("cache holds %d bytes matching none of the written bodies (interleaved write)", len(got))
	}
	assertNoTempFiles(t, dir)
}

// TestWriteCacheErrorsWhenTheDirIsUnusable pins the failure path: when the
// cache directory cannot be created (a regular file sits where it should be),
// writeCache returns a wrapped error rather than panicking or succeeding.
func TestWriteCacheErrorsWhenTheDirIsUnusable(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("seeding the blocker file: %v", err)
	}

	err := writeCache(filepath.Join(blocker, "gogios-report.json"), []byte(reportJSON))
	if err == nil {
		t.Fatal("writeCache under a regular file succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "Gogios cache dir") {
		t.Errorf("error = %v, want it to name the cache dir", err)
	}
}

// TestWriteCacheCleansUpWhenTheRenameFails pins the temp-file cleanup: if the
// final rename fails (here: a directory occupies the cache path), the temp
// file must be removed rather than accumulating in the state dir.
func TestWriteCacheCleansUpWhenTheRenameFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gogios-report.json")
	// A non-empty directory at the target path makes rename(2) fail.
	if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o700); err != nil {
		t.Fatalf("seeding the blocking dir: %v", err)
	}

	err := writeCache(path, []byte(reportJSON))
	if err == nil {
		t.Fatal("writeCache over a non-empty directory succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "renaming the Gogios cache") {
		t.Errorf("error = %v, want it to be the rename failure", err)
	}
	assertNoTempFiles(t, dir)
}

// seedTemp writes a cache temp file named like writeCache's into dir, with
// its mtime set age in the past, and returns its path.
func seedTemp(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
		t.Fatalf("seeding %s: %v", name, err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatalf("backdating %s: %v", name, err)
	}
	return p
}

// TestWriteCacheSweepsStaleTempFiles pins the orphan cleanup: a temp file
// left by a writer killed mid-write (older than staleTempAge) is removed on
// the next write, while a fresh one -- possibly a concurrent writer still in
// flight -- and unrelated files that merely look similar are left alone.
func TestWriteCacheSweepsStaleTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gogios-report.json")
	stale := seedTemp(t, dir, "gogios-report.json.111.tmp", staleTempAge+time.Minute)
	fresh := seedTemp(t, dir, "gogios-report.json.222.tmp", time.Second)
	// Old, but not one of ours: a different base name and a non-.tmp suffix.
	other := seedTemp(t, dir, "other.json.333.tmp", time.Hour)
	notTmp := seedTemp(t, dir, "gogios-report.json.444.bak", time.Hour)

	if err := writeCache(path, []byte(reportJSON)); err != nil {
		t.Fatalf("writeCache: %v", err)
	}

	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stale temp file survived the write: err=%v", err)
	}
	for _, keep := range []string{fresh, other, notTmp} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s was removed, want it kept: %v", filepath.Base(keep), err)
		}
	}
}

// TestClearCacheSweepsStaleTempFiles pins that ClearCache also removes stale
// temp files, but not fresh ones.
func TestClearCacheSweepsStaleTempFiles(t *testing.T) {
	srv, _ := countingServer(t, reportJSON, http.StatusOK)
	cfg := testCfg(t, srv)
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	stale := seedTemp(t, cfg.StateDir, "gogios-report.json.111.tmp", staleTempAge+time.Minute)
	fresh := seedTemp(t, cfg.StateDir, "gogios-report.json.222.tmp", time.Second)

	if err := ClearCache(cfg); err != nil {
		t.Fatalf("ClearCache: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stale temp file survived ClearCache: err=%v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh temp file removed by ClearCache: %v", err)
	}
}

// TestFetchErrorsOnAnOversizedBody pins the size cap: a body larger than
// maxReportBytes is an explicit ErrReportTooLarge, not a silently truncated
// body surfacing as a baffling JSON parse error, and it is not cached.
func TestFetchErrorsOnAnOversizedBody(t *testing.T) {
	// Valid JSON prefix padded past the limit: truncation would parse-fail,
	// the explicit check must fire first.
	body := `{"subject":"` + strings.Repeat("x", maxReportBytes) + `"}`
	srv, _ := countingServer(t, body, http.StatusOK)
	cfg := testCfg(t, srv)

	_, err := Fetch(context.Background(), cfg)
	if !errors.Is(err, ErrReportTooLarge) {
		t.Fatalf("Fetch on an oversized body: err = %v, want ErrReportTooLarge", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "gogios-report.json")); err == nil {
		t.Error("an oversized body left a cache file behind")
	}
}

// TestReadBodyBoundary pins readBody's off-by-one handling: a body of exactly
// the limit is accepted whole, one byte more is ErrReportTooLarge.
func TestReadBodyBoundary(t *testing.T) {
	const limit = 8
	got, err := readBody(strings.NewReader(strings.Repeat("a", limit)), limit)
	if err != nil || len(got) != limit {
		t.Errorf("readBody(exactly limit) = %d bytes, err %v; want %d bytes, nil", len(got), err, limit)
	}
	_, err = readBody(strings.NewReader(strings.Repeat("a", limit+1)), limit)
	if !errors.Is(err, ErrReportTooLarge) {
		t.Fatalf("readBody(limit+1) err = %v, want ErrReportTooLarge", err)
	}
	if !strings.Contains(err.Error(), "of 8 bytes") {
		t.Errorf("error = %q, want it to state the limit", err)
	}

	// A read failure is passed through wrapped, not mistaken for oversize.
	readErr := errors.New("connection reset")
	_, err = readBody(iotest.ErrReader(readErr), limit)
	if !errors.Is(err, readErr) {
		t.Errorf("readBody(failing reader) err = %v, want it to wrap %v", err, readErr)
	}
	if errors.Is(err, ErrReportTooLarge) {
		t.Errorf("readBody(failing reader) err = %v, must not be ErrReportTooLarge", err)
	}
}

// TestWriteCacheErrorsWhenTheTempFileCannotBeCreated pins the CreateTemp
// failure path: in a read-only cache dir writeCache returns a wrapped error
// naming the temp file and leaves nothing behind.
func TestWriteCacheErrorsWhenTheTempFileCannotBeCreated(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("making the dir read-only: %v", err)
	}
	// Restore write permission so t.TempDir's cleanup can remove it.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := writeCache(filepath.Join(dir, "gogios-report.json"), []byte(reportJSON))
	if err == nil {
		t.Fatal("writeCache into a read-only dir succeeded, want an error")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("error = %v, want it to wrap fs.ErrPermission", err)
	}
	if !strings.Contains(err.Error(), "cache temp file") {
		t.Errorf("error = %v, want it to name the cache temp file", err)
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatalf("reading the dir: %v", rerr)
	}
	if len(entries) != 0 {
		t.Errorf("read-only dir holds %d entries after a failed write, want 0", len(entries))
	}
}

// fakeFile is a syncWriteCloser whose steps can each be made to fail; it
// records what was called so a test can check Close always runs.
type fakeFile struct {
	writeErr, syncErr, closeErr error
	synced, closed              bool
}

func (f *fakeFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(p), nil
}

func (f *fakeFile) Sync() error {
	f.synced = true
	return f.syncErr
}

func (f *fakeFile) Close() error {
	f.closed = true
	return f.closeErr
}

// TestWriteAndSyncFailures pins writeAndSync's error handling: a failure at
// Write, Sync or Close is returned wrapped with the failing step named, a
// Write failure skips the Sync, and Close runs on every path so no file
// descriptor leaks.
func TestWriteAndSyncFailures(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name       string
		f          *fakeFile
		wantStep   string
		wantSynced bool
	}{
		{"write", &fakeFile{writeErr: boom}, "writing", false},
		{"sync", &fakeFile{syncErr: boom}, "syncing", true},
		{"close", &fakeFile{closeErr: boom}, "closing", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := writeAndSync(tc.f, []byte("x"))
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want it to wrap %v", err, boom)
			}
			if !strings.Contains(err.Error(), tc.wantStep) {
				t.Errorf("err = %v, want it to name the %q step", err, tc.wantStep)
			}
			if tc.f.synced != tc.wantSynced {
				t.Errorf("synced = %v, want %v", tc.f.synced, tc.wantSynced)
			}
			if !tc.f.closed {
				t.Error("Close was not called after the failure")
			}
		})
	}

	// Happy path: all three steps run and no error is returned.
	ok := &fakeFile{}
	if err := writeAndSync(ok, []byte("x")); err != nil || !ok.synced || !ok.closed {
		t.Errorf("writeAndSync(ok) = %v, synced=%v closed=%v; want nil, true, true", err, ok.synced, ok.closed)
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
// CRITICAL that is unhandled both land under CRITICAL here; a suppressed
// check lands nowhere, as Gogios leaves it out of its summary counts.
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
	if len(by["UNKNOWN"]) != 0 {
		t.Errorf("UNKNOWN = %+v, want none (the only UNKNOWN is suppressed)", by["UNKNOWN"])
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

// TestByStatusListsStatusChangedChecksOnce pins the regression: Gogios lists a
// changed check in statusChanged AND in its lifecycle section, so a union of
// every section showed it twice (summary critical=1, drill-down 2 entries).
// Each severity's drill-down must match the summary count, name each check
// once, and still carry the changed check's PrevStatus.
func TestByStatusListsStatusChangedChecksOnce(t *testing.T) {
	r, err := parse([]byte(reportJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(r.Sections.StatusChanged) == 0 {
		t.Fatal("fixture has no statusChanged checks; the regression is not exercised")
	}

	by := r.ByStatus()
	for status, want := range map[string]int{
		"CRITICAL": r.Summary.Critical,
		"WARNING":  r.Summary.Warning,
		"UNKNOWN":  r.Summary.Unknown,
		"OK":       r.Summary.Ok,
	} {
		if got := len(by[status]); got != want {
			t.Errorf("%s = %d checks, want %d (the summary count): %+v", status, got, want, by[status])
		}
		seen := map[string]bool{}
		for _, c := range by[status] {
			if seen[c.Name] {
				t.Errorf("%s lists %q more than once", status, c.Name)
			}
			seen[c.Name] = true
		}
	}

	if got := by["CRITICAL"]; len(got) == 1 && got[0].PrevStatus != "OK" {
		t.Errorf("CRITICAL[0].PrevStatus = %q, want OK (from statusChanged)", got[0].PrevStatus)
	}
	if r.Sections.Unhandled[0].PrevStatus != "" {
		t.Errorf("ByStatus mutated the report: Unhandled[0].PrevStatus = %q", r.Sections.Unhandled[0].PrevStatus)
	}
}

// TestByStatusLeavesOutSuppressedChecks pins that a suppressed CRITICAL is
// not a CRITICAL drill-down entry (Gogios excludes it from summary.critical;
// it has its own "suppressed" drill-down), while Check still finds it.
func TestByStatusLeavesOutSuppressedChecks(t *testing.T) {
	r, err := parse([]byte(reportJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	const name = "Check Load r2.wg0.wan.buetow.org"

	for _, c := range r.ByStatus()["CRITICAL"] {
		if c.Name == name {
			t.Errorf("CRITICAL drill-down lists the suppressed %q", name)
		}
	}
	if got, ok := r.Check(name); !ok || got.Status != "CRITICAL" {
		t.Errorf("Check(suppressed) = %+v ok=%v, want the suppressed CRITICAL", got, ok)
	}
}

// TestByStatusKeepsAnOrphanedStatusChange pins the safety net: a
// statusChanged entry with no twin in any lifecycle section (Gogios never
// writes one) is still listed once under its Status and found by Check,
// rather than silently dropped -- while a statusChanged entry whose twin is
// suppressed is not an orphan and stays out of the severity drill-down.
func TestByStatusKeepsAnOrphanedStatusChange(t *testing.T) {
	r := &Report{Sections: Sections{
		StatusChanged: []Check{
			{Name: "orphan", Status: "WARNING", PrevStatus: "OK"},
			{Name: "muted", Status: "CRITICAL", PrevStatus: "OK"},
		},
		Unhandled:  []Check{{Name: "w1", Status: "WARNING"}},
		Suppressed: []Check{{Name: "muted", Status: "CRITICAL"}},
	}}

	by := r.ByStatus()
	if got := by["WARNING"]; len(got) != 2 || got[1].Name != "orphan" || got[1].PrevStatus != "OK" {
		t.Errorf("WARNING = %+v, want [w1 orphan(prev OK)]", got)
	}
	if got := by["CRITICAL"]; len(got) != 0 {
		t.Errorf("CRITICAL = %+v, want none (muted's twin is suppressed)", got)
	}
	if got, ok := r.Check("orphan"); !ok || got.PrevStatus != "OK" {
		t.Errorf("Check(orphan) = %+v ok=%v, want the statusChanged entry", got, ok)
	}
}

// TestPrevStatusKeepsTheLifecycleEntrysOwn pins withPrevStatus's other
// branch: a lifecycle entry that already carries a PrevStatus keeps it rather
// than being overwritten by its statusChanged twin's.
func TestPrevStatusKeepsTheLifecycleEntrysOwn(t *testing.T) {
	r := &Report{Sections: Sections{
		StatusChanged: []Check{{Name: "c1", Status: "CRITICAL", PrevStatus: "OK"}},
		Unhandled:     []Check{{Name: "c1", Status: "CRITICAL", PrevStatus: "WARNING"}},
	}}

	if got := r.ByStatus()["CRITICAL"]; len(got) != 1 || got[0].PrevStatus != "WARNING" {
		t.Errorf("ByStatus CRITICAL = %+v, want one entry keeping prevStatus WARNING", got)
	}
	if got, ok := r.Check("c1"); !ok || got.PrevStatus != "WARNING" {
		t.Errorf("Check(c1) = %+v ok=%v, want prevStatus WARNING", got, ok)
	}
}

// TestCheckCarriesPrevStatusFromStatusChanged pins that the detail lookup
// returns the lifecycle entry enriched with the statusChanged PrevStatus, and
// that an unchanged check gets none.
func TestCheckCarriesPrevStatusFromStatusChanged(t *testing.T) {
	r, err := parse([]byte(reportJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	got, ok := r.Check("Check HTTP IPv4 foo.zone")
	if !ok || got.Status != "OK" || got.PrevStatus != "WARNING" {
		t.Errorf("Check(changed OK) = %+v ok=%v, want status OK, prevStatus WARNING", got, ok)
	}
	got, ok = r.Check("Check Ping4 master.buetow.org")
	if !ok || got.PrevStatus != "" {
		t.Errorf("Check(unchanged OK) = %+v ok=%v, want no prevStatus", got, ok)
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
