package infra

import (
	"strings"
	"testing"
)

// TestParseChallengeHandlesCommasInQuotes checks the one genuinely fiddly part
// of the digest handshake: a realm or nonce may contain a comma, so the
// challenge cannot be split naively on commas.
func TestParseChallengeHandlesCommasInQuotes(t *testing.T) {
	got := parseChallenge(`realm="shelly, plug", nonce="ab,cd", algorithm=SHA-256, qop="auth"`)

	want := map[string]string{
		"realm":     "shelly, plug",
		"nonce":     "ab,cd",
		"algorithm": "SHA-256",
		"qop":       "auth",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// TestDigestAuthUsesTheAdvertisedAlgorithm checks that a SHA-256 challenge is
// answered with SHA-256 and an unspecified one with MD5, as RFC 2617 requires.
// Answering with the wrong hash produces a 401 loop that looks like a wrong
// password.
func TestDigestAuthUsesTheAdvertisedAlgorithm(t *testing.T) {
	sha, err := digestAuth(`Digest realm="shelly", nonce="abc", algorithm=SHA-256, qop="auth"`,
		"admin", "pw", "GET", "/rpc/Switch.GetStatus?id=0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(sha, "algorithm=SHA-256") {
		t.Errorf("SHA-256 challenge not echoed back: %s", sha)
	}

	md5, err := digestAuth(`Digest realm="shelly", nonce="abc", qop="auth"`,
		"admin", "pw", "GET", "/rpc/Switch.GetStatus?id=0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(md5, "algorithm=MD5") {
		t.Errorf("an unspecified algorithm should default to MD5: %s", md5)
	}

	// The two must not produce the same response, or the algorithm selection
	// is not actually taking effect.
	if responseOf(sha) == responseOf(md5) {
		t.Error("SHA-256 and MD5 produced the same digest response")
	}
}

// TestDigestAuthRejectsNonDigest checks we fail with a clear message if the
// plug ever answers with Basic or something unexpected, rather than sending a
// malformed header and getting an opaque 401.
func TestDigestAuthRejectsNonDigest(t *testing.T) {
	if _, err := digestAuth(`Basic realm="shelly"`, "admin", "pw", "GET", "/"); err == nil {
		t.Error("a Basic challenge was accepted as digest")
	}
	if _, err := digestAuth(`Digest realm="shelly"`, "admin", "pw", "GET", "/"); err == nil {
		t.Error("a challenge with no nonce was accepted")
	}
}

func responseOf(header string) string {
	_, after, ok := strings.Cut(header, `response="`)
	if !ok {
		return ""
	}
	resp, _, _ := strings.Cut(after, `"`)
	return resp
}
