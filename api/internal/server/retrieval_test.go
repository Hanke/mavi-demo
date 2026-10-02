package server

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/colehanke/mavi-demo/api/internal/tasks"
)

// direction is a unit vector at degrees from direction(0), in pgvector's
// text form: its cosine similarity to direction(0) is cos(degrees).
func direction(degrees float64) string {
	rad := degrees * math.Pi / 180
	return "[" + strings.Repeat("0,", tasks.EmbeddingDim-2) +
		strconv.FormatFloat(math.Cos(rad), 'g', -1, 64) + "," + strconv.FormatFloat(math.Sin(rad), 'g', -1, 64) + "]"
}

// embed gives the named candidates' profiles an embedding at that many
// degrees from the role's (see embedRole).
func (p *filterPool) embed(degrees map[string]float64) {
	p.t.Helper()
	for id, name := range p.names {
		if deg, ok := degrees[name]; ok {
			p.exec(`UPDATE candidate_profiles SET embedding = $2::vector WHERE candidate_id = $1`, id, direction(deg))
		}
	}
}

func (p *filterPool) embedRole(roleID string) {
	p.t.Helper()
	p.exec(`UPDATE roles SET embedding = $2::vector WHERE id = $1`, roleID, direction(0))
}

// id is the candidate added under that name.
func (p *filterPool) id(name string) string {
	p.t.Helper()
	for id, n := range p.names {
		if n == name {
			return id
		}
	}
	p.t.Fatalf("no candidate named %q", name)
	return ""
}

var zeroVector = "[" + strings.TrimSuffix(strings.Repeat("0,", tasks.EmbeddingDim), ",") + "]"

// retrieval is the shortlist of a run as the tests read it.
type retrieval struct {
	raw          string
	limit        int
	names        []string
	similarities []float64
	unranked     []string // names, in the order returned
	roleEmbedded bool
	passed       int
}

// retrieve runs the role's filters on a server that retrieves up to limit
// (0 for the default) and reads back the shortlist.
func (p *filterPool) retrieve(roleID string, limit int) retrieval {
	p.t.Helper()
	a := newAPIOn(p.t, p.pool, stub{}, limit)
	r := a.want(a.do("POST", "/roles/"+roleID+"/filter-runs", "ops", "", nil), 201, "run filters")
	out := retrieval{raw: r.Raw, limit: int(r.Body["retrieval_limit"].(float64)), roleEmbedded: r.Body["role_embedded"].(bool), passed: int(r.Body["passed"].(float64))}
	passed := r.Body["candidate_ids"].([]any)
	for _, id := range r.Body["unranked_ids"].([]any) {
		if !slices.Contains(passed, id) {
			p.t.Fatalf("an unranked candidate did not pass the filters: %s", r.Raw)
		}
		out.unranked = append(out.unranked, p.names[id.(string)])
	}
	for _, e := range r.Body["retrieved"].([]any) {
		entry := e.(map[string]any)
		if !slices.Contains(passed, entry["candidate_id"]) {
			p.t.Fatalf("retrieved a candidate who did not pass the filters: %s", r.Raw)
		}
		out.names = append(out.names, p.names[entry["candidate_id"].(string)])
		out.similarities = append(out.similarities, entry["similarity"].(float64))
	}
	return out
}

