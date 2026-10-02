package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/aiclient"
	"github.com/colehanke/mavi-demo/api/internal/jobs"
	"github.com/colehanke/mavi-demo/api/internal/tasks"
)

// maxResumeBytes is the AI service's own upload limit (MAX_UPLOAD_BYTES in
// ai/app/documents.py). It is applied here as well so a larger body is
// refused before it is read into memory and sent on.
const maxResumeBytes = 5 << 20

// uploadResume is talent intake: the request body is the resume file itself.
// Its text is extracted in the request (quick, and what is wrong with a file
// is best said to the person uploading it), stored as the candidate's
// resume_text, and a parse_resume job is queued to turn it into the profile.
// The answer is that job; a second upload while it still waits replaces the
// text and gets the same job back.
func (s *Server) uploadResume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !ownsCandidate(w, r, id) {
		return
	}
	if _, err := s.store.GetCandidate(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	file, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxResumeBytes))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "file too large: the limit is 5.0 MB")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	case len(file) == 0:
		writeError(w, http.StatusUnprocessableEntity, "the request body is empty: send the PDF itself as the body")
		return
	}
	doc, err := s.ai.ExtractText(r.Context(), file)
	if err != nil {
		failExtract(w, err)
		return
	}

	// The file was accepted; from here a client that disconnects must not
	// leave the text stored with no job to parse it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if err := s.store.SetResumeText(ctx, id, doc.Text); err != nil {
		fail(w, err)
		return
	}
	job, _, err := s.jobs.Enqueue(ctx, jobs.EnqueueInput{Kind: tasks.KindParseResume, Payload: resumeJobPayload(id)})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

// getResumeJob is the candidate's most recent parse_resume job: what the
// talent UI polls after an upload until status is succeeded (the profile is
// then at GET …/profile) or failed.
func (s *Server) getResumeJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !ownsCandidate(w, r, id) {
		return
	}
	job, err := s.jobs.Latest(r.Context(), tasks.KindParseResume, resumeJobPayload(id))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func resumeJobPayload(candidateID string) json.RawMessage {
	payload, _ := json.Marshal(map[string]string{"candidate_id": candidateID})
	return payload
}

// failExtract answers for a failed text extraction. A file the AI service
// refused (too large, not a PDF or DOCX, unreadable, no text) is the
// caller's to fix, so its status and reason are passed on; anything else is
// the service's trouble and a 503.
func failExtract(w http.ResponseWriter, err error) {
	var ae *aiclient.Error
	if errors.As(err, &ae) {
		switch ae.StatusCode {
		case http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
			writeError(w, ae.StatusCode, ae.Detail)
			return
		}
	}
	if errors.Is(err, context.Canceled) {
		return // client went away
	}
	logf("extract resume text: %v", err)
	writeError(w, http.StatusServiceUnavailable, "the resume could not be read right now; try again shortly")
}
