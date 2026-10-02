package tasks

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/colehanke/mavi-demo/api/internal/dbtest"
	"github.com/colehanke/mavi-demo/api/internal/store"
	"github.com/colehanke/mavi-demo/api/internal/taxonomy"
)

func TestAcceptableCertifications(t *testing.T) {
	tax, err := taxonomy.Load(dbtest.TaxonomyPath())
	if err != nil {
		t.Fatal(err)
	}
	role := func(requirements string, required ...string) store.Role {
		return store.Role{RequiredCertifications: required, Requirements: json.RawMessage(requirements)}
	}
	strict := `{"required_qualifications": [{"canonical": "cpa_us", "accept_equivalents": false}]}`

	// No record of the requirement: equivalents are accepted, but not the
	// "CPA" that names no country.
	got := AcceptableCertifications(tax, role(`{}`, "cpa_us", "cma"))
	if len(got) != 2 {
		t.Fatalf("want one set per requirement, got %v", got)
	}
	if !slices.Contains(got[0], "cpa_us") || !slices.Contains(got[0], "acca") || slices.Contains(got[0], "cpa") {
		t.Errorf("cpa_us with equivalents = %v", got[0])
	}
	if !slices.Contains(got[1], "cma") || slices.Contains(got[1], "cpa_us") {
		t.Errorf("cma = %v", got[1])
	}

	// The record rules equivalents out, for that requirement only.
	got = AcceptableCertifications(tax, role(strict, "cpa_us", "cma"))
	if !slices.Equal(got[0], []string{"cpa_us"}) || !slices.Contains(got[1], "cima") {
		t.Errorf("strict cpa_us = %v, cma = %v", got[0], got[1])
	}
	// A record that accepts them, or says nothing, is the default.
	for _, doc := range []string{
		`{"required_qualifications": [{"canonical": "cpa_us", "accept_equivalents": true}]}`,
		`{"required_qualifications": [{"canonical": "cpa_us"}]}`,
		`{"required_qualifications": "not a list"}`,
		``,
	} {
		if got := AcceptableCertifications(tax, role(doc, "cpa_us")); !slices.Contains(got[0], "acca") {
			t.Errorf("%q: want equivalents accepted, got %v", doc, got[0])
		}
	}

	// An id the taxonomy does not know is a requirement nobody meets, not
	// one that is skipped; no requirements is no sets.
	if got := AcceptableCertifications(tax, role(`{}`, "not_a_certification")); len(got) != 1 || got[0] == nil || len(got[0]) != 0 {
		t.Errorf("unknown id = %#v, want one empty set", got)
	}
	if got := AcceptableCertifications(tax, role(`{}`)); got == nil || len(got) != 0 {
		t.Errorf("no requirements = %#v, want no sets", got)
	}
}
