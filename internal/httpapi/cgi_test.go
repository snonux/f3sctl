package httpapi

import (
	"strings"
	"testing"
)

// TestParseCGIRequestReadsTheVerifiedVariables pins the CGI contract this API
// depends on, as measured against bozohttpd 20260508 on NetBSD 11.0.
func TestParseCGIRequestReadsTheVerifiedVariables(t *testing.T) {
	t.Setenv("REQUEST_METHOD", "POST")
	t.Setenv("PATH_INFO", "/fans/off")
	t.Setenv("QUERY_STRING", "force=true&other=1")
	t.Setenv("HTTP_X_API_KEY", "sekrit")
	t.Setenv("CONTENT_LENGTH", "10")

	req, err := parseCGIRequest(strings.NewReader("force=true"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if req.Method != "POST" {
		t.Errorf("method = %q", req.Method)
	}
	if req.Path != "/fans/off" {
		t.Errorf("path = %q", req.Path)
	}
	if req.APIKey != "sekrit" {
		t.Errorf("api key = %q; bozohttpd exports X-API-Key as HTTP_X_API_KEY", req.APIKey)
	}
	if !req.BoolField("force") {
		t.Error("force field not read from the body")
	}
}

// TestBoolFieldAcceptsCommonSpellings keeps a client from being tripped up by
// how its HTTP library encodes a checkbox.
func TestBoolFieldAcceptsCommonSpellings(t *testing.T) {
	t.Setenv("REQUEST_METHOD", "GET")
	t.Setenv("PATH_INFO", "/fans/off")

	for _, spelling := range []string{"true", "TRUE", "1", "on", "yes"} {
		t.Setenv("QUERY_STRING", "force="+spelling)
		req, err := parseCGIRequest(strings.NewReader(""))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !req.BoolField("force") {
			t.Errorf("force=%s was not read as true", spelling)
		}
	}

	for _, spelling := range []string{"false", "0", "off", "no", ""} {
		t.Setenv("QUERY_STRING", "force="+spelling)
		req, _ := parseCGIRequest(strings.NewReader(""))
		if req.BoolField("force") {
			t.Errorf("force=%q was read as true", spelling)
		}
	}
}

// TestNormalisePath makes "/status" and "/status/" the same resource, and maps
// a missing PATH_INFO (which is what bozohttpd gives for the bare script URL)
// to the root.
func TestNormalisePath(t *testing.T) {
	cases := map[string]string{
		"":           "/",
		"/":          "/",
		"/status":    "/status",
		"/status/":   "/status",
		"/power/off": "/power/off",
	}
	for in, want := range cases {
		if got := normalisePath(in); got != want {
			t.Errorf("normalisePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBodyIsBounded checks the body read is capped. Every action here takes at
// most one small field, so an unbounded read would only ever serve an attacker.
func TestBodyIsBounded(t *testing.T) {
	t.Setenv("REQUEST_METHOD", "POST")
	t.Setenv("PATH_INFO", "/fans/off")
	t.Setenv("QUERY_STRING", "")
	t.Setenv("CONTENT_LENGTH", "999999999")

	huge := strings.NewReader("force=true&pad=" + strings.Repeat("x", 1<<20))
	req, err := parseCGIRequest(huge)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(req.Form.Get("pad")) >= 1<<20 {
		t.Error("the request body was not truncated at maxBodyBytes")
	}
}
