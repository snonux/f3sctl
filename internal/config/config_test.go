package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This file is the first test coverage for package config, which was at 0%.
// The package is small but central -- every entry point resolves its settings
// through it -- so the behaviours worth pinning are the ones a silent
// regression would break quietly: the dual-form Duration wire format, the
// candidate-list resolution that lets one shipped config serve every caller,
// home expansion under doas/sudo, and the overlay that keeps absent keys at
// their compiled-in default.

// --- Duration ---------------------------------------------------------------

// TestDurationMarshalRendersGoString pins MarshalJSON's contract: a Duration
// serialises as the same string time.Duration.String would produce, not raw
// nanoseconds, so the JSON a human reads stays "4m0s" rather than
// raw nanoseconds (240000000000 for 240s).
func TestDurationMarshalRendersGoString(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    Duration
		want string
	}{
		{"seconds-normalised", Duration(240 * time.Second), `"4m0s"`},
		{"minutes", Duration(4 * time.Minute), `"4m0s"`},
		{"zero", Duration(0), `"0s"`},
		{"mixed", Duration(2*time.Hour + 30*time.Minute), `"2h30m0s"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.d)
			if err != nil {
				t.Fatalf("MarshalJSON: %v", err)
			}
			if string(raw) != tc.want {
				t.Errorf("MarshalJSON(%v) = %s, want %s", tc.d, raw, tc.want)
			}
		})
	}
}

// TestDurationUnmarshalAcceptsStringAndSeconds pins UnmarshalJSON's two
// accepted forms -- a Go duration string ("4m") for a human-written config
// and a plain number of seconds for one that reads better as a count --
// plus the two error paths: an unparseable string, and a value that is
// neither a string nor a number.
func TestDurationUnmarshalAcceptsStringAndSeconds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		json    string
		want    Duration
		wantErr bool
	}{
		{name: "string seconds", json: `"240s"`, want: Duration(240 * time.Second)},
		{name: "string minutes", json: `"4m"`, want: Duration(4 * time.Minute)},
		{name: "string with unit", json: `"1h30m"`, want: Duration(90 * time.Minute)},
		{name: "number of seconds", json: `240`, want: Duration(240 * time.Second)},
		{name: "fractional seconds", json: `1.5`, want: Duration(1500 * time.Millisecond)},
		{name: "zero number", json: `0`, want: Duration(0)},
		{name: "invalid string", json: `"not a duration"`, wantErr: true},
		{name: "object", json: `{"x":1}`, wantErr: true},
		{name: "bare garbage", json: `abc`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var d Duration
			err := json.Unmarshal([]byte(tc.json), &d)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("UnmarshalJSON(%s) = nil error, want one", tc.json)
				}
				return
			}
			if err != nil {
				t.Fatalf("UnmarshalJSON(%s): %v", tc.json, err)
			}
			if d != tc.want {
				t.Errorf("UnmarshalJSON(%s) = %v, want %v", tc.json, d, tc.want)
			}
		})
	}
}

// TestDurationRoundTripsThroughJSON pins that a Marshal then Unmarshal of the
// same Duration reproduces it exactly, for every form the fleet actually
// configures. A round-trip that lost precision would silently change an
// operator's timeout at the next config read.
func TestDurationRoundTripsThroughJSON(t *testing.T) {
	for _, d := range []Duration{
		Duration(0),
		Duration(2 * time.Second),
		Duration(240 * time.Second),
		Duration(1200 * time.Second),
		Duration(5 * time.Minute),
		Duration(360 * time.Second),
	} {
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatalf("Marshal(%v): %v", d, err)
		}
		var back Duration
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("Unmarshal(%s): %v", raw, err)
		}
		if back != d {
			t.Errorf("round-trip %v -> %s -> %v: not preserved", d, raw, back)
		}
	}
}

// TestDReturnsTheUnderlyingTimeDuration pins the trivial accessor: D() is the
// one place the rest of the codebase converts a config Duration to a
// time.Duration, so a change to its meaning here would change every timeout
// at once.
func TestDReturnsTheUnderlyingTimeDuration(t *testing.T) {
	d := Duration(42 * time.Second)
	if got, want := d.D(), 42*time.Second; got != want {
		t.Errorf("D() = %v, want %v", got, want)
	}
}

// --- expandHome -------------------------------------------------------------

// TestExpandHomeExpandsALeadingTilde pins the one behaviour that matters under
// doas/sudo: a leading "~/" is joined onto $HOME (the target user's, which
// is whose files we want), and anything without that prefix is returned
// unchanged -- including an absolute path, which must not be mangled.
func TestExpandHomeExpandsALeadingTilde(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")

	for _, tc := range []struct {
		name, in, want string
	}{
		{"tilde config", "~/.config/f3sctl.json", "/home/testuser/.config/f3sctl.json"},
		{"tilde ssh", "~/.ssh/id_ed25519", "/home/testuser/.ssh/id_ed25519"},
		{"absolute path unchanged", "/var/db/f3sctl/apikey", "/var/db/f3sctl/apikey"},
		{"relative path unchanged", "f3sctl.json", "f3sctl.json"},
		{"bare tilde unchanged", "~", "~"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := expandHome(tc.in); got != tc.want {
				t.Errorf("expandHome(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// --- firstReadable ----------------------------------------------------------

// TestFirstReadableReturnsTheFirstOpenableCandidate pins the candidate-list
// resolution that lets one shipped config serve every host: the first
// readable entry wins, and entries before it being absent (or unreadable)
// are simply skipped, not fatal.
func TestFirstReadableReturnsTheFirstOpenableCandidate(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent")
	present := filepath.Join(dir, "present")
	if err := os.WriteFile(present, []byte("ok\n"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	got, err := firstReadable([]string{missing, present, "ignored-after-a-hit"}, "fixture")
	if err != nil {
		t.Fatalf("firstReadable: %v", err)
	}
	if got != present {
		t.Errorf("firstReadable = %q, want the present candidate %q", got, present)
	}
}

// TestFirstReadableErrorNamesEveryCandidateTried pins the error shape
// ResolveSSHIdentity/ResolveShellyPassword surface when nothing is readable:
// it lists every candidate that was tried, so an operator diagnosing "it
// could not find the key" sees every path it looked at, not just the last.
func TestFirstReadableErrorNamesEveryCandidateTried(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	c := filepath.Join(dir, "c")

	_, err := firstReadable([]string{a, b, c}, "fixture")
	if err == nil {
		t.Fatal("firstReadable with no readable candidate succeeded, want an error")
	}
	for _, tried := range []string{a, b, c} {
		if !strings.Contains(err.Error(), tried) {
			t.Errorf("error %q does not name tried candidate %q", err, tried)
		}
	}
}

// --- ResolveShellyPassword --------------------------------------------------

// TestResolveShellyPasswordReturnsTheFirstLine pins the file format the Shelly
// digest-auth path reads: the first line of the first readable candidate,
// trimmed, is the password. A second line in the file is irrelevant.
func TestResolveShellyPasswordReturnsTheFirstLine(t *testing.T) {
	dir := t.TempDir()
	pwFile := filepath.Join(dir, "shelly_plug")
	if err := os.WriteFile(pwFile, []byte("hunter2\nthis line is ignored\n"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	cfg := Config{ShellyPasswordFile: []string{pwFile}}

	got, err := cfg.ResolveShellyPassword()
	if err != nil {
		t.Fatalf("ResolveShellyPassword: %v", err)
	}
	if got != "hunter2" {
		t.Errorf("ResolveShellyPassword = %q, want the first line %q", got, "hunter2")
	}
}

// TestResolveShellyPasswordErrorsOnAnEmptyFile pins the hard-error contract
// the field's doc states: digest auth is mandatory, so a present-but-empty
// password file is a configuration error (not a degraded "no password" mode),
// because the only alternative is sending a blank credential and getting a
// 401 loop that looks exactly like a wrong password.
func TestResolveShellyPasswordErrorsOnAnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	pwFile := filepath.Join(dir, "empty")
	if err := os.WriteFile(pwFile, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	cfg := Config{ShellyPasswordFile: []string{pwFile}}

	if _, err := cfg.ResolveShellyPassword(); err == nil {
		t.Error("ResolveShellyPassword on an empty file succeeded, want an error")
	}
}

// --- ResolveAPIKey ----------------------------------------------------------

// TestResolveAPIKeyEnvironmentWinsOverTheFile pins the override order
// ResolveAPIKey's doc promises: F3SCTL_KEY wins over the file, so a second
// endpoint or a rotated key can be used without editing anything -- which is
// also what makes the tool easy to point at a test instance.
func TestResolveAPIKeyEnvironmentWinsOverTheFile(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "apikey")
	if err := os.WriteFile(keyFile, []byte("from-file\n"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	t.Setenv("F3SCTL_KEY", "from-env")
	cfg := Config{APIKeyFile: keyFile}

	got, err := cfg.ResolveAPIKey()
	if err != nil {
		t.Fatalf("ResolveAPIKey: %v", err)
	}
	if got != "from-env" {
		t.Errorf("ResolveAPIKey = %q, want the env value %q", got, "from-env")
	}
}

// TestResolveAPIKeyReadsTheFirstNonBlankLineSkippingComments pins the file
// format the server side relies on: several accepted keys, one per line, with
// blank and '#'-comment lines ignored. A client presents only its own -- the
// first non-blank, non-comment line.
func TestResolveAPIKeyReadsTheFirstNonBlankLineSkippingComments(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "apikey")
	if err := os.WriteFile(keyFile, []byte("# a comment\n\nclient-key\nserver-key-2\n"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	t.Setenv("F3SCTL_KEY", "") // no env override
	cfg := Config{APIKeyFile: keyFile}

	got, err := cfg.ResolveAPIKey()
	if err != nil {
		t.Fatalf("ResolveAPIKey: %v", err)
	}
	if got != "client-key" {
		t.Errorf("ResolveAPIKey = %q, want the first non-blank non-comment line %q", got, "client-key")
	}
}

// TestResolveAPIKeyErrorsOnAnEmptyFile pins the negative case: a key file
// with nothing but blanks and comments is empty for this purpose, and must
// not silently fall back to a blank credential.
func TestResolveAPIKeyErrorsOnAnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "empty")
	if err := os.WriteFile(keyFile, []byte("# only a comment\n\n"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	t.Setenv("F3SCTL_KEY", "")
	cfg := Config{APIKeyFile: keyFile}

	if _, err := cfg.ResolveAPIKey(); err == nil {
		t.Error("ResolveAPIKey on a file with no key line succeeded, want an error")
	}
}

// --- ResolveAPIURL ----------------------------------------------------------

// TestResolveAPIURLEnvironmentWinsOverConfig pins the same override order
// ResolveAPIURL documents as ResolveAPIKey's: F3SCTL_URL lets a caller point
// at a test instance without editing the config.
func TestResolveAPIURLEnvironmentWinsOverConfig(t *testing.T) {
	t.Setenv("F3SCTL_URL", "https://test.example/api")
	cfg := Config{APIURL: "https://prod.example/api"}

	if got := cfg.ResolveAPIURL(); got != "https://test.example/api" {
		t.Errorf("ResolveAPIURL = %q, want the env value", got)
	}

	t.Setenv("F3SCTL_URL", "")
	if got := cfg.ResolveAPIURL(); got != "https://prod.example/api" {
		t.Errorf("ResolveAPIURL with no env = %q, want the config value %q", got, "https://prod.example/api")
	}
}

// --- Load overlay -----------------------------------------------------------

// TestLoadOverlaysOntoDefaultsKeepingAbsentKeys pins the
// overlay-into-populated-struct semantics Load's doc promises: decoding into
// the already-Default()-populated Config leaves absent keys at their default,
// so a config file may override just one setting. Without this, an operator
// setting only api_url would silently zero every timeout.
func TestLoadOverlaysOntoDefaultsKeepingAbsentKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f3sctl.json")
	// Override a single Duration and a single string; leave the rest absent.
	if err := os.WriteFile(path, []byte(`{"api_url":"https://override/api","unmute_timeout":"37m"}`), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.APIURL != "https://override/api" {
		t.Errorf("APIURL = %q, want the overridden value", cfg.APIURL)
	}
	if cfg.UnmuteTimeout != Duration(37*time.Minute) {
		t.Errorf("UnmuteTimeout = %v, want 37m (overridden via string form)", cfg.UnmuteTimeout)
	}

	// Absent keys keep their compiled-in defaults.
	def := Default()
	if cfg.VMShutdownTimeout != def.VMShutdownTimeout {
		t.Errorf("VMShutdownTimeout = %v, want the default %v (absent key must keep its default)",
			cfg.VMShutdownTimeout, def.VMShutdownTimeout)
	}
	if cfg.SSHVerbTimeout != def.SSHVerbTimeout {
		t.Errorf("SSHVerbTimeout = %v, want the default %v (absent key must keep its default)",
			cfg.SSHVerbTimeout, def.SSHVerbTimeout)
	}
	if cfg.CGITimeout != def.CGITimeout {
		t.Errorf("CGITimeout = %v, want the default %v (absent key must keep its default)",
			cfg.CGITimeout, def.CGITimeout)
	}
	if cfg.StateDir != def.StateDir {
		t.Errorf("StateDir = %q, want the default %q (absent key must keep its default)",
			cfg.StateDir, def.StateDir)
	}
	if cfg.GogiosURL != def.GogiosURL {
		t.Errorf("GogiosURL = %q, want the default %q (absent key must keep its default)",
			cfg.GogiosURL, def.GogiosURL)
	}
	if cfg.GogiosFetchTimeout != def.GogiosFetchTimeout {
		t.Errorf("GogiosFetchTimeout = %v, want the default %v (absent key must keep its default)",
			cfg.GogiosFetchTimeout, def.GogiosFetchTimeout)
	}
	if cfg.GogiosCacheTTL != def.GogiosCacheTTL {
		t.Errorf("GogiosCacheTTL = %v, want the default %v (absent key must keep its default)",
			cfg.GogiosCacheTTL, def.GogiosCacheTTL)
	}
}

// TestDefaultGogiosValues pins the literal Gogios defaults: the federated
// endpoint, a 10s fetch timeout, and a 60s cache TTL. Unlike the other
// Duration fields (only checked relative to Default() elsewhere), these are
// asserted against their literal values once so a typo in Default() is
// caught directly.
func TestDefaultGogiosValues(t *testing.T) {
	def := Default()
	if def.GogiosURL != "https://gogios.buetow.org/index.json" {
		t.Errorf("GogiosURL = %q, want the federated endpoint", def.GogiosURL)
	}
	if def.GogiosFetchTimeout != Duration(10*time.Second) {
		t.Errorf("GogiosFetchTimeout = %v, want 10s", def.GogiosFetchTimeout)
	}
	if def.GogiosCacheTTL != Duration(60*time.Second) {
		t.Errorf("GogiosCacheTTL = %v, want 60s", def.GogiosCacheTTL)
	}
}

// TestLoadAcceptsANumberDurationInTheOverlay pins that the seconds form
// UnmarshalJSON accepts works through Load too, so a config written with
// "unmute_timeout": 1200 (seconds) overlays the same as "20m".
func TestLoadAcceptsANumberDurationInTheOverlay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f3sctl.json")
	if err := os.WriteFile(path, []byte(`{"probe_timeout":3}`), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ProbeTimeout != Duration(3*time.Second) {
		t.Errorf("ProbeTimeout = %v, want 3s (number-of-seconds form)", cfg.ProbeTimeout)
	}
}

// TestLoadReturnsDefaultForAMissingFile pins the documented tolerance: a
// missing config is not an error -- f3sctl is meant to work with no config at
// all -- so an explicit path that does not exist returns the compiled-in
// default rather than failing.
func TestLoadReturnsDefaultForAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load on a missing file: %v, want nil (a missing config is not an error)", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Errorf("Load on a missing file differs from Default(): got %+v", cfg)
	}
}

// TestLoadErrorsOnAMalformedFile pins the other half: a malformed config IS an
// error, because silently ignoring a config the operator wrote is worse than
// stopping. A truncated JSON document is the realistic shape.
func TestLoadErrorsOnAMalformedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f3sctl.json")
	if err := os.WriteFile(path, []byte(`{"api_url":"https://`), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Error("Load on a malformed file succeeded, want a parse error")
	}
}

// TestLoadErrorsOnAnInvalidDuration pins that a Duration the UnmarshalJSON
// rejects surfaces as a Load error rather than being swallowed into a
// zero-value timeout -- which would silently unbound the very calls the
// timeout exists to bound.
func TestLoadErrorsOnAnInvalidDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f3sctl.json")
	if err := os.WriteFile(path, []byte(`{"unmute_timeout":"not a duration"}`), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Error("Load on an invalid duration succeeded, want a parse error")
	}
}
