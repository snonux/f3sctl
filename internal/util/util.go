// Package util holds tiny, purely-syntactic helpers with no domain
// knowledge of their own -- small enough that splitting each into its own
// file-scoped package would be worse than the one import this costs.
//
// Anything here is a candidate to move out again the moment it grows a
// dependency on power, httpapi, or any other f3sctl package: this package
// must stay a leaf so every other package can depend on it without risking a
// cycle.
package util

// OrDefault returns s, or def if s is empty.
//
// Used wherever an optional input (an environment variable, a parsed
// challenge parameter) needs a fallback when unset -- see
// internal/httpapi/cgi.go (CGI's REQUEST_METHOD) and internal/power/shelly.go
// (the Shelly digest challenge's algorithm parameter, which RFC 2617 says
// defaults to MD5 when absent). One implementation rather than two copies
// that could quietly diverge on what "empty" means.
func OrDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
