package httpapi

import (
	"github.com/snonux/f3sctl/internal/httpapi/contract"

	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// splitCGIResponse separates the CGI header block from the body, the same
// split bozohttpd performs on the blank line.
func splitCGIResponse(t *testing.T, raw string) (headers map[string]string, body string) {
	t.Helper()
	parts := strings.SplitN(raw, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatalf("response has no header/body separator: %q", raw)
	}
	headers = map[string]string{}
	for _, line := range strings.Split(parts[0], "\r\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			t.Fatalf("malformed header line: %q", line)
		}
		headers[k] = v
	}
	return headers, parts[1]
}

// TestSirenRendererWriteEntityShape pins the wire format: CRLF headers, the
// Siren media type, and the entity marshalled as its own JSON body.
func TestSirenRendererWriteEntityShape(t *testing.T) {
	var buf bytes.Buffer
	e := contract.Entity{Class: []string{"status"}, Properties: map[string]any{"node": "pi0"}}

	if err := (SirenRenderer{}).WriteEntity(&buf, http.StatusOK, e); err != nil {
		t.Fatalf("WriteEntity: %v", err)
	}

	headers, body := splitCGIResponse(t, buf.String())
	if headers["Status"] != "200 OK" {
		t.Errorf("Status header = %q, want %q", headers["Status"], "200 OK")
	}
	if headers["Content-Type"] != sirenMediaType {
		t.Errorf("Content-Type = %q, want %q", headers["Content-Type"], sirenMediaType)
	}
	if headers["Cache-Control"] != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", headers["Cache-Control"], "no-store")
	}

	var got contract.Entity
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", err, body)
	}
	if len(got.Class) != 1 || got.Class[0] != "status" {
		t.Errorf("decoded entity class = %v, want [status]", got.Class)
	}
}

// TestSirenRendererWriteErrorShape pins the error entity's shape: it carries
// the same "class"/"properties" envelope as any other entity, so a client
// needs only one parser for both.
func TestSirenRendererWriteErrorShape(t *testing.T) {
	var buf bytes.Buffer
	if err := (SirenRenderer{}).WriteError(&buf, http.StatusNotFound, "no such resource: /nope"); err != nil {
		t.Fatalf("WriteError: %v", err)
	}

	headers, body := splitCGIResponse(t, buf.String())
	if headers["Status"] != "404 Not Found" {
		t.Errorf("Status header = %q, want %q", headers["Status"], "404 Not Found")
	}
	if headers["Content-Type"] != sirenMediaType {
		t.Errorf("error responses must use the Siren media type, got %q", headers["Content-Type"])
	}

	var got struct {
		Class      []string       `json:"class"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", err, body)
	}
	if len(got.Class) != 1 || got.Class[0] != "error" {
		t.Errorf("class = %v, want [error]", got.Class)
	}
	if msg, _ := got.Properties["message"].(string); msg != "no such resource: /nope" {
		t.Errorf("properties.message = %q, want the given message", msg)
	}
	if status, _ := got.Properties["status"].(float64); int(status) != http.StatusNotFound {
		t.Errorf("properties.status = %v, want %d", got.Properties["status"], http.StatusNotFound)
	}
}

// TestSirenRendererWriteJSONIsNotWrappedInSiren pins that the one non-Siren
// route (OpenAPI) is written as the raw document, with no "class"/"properties"
// envelope, since a tool reading OpenAPI expects the document at the top
// level.
func TestSirenRendererWriteJSONIsNotWrappedInSiren(t *testing.T) {
	var buf bytes.Buffer
	doc := map[string]any{"openapi": "3.1.0"}

	if err := (SirenRenderer{}).WriteJSON(&buf, http.StatusOK, doc); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	headers, body := splitCGIResponse(t, buf.String())
	if headers["Content-Type"] != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", headers["Content-Type"])
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if _, ok := got["class"]; ok {
		t.Error("WriteJSON output carries a Siren \"class\" field; it must be the document, unwrapped")
	}
	if got["openapi"] != "3.1.0" {
		t.Errorf(`got["openapi"] = %v, want "3.1.0"`, got["openapi"])
	}
}

// TestSirenRendererCarriesTheNodeHeader pins X-F3sctl-Node, which is present
// on every response including errors -- it is what lets a client blamed for
// "the wrong node answered" actually tell which node that was.
func TestSirenRendererCarriesTheNodeHeader(t *testing.T) {
	var buf bytes.Buffer
	if err := (SirenRenderer{}).WriteError(&buf, http.StatusInternalServerError, "boom"); err != nil {
		t.Fatalf("WriteError: %v", err)
	}
	headers, _ := splitCGIResponse(t, buf.String())
	if _, ok := headers["X-F3sctl-Node"]; !ok {
		t.Error("response carries no X-F3sctl-Node header")
	}
}
