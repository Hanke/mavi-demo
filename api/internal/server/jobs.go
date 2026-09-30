package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/contract"
	"github.com/colehanke/mavi-demo/api/internal/jobs"
	"github.com/colehanke/mavi-demo/api/internal/store"
)

// createJob enqueues a job by hand (ops). The kind has to be one the worker
// can run, so a typo does not sit in the queue forever.
func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	var b contract.JobCreate
	if _, ok := decodeBody(w, r, &b, false); !ok {
		return
	}
	v := &validationError{}
	in := jobs.EnqueueInput{Kind: strings.TrimSpace(b.Kind)}
	switch {
	case in.Kind == "":
		v.add("kind", "required")
	case !slices.Contains(s.jobKinds, in.Kind):
		v.add("kind", "no handler registered; known kinds: "+strings.Join(s.jobKinds, ", "))
	}
	in.Payload = jsonObject(v, "payload", b.Payload)
	if b.Priority != nil {
		if *b.Priority < -32768 || *b.Priority > 32767 {
			v.add("priority", "must be between -32768 and 32767")
		}
		in.Priority = *b.Priority
	}
	if b.RunAt != nil {
		in.RunAt = *b.RunAt
	}
	if b.MaxAttempts != nil {
		if *b.MaxAttempts < 1 || *b.MaxAttempts > 20 {
			v.add("max_attempts", "must be between 1 and 20")
		}
		in.MaxAttempts = *b.MaxAttempts
	}
	if err := v.err(); err != nil {
		fail(w, err)
		return
	}
	job, created, err := s.jobs.Enqueue(r.Context(), in)
	if err != nil {
		fail(w, err)
		return
	}
	code := http.StatusCreated
	if !created {
		code = http.StatusOK // an identical job was already queued; that one is returned
	}
	writeJSON(w, code, job)
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	v := &validationError{}
	f := jobs.Filter{Status: r.URL.Query().Get("status"), Kind: strings.TrimSpace(r.URL.Query().Get("kind"))}
	if f.Status != "" {
		validEnum[contract.JobStatus](v, "status", f.Status)
	}
	if err := v.err(); err != nil {
		fail(w, err)
		return
	}
	out, err := s.jobs.List(r.Context(), f, pageFrom(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// getJob is what the UI polls until status is succeeded or failed.
func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, store.ErrNotFound)
		return
	}
	job, err := s.jobs.Get(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// enqueueEmbedding queues an embedding job for a row a write just left
// without one. The write is already committed, so the enqueue is detached
// from the request's context: a client that disconnects now must not take
// the job with it. A failure here is logged, not returned: the row is
// correct, it is just not embedded yet, and the next edit (or a manual
// POST /jobs) will queue it again. An identical job already waiting is
// reused, not duplicated.
func (s *Server) enqueueEmbedding(r *http.Request, kind, idField, id string) {
	payload, err := json.Marshal(map[string]string{idField: id})
	if err != nil {
		logf("enqueue %s for %s %s: %v", kind, idField, id, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if _, _, err := s.jobs.Enqueue(ctx, jobs.EnqueueInput{Kind: kind, Payload: payload}); err != nil {
		logf("enqueue %s for %s %s: %v", kind, idField, id, err)
	}
}
