package httpapi

import (
	"os"
	"path/filepath"
	"testing"
)

// keyFile writes key (verbatim, so a caller can include trailing whitespace
// to test trimming) to a temp file and returns its path.
func keyFile(t *testing.T, key string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "apikey")
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		t.Fatalf("writing key file: %v", err)
	}
	return path
}

// TestAuthenticatorAcceptsTheConfiguredKey is the positive case: the one key
// that must always be let through.
func TestAuthenticatorAcceptsTheConfiguredKey(t *testing.T) {
	a := NewAuthenticator(keyFile(t, "sekrit\n"))
	if err := a.Check("sekrit"); err != nil {
		t.Errorf("Check(right key) = %v, want nil", err)
	}
}

// TestAuthenticatorRejectsAWrongKey pins the failure path a client hits on
// every typo or stale credential.
func TestAuthenticatorRejectsAWrongKey(t *testing.T) {
	a := NewAuthenticator(keyFile(t, "sekrit\n"))
	if err := a.Check("wrong"); err == nil {
		t.Error("Check(wrong key) = nil, want an error")
	}
}

// TestAuthenticatorRejectsAnEmptyKey covers the client that sends no
// X-API-Key header at all, which arrives here as an empty string.
func TestAuthenticatorRejectsAnEmptyKey(t *testing.T) {
	a := NewAuthenticator(keyFile(t, "sekrit\n"))
	if err := a.Check(""); err == nil {
		t.Error("Check(\"\") = nil, want an error")
	}
}

// TestAuthenticatorRejectsAnEmptyConfiguredKey guards against a misconfigured
// node silently accepting anything: an empty key file must refuse every
// request rather than being treated as "no key required".
func TestAuthenticatorRejectsAnEmptyConfiguredKey(t *testing.T) {
	a := NewAuthenticator(keyFile(t, "\n"))
	for _, apiKey := range []string{"", "anything"} {
		if err := a.Check(apiKey); err == nil {
			t.Errorf("Check(%q) = nil with an empty configured key, want an error", apiKey)
		}
	}
}

// TestAuthenticatorMissingKeyFile is the server-fault case: an unreadable key
// file must be reported as an error, not silently treated as "no key".
func TestAuthenticatorMissingKeyFile(t *testing.T) {
	a := NewAuthenticator(filepath.Join(t.TempDir(), "does-not-exist"))
	if err := a.Check("anything"); err == nil {
		t.Error("Check with a missing key file = nil, want an error")
	}
}

// TestAuthenticatorTrimsWhitespace pins that a trailing newline in the key
// file (which is how the key is normally written to disk) is not itself part
// of the key.
func TestAuthenticatorTrimsWhitespace(t *testing.T) {
	a := NewAuthenticator(keyFile(t, "  sekrit  \n"))
	if err := a.Check("sekrit"); err != nil {
		t.Errorf("Check(\"sekrit\") = %v, want nil once surrounding whitespace is trimmed", err)
	}
	if err := a.Check("  sekrit  "); err == nil {
		t.Error("Check(\"  sekrit  \") = nil, want an error: the client's key is compared verbatim")
	}
}

// TestAuthenticatorRereadsTheKeyFile pins that the key is not cached, so
// rotating it on disk takes effect without restarting anything.
func TestAuthenticatorRereadsTheKeyFile(t *testing.T) {
	path := keyFile(t, "old\n")
	a := NewAuthenticator(path)

	if err := a.Check("old"); err != nil {
		t.Fatalf("Check(\"old\") before rotation = %v, want nil", err)
	}

	if err := os.WriteFile(path, []byte("new\n"), 0o600); err != nil {
		t.Fatalf("rotating the key file: %v", err)
	}

	if err := a.Check("old"); err == nil {
		t.Error("Check(\"old\") after rotation = nil, want an error: the old key must no longer work")
	}
	if err := a.Check("new"); err != nil {
		t.Errorf("Check(\"new\") after rotation = %v, want nil", err)
	}
}

// TestAuthenticatorAcceptsAnyListedKey pins the multi-key feature: a file with
// one key per line lets each client be issued its own key without rotating
// the key everyone else shares. Every listed key must be accepted.
func TestAuthenticatorAcceptsAnyListedKey(t *testing.T) {
	a := NewAuthenticator(keyFile(t, "alpha\nbravo\ncharlie\n"))
	for _, k := range []string{"alpha", "bravo", "charlie"} {
		if err := a.Check(k); err != nil {
			t.Errorf("Check(%q) = %v, want nil", k, err)
		}
	}
}

// TestAuthenticatorRejectsAnUnlistedKey is the negative side of the multi-key
// feature: a key that is not one of the listed lines is refused.
func TestAuthenticatorRejectsAnUnlistedKey(t *testing.T) {
	a := NewAuthenticator(keyFile(t, "alpha\nbravo\n"))
	if err := a.Check("charlie"); err == nil {
		t.Error("Check(\"charlie\") = nil, want an error")
	}
}

// TestAuthenticatorIgnoresBlankAndCommentLines pins that blank lines and '#'
// comments are skipped, so an operator can label which client a key belongs
// to without the label becoming part of a key. It also covers the trailing
// newline that a normal file carries: that must not count as an empty
// configured key.
func TestAuthenticatorIgnoresBlankAndCommentLines(t *testing.T) {
	a := NewAuthenticator(keyFile(t, "# pebble watchface\nalpha\n\n# laptop CLI\nbravo\n"))
	for _, k := range []string{"alpha", "bravo"} {
		if err := a.Check(k); err != nil {
			t.Errorf("Check(%q) = %v, want nil", k, err)
		}
	}
	for _, k := range []string{"# pebble watchface", "", "bravo\nalpha"} {
		if err := a.Check(k); err == nil {
			t.Errorf("Check(%q) = nil, want an error", k)
		}
	}
}

// TestAuthenticatorEmptyFileRejectsAll guards against a misconfigured node
// silently accepting anything: a file with only blank lines and comments has
// no accepted keys, so every request must be refused rather than treated as
// "no key required".
func TestAuthenticatorEmptyFileRejectsAll(t *testing.T) {
	a := NewAuthenticator(keyFile(t, "\n# just a comment\n\n"))
	for _, apiKey := range []string{"", "anything", "alpha"} {
		if err := a.Check(apiKey); err == nil {
			t.Errorf("Check(%q) = nil with no keys configured, want an error", apiKey)
		}
	}
}

// TestAuthenticatorRevocationTakesEffectWithoutRestart confirms that removing
// a key from the file (revoking a client) is picked up on the next request,
// which is the operational reason the file is re-read rather than cached.
func TestAuthenticatorRevocationTakesEffectWithoutRestart(t *testing.T) {
	path := keyFile(t, "alpha\nbravo\n")
	a := NewAuthenticator(path)

	if err := a.Check("bravo"); err != nil {
		t.Fatalf("Check(\"bravo\") before revocation = %v, want nil", err)
	}

	if err := os.WriteFile(path, []byte("alpha\n"), 0o600); err != nil {
		t.Fatalf("revoking bravo: %v", err)
	}

	if err := a.Check("bravo"); err == nil {
		t.Error("Check(\"bravo\") after revocation = nil, want an error")
	}
	if err := a.Check("alpha"); err != nil {
		t.Errorf("Check(\"alpha\") after revocation = %v, want nil", err)
	}
}
