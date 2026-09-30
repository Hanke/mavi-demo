// Package taxonomy loads the shared canonical vocabulary (infra/taxonomy.json)
// that the hard-filter columns hold: certifications, software and industries.
//
// The Python parsers (ai/app/taxonomy.py) read the same file and implement the
// same Key function, so a value extracted from a resume and a value the API
// writes or seeds land on the same id and `p.software @> r.required_software`
// compares exactly. Change the key rules in both places together and keep
// infra/taxonomy_cases.json green on both sides.
package taxonomy

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// Kind is one of the three vocabularies in the taxonomy file.
type Kind string

const (
	Certifications Kind = "certifications"
	Software       Kind = "software"
	Industries     Kind = "industries"
)

// Kinds lists every vocabulary, in file order.
var Kinds = []Kind{Certifications, Software, Industries}

// Term is one canonical value plus the free-text variants that resolve to it.
type Term struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Aliases []string `json:"aliases"`
}

// Taxonomy is an immutable, loaded vocabulary.
type Taxonomy struct {
	terms map[Kind][]Term
	index map[Kind]map[string]string // normalised key -> id
	ids   map[Kind]map[string]bool
}

// credentialWords describe holding a credential rather than the credential
// itself, so "PMP certified", "CPA license" and "PMP" share a key. They are
// only dropped for certifications so a product or industry name keeps them.
var credentialWords = map[string]bool{
	"certified": true, "certification": true, "certifications": true, "certificate": true,
	"credential": true, "license": true, "licensed": true, "licence": true,
}

var (
	// A parenthetical that stands apart from a word is an annotation
	// ("CPA (active)"); one glued to a word is part of the name ("501(c)(3)").
	parenthetical = regexp.MustCompile(`(^|\s)\([^)]*\)`)
	nonAlnum      = regexp.MustCompile(`[^a-z0-9]+`)
	idPattern     = regexp.MustCompile(`^[a-z0-9]+(_[a-z0-9]+)*$`)
)

// Key reduces free text to the key used for alias lookup within a kind. It
// mirrors normalize_key in ai/app/taxonomy.py exactly: lower-case, "&" ->
// "and", drop free-standing parentheticals, drop credential words
// (certifications only), then drop every non-alphanumeric character. It
// returns "" when nothing is left.
func Key(kind Kind, raw string) string {
	s := strings.ToLower(raw)
	s = strings.ReplaceAll(s, "&", " and ")
	s = parenthetical.ReplaceAllString(s, " ")
	var b strings.Builder
	for _, tok := range nonAlnum.Split(s, -1) {
		if tok == "" || (kind == Certifications && credentialWords[tok]) {
			continue
		}
		b.WriteString(tok)
	}
	return b.String()
}

// Load reads and validates a taxonomy file.
func Load(path string) (*Taxonomy, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("taxonomy: %w", err)
	}
	defer f.Close()
	t, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("taxonomy %s: %w", path, err)
	}
	return t, nil
}

// Parse decodes and validates taxonomy JSON. Every id must be snake_case, and
// no key may resolve to two different ids within a kind.
func Parse(r io.Reader) (*Taxonomy, error) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, err
	}
	t := &Taxonomy{
		terms: map[Kind][]Term{},
		index: map[Kind]map[string]string{},
		ids:   map[Kind]map[string]bool{},
	}
	for _, kind := range Kinds {
		msg, ok := raw[string(kind)]
		if !ok {
			return nil, fmt.Errorf("%s: missing", kind)
		}
		var terms []Term
		if err := json.Unmarshal(msg, &terms); err != nil {
			return nil, fmt.Errorf("%s: %w", kind, err)
		}
		if len(terms) == 0 {
			return nil, fmt.Errorf("%s: must be a non-empty list", kind)
		}
		index := map[string]string{}
		ids := map[string]bool{}
		for i, term := range terms {
			if term.ID == "" || term.Label == "" {
				return nil, fmt.Errorf("%s[%d]: needs id and label", kind, i)
			}
			if !idPattern.MatchString(term.ID) {
				return nil, fmt.Errorf("%s[%d]: id %q must be snake_case ascii", kind, i, term.ID)
			}
			if ids[term.ID] {
				return nil, fmt.Errorf("%s: duplicate id %q", kind, term.ID)
			}
			ids[term.ID] = true
			variants := append([]string{term.ID, term.Label}, term.Aliases...)
			for _, v := range variants {
				k := Key(kind, v)
				if k == "" {
					return nil, fmt.Errorf("%s/%s: %q normalises to nothing", kind, term.ID, v)
				}
				if other, dup := index[k]; dup && other != term.ID {
					return nil, fmt.Errorf("%s: %q maps to both %q and %q", kind, v, other, term.ID)
				}
				index[k] = term.ID
			}
		}
		t.terms[kind] = terms
		t.index[kind] = index
		t.ids[kind] = ids
	}
	return t, nil
}

// Terms returns the canonical entries of a kind in file order.
func (t *Taxonomy) Terms(kind Kind) []Term {
	out := make([]Term, len(t.terms[kind]))
	copy(out, t.terms[kind])
	return out
}

// IDs returns every canonical id of a kind in file order.
func (t *Taxonomy) IDs(kind Kind) []string {
	out := make([]string, 0, len(t.terms[kind]))
	for _, term := range t.terms[kind] {
		out = append(out, term.ID)
	}
	return out
}

// IsCanonical reports whether value is exactly an id of kind. Labels and
// aliases are not canonical; resolve them first.
func (t *Taxonomy) IsCanonical(kind Kind, value string) bool {
	return t.ids[kind][value]
}

// Resolve maps free text to a canonical id. ok is false when the value is not
// in the taxonomy; callers keep such values in a free-text field.
func (t *Taxonomy) Resolve(kind Kind, raw string) (id string, ok bool) {
	k := Key(kind, raw)
	if k == "" {
		return "", false
	}
	id, ok = t.index[kind][k]
	return id, ok
}

// ResolveAll splits values into canonical ids and unknown free text. Both
// results are de-duplicated and keep first-seen order; unknown values are
// trimmed but otherwise untouched. Both slices are non-nil.
func (t *Taxonomy) ResolveAll(kind Kind, values []string) (canonical, unknown []string) {
	canonical, unknown = []string{}, []string{}
	seenID := map[string]bool{}
	seenUnknown := map[string]bool{}
	for _, raw := range values {
		if id, ok := t.Resolve(kind, raw); ok {
			if !seenID[id] {
				seenID[id] = true
				canonical = append(canonical, id)
			}
			continue
		}
		text := strings.TrimSpace(raw)
		if text == "" {
			continue
		}
		if k := Key(kind, text); !seenUnknown[k] {
			seenUnknown[k] = true
			unknown = append(unknown, text)
		}
	}
	return canonical, unknown
}
