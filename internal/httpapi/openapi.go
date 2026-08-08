package httpapi

import (
	"net/http"
	"strings"

	"github.com/snonux/f3sctl/internal"
	"github.com/snonux/f3sctl/internal/power"
)

// handleOpenAPI renders an OpenAPI 3.1 description generated from the same
// route registry that serves the requests.
//
// This is the second half of "self-describing", aimed at a different reader.
// Siren tells a running client what it may do *at this moment*; OpenAPI tells
// a code generator, a test, or a person what the surface is in general. Both
// come from routes(), so neither can describe an endpoint that does not exist
// or miss one that does.
// It is the one route not wrapped in a Siren envelope -- an OpenAPI document
// has its own well-known shape, and burying it inside "properties" would make
// it useless to every tool that reads OpenAPI. serve() recognises it by path
// and writes Properties out as the whole body.
func (s *Server) handleOpenAPI(_ State, _ request) (Entity, int, error) {
	return Entity{Properties: s.openAPIDoc()}, http.StatusOK, nil
}

// openAPIPath is the route whose body is emitted raw rather than as Siren.
const openAPIPath = "/openapi.json"

func (s *Server) openAPIDoc() map[string]any {
	paths := map[string]any{}

	for _, r := range routes() {
		if r.Path == openAPIPath {
			continue
		}

		op := map[string]any{
			"operationId": r.Name,
			"summary":     r.Title,
			"responses": map[string]any{
				"200": map[string]any{"description": "Siren entity"},
				"401": map[string]any{"description": "missing or bad X-API-Key"},
			},
		}

		if r.Action {
			// Availability is state-dependent and therefore cannot be
			// expressed here; it is described in prose so a reader of the
			// static document is not misled into thinking every action is
			// always callable.
			op["description"] = "Advertised in the parent entity's actions only when currently available. " +
				"A 409 means it was attempted when it was not."
			op["responses"].(map[string]any)["202"] = map[string]any{"description": "accepted; poll the job resource"}
			op["responses"].(map[string]any)["409"] = map[string]any{"description": "not available now, or another job is running"}

			if fields := describeFields(r); len(fields) > 0 {
				op["requestBody"] = map[string]any{
					"required": false,
					"content": map[string]any{
						"application/x-www-form-urlencoded": map[string]any{
							"schema": map[string]any{"type": "object", "properties": fields},
						},
					},
				}
			}
		}

		key := s.href(r.Path)
		entry, _ := paths[key].(map[string]any)
		if entry == nil {
			entry = map[string]any{}
		}
		entry[strings.ToLower(r.Method)] = op
		paths[key] = entry
	}

	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "f3sctl",
			"version":     internal.Version,
			"description": "Power, status and rack-fan control for the f3s homelab. Hypermedia (Siren): fetch the root and follow what it offers rather than hard-coding these paths.",
		},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"apiKey": map[string]any{"type": "apiKey", "in": "header", "name": "X-API-Key"},
			},
		},
		"security": []any{map[string]any{"apiKey": []any{}}},
		"paths":    paths,
	}
}

// describeFields renders a route's parameters for the static document.
//
// Fields are state-dependent, so this evaluates them against the widest state:
// one in which every conditional field appears. A static description should
// list everything an action can ever accept, and say (as the operation
// description does) that availability is decided at runtime.
func describeFields(r route) map[string]any {
	if r.Fields == nil {
		return nil
	}

	out := map[string]any{}
	for _, f := range r.Fields(widestState()) {
		typ := "string"
		if f.Type == "checkbox" {
			typ = "boolean"
		}
		out[f.Name] = map[string]any{"type": typ, "description": f.Title}
	}
	return out
}

// widestState is a synthetic state chosen to make every conditional field
// appear: an f-host up (which is what adds the fans-off confirmation) and no
// job running.
func widestState() State {
	return State{
		Hosts: []power.HostStatus{
			{Name: "f0", Role: "f", Ping: true, SSH: true},
		},
	}
}
