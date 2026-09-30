package server

import (
	"context"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colehanke/mavi-demo/api/internal/auth"
	"github.com/colehanke/mavi-demo/api/internal/dbtest"
	"github.com/getkin/kin-openapi/openapi3"
)

// TestRoutesMatchOpenAPISpec pins the server to api/openapi.yaml: every
// role-gated route must be an operation in the spec with the same x-roles,
// and every operation in the spec must be routed. Adding an endpoint or
// changing who may call it in one place without the other fails here.
func TestRoutesMatchOpenAPISpec(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile(filepath.Join(dbtest.RepoRoot(), "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("api/openapi.yaml is not a valid OpenAPI document: %v", err)
	}

	// method + path -> x-roles, from the spec.
	specOps := map[string][]string{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			key := method + " " + path
			if key == healthRoute {
				continue // unauthenticated; not in the route table
			}
			raw, ok := op.Extensions["x-roles"]
			if !ok {
				t.Errorf("%s: missing x-roles in api/openapi.yaml", key)
				continue
			}
			roles, ok := stringList(raw)
			if !ok {
				t.Errorf("%s: x-roles must be a list of persona names, got %v", key, raw)
				continue
			}
			specOps[key] = roles
		}
	}

	seen := map[string]bool{}
	for _, rt := range (&Server{}).routes() {
		if seen[rt.pattern] {
			t.Errorf("%s: registered twice", rt.pattern)
		}
		seen[rt.pattern] = true
		want, ok := specOps[rt.pattern]
		if !ok {
			t.Errorf("%s: routed by the server but not in api/openapi.yaml", rt.pattern)
			continue
		}
		got := make([]string, len(rt.roles))
		for i, r := range rt.roles {
			got[i] = string(r)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: server allows %v, spec x-roles says %v", rt.pattern, got, want)
		}
		for _, r := range rt.roles {
			if _, ok := auth.Parse(string(r)); !ok {
				t.Errorf("%s: %q is not a persona", rt.pattern, r)
			}
		}
	}
	for key := range specOps {
		if !seen[key] {
			t.Errorf("%s: in api/openapi.yaml but not routed by the server", key)
		}
	}
	if !slices.Contains(mustPatterns(doc), healthRoute) {
		t.Errorf("%s missing from the spec", healthRoute)
	}
}

// TestSpecPersonasMatchAuth checks the Persona enum against the roles the
// middleware accepts, and that every method the mux uses is one the spec knows.
func TestSpecPersonasMatchAuth(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile(filepath.Join(dbtest.RepoRoot(), "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	personas, ok := stringList(doc.Components.Schemas["Persona"].Value.Enum)
	if !ok {
		t.Fatalf("Persona enum is not a list of strings: %v", doc.Components.Schemas["Persona"].Value.Enum)
	}
	var all []string
	for _, r := range auth.All {
		all = append(all, string(r))
	}
	slices.Sort(personas)
	slices.Sort(all)
	if !slices.Equal(personas, all) {
		t.Fatalf("Persona enum %v != auth.All %v", personas, all)
	}
	for _, p := range mustPatterns(doc) {
		method := strings.Fields(p)[0]
		switch method {
		case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete:
		default:
			t.Errorf("%s: method not allowed by the CORS preflight", p)
		}
	}
}

// stringList converts a decoded YAML/JSON extension value to []string.
func stringList(raw any) ([]string, bool) {
	items, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func mustPatterns(doc *openapi3.T) []string {
	var out []string
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			out = append(out, method+" "+path)
		}
	}
	return out
}
