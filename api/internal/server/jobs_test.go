package server

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/tasks"
)

func TestJobsAPI(t *testing.T) {
	a := newAPI(t)

	// Only ops may touch the queue.
	a.want(a.do("POST", "/jobs", "employer", "", map[string]any{"kind": tasks.KindEmbedRole}), 403, "employer enqueue")
	a.want(a.do("GET", "/jobs", "talent", "x", nil), 403, "talent list")

	// Validation: unknown kind names the known ones; bad payload / bounds.
	r := a.want(a.do("POST", "/jobs", "ops", "", map[string]any{"kind": "make_coffee"}), 422, "unknown kind")
	if !strings.Contains(r.field("kind"), tasks.KindEmbedRole) || !strings.Contains(r.field("kind"), tasks.KindEmbedProfile) {
		t.Fatalf("unknown kind should list the known kinds: %s", r.Raw)
	}
	a.want(a.do("POST", "/jobs", "ops", "", map[string]any{}), 422, "missing kind")
	a.want(a.do("POST", "/jobs", "ops", "", map[string]any{"kind": tasks.KindEmbedRole, "payload": []int{1}}), 422, "payload not an object")
	a.want(a.do("POST", "/jobs", "ops", "", map[string]any{"kind": tasks.KindEmbedRole, "max_attempts": 0}), 422, "max_attempts 0")
	a.want(a.do("POST", "/jobs", "ops", "", map[string]any{"kind": tasks.KindEmbedRole, "priority": 40000}), 422, "priority out of range")
	a.want(a.do("POST", "/jobs", "ops", "", map[string]any{"kind": tasks.KindEmbedRole, "colour": "blue"}), 400, "unknown field")

	// Enqueue with every option set.
	later := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	r = a.want(a.do("POST", "/jobs", "ops", "", map[string]any{
		"kind": tasks.KindEmbedRole, "payload": map[string]any{"role_id": "11111111-0000-0000-0000-000000000001"},
		"priority": 5, "max_attempts": 7, "run_at": later.Format(time.RFC3339),
	}), 201, "enqueue")
	id := r.Body["id"].(float64)
	if r.str("status") != "queued" || r.Body["priority"].(float64) != 5 || r.Body["max_attempts"].(float64) != 7 || r.Body["attempts"].(float64) != 0 {
		t.Fatalf("enqueued job: %s", r.Raw)
	}
	if runAt, _ := time.Parse(time.RFC3339, r.str("run_at")); !runAt.Equal(later) {
		t.Fatalf("run_at = %s, want %s", r.str("run_at"), later)
	}
	if got, _ := json.Marshal(r.Body["payload"]); string(got) != `{"role_id":"11111111-0000-0000-0000-000000000001"}` {
		t.Fatalf("payload round trip: %s", got)
	}
	for _, k := range []string{"last_error", "worker", "started_at", "finished_at"} {
		if v, ok := r.Body[k]; !ok || v != nil {
			t.Fatalf("%s should be present and null on a fresh job: %s", k, r.Raw)
		}
	}

	// The same kind and payload again while it waits is the same job, not a duplicate.
	dup := a.want(a.do("POST", "/jobs", "ops", "", map[string]any{
		"kind": tasks.KindEmbedRole, "payload": map[string]any{"role_id": "11111111-0000-0000-0000-000000000001"},
	}), 200, "duplicate enqueue")
	if dup.Body["id"].(float64) != id {
		t.Fatalf("duplicate should return the waiting job %v, got %s", id, dup.Raw)
	}

	// Poll it, list it, filter it.
	get := a.want(a.do("GET", "/jobs/"+strconv.FormatInt(int64(id), 10), "ops", "", nil), 200, "get")
	if get.str("kind") != tasks.KindEmbedRole {
		t.Fatalf("get: %s", get.Raw)
	}
	a.want(a.do("GET", "/jobs/999999", "ops", "", nil), 404, "get missing")
	a.want(a.do("GET", "/jobs/not-a-number", "ops", "", nil), 404, "get bad id")
	list := a.want(a.do("GET", "/jobs?kind="+tasks.KindEmbedRole+"&status=queued", "ops", "", nil), 200, "list")
	if len(list.List) != 1 {
		t.Fatalf("list: %s", list.Raw)
	}
	a.want(a.do("GET", "/jobs?status=bogus", "ops", "", nil), 422, "list bad status")
	if empty := a.want(a.do("GET", "/jobs?status=failed", "ops", "", nil), 200, "list failed"); len(empty.List) != 0 {
		t.Fatalf("no failed jobs expected: %s", empty.Raw)
	}
}

