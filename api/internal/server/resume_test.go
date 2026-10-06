package server

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/colehanke/mavi-demo/api/internal/aiclient"
	"github.com/colehanke/mavi-demo/api/internal/tasks"
)

// An upload stores the extracted text and answers with the parse job; the
// job is what both the uploader and ops poll.
func TestResumeUploadStoresTextAndQueuesParse(t *testing.T) {
	a := newAPI(t)
	cand := a.candidate("Ada Okafor", "ada@example.com")
	other := a.candidate("Ben Larsen", "ben@example.com")
	resumeText := func() (text string) {
		a.t.Helper()
		if err := a.pool.QueryRow(t.Context(), `SELECT resume_text FROM candidates WHERE id = $1`, cand).Scan(&text); err != nil {
			t.Fatal(err)
		}
		return text
	}

	// Nothing uploaded yet: no job to report.
	a.want(a.do("GET", "/candidates/"+cand+"/resume/job", "talent", cand, nil), 404, "status before upload")

	// Talent may upload to their own record only; employers not at all.
	a.want(a.do("POST", "/candidates/"+cand+"/resume", "employer", "", "%PDF-1.7 x"), 403, "employer upload")
	a.want(a.do("POST", "/candidates/"+other+"/resume", "talent", cand, "%PDF-1.7 x"), 404, "talent uploads to another candidate")
	a.want(a.do("POST", "/candidates/11111111-0000-0000-0000-000000000009/resume", "ops", "", "%PDF-1.7 x"), 404, "no such candidate")
	a.want(a.do("POST", "/candidates/"+cand+"/resume", "talent", cand, nil), 422, "empty body")

	// The stub AI returns the file's bytes as its text.
	r := a.want(a.do("POST", "/candidates/"+cand+"/resume", "talent", cand, "%PDF-1.7 Ada Okafor, CPA"), 202, "upload")
	id := r.Body["id"].(float64)
	if r.str("kind") != tasks.KindParseResume || r.str("status") != "queued" {
		t.Fatalf("upload should answer with the queued parse job: %s", r.Raw)
	}
	if payload, _ := r.Body["payload"].(map[string]any); payload["candidate_id"] != cand {
		t.Fatalf("job payload: %s", r.Raw)
	}
	if got := resumeText(); got != "%PDF-1.7 Ada Okafor, CPA" {
		t.Fatalf("resume_text = %q", got)
	}

	// A second upload while the job waits replaces the text and reuses the job.
	r = a.want(a.do("POST", "/candidates/"+cand+"/resume", "ops", "", "%PDF-1.7 Ada Okafor, CPA, CMA"), 202, "second upload")
	if r.Body["id"].(float64) != id || resumeText() != "%PDF-1.7 Ada Okafor, CPA, CMA" {
		t.Fatalf("second upload: job %v (want %v), text %q", r.Body["id"], id, resumeText())
	}
	if n := a.count(`SELECT count(*) FROM jobs WHERE kind = $1`, tasks.KindParseResume); n != 1 {
		t.Fatalf("want 1 parse job, got %d", n)
	}

	// The same job is readable by its owner, by ops, and over /jobs.
	for _, who := range [][2]string{{"talent", cand}, {"ops", ""}} {
		r = a.want(a.do("GET", "/candidates/"+cand+"/resume/job", who[0], who[1], nil), 200, "status as "+who[0])
		if r.Body["id"].(float64) != id {
			t.Fatalf("status as %s: %s", who[0], r.Raw)
		}
	}
	a.want(a.do("GET", "/candidates/"+cand+"/resume/job", "talent", other, nil), 404, "another candidate's status")
	a.want(a.do("GET", "/jobs/"+strconv.FormatInt(int64(id), 10), "ops", "", nil), 200, "job by id")

	// Once that job has run, a new upload is a new job, and it is the one reported.
	a.exec(`UPDATE jobs SET status = 'succeeded', finished_at = now() WHERE id = $1`, int64(id))
	if r = a.want(a.do("GET", "/candidates/"+cand+"/resume/job", "talent", cand, nil), 200, "status after run"); r.str("status") != "succeeded" {
		t.Fatalf("status after run: %s", r.Raw)
	}
	r = a.want(a.do("POST", "/candidates/"+cand+"/resume", "talent", cand, "%PDF-1.7 v3"), 202, "upload after run")
	if r.Body["id"].(float64) == id {
		t.Fatalf("an upload after the job ran should queue a new one: %s", r.Raw)
	}
	latest := a.want(a.do("GET", "/candidates/"+cand+"/resume/job", "talent", cand, nil), 200, "status of newest")
	if latest.Body["id"].(float64) != r.Body["id"].(float64) || latest.str("status") != "queued" {
		t.Fatalf("status should be the newest job: %s", latest.Raw)
	}
}

// A file the AI service refuses is the uploader's to fix: its status and
// reason are passed on, and nothing is stored or queued.
func TestResumeUploadRejections(t *testing.T) {
	refuse := func(status int, detail string) error {
		return &aiclient.Error{Op: "extract-text", StatusCode: status, Detail: detail}
	}
	a := newAPIWith(t, stub{extract: func(file []byte) (aiclient.ExtractTextResponse, error) {
		switch {
		case strings.HasPrefix(string(file), "GIF"):
			return aiclient.ExtractTextResponse{}, refuse(415, "unsupported file type: only PDF and DOCX files are accepted")
		case strings.HasPrefix(string(file), "%PDF-scan"):
			return aiclient.ExtractTextResponse{}, refuse(422, "no text found in the file")
		default:
			return aiclient.ExtractTextResponse{}, errors.New("connection refused")
		}
	}})
	silence(t)
	cand := a.candidate("Ada Okafor", "ada@example.com")

	r := a.want(a.do("POST", "/candidates/"+cand+"/resume", "ops", "", "GIF89a"), 415, "not a PDF")
	if !strings.Contains(r.str("error"), "only PDF and DOCX") {
		t.Fatalf("the refusal should say why: %s", r.Raw)
	}
	r = a.want(a.do("POST", "/candidates/"+cand+"/resume", "ops", "", "%PDF-scan"), 422, "no text")
	if r.str("error") != "no text found in the file" {
		t.Fatalf("the refusal should say why: %s", r.Raw)
	}
	a.want(a.do("POST", "/candidates/"+cand+"/resume", "ops", "", "%PDF-1.7 fine"), 503, "AI service down")
	a.want(a.do("POST", "/candidates/"+cand+"/resume", "ops", "", strings.Repeat("x", maxResumeBytes+1)), 413, "over the size limit")

	if n := a.count(`SELECT count(*) FROM jobs`); n != 0 {
		t.Fatalf("refused uploads queued %d jobs", n)
	}
	if n := a.count(`SELECT count(*) FROM candidates WHERE id = $1 AND resume_text = ''`, cand); n != 1 {
		t.Fatal("a refused upload changed resume_text")
	}
}

// silence drops the server's log lines for the test.
func silence(t *testing.T) {
	t.Helper()
	old := logf
	logf = func(context.Context, string, ...any) {}
	t.Cleanup(func() { logf = old })
}
