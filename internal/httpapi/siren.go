package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/snonux/f3sctl/internal/httpapi/contract"
)

// The Siren entity vocabulary (Entity, Link, Action, Field) lives in
// internal/httpapi/contract, shared with the gogiosapi and powerapi surface
// packages; this file is the wire half -- the writer that marshals that
// vocabulary into CGI responses. Siren
// (https://github.com/kevinswiber/siren) is used rather than HAL because HAL
// describes links but not *actions*: it can say "here is the fans resource",
// but not "you may POST here, with this field, to switch it off". Actions
// with parameters are the entire point of this API being self-describing, so
// the representation has to carry them.

// sirenMediaType is the content type for every entity response.
const sirenMediaType = "application/vnd.siren+json"

// SirenRenderer writes API responses in Siren hypermedia JSON, and the one
// endpoint (OpenAPI) that is deliberately emitted as plain JSON instead.
//
// It carries no state -- CGI is one request per process, so there is nothing
// to share between writes -- which is what lets ServeCGI use it for the
// error paths that run before a Server has even been constructed (a bad
// request, or a Server that failed to construct at all).
type SirenRenderer struct{}

// NewSirenRenderer returns a renderer. It exists so Server's constructor
// follows the same injection pattern as its other collaborators; the zero
// value renders identically.
func NewSirenRenderer() SirenRenderer { return SirenRenderer{} }

// WriteEntity emits a Siren entity as a CGI response.
func (SirenRenderer) WriteEntity(out io.Writer, status int, e contract.Entity) error {
	body, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return writeResponse(out, status, sirenMediaType, append(body, '\n'))
}

// WriteJSON emits a plain JSON document, for the one route that is not Siren.
func (SirenRenderer) WriteJSON(out io.Writer, status int, doc any) error {
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeResponse(out, status, "application/json", append(body, '\n'))
}

// WriteError emits a problem response. It carries the same shape as any other
// entity so a client needs only one parser.
func (SirenRenderer) WriteError(out io.Writer, status int, msg string) error {
	e := contract.Entity{
		Class:      []string{"error"},
		Properties: map[string]any{"status": status, "message": msg},
	}
	body, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return writeResponse(out, status, sirenMediaType, append(body, '\n'))
}

// writeResponse writes the CGI response header block, followed by body.
func writeResponse(out io.Writer, status int, contentType string, body []byte) error {
	// X-F3sctl-Node names the node that served this response, on EVERY reply
	// including errors. relayd load-balances pi0 and pi1, so "which node
	// answered" explains a great deal -- a job the other node knows nothing
	// about, most of all -- and it cannot be read off the URL. Putting it in a
	// header rather than only in the entity means it is present even when the
	// body is an error, which has no node property to carry it.
	node, _ := os.Hostname()

	// CGI headers use CRLF and a blank line before the body. bozohttpd turns
	// the Status header into the HTTP status line.
	_, err := fmt.Fprintf(out,
		"Status: %d %s\r\nContent-Type: %s\r\nCache-Control: no-store\r\nX-F3sctl-Node: %s\r\n\r\n%s",
		status, http.StatusText(status), contentType, node, body)
	return err
}
