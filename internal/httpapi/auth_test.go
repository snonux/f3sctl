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
