// Package e2e holds the tests that take one role through the whole product,
// from the candidates in the pool to the two profiles the employer is shown,
// as its users do it: over HTTP, one persona at a time.
//
// pipeline_test.go runs the API and its worker in the test process against a
// real Postgres with pgvector (internal/dbtest) and a scripted stand-in for
// the AI service, so who reaches the review queue is decided by the test.
// smoke_test.go, built only with the `smoke` tag, drives a running compose
// stack (`make smoke`). This file is the client both use.
package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/auth"
	"github.com/colehanke/mavi-demo/api/internal/contract"
)

// persona is who a request is made as: the X-Role header and, for a persona
// that has to say who they are, X-Actor. The zero value is nobody, which is
// enough for /health.
type persona struct {
	role  auth.Role
	actor string
}

var (
	employer = persona{role: auth.Employer}
	ops      = persona{role: auth.Ops, actor: "ops@example.com"}
)

// talent is a candidate acting on their own record.
func talent(candidateID string) persona { return persona{role: auth.Talent, actor: candidateID} }

// client calls the API at base on behalf of a test.
type client struct {
	t    *testing.T
	base string
	http *http.Client
}

func newClient(t *testing.T, base string) *client {
	return &client{t: t, base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: time.Minute}}
}

// send makes one request as who and fails the test unless the answer has the
// status want; the answer's body, when it has one, is decoded into T. body
// is sent as it is when it is a []byte (a file) and as JSON otherwise.
func send[T any](c *client, who persona, method, path string, body any, want int) T {
	c.t.Helper()
	var reader io.Reader
	contentType := ""
	switch b := body.(type) {
	case nil:
	case []byte:
		reader, contentType = bytes.NewReader(b), "application/octet-stream"
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			c.t.Fatal(err)
		}
		reader, contentType = bytes.NewReader(buf), "application/json"
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		c.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if who.role != "" {
		req.Header.Set(auth.RoleHeader, string(who.role))
	}
	if who.actor != "" {
		req.Header.Set(auth.ActorHeader, who.actor)
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s as %s: %v", method, path, who.role, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		c.t.Fatalf("%s %s as %s: read body: %v", method, path, who.role, err)
	}
	if res.StatusCode != want {
		c.t.Fatalf("%s %s as %s: status = %d, want %d; body: %s", method, path, who.role, res.StatusCode, want, raw)
	}
	var out T
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			c.t.Fatalf("%s %s as %s: decode %T: %v; body: %s", method, path, who.role, out, err, raw)
		}
	}
	return out
}

// eventually polls done, which reports whether what the test waits for has
// happened and otherwise where things stand, until it has or limit has passed.
func (c *client) eventually(what string, limit time.Duration, done func() (ok bool, state string)) {
	c.t.Helper()
	deadline := time.Now().Add(limit)
	for {
		ok, state := done()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("timed out after %s waiting for %s: %s", limit, what, state)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// finished reports whether a background job is done. One that failed for
// good fails the test with the job's own error.
func (c *client) finished(job contract.Job) (bool, string) {
	c.t.Helper()
	if job.Status == contract.JobStatusFailed {
		c.t.Fatalf("%s job %d failed: %s", job.Kind, job.ID, deref(job.LastError))
	}
	return job.Status == contract.JobStatusSucceeded, job.Kind + " job is " + string(job.Status) + ": " + deref(job.LastError)
}

// names is the candidates of a list of matches, in order.
func names(matches []contract.Match) string {
	out := make([]string, len(matches))
	for i, m := range matches {
		out[i] = m.CandidateName
	}
	return strings.Join(out, ", ")
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
