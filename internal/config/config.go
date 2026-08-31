// Package config resolves f3sctl's runtime settings: compiled-in defaults
// overlaid by an optional JSON file.
//
// The overlay exists because the same binary runs in places with very
// different filesystem layouts — as _httpd under bozohttpd on pi0/pi1, as paul
// on earth, and as the f3sctl user on the f-hosts — and each needs different
// paths to the SSH identity and secrets. Path settings are therefore *lists*:
// the first readable entry wins, so one shipped config serves every caller.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/snonux/f3sctl/internal/inventory"
)

// DefaultPath is where the NetBSD/FreeBSD/OpenBSD packages install the
// optional config. Absent is not an error.
const DefaultPath = "/usr/local/etc/f3sctl.json"

// searchPath is where Load looks when no explicit path is given, most specific
// first.
//
// The per-user file comes first so a laptop can configure --remote (an API URL
// and a key) without root, while the packaged hosts keep their system-wide
// file. Only the first file found is read; they are not merged, because
// silently combining two configs makes "which setting won" unanswerable.
var searchPath = []string{
	"~/.config/f3sctl.json",
	DefaultPath,
}

// Config is everything f3sctl needs beyond the host inventory.
type Config struct {
	Inventory inventory.Inventory `json:"inventory"`

	// SSHIdentity lists candidate private keys, most specific first. The
	// first readable one is used.
	SSHIdentity []string `json:"ssh_identity"`
	// ShellyPasswordFile lists candidate files whose first line is the
	// Shelly plug's digest password.
	ShellyPasswordFile []string `json:"shelly_password_file"`
	// APIKeyFile is the file holding the accepted API key(s). On the API
	// nodes (pi0/pi1, CGI mode) it may list several keys, one per line, so
	// each client can be issued its own key without rotating the rest; blank
	// and '#'-comment lines are ignored. On a client (--remote mode) the
	// first non-blank, non-comment line is the key this client presents.
	APIKeyFile string `json:"api_key_file"`
	// StateDir holds the job lock, job state and job log.
	StateDir string `json:"state_dir"`

	// PeerNodes are f3sctl's own other API-serving hosts (pi0 and pi1),
	// consulted by internal/coordination.PeerSet before a job starts.
	//
	// The job lock in StateDir only serialises requests that reach one node,
	// and relayd load-balances pi0 and pi1, so two clicks seconds apart can
	// land on different nodes and start two conflicting jobs -- observed on
	// 2026-08-08. Living here, rather than as a package-level var inside the
	// coordination package, is what lets that package be constructed and
	// tested without hardcoding which two hosts run f3sctl.
	PeerNodes []string `json:"peer_nodes"`
	// PeerJobPath is the full URL path (including the CGI mount point) a
	// peer's current job is read from over plain HTTP, e.g.
	// "/cgi-bin/f3sctl/job".
	//
	// Empty (the default) means "derive it": httpapi.newServer builds it from
	// this node's own SCRIPT_NAME plus its own "/job" route, the same
	// mechanism every href handed back to a client already goes through. pi0
	// and pi1 are symmetric peers running the same CGI mount, so this node's
	// own mount point is the right answer for the other one too -- and it
	// means remounting the script is one change (SCRIPT_NAME), not two.
	//
	// Set this explicitly only for the rarer case where the two peers are
	// *not* mounted the same way; an explicit value always wins over the
	// derivation.
	PeerJobPath string `json:"peer_job_path"`

	// APIURL is the endpoint --remote talks to. Set on clients (earth), unset
	// on the Pis, which serve the API rather than call it.
	APIURL string `json:"api_url"`

	// VMShutdownTimeout bounds how long a host waits for its bhyve guests to
	// power off before resorting to SIGKILL.
	//
	// This must stay below the f-hosts' rcshutdown_timeout (currently 300s,
	// raised from the 90s default on 2026-06-28). If a host's rc.shutdown
	// watchdog fires first, init drops it to single-user *powered on but with
	// no network*, and Wake-on-LAN cannot wake it again — recovery then needs
	// physical access. See the f3s skill,
	// references/console-jetkvm-shutdown.md.
	VMShutdownTimeout Duration `json:"vm_shutdown_timeout"`

	// UnmuteTimeout bounds how long the wake path waits for r0/r1/r2 before
	// giving up on clearing the Gogios mute marker.
	//
	// Budget it against a cold boot, not a warm one: an f-host reaches sshd
	// roughly a minute after the magic packet, and its k3s guest needs a
	// further couple of minutes on top. The old 600s looked generous but was
	// not — while ntpd_sync_on_start blocked rc for ~14 minutes (fixed
	// 2026-08-09, see the f3s skill) it expired every time, and each expiry
	// stranded the mute and left Gogios blind until someone noticed.
	UnmuteTimeout Duration `json:"unmute_timeout"`

	// ProbeTimeout bounds a single ping or TCP dial during status probing.
	ProbeTimeout Duration `json:"probe_timeout"`

	// SSHConnectTimeout bounds establishing an SSH connection to a host.
	SSHConnectTimeout Duration `json:"ssh_connect_timeout"`

	// SSHVerbTimeout bounds a single agent verb run over SSH end-to-end:
	// connection establishment plus the verb's execution on the remote host.
	// It is a backstop against a verb that hangs for a reason the remote
	// agent does not bound itself -- a wedged ssh(1) after connect, a verb
	// other than poweroff whose remote work has no internal deadline -- so a
	// wedge becomes a clean abort the caller can report rather than a job
	// that holds its lock until stale() fires.
	//
	// It must exceed VMShutdownTimeout + SSHConnectTimeout: the poweroff verb
	// legitimately waits up to VMShutdownTimeout for its bhyve guests to stop
	// (see VMShutdownTimeout's own doc, and the agent's internal bound in
	// internal/agent/poweroff.go), plus connect time on top. The poweroff verb
	// returns at `poweroff`(8), which signals init and comes back before the
	// host's rc.shutdown runs to completion, so SSHVerbTimeout has no coupling
	// to rcshutdown_timeout -- that constraint belongs to VMShutdownTimeout
	// alone. The default leaves generous slack above the legitimate bound so
	// a healthy shutdown is never cut short while a genuinely wedged verb
	// still aborts in minutes, not tens of them.
	SSHVerbTimeout Duration `json:"ssh_verb_timeout"`

	// UmountTimeout bounds a single umount(8) of a locally mounted NFS
	// filesystem during the shutdown pre-flight (checkLocalNFS). A hard NFS
	// mount against a server that has gone away blocks umount(8) indefinitely,
	// which used to wedge the whole power-off job -- holding its lock until
	// stale() fired ~20 minutes later. Bounding it turns that wedge into a
	// clean error checkLocalNFS already knows how to act on (abort the
	// shutdown before the rack is touched). 30s is generous for a healthy
	// unmount, which is sub-second, and short enough that a dead server does
	// not stall a shutdown the operator is watching.
	UmountTimeout Duration `json:"umount_timeout"`

	// CGITimeout bounds a single CGI request served by the HTTP API. The
	// detached power job is spawned by the request but deliberately outlives
	// it (see powerapi's action handler), so this bounds only the request itself: the
	// fleet probe, the Shelly plug read, the peer job round trip and the
	// fan-guard re-confirm a `fans off` runs -- all of which finish in tens of
	// seconds on a healthy rack. It is a backstop so a request wedged on a
	// slow or dead backend aborts cleanly rather than holding the CGI
	// process open indefinitely.
	CGITimeout Duration `json:"cgi_timeout"`

	// GogiosURL is where f3sctl fetches the Gogios alert report from. The
	// default is the relayd-fronted, federated endpoint -- each OpenBSD
	// frontend peers with the other, so one fetch is the merged view. An
	// operator can point this at a per-frontend URL or a LAN address if that
	// endpoint is the wrong path from the API node.
	GogiosURL string `json:"gogios_url"`

	// GogiosFetchTimeout bounds the HTTP GET of the Gogios report.
	GogiosFetchTimeout Duration `json:"gogios_fetch_timeout"`

	// GogiosCacheTTL is how long the on-disk cache of the report is served
	// before f3sctl re-fetches. A browse session (overview, drill-down by
	// status, per-check detail) reads the report several times in quick
	// succession; the cache stops each click re-fetching. The report itself
	// is only refreshed by Gogios every few minutes, so a short TTL is mostly
	// a polite-traffic measure, not a freshness one.
	GogiosCacheTTL Duration `json:"gogios_cache_ttl"`
}

