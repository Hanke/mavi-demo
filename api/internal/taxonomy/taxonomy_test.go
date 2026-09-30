package taxonomy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const infraDir = "../../../infra"

func loadShared(t *testing.T) *Taxonomy {
	t.Helper()
	tax, err := Load(filepath.Join(infraDir, "taxonomy.json"))
	if err != nil {
		t.Fatal(err)
	}
	return tax
}

func TestEveryIDLabelAndAliasResolves(t *testing.T) {
	tax := loadShared(t)
	for _, kind := range Kinds {
		terms := tax.Terms(kind)
		if len(terms) == 0 {
			t.Fatalf("%s: no terms", kind)
		}
		for _, term := range terms {
			if !tax.IsCanonical(kind, term.ID) {
				t.Errorf("%s/%s: not reported canonical", kind, term.ID)
			}
			for _, v := range append([]string{term.ID, term.Label}, term.Aliases...) {
				if got, ok := tax.Resolve(kind, v); !ok || got != term.ID {
					t.Errorf("%s: Resolve(%q) = %q, %v; want %q", kind, v, got, ok, term.ID)
				}
			}
		}
	}
}

// TestSharedAliasCases runs the fixture that ai/tests/test_taxonomy.py also
// runs, so the Python parsers and the Go API cannot drift apart.
func TestSharedAliasCases(t *testing.T) {
	tax := loadShared(t)
	b, err := os.ReadFile(filepath.Join(infraDir, "taxonomy_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Kind  Kind    `json:"kind"`
			Input string  `json:"input"`
			Want  *string `json:"want"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range fixture.Cases {
		got, ok := tax.Resolve(c.Kind, c.Input)
		switch {
		case c.Want == nil && ok:
			t.Errorf("%s: Resolve(%q) = %q, want unknown", c.Kind, c.Input, got)
		case c.Want != nil && (!ok || got != *c.Want):
			t.Errorf("%s: Resolve(%q) = %q, %v; want %q", c.Kind, c.Input, got, ok, *c.Want)
		}
	}
}

func TestKey(t *testing.T) {
	cases := []struct {
		kind      Kind
		raw, want string
	}{
		{Software, "QuickBooks Online", "quickbooksonline"},
		{Certifications, "  C.P.A.  ", "cpa"},
		{Industries, "Food & Beverage", "foodandbeverage"},
		{Certifications, "CPA (active)", "cpa"},
		{Certifications, "Certified Public Accountant (CPA)", "publicaccountant"},
		{Certifications, "PMP Certified", "pmp"},
		{Certifications, "certified", ""},
		// Credential words are only noise for certifications.
		{Software, "Certified Payroll Suite", "certifiedpayrollsuite"},
		{Industries, "Licensed Cannabis", "licensedcannabis"},
		// A parenthetical glued to a word is part of the name.
		{Industries, "501(c)(3)", "501c3"},
		{Certifications, "CA(SA)", "casa"},
		{Software, "", ""},
		{Software, "---", ""},
	}
	for _, c := range cases {
		if got := Key(c.kind, c.raw); got != c.want {
			t.Errorf("Key(%s, %q) = %q, want %q", c.kind, c.raw, got, c.want)
		}
	}
}

func TestResolveAllSplitsAndDedupes(t *testing.T) {
	tax := loadShared(t)
	canonical, unknown := tax.ResolveAll(Software, []string{"QBO", "QuickBooks Online", "NetSuite", "Zoho Books", " zoho books ", "", "Zoho Books!"})
	if want := []string{"quickbooks", "netsuite"}; !reflect.DeepEqual(canonical, want) {
		t.Errorf("canonical = %v, want %v", canonical, want)
	}
	if want := []string{"Zoho Books"}; !reflect.DeepEqual(unknown, want) {
		t.Errorf("unknown = %v, want %v", unknown, want)
	}
	c, u := tax.ResolveAll(Software, nil)
	if c == nil || u == nil || len(c)+len(u) != 0 {
		t.Errorf("empty input should give empty non-nil slices, got %v %v", c, u)
	}
}

func TestParseRejectsConflicts(t *testing.T) {
	base := `"software": [{"id": "s", "label": "S"}], "industries": [{"id": "i", "label": "I"}]`
	cases := map[string]string{
		"maps to both": `{"certifications": [{"id": "a", "label": "A", "aliases": ["x"]}, {"id": "b", "label": "B", "aliases": ["X"]}], ` + base + `}`,
		"snake_case":   `{"certifications": [{"id": "Bad-Id", "label": "A"}], ` + base + `}`,
		"duplicate id": `{"certifications": [{"id": "a", "label": "A"}, {"id": "a", "label": "B"}], ` + base + `}`,
		"missing":      `{"certifications": [{"id": "a", "label": "A"}], "software": [{"id": "s", "label": "S"}]}`,
		"non-empty":    `{"certifications": [], ` + base + `}`,
	}
	for want, doc := range cases {
		_, err := Parse(strings.NewReader(doc))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("expected error containing %q, got %v", want, err)
		}
	}
}