func TestRetrieval(t *testing.T) {
	p := newFilterPool(t)
	// Everyone with a profile is embedded, nearest first as listed. The two
	// nearest of all are candidates no role can take.
	p.embed(map[string]float64{
		"No Answers": 1, "Archived": 2,
		"Tokyo": 10, "Fits": 20, "Part Time": 30, "ACCA": 40, "Two Years": 50, "Starts Late": 60,
		"Excel Only": 70, "Years Unknown": 80, "No Certificate": 100, "Unplaced CPA": 170,
	})
	open := p.role("Anyone")
	p.embedRole(open)

	t.Run("the nearest of those who pass, up to the limit", func(t *testing.T) {
		res := p.retrieve(open, 3)
		if res.limit != 3 || !slices.Equal(res.names, []string{"Tokyo", "Fits", "Part Time"}) || len(res.unranked) != 0 || !res.roleEmbedded {
			t.Fatalf("want Tokyo, Fits and Part Time of the %d who passed: %s", res.passed, res.raw)
		}
		for i, deg := range []float64{10, 20, 30} {
			if want := math.Cos(deg * math.Pi / 180); math.Abs(res.similarities[i]-want) > 1e-6 {
				t.Errorf("similarity of %s = %v, want %v", res.names[i], res.similarities[i], want)
			}
		}
		if n := p.count(`SELECT count(*) FROM filter_runs WHERE role_id = $1 AND retrieval_limit = 3 AND cardinality(retrieved_ids) = 3`, open); n != 1 {
			t.Fatalf("the shortlist was not recorded with the run")
		}
		list := p.want(p.do("GET", "/roles/"+open+"/filter-runs", "ops", "", nil), 200, "list runs")
		if got := list.List[0]["retrieved"].([]any); len(got) != 3 {
			t.Fatalf("the recorded run should list its shortlist: %s", list.Raw)
		}
	})

	t.Run("the limit is the server's, 20 unless configured", func(t *testing.T) {
		if res := p.retrieve(open, 1); res.limit != 1 || !slices.Equal(res.names, []string{"Tokyo"}) {
			t.Fatalf("want Tokyo alone: %s", res.raw)
		}
		res := p.retrieve(open, 0)
		want := []string{"Tokyo", "Fits", "Part Time", "ACCA", "Two Years", "Starts Late", "Excel Only", "Years Unknown", "No Certificate", "Unplaced CPA"}
		if res.limit != 20 || !slices.Equal(res.names, want) {
			t.Fatalf("with fewer than 20 passing, want all %d, nearest first: %s", res.passed, res.raw)
		}
		for i := 1; i < len(res.similarities); i++ {
			if res.similarities[i] > res.similarities[i-1] {
				t.Errorf("similarities should fall: %v", res.similarities)
			}
		}
	})

	t.Run("the filters and the order in one run", func(t *testing.T) {
		strict := p.want(p.do("POST", "/roles", "employer", "", map[string]any{
			"title": "Senior Accountant", "required_certifications": []string{"cpa_us"}, "required_software": []string{"netsuite"},
			"min_years_experience": 5, "starts_on": "2030-01-01", "hours_per_week": 40,
			"timezone": "America/Chicago", "min_overlap_hours": 4,
		}), 201, "role").str("id")
		p.embedRole(strict)
		// Tokyo and Part Time are nearer than ACCA, and filtered out.
		if res := p.retrieve(strict, 20); res.passed != 2 || !slices.Equal(res.names, []string{"Fits", "ACCA"}) {
			t.Fatalf("want the two who pass, Fits first: %s", res.raw)
		}
		if res := p.retrieve(strict, 1); !slices.Equal(res.names, []string{"Fits"}) {
			t.Fatalf("want Fits alone: %s", res.raw)
		}
		// A role nobody passes retrieves nobody.
		nobody := p.want(p.do("POST", "/roles", "employer", "", map[string]any{"title": "Both", "required_certifications": []string{"cpa_us", "cma"}}), 201, "role").str("id")
		p.embedRole(nobody)
		if res := p.retrieve(nobody, 20); res.passed != 0 || len(res.names) != 0 || len(res.unranked) != 0 || !res.roleEmbedded || !strings.Contains(res.raw, `"retrieved":[]`) {
			t.Fatalf("want an empty shortlist: %s", res.raw)
		}
	})

	t.Run("equal distances are ordered by name", func(t *testing.T) {
		p.embed(map[string]float64{"Tokyo": 20, "ACCA": 20})
		defer p.embed(map[string]float64{"Tokyo": 10, "ACCA": 40})
		if res := p.retrieve(open, 3); !slices.Equal(res.names, []string{"ACCA", "Fits", "Tokyo"}) {
			t.Fatalf("want ACCA, Fits, Tokyo: %s", res.raw)
		}
	})

	t.Run("a profile that is not embedded is counted, not retrieved", func(t *testing.T) {
		p.exec(`UPDATE candidate_profiles SET embedding = NULL WHERE candidate_id = $1`, p.id("Tokyo"))
		// A vector of zeros has no direction.
		p.exec(`UPDATE candidate_profiles SET embedding = $2::vector WHERE candidate_id = $1`, p.id("Fits"), zeroVector)
		// One from another provider is not in the role's space.
		p.exec(`UPDATE candidate_profiles SET embedding_model = 'other' WHERE candidate_id = $1`, p.id("Part Time"))
		res := p.retrieve(open, 3)
		if !slices.Equal(res.names, []string{"ACCA", "Two Years", "Starts Late"}) || !slices.Equal(res.unranked, []string{"Fits", "Part Time", "Tokyo"}) || !res.roleEmbedded {
			t.Fatalf("want the next three, and Fits, Part Time and Tokyo unranked: %s", res.raw)
		}
		// Unranked however much room the list has.
		if res := p.retrieve(open, 20); len(res.names) != 7 || len(res.unranked) != 3 {
			t.Fatalf("want seven retrieved and three unranked: %s", res.raw)
		}
	})

	t.Run("a role that is not embedded retrieves nobody and says so", func(t *testing.T) {
		role := p.role("Not Embedded")
		res := p.retrieve(role, 20)
		if len(res.names) != 0 || res.passed != 10 || len(res.unranked) != res.passed || res.roleEmbedded {
			t.Fatalf("want everyone who passed unranked and role_embedded false: %s", res.raw)
		}
		p.exec(`UPDATE roles SET embedding = $2::vector WHERE id = $1`, role, zeroVector)
		if res := p.retrieve(role, 20); len(res.names) != 0 || res.roleEmbedded {
			t.Fatalf("a role embedded as zeros has nothing to compare with: %s", res.raw)
		}
	})

	t.Run("a limit above what the rerank takes is cut to it", func(t *testing.T) {
		if res := p.retrieve(open, 500); res.limit != 50 {
			t.Fatalf("retrieval_limit = %d, want 50: %s", res.limit, res.raw)
		}
	})
}
