package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/store"
	"github.com/colehanke/mavi-demo/api/internal/taxonomy"
)

// HardFilter is the first two stages of a matching run, in one SQL statement
// (Store.RunHardFilter): the active candidates narrowed to those who meet
// every must-have of the role, and of those the retrieve closest to the role
// by embedding (store.DefaultRetrievalLimit when retrieve is below 1). The
// run is recorded in filter_runs and logged with the count left after each
// filter. The candidates the rerank may look at are the run's Retrieved and
// no others. today is the day the time-zone overlap is worked out for when
// the role has no start date.
func HardFilter(ctx context.Context, st *store.Store, tax *taxonomy.Taxonomy, roleID string, today time.Time, retrieve int) (store.FilterRun, error) {
	var role store.Role
	run, err := st.RunHardFilter(ctx, roleID, today, retrieve, func(r store.Role) [][]string {
		role = r
		return AcceptableCertifications(tax, r)
	})
	if err != nil {
		return store.FilterRun{}, err
	}
	logf("hard filter: role %s (%q): %s", role.ID, role.Title, funnel(run))
	return run, nil
}

// AcceptableCertifications returns, for each certification the role
// requires, the ids that satisfy it: the certification itself and, when the
// requirement accepts equivalents, its equivalents (taxonomy.Acceptable).
// Whether it does is in the parser's record for it
// (requirements.required_qualifications). An id with no record, as on a role
// written through the API as a bare list, accepts equivalents: the same
// default as the AI service's matching.check_hard_filters. An id the taxonomy
// does not know gets an empty set, which no candidate meets.
func AcceptableCertifications(tax *taxonomy.Taxonomy, role store.Role) [][]string {
	var doc struct {
		RequiredQualifications []struct {
			Canonical         *string `json:"canonical"`
			AcceptEquivalents *bool   `json:"accept_equivalents"`
		} `json:"required_qualifications"`
	}
	// requirements is free-form JSON in the API; one that does not have this
	// shape has no records, and every requirement takes the default.
	_ = json.Unmarshal(role.Requirements, &doc)
	strict := map[string]bool{}
	for _, q := range doc.RequiredQualifications {
		if q.Canonical != nil && q.AcceptEquivalents != nil && !*q.AcceptEquivalents {
			strict[*q.Canonical] = true
		}
	}
	out := make([][]string, 0, len(role.RequiredCertifications))
	for _, id := range role.RequiredCertifications {
		ids := tax.Acceptable(id, !strict[id])
		if ids == nil {
			ids = []string{}
		}
		out = append(out, ids)
	}
	return out
}

// funnel writes a run as "pool 212 -> profile 200 -> ... -> timezone_overlap 31
// -> retrieved 20 of 20", with ", 3 unranked" when some could not be compared
// and "(role not embedded)" when the role had no vector to compare with.
func funnel(run store.FilterRun) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pool %d", run.Pool)
	for _, s := range run.Stages {
		fmt.Fprintf(&b, " -> %s %d", s.Filter, s.Remaining)
	}
	fmt.Fprintf(&b, " -> retrieved %d of %d", len(run.Retrieved), run.RetrievalLimit)
	if n := len(run.UnrankedIds); n > 0 {
		fmt.Fprintf(&b, ", %d unranked", n)
	}
	if !run.RoleEmbedded {
		b.WriteString(" (role not embedded)")
	}
	return b.String()
}