// Default returns the compiled-in configuration.
func Default() Config {
	return Config{
		Inventory: inventory.Default(),
		SSHIdentity: []string{
			"/var/db/f3sctl/id_ed25519",
			"~/.ssh/id_ed25519",
		},
		ShellyPasswordFile: []string{
			"/var/db/f3sctl/shelly_plug",
			"/keys/shelly_plug.secret",
			"~/.shelly_plug",
		},
		APIKeyFile: "/var/db/f3sctl/apikey",
		StateDir:   "/var/db/f3sctl",
		PeerNodes:  []string{"192.168.1.125", "192.168.1.126"},
		// PeerJobPath is left empty so it is derived from this node's own
		// SCRIPT_NAME at request time -- see the field's doc comment.
		PeerJobPath:        "",
		VMShutdownTimeout:  Duration(240 * time.Second),
		UnmuteTimeout:      Duration(1200 * time.Second),
		ProbeTimeout:       Duration(2 * time.Second),
		SSHConnectTimeout:  Duration(5 * time.Second),
		SSHVerbTimeout:     Duration(360 * time.Second),
		UmountTimeout:      Duration(30 * time.Second),
		CGITimeout:         Duration(120 * time.Second),
		GogiosURL:          "https://gogios.buetow.org/index.json",
		GogiosFetchTimeout: Duration(10 * time.Second),
		GogiosCacheTTL:     Duration(60 * time.Second),
	}
}

