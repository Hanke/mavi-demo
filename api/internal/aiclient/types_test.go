package aiclient

import (
	"encoding/json"
	"testing"
)

// The parser models are what the AI service will return and what the API
// stores in JSONB. Pydantic serialises an unset optional as null and an
// unset list as [], so the generated Go types must accept exactly that.
func TestGeneratedParserTypesDecodePydanticOutput(t *testing.T) {
	var p CandidateProfile
	err := json.Unmarshal([]byte(`{"headline":null,"available_from":null,"years_experience":null,"timezone":null,
		"certifications":[],"software":["quickbooks"],"skills":[],"availability":"unknown"}`), &p)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.AvailableFrom != nil || p.Headline != nil || len(p.Software) != 1 {
		t.Fatalf("unexpected decode: %+v", p)
	}
	out, err := json.Marshal(CandidateProfile{Software: []string{"excel"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"software":["excel"]}` {
		t.Fatalf("an unset date must not be serialised as year 1: %s", out)
	}
	var r RoleRequirements
	if err := json.Unmarshal([]byte(`{"starts_on":null,"title":null,"must_haves":["CPA"]}`), &r); err != nil {
		t.Fatalf("decode role requirements: %v", err)
	}
}
