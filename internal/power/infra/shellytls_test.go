package infra

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests exercise the HTTPS path added for task p34 against real TLS
// listeners (httptest) and a fake plug that really verifies SHA-256 digest
// auth, so a pass means the client completed a verified handshake and a
// correct challenge-response -- not that a mock was called.

const testPassword = "correct horse"

// digestPlug is a fake Shelly plug that demands SHA-256 digest auth on every
// /rpc call, checks the response hash against testPassword, and answers
// Switch.GetStatus. hits counts every request that reached the handler, which
// is how the no-fallback tests prove nothing was sent over plain HTTP.
type digestPlug struct {
	hits atomic.Int32
}

func (p *digestPlug) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.hits.Add(1)
	const realm, nonce = "shellyplugmg3-test", "n0nce"
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Digest ") || !validDigest(auth[len("Digest "):], realm, nonce, r) {
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Digest qop="auth", realm="%s", nonce="%s", algorithm=SHA-256`, realm, nonce))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.URL.Path != "/rpc/Switch.GetStatus" {
		http.Error(w, "unexpected path", http.StatusNotFound)
		return
	}
	_, _ = w.Write([]byte(`{"id":0,"output":true}`))
}

// validDigest recomputes the RFC 7616 response the way the plug does.
func validDigest(header, realm, nonce string, r *http.Request) bool {
	p := parseChallenge(header)
	if p["username"] != "admin" || p["realm"] != realm || p["nonce"] != nonce || p["uri"] != r.URL.RequestURI() {
		return false
	}
	ha1 := sha256Hex("admin:" + realm + ":" + testPassword)
	ha2 := sha256Hex(r.Method + ":" + p["uri"])
	want := sha256Hex(strings.Join([]string{ha1, nonce, p["nc"], p["cnonce"], "auth", ha2}, ":"))
	return p["response"] == want
}

// newTLSPlug starts an HTTPS fake plug and returns it with the PEM of the
// certificate it serves (httptest's cert is self-signed, so it is its own CA;
// its SANs include 127.0.0.1, the address dialled).
func newTLSPlug(t *testing.T) (*httptest.Server, *digestPlug, []byte) {
	t.Helper()
	plug := &digestPlug{}
	srv := httptest.NewTLSServer(plug)
	t.Cleanup(srv.Close)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	return srv, plug, caPEM
}

// newHTTPPlug starts a plain-HTTP fake plug, the plug's port-80 side.
func newHTTPPlug(t *testing.T) (*httptest.Server, *digestPlug) {
	t.Helper()
	plug := &digestPlug{}
	srv := httptest.NewServer(plug)
	t.Cleanup(srv.Close)
	return srv, plug
}

// closedAddr returns a loopback address nothing listens on, so dialling it
// fails at connect time the way a filtered or refused port 443 does.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// selfSignedPEM mints a throwaway EC P-256 CA certificate unrelated to the one
// httptest serves -- the "wrong CA" a misconfigured client would pin.
func selfSignedPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "wrong CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func pinned(t *testing.T, caPEM []byte) func() (*tls.Config, error) {
	t.Helper()
	cfg, err := NewShellyTLSConfig(caPEM)
	if err != nil {
		t.Fatalf("NewShellyTLSConfig: %v", err)
	}
	return func() (*tls.Config, error) { return cfg, nil }
}

func addrOf(srv *httptest.Server) string {
	return srv.Listener.Addr().String()
}

func client(httpAddr, httpsAddr, password string, tlsFn func() (*tls.Config, error), fallback bool) *ShellyClient {
	return &ShellyClient{
		IP:           httpAddr,
		HTTPSAddr:    httpsAddr,
		TLS:          tlsFn,
		HTTPFallback: fallback,
		Password:     func() (string, error) { return password, nil },
		Timeout:      5 * time.Second,
	}
}

// TestStatusOverHTTPSWithPinnedCA is the positive path: a verified TLS
// handshake against the pinned CA plus a correct digest exchange.
func TestStatusOverHTTPSWithPinnedCA(t *testing.T) {
	srv, plug, caPEM := newTLSPlug(t)
	c := client(closedAddr(t), addrOf(srv), testPassword, pinned(t, caPEM), false)

	on, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status over HTTPS: %v", err)
	}
	if !on {
		t.Error("Status = off, want the fake's on")
	}
	if got := plug.hits.Load(); got != 2 {
		t.Errorf("HTTPS plug saw %d requests, want 2 (challenge + authenticated)", got)
	}
}

// TestHTTPSRejectsAPlugTheCADoesNotVouchFor pins that an untrusted certificate
// is an error even with the HTTP fallback enabled: a TLS failure is not a dial
// failure, so nothing may be sent over plain HTTP.
func TestHTTPSRejectsAPlugTheCADoesNotVouchFor(t *testing.T) {
	srv, tlsPlug, _ := newTLSPlug(t)
	otherCA := selfSignedPEM(t) // httptest's own cert is shared, so mint a distinct CA
	httpSrv, httpPlug := newHTTPPlug(t)
	c := client(addrOf(httpSrv), addrOf(srv), testPassword, pinned(t, otherCA), true)

	_, err := c.Status(context.Background())
	if err == nil {
		t.Fatal("Status succeeded against a certificate the pinned CA did not issue")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Errorf("error %q does not name the certificate problem", err)
	}
	if got := httpPlug.hits.Load(); got != 0 {
		t.Errorf("plain-HTTP plug saw %d requests after a TLS failure, want 0 (no downgrade)", got)
	}
	if got := tlsPlug.hits.Load(); got != 0 {
		t.Errorf("HTTPS handler saw %d requests through an unverified handshake, want 0", got)
	}
}

// TestHTTPSWrongPasswordFails pins that TLS does not paper over auth: with a
// trusted certificate but the wrong password the plug keeps answering 401 and
// the call fails without falling back.
func TestHTTPSWrongPasswordFails(t *testing.T) {
	srv, _, caPEM := newTLSPlug(t)
	httpSrv, httpPlug := newHTTPPlug(t)
	c := client(addrOf(httpSrv), addrOf(srv), "wrong", pinned(t, caPEM), true)

	_, err := c.Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("Status with a wrong password = %v, want a 401 error", err)
	}
	if got := httpPlug.hits.Load(); got != 0 {
		t.Errorf("plain-HTTP plug saw %d requests after an auth failure, want 0", got)
	}
}

// TestHTTPFallbackOnlyWhenHTTPSCannotBeDialled covers the migration aid: a
// refused 443 retries over HTTP when, and only when, the fallback is enabled.
func TestHTTPFallbackOnlyWhenHTTPSCannotBeDialled(t *testing.T) {
	_, _, caPEM := newTLSPlug(t)
	httpSrv, httpPlug := newHTTPPlug(t)
	dead := closedAddr(t)

	on, err := client(addrOf(httpSrv), dead, testPassword, pinned(t, caPEM), true).Status(context.Background())
	if err != nil || !on {
		t.Fatalf("Status with fallback = %v, %v; want on via plain HTTP", on, err)
	}
	if got := httpPlug.hits.Load(); got != 2 {
		t.Errorf("plain-HTTP plug saw %d requests, want 2 (challenge + authenticated)", got)
	}

	_, err = client(addrOf(httpSrv), dead, testPassword, pinned(t, caPEM), false).Status(context.Background())
	if err == nil {
		t.Fatal("Status without fallback succeeded although HTTPS could not be dialled")
	}
	if !isDialError(err) {
		t.Errorf("error %q is not reported as a dial failure", err)
	}
}

// TestTLSSetupErrorDoesNotFallBack pins that a missing or broken CA is a hard
// error: it is a misconfiguration, not an unreachable port.
func TestTLSSetupErrorDoesNotFallBack(t *testing.T) {
	httpSrv, httpPlug := newHTTPPlug(t)
	broken := func() (*tls.Config, error) { return nil, errors.New("no readable Shelly CA file") }
	c := client(addrOf(httpSrv), closedAddr(t), testPassword, broken, true)

	_, err := c.Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Shelly CA") {
		t.Fatalf("Status with a broken CA = %v, want the CA error", err)
	}
	if got := httpPlug.hits.Load(); got != 0 {
		t.Errorf("plain-HTTP plug saw %d requests, want 0", got)
	}
}

// TestPlainHTTPUnchangedWithoutTLS pins the pre-p34 behaviour: no TLS func
// means plain HTTP to IP, whatever HTTPSAddr says.
func TestPlainHTTPUnchangedWithoutTLS(t *testing.T) {
	httpSrv, httpPlug := newHTTPPlug(t)
	c := client(addrOf(httpSrv), closedAddr(t), testPassword, nil, false)

	if on, err := c.Status(context.Background()); err != nil || !on {
		t.Fatalf("Status over plain HTTP = %v, %v; want on", on, err)
	}
	if got := httpPlug.hits.Load(); got != 2 {
		t.Errorf("plain-HTTP plug saw %d requests, want 2", got)
	}
}

// TestNewShellyTLSConfig checks the config is pinned and strict, and that a
// PEM without a certificate is refused rather than yielding an empty pool
// (which Go would treat as "trust nothing" -- safe, but a confusing failure).
func TestNewShellyTLSConfig(t *testing.T) {
	_, _, caPEM := newTLSPlug(t)
	cfg, err := NewShellyTLSConfig(caPEM)
	if err != nil {
		t.Fatalf("NewShellyTLSConfig: %v", err)
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify is set")
	}
	if cfg.RootCAs == nil {
		t.Error("RootCAs is nil, which would fall back to the system roots")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2", cfg.MinVersion)
	}

	for name, bad := range map[string][]byte{
		"empty":    nil,
		"garbage":  []byte("not a pem"),
		"key-only": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2, 3}}),
	} {
		if _, err := NewShellyTLSConfig(bad); err == nil {
			t.Errorf("%s: NewShellyTLSConfig accepted a PEM with no certificate", name)
		}
	}
}

// TestHTTPSAddr pins the address derivation: the plug's HTTP port (if any) is
// replaced by 443, and an explicit HTTPSAddr wins.
func TestHTTPSAddr(t *testing.T) {
	for _, tc := range []struct{ ip, override, want string }{
		{"10.30.0.28", "", "10.30.0.28:443"},
		{"10.30.0.28:80", "", "10.30.0.28:443"},
		{"shelly1.lan", "", "shelly1.lan:443"},
		{"10.30.0.28", "127.0.0.1:8443", "127.0.0.1:8443"},
	} {
		c := &ShellyClient{IP: tc.ip, HTTPSAddr: tc.override}
		if got := c.httpsAddr(); got != tc.want {
			t.Errorf("httpsAddr(IP=%q, HTTPSAddr=%q) = %q, want %q", tc.ip, tc.override, got, tc.want)
		}
	}
}
