// Package contract holds the Go side of the API's OpenAPI contract.
//
// types.gen.go is generated from api/openapi.yaml by oapi-codegen and is the
// only definition of the JSON shapes the API accepts and returns: the store's
// row types are aliases of the generated response types and the handlers
// decode request bodies into the generated input types. Change the YAML, run
// `make generate` (or `go generate ./...`), and the compiler points at every
// place the Go code no longer matches. The web app's TypeScript types are
// generated from the same file.
//
// This file supplies the two hand-written types the spec maps to with
// x-go-type: Date for `format: date` strings and RawJSON for free-form JSONB
// objects.
package contract

import (
	"encoding/json"
	"fmt"
	"time"
)

//go:generate go tool oapi-codegen -config oapi-codegen.yaml ../../openapi.yaml

// RawJSON is a free-form JSON object passed through to and from JSONB
// columns without decoding (the spec's JSONObject).
type RawJSON = json.RawMessage

// Date is a calendar day serialised as YYYY-MM-DD (the spec's CalendarDate).
type Date time.Time

func (d Date) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Time(d).Format("2006-01-02"))
}

func (d *Date) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return fmt.Errorf("date %q: want YYYY-MM-DD", s)
	}
	*d = Date(t)
	return nil
}

// TimePtr converts an optional Date for a query parameter; nil stays nil.
func (d *Date) TimePtr() *time.Time {
	if d == nil {
		return nil
	}
	t := time.Time(*d)
	return &t
}

// DatePtr wraps an optional scanned timestamp as a Date; nil stays nil.
func DatePtr(t *time.Time) *Date {
	if t == nil {
		return nil
	}
	d := Date(*t)
	return &d
}
