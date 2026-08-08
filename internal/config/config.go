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

// Config is everything f3sctl needs beyond the host inventory.
type Config struct {
	Inventory inventory.Inventory `json:"inventory"`

	// SSHIdentity lists candidate private keys, most specific first. The
	// first readable one is used.
	SSHIdentity []string `json:"ssh_identity"`
	// ShellyPasswordFile lists candidate files whose first line is the
	// Shelly plug's digest password.
	ShellyPasswordFile []string `json:"shelly_password_file"`
	// APIKeyFile holds the single API key accepted in CGI mode.
	APIKeyFile string `json:"api_key_file"`
	// StateDir holds the job lock, job state and job log.
	StateDir string `json:"state_dir"`

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
		UnmuteTimeout:     Duration(600 * time.Second),
		ProbeTimeout:      Duration(2 * time.Second),
		SSHConnectTimeout: Duration(5 * time.Second),
	}
}

// Load returns the default config overlaid with path, if that file exists.
// A missing file is not an error; a malformed one is.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		path = DefaultPath
	}

	raw, err := os.ReadFile(path)
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