// Writes that leave a row without an embedding queue the job that computes
// one; writes that keep the embedding do not.
func TestWritesEnqueueEmbeddingJobs(t *testing.T) {
	a := newAPI(t)
	jobsFor := func(kind, field, id string) int {
		return a.count(`SELECT count(*) FROM jobs WHERE kind = $1 AND payload->>$2 = $3`, kind, field, id)
	}

	// A role without a description has nothing to embed.
	bare := a.role("Bookkeeper")
	if n := jobsFor(tasks.KindEmbedRole, "role_id", bare); n != 0 {
		t.Fatalf("role without description queued %d embed jobs", n)
	}
	// Giving it one queues exactly one job; saving again while that job waits
	// does not add another (dedupe), and neither does a status-only edit.
	a.want(a.do("PUT", "/roles/"+bare, "employer", "", map[string]any{"description": "Keeps the books."}), 200, "add description")
	a.want(a.do("PUT", "/roles/"+bare, "employer", "", map[string]any{"description": "Keeps the books, daily."}), 200, "edit again while queued")
	if n := jobsFor(tasks.KindEmbedRole, "role_id", bare); n != 1 {
		t.Fatalf("two saves while the job waits should leave 1 embed_role job, got %d", n)
	}
	workerRan := func() { // pretend the worker embedded it and finished the job
		a.exec(`UPDATE roles SET embedding_model = 'stub', embedded_at = now() WHERE id = $1`, bare)
		a.exec(`UPDATE jobs SET status = 'succeeded', finished_at = now() WHERE status = 'queued' AND payload->>'role_id' = $1`, bare)
	}
	workerRan()
	a.want(a.do("PUT", "/roles/"+bare, "employer", "", map[string]any{"status": "filled"}), 200, "status edit")
	if n := jobsFor(tasks.KindEmbedRole, "role_id", bare); n != 1 {
		t.Fatalf("want 1 embed_role job after a status edit, got %d", n)
	}
	// Changing the description clears the embedding and queues again.
	a.want(a.do("PUT", "/roles/"+bare, "employer", "", map[string]any{"description": "Keeps the books and the ledgers."}), 200, "edit description")
	if n := jobsFor(tasks.KindEmbedRole, "role_id", bare); n != 2 {
		t.Fatalf("want 2 embed_role jobs after a description change, got %d", n)
	}
	// Creating with a description queues on create.
	r := a.want(a.do("POST", "/roles", "employer", "", map[string]any{"title": "Controller", "description": "Owns the close."}), 201, "create with description")
	if n := jobsFor(tasks.KindEmbedRole, "role_id", r.str("id")); n != 1 {
		t.Fatalf("create with description queued %d jobs, want 1", n)
	}

	// Profiles: the upsert always queues when the embedding is missing.
	cand := a.candidate("Dana Ito", "dana@example.com")
	a.want(a.do("PUT", "/candidates/"+cand+"/profile", "ops", "", map[string]any{"headline": "Senior accountant"}), 201, "create profile")
	if n := jobsFor(tasks.KindEmbedProfile, "candidate_id", cand); n != 1 {
		t.Fatalf("profile create queued %d jobs, want 1", n)
	}
	a.exec(`UPDATE candidate_profiles SET embedding_model = 'stub', embedded_at = now() WHERE candidate_id = $1`, cand)
	a.exec(`UPDATE jobs SET status = 'succeeded', finished_at = now() WHERE status = 'queued' AND payload->>'candidate_id' = $1`, cand)
	a.want(a.do("PUT", "/candidates/"+cand+"/profile", "ops", "", map[string]any{"headline": "Senior accountant"}), 200, "unchanged profile")
	if n := jobsFor(tasks.KindEmbedProfile, "candidate_id", cand); n != 1 {
		t.Fatalf("unchanged profile should not queue: got %d jobs", n)
	}
	a.want(a.do("PUT", "/candidates/"+cand+"/profile", "ops", "", map[string]any{"headline": "Controller"}), 200, "changed profile")
	if n := jobsFor(tasks.KindEmbedProfile, "candidate_id", cand); n != 2 {
		t.Fatalf("changed profile should queue again: got %d jobs", n)
	}
}
