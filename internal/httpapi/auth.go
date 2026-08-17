package httpapi

import (
	"crypto/subtle"
	"fmt"
	"os"
	"strings"
)

// Authenticator checks a request's API key against the keys configured for
// this node.
//
// The key file may list several accepted keys, one per line, so a new client
// can be issued its own key without rotating the key every existing client
// shares. A single-line file behaves exactly as before -- it is just a list
// of length one -- so this is backward compatible with a deployed `apikey`
// file. Blank lines and lines beginning with '#' are ignored, so an operator
// can label which client a key belongs to:
//
//	# pebble watchface (earth, 2026-08)
//	9f3a...
//	# laptop CLI (earth, 2026-08)
//	c71e...
//
// The key is never accepted in the query string: bozohttpd logs request URIs
// to syslog and relayd logs connections, so a key in a URL would end up in
// two logs on three machines.
//
// Split out of Server so the comparison -- the one piece of this package that
// is a security control rather than a rendering or routing decision -- can be
// read, tested and reasoned about on its own.
type Authenticator struct {
	// KeyFile is the path to the file holding the accepted API key(s), one
	// per line.
	KeyFile string
}

// NewAuthenticator returns an Authenticator that reads the accepted key(s)
// from keyFile on every Check, rather than caching them, so editing the file
// (adding a key, revoking one, rotating) takes effect on the very next
// request without restarting anything.
func NewAuthenticator(keyFile string) *Authenticator {
	return &Authenticator{KeyFile: keyFile}
}

// Check compares apiKey against every accepted key, returning nil only for an
// exact match against one of them.
func (a *Authenticator) Check(apiKey string) error {
	raw, err := os.ReadFile(a.KeyFile)
	if err != nil {
		return fmt.Errorf("reading the API key file: %w", err)
	}

	keys := parseKeys(string(raw))
	if len(keys) == 0 {
		return fmt.Errorf("the API key file is empty")
	}

	// Each comparison is constant-time, so the loop cannot be turned into an
	// oracle for guessing a key one byte at a time. Returning on the first
	// match only tells a valid client its own key was accepted, which it
	// already knows; an invalid client falls through every comparison.
	for _, want := range keys {
		if subtle.ConstantTimeCompare([]byte(apiKey), []byte(want)) == 1 {
			return nil
		}
	}
	return fmt.Errorf("bad API key")
}

// parseKeys splits the key file into the list of accepted keys. Each non-blank,
// non-comment line is one key, with surrounding whitespace trimmed, so a
// trailing newline or an inline annotation never becomes part of a key.
//
// A line beginning with '#' is a comment, for labelling which client a key
// belongs to; a generated key never starts with '#'.
func parseKeys(raw string) []string {
	var keys []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keys = append(keys, line)
	}
	return keys
}