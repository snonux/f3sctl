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
	// APIKeyFile holds the API key: the single key accepted in CGI mode, and
	// the key presented in --remote mode.
	APIKeyFile string `json:"api_key_file"`
	// StateDir holds the job lock, job state and job log.
	StateDir string `json:"state_dir"`

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
		APIKeyFile:        "/var/db/f3sctl/apikey",
		StateDir:          "/var/db/f3sctl",
		VMShutdownTimeout: Duration(240 * time.Second),
		UnmuteTimeout:     Duration(1200 * time.Second),
		ProbeTimeout:      Duration(2 * time.Second),
		SSHConnectTimeout: Duration(5 * time.Second),
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
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return "", fmt.Errorf("%s is empty", c.APIKeyFile)
	}
	return key, nil
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
		f.Close()
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
