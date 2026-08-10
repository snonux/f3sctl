package httpapi

import (
	"crypto/subtle"
	"fmt"
	"os"
	"strings"
)

// Authenticator checks a request's API key against the key configured for
// this node.
//
// The key is never accepted in the query string: bozohttpd logs request URIs
// to syslog and relayd logs connections, so a key in a URL would end up in
// two logs on three machines.
//
// Split out of Server so the comparison -- the one piece of this package that
// is a security control rather than a rendering or routing decision -- can be
// read, tested and reasoned about on its own.
type Authenticator struct {
	// KeyFile is the path to the file holding the expected API key.
	KeyFile string
}

// NewAuthenticator returns an Authenticator that reads the expected key from
// keyFile on every Check, rather than caching it, so rotating the key on disk
// takes effect on the very next request without restarting anything.
func NewAuthenticator(keyFile string) *Authenticator {
	return &Authenticator{KeyFile: keyFile}
}

// Check compares apiKey against the configured key, returning nil only for an
// exact match.
func (a *Authenticator) Check(apiKey string) error {
	want, err := os.ReadFile(a.KeyFile)
	if err != nil {
		return fmt.Errorf("reading the API key file: %w", err)
	}

	wantTrimmed := strings.TrimSpace(string(want))
	if wantTrimmed == "" {
		return fmt.Errorf("the API key file is empty")
	}

	// Constant-time so the comparison cannot be turned into an oracle for
	// guessing the key one byte at a time.
	if subtle.ConstantTimeCompare([]byte(apiKey), []byte(wantTrimmed)) != 1 {
		return fmt.Errorf("bad API key")
	}
	return nil
}
