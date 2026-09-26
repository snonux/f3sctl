package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/power"
)

// setCGIEnv sets the CGI environment parseCGIRequest reads for an
// authenticated GET of path.
func setCGIEnv(t *testing.T, path, apiKey string) {
	t.Helper()
	t.Setenv("REQUEST_METHOD", http.MethodGet)
	t.Setenv("PATH_INFO", path)
	t.Setenv("QUERY_STRING", "")
	t.Setenv("HTTP_X_API_KEY", apiKey)
	t.Setenv("SCRIPT_NAME", "")
	t.Setenv("CONTENT_LENGTH", "")
}

// assertPanicAnsweredAs500 checks that a recovered panic reached the client
// as a generic Siren 500 -- without the panic's own text -- and reached the
// log with its value and a stack trace.
func assertPanicAnsweredAs500(t *testing.T, out, logw, panicText string) {
	t.Helper()
	headers, body := splitCGIResponse(t, out)
	if !strings.HasPrefix(headers["Status"], "500") {
		t.Errorf("Status = %q, want 500", headers["Status"])
	}
	var e contract.Entity
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("body is not a Siren entity: %v\nbody: %s", err, body)
	}
	if e.Properties["message"] != "internal server error" {
		t.Errorf("message = %v, want \"internal server error\"", e.Properties["message"])
	}
	if strings.Contains(out, panicText) {
		t.Error("the panic's text leaked into the response")
	}
	if !strings.Contains(logw, panicText) || !strings.Contains(logw, "goroutine") {
		t.Errorf("log = %q, want the panic value and a stack trace", logw)
	}
}

// TestServeCGIAnswersAHandlerPanicWithA500 drives a request whose serving
// panics -- through the fleet-probe seam /status calls -- and requires a
// Siren 500 for the client and the panic, with its stack, on the log.
func TestServeCGIAnswersAHandlerPanicWithA500(t *testing.T) {
	setCGIEnv(t, "/status", "sekrit")
	srv := docServer(t, docOpts{})
	srv.probeHosts = func(context.Context) []power.HostStatus { panic("probe exploded") }

	var out, logw bytes.Buffer
	err := serveCGI(srv.cfg, strings.NewReader(""), &out, &logw,
		func(config.Config) (*Server, error) { return srv, nil })
	if err != nil {
		t.Fatalf("serveCGI: %v", err)
	}
	assertPanicAnsweredAs500(t, out.String(), logw.String(), "probe exploded")
}

// TestServeCGIAnswersAConstructionPanicWithA500 covers the other half of
// what the recover guards: a panic while the Server is still being built
// (a nil ActionRenderer handed to a surface constructor, say).
func TestServeCGIAnswersAConstructionPanicWithA500(t *testing.T) {
	setCGIEnv(t, "/status", "sekrit")

	var out, logw bytes.Buffer
	err := serveCGI(config.Default(), strings.NewReader(""), &out, &logw,
		func(config.Config) (*Server, error) { panic("wiring exploded") })
	if err != nil {
		t.Fatalf("serveCGI: %v", err)
	}
	assertPanicAnsweredAs500(t, out.String(), logw.String(), "wiring exploded")
}
