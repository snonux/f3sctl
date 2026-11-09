package infra

// This file holds the TLS half of the Shelly mechanism added for task p34:
// building the pinned-CA client config, deriving the HTTPS address, and
// telling a failed dial (safe to retry over HTTP) apart from every other
// failure (never retried).

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
)

// shellyHTTPSPort is where the plug firmware's HTTPS listener sits; it is not
// configurable on the device.
const shellyHTTPSPort = "443"

// NewShellyTLSConfig returns a client TLS config that trusts exactly the CA
// certificates in caPEM. The system roots are deliberately not added: the
// plugs carry certificates from f3s's private CA, and a public CA has no say
// over who is a plug. Hostname verification stays on (Go checks the dialled
// IP against the certificate's IP SANs), InsecureSkipVerify is never set,
// and TLS 1.2 is the floor because it is all the firmware speaks.
func NewShellyTLSConfig(caPEM []byte) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("no CA certificate found in the Shelly CA PEM")
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// httpsAddr is the "host:port" an HTTPS call dials: HTTPSAddr when set,
// otherwise IP's host (any HTTP port stripped) on 443.
func (c *ShellyClient) httpsAddr() string {
	if c.HTTPSAddr != "" {
		return c.HTTPSAddr
	}
	host := c.IP
	if h, _, err := net.SplitHostPort(c.IP); err == nil {
		host = h
	}
	return net.JoinHostPort(host, shellyHTTPSPort)
}

// isDialError reports whether err is a failure to establish the TCP
// connection at all -- refused, unreachable, timed out while connecting. Such
// a request never reached the plug, which is what makes an HTTP retry safe.
// A TLS handshake or certificate error is not a dial error.
func isDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}