// Load returns the default config overlaid with the first config file found.
//
// If path is given it is used exactly; otherwise searchPath is tried in order.
// A missing file is not an error -- f3sctl is meant to work with no
// configuration at all -- but a malformed one is, because silently ignoring a
// config the operator wrote is worse than stopping.
func Load(path string) (Config, error) {
	cfg := Default()

	if path == "" {
		if found, ok := firstExisting(searchPath); ok {
			path = found
		} else {
			return cfg, nil
		}
	}

	raw, err := os.ReadFile(expandHome(path))
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("reading %s: %w", path, err)
	}

	// Decoding into the already-populated struct leaves absent keys at their
	// default, so a config file may override just one setting.
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg, nil
}

// ResolveSSHIdentity returns the first readable private key, or an error
// naming every candidate that was tried.
func (c Config) ResolveSSHIdentity() (string, error) {
	return firstReadable(c.SSHIdentity, "SSH identity")
}

// ResolveShellyPassword returns the first line of the first readable Shelly
// password file. Digest auth is mandatory on the plug, so a missing password
// is a hard error rather than a degraded mode.
func (c Config) ResolveShellyPassword() (string, error) {
	path, err := firstReadable(c.ShellyPasswordFile, "Shelly password file")
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	pw := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
	if pw == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return pw, nil
}

// ResolveAPIKey returns the API key for --remote mode.
//
// The environment wins over the config file so a second endpoint, or a rotated
// key, can be used without editing anything -- which is also what makes the
// tool easy to point at a test instance.
func (c Config) ResolveAPIKey() (string, error) {
	if k := strings.TrimSpace(os.Getenv("F3SCTL_KEY")); k != "" {
		return k, nil
	}
	raw, err := os.ReadFile(expandHome(c.APIKeyFile))
	if err != nil {
		return "", fmt.Errorf("reading the API key from %s: %w (or set F3SCTL_KEY)", c.APIKeyFile, err)
	}
	// The key file may list several accepted keys (one per line, for the
	// server side); a client presents only its own, which is the first
	// non-blank, non-comment line. A single-line file behaves as before.
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line, nil
	}
	return "", fmt.Errorf("%s is empty", c.APIKeyFile)
}

// ResolveAPIURL returns the endpoint --remote talks to.
func (c Config) ResolveAPIURL() string {
	if u := strings.TrimSpace(os.Getenv("F3SCTL_URL")); u != "" {
		return u
	}
	return c.APIURL
}

// firstExisting returns the first candidate path present on disk.
func firstExisting(candidates []string) (string, bool) {
	for _, c := range candidates {
		if _, err := os.Stat(expandHome(c)); err == nil {
			return c, true
		}
	}
	return "", false
}

// firstReadable returns the first candidate path that can actually be opened,
// after expanding a leading ~.
func firstReadable(candidates []string, what string) (string, error) {
	var tried []string
	for _, c := range candidates {
		p := expandHome(c)
		tried = append(tried, p)
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		// firstReadable only opens to confirm the file is openable; the close
		// error is not actionable and is explicitly discarded so errcheck (see
		// .golangci.yml) keeps flagging ignored write-path os.File closes.
		_ = f.Close()
		return p, nil
	}
	return "", fmt.Errorf("no readable %s; tried: %s", what, strings.Join(tried, ", "))
}

// expandHome expands a leading ~ using $HOME. It deliberately does not consult
// the password database: under doas/sudo $HOME is the target user's, which is
// exactly whose files we want.
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}

// Duration is a time.Duration that marshals as a Go duration string ("240s"),
// so the JSON config stays readable instead of carrying raw nanoseconds.
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts either a duration string ("4m") or a plain number of
// seconds, so hand-written configs can use whichever reads better.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		*d = Duration(parsed)
		return nil
	}

	var secs float64
	if err := json.Unmarshal(b, &secs); err != nil {
		return fmt.Errorf("duration must be a string like \"4m\" or a number of seconds")
	}
	*d = Duration(time.Duration(secs * float64(time.Second)))
	return nil
}
