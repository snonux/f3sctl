package httpapi

import (
	"testing"

	"github.com/snonux/f3sctl/internal/inventory"
)

// TestOpenAPIDocumentsForceForAnyInventory is the regression test for the
// static document's widest state naming a host of its own: the plug guards
// select the snapshot's hosts by name from the served inventory, so a
// synthetic "f0" meant nothing to an inventory without one, the rack read as
// cold, and fans-off and ac-off lost their `force` field from /openapi.json.
// The widest state must be built from the inventory the routes are served
// with.
func TestOpenAPIDocumentsForceForAnyInventory(t *testing.T) {
	renamed := inventory.Inventory{Hosts: []inventory.Host{
		{Name: "g0", Role: inventory.RoleF},
		{Name: "g1", Role: inventory.RoleF, Standalone: true},
		{Name: "q0", Role: inventory.RoleCluster},
	}}

	for _, tc := range []struct {
		name string
		inv  inventory.Inventory
	}{
		{"default", inventory.Default()},
		{"renamed", renamed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := mustRouter("", testRoutes(tc.inv))
			doc := NewOpenAPIBuilder(router, tc.inv).Build()
			paths, _ := doc["paths"].(map[string]any)

			for _, path := range []string{"/fans/off", "/ac/off"} {
				if !documentsForce(paths, router.Href(path)) {
					t.Errorf("POST %s does not document the force field", path)
				}
			}
		})
	}
}

// documentsForce reports whether the POST operation at href has a
// form-encoded requestBody with a "force" property.
func documentsForce(paths map[string]any, href string) bool {
	entry, _ := paths[href].(map[string]any)
	op, _ := entry["post"].(map[string]any)
	body, _ := op["requestBody"].(map[string]any)
	content, _ := body["content"].(map[string]any)
	form, _ := content["application/x-www-form-urlencoded"].(map[string]any)
	schema, _ := form["schema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	_, ok := props["force"]
	return ok
}
