// Package server is the HTTP surface of the API: JSON handlers for
// candidates, profiles, roles, matches and background jobs, each gated by
// the demo role switcher in internal/auth.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/aiclient"
	"github.com/colehanke/mavi-demo/api/internal/auth"
	"github.com/colehanke/mavi-demo/api/internal/contract"
	"github.com/colehanke/mavi-demo/api/internal/jobs"
	"github.com/colehanke/mavi-demo/api/internal/reqlog"
	"github.com/colehanke/mavi-demo/api/internal/store"
	"github.com/colehanke/mavi-demo/api/internal/tasks"
	"github.com/colehanke/mavi-demo/api/internal/taxonomy"
)

// Pinger is the subset of *pgxpool.Pool the health check needs, so tests can stub it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// AI is the subset of *aiclient.Client the server needs: the health probe,
// text extraction for resume uploads, and the JD parse of role intake, whose
// result is the response. Everything else slow goes through the job queue.
type AI interface {
	Health(ctx context.Context) error
	ExtractText(ctx context.Context, file []byte) (aiclient.ExtractTextResponse, error)
	ParseJD(ctx context.Context, text string) (aiclient.ParsedJD, error)
}

var _ AI = (*aiclient.Client)(nil)

// Config wires the server's dependencies. Store, Taxonomy and Jobs may be
// nil for a health-only server (every other route then 503s).
type Config struct {
	DB       Pinger
	AI       AI
	Store    *store.Store
	Taxonomy *taxonomy.Taxonomy
	Jobs     *jobs.Queue
	JobKinds []string // kinds POST /jobs accepts: what the worker has handlers for
	// EmbedRole embeds a stored role now (tasks.EmbedRole), for role intake.
	// Nil leaves every embedding to the embed_role job.
	EmbedRole func(ctx context.Context, roleID string) error
	// RetrievalSize is how many of the candidates who pass a role's hard
	// filters a run retrieves by embedding similarity. Below 1 is
	// store.DefaultRetrievalLimit.
	RetrievalSize int
	// MinScore is the least a candidate in reserve may score and be brought
	// into the review queue by a swap: the matching run's own threshold
	// (tasks.MatchConfig). 0 or less is tasks.DefaultMinScore.
	MinScore   float64
	CORSOrigin string
}

type Server struct {
	db            Pinger
	ai            AI
	store         *store.Store
	tax           *taxonomy.Taxonomy
	jobs          *jobs.Queue
	jobKinds      []string
	embedRoleNow  func(ctx context.Context, roleID string) error
	retrievalSize int
	minScore      float64
	corsOrigin    string
}

const maxBody = 1 << 20

func New(cfg Config) http.Handler {
	s := &Server{db: cfg.DB, ai: cfg.AI, store: cfg.Store, tax: cfg.Taxonomy, jobs: cfg.Jobs, jobKinds: cfg.JobKinds, embedRoleNow: cfg.EmbedRole, retrievalSize: cfg.RetrievalSize, minScore: cfg.MinScore, corsOrigin: cfg.CORSOrigin}
	if s.minScore <= 0 {
		s.minScore = tasks.DefaultMinScore
	}
	mux := http.NewServeMux()
	mux.HandleFunc(healthRoute, s.handleHealth)
	for _, rt := range s.routes() {
		mux.Handle(rt.pattern, auth.Require(writeError, s.ready(rt.handler), rt.roles...))
	}
	return s.requestLog(s.cors(mux))
}

const healthRoute = "GET /health"

// routeDef is one row of the route table: a ServeMux pattern and the roles
// that may call it.
type routeDef struct {
	pattern string
	handler http.HandlerFunc
	roles   []auth.Role
}

// routes is every role-gated route. contract_test.go checks it against the
// operations and x-roles in api/openapi.yaml, so the spec and the server
// cannot disagree about what exists or who may call it.
func (s *Server) routes() []routeDef {
	talent, employer, ops := auth.Talent, auth.Employer, auth.Ops
	r := func(pattern string, h http.HandlerFunc, roles ...auth.Role) routeDef {
		return routeDef{pattern: pattern, handler: h, roles: roles}
	}
	return []routeDef{
		// Candidates: talent manages their own record, ops manages all.
		r("POST /candidates", s.createCandidate, talent, ops),
		r("GET /candidates", s.listCandidates, ops),
		r("GET /candidates/{id}", s.getCandidate, talent, ops),
		r("PUT /candidates/{id}", s.updateCandidate, talent, ops),
		r("DELETE /candidates/{id}", s.deleteCandidate, ops),
		r("GET /candidates/{id}/profile", s.getProfile, talent, ops),
		r("PUT /candidates/{id}/profile", s.putProfile, talent, ops),
		r("DELETE /candidates/{id}/profile", s.deleteProfile, ops),

		// Resume intake: the upload stores the file's text and queues the parse
		// that fills the profile above; the job route is how talent, who cannot
		// read /jobs, follows it.
		r("POST /candidates/{id}/resume", s.uploadResume, talent, ops),
		r("GET /candidates/{id}/resume/job", s.getResumeJob, talent, ops),

		// Availability: the hard-filter answers a resume cannot give, supplied
		// by the candidate; and, for ops, who passes a role's filters on them.
		r("GET /candidates/{id}/availability", s.getAvailability, talent, ops),
		r("PUT /candidates/{id}/availability", s.putAvailability, talent, ops),
		r("GET /roles/{id}/availability", s.listRoleAvailability, ops),

		// Hard filters: the first stage of a matching run, on its own. Ops
		// runs it for a role and reads back how the pool narrowed.
		r("POST /roles/{id}/filter-runs", s.runRoleFilters, ops),
		r("GET /roles/{id}/filter-runs", s.listRoleFilterRuns, ops),
		// Where the role's matching stands. An employer is told in_review or
		// ready; why a run needs attention is for ops.
		r("GET /roles/{id}/match-status", s.getRoleMatchStatus, employer, ops),

		// Roles: employers and ops write; talent reads open roles. Intake is
		// the employer's way in: a pasted JD, parsed, stored, embedded and
		// queued for matching.
		r("POST /roles", s.createRole, employer, ops),
		r("POST /roles/intake", s.intakeRole, employer, ops),
		r("GET /roles", s.listRoles, talent, employer, ops),
		r("GET /roles/{id}", s.getRole, talent, employer, ops),
		r("PUT /roles/{id}", s.updateRole, employer, ops),
		r("DELETE /roles/{id}", s.deleteRole, employer, ops),

		// Review: ops decides on the role's matches, one at a time, and
		// releases the role once exactly two are approved. Each decision is
		// recorded in review_events, which the last of these reads back.
		r("GET /roles/{id}/review-queue", s.getReviewQueue, ops),
		r("POST /matches/{id}/approve", s.approveMatch, ops),
		r("POST /matches/{id}/reject", s.rejectMatch, ops),
		r("POST /matches/{id}/swap", s.swapMatch, ops),
		r("POST /roles/{id}/release", s.releaseRole, ops),
		r("POST /matches/{id}/unrelease", s.unreleaseMatch, ops),
		r("GET /roles/{id}/review-events", s.listReviewEvents, ops),

		// Matches: ops writes them; employers and talent only ever see
		// released rows (talent only their own).
		r("POST /matches", s.createMatch, ops),
		r("GET /matches", s.listMatches, talent, employer, ops),
		r("GET /matches/{id}", s.getMatch, talent, employer, ops),
		r("PUT /matches/{id}", s.updateMatch, ops),
		r("DELETE /matches/{id}", s.deleteMatch, ops),

		// Jobs: the background queue. Ops enqueues by hand and polls status;
		// the write paths above enqueue embedding jobs on their own.
		r("POST /jobs", s.createJob, ops),
		r("GET /jobs", s.listJobs, ops),
		r("GET /jobs/{id}", s.getJob, ops),
	}
}

// ready refuses CRUD requests on a server built without a store.
func (s *Server) ready(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.store == nil || s.tax == nil || s.jobs == nil {
			writeError(w, http.StatusServiceUnavailable, "database not configured")
			return
		}
		next(w, r)
	})
}

// handleHealth reports 200 only when both downstream dependencies are reachable,
// which is what the compose healthcheck and the acceptance criteria key off.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	resp := contract.HealthResponse{Status: contract.HealthStatusOk, Checks: map[string]string{}}
	check := func(name string, err error) {
		if err != nil {
			resp.Status = contract.HealthStatusDegraded
			resp.Checks[name] = "error: " + err.Error()
			return
		}
		resp.Checks[name] = "ok"
	}
	check("postgres", s.db.Ping(ctx))
	check("ai", s.ai.Health(ctx))

	code := http.StatusOK
	if resp.Status != contract.HealthStatusOk {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, resp)
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", s.corsOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, "+auth.RoleHeader+", "+auth.ActorHeader+", "+reqlog.Header)
		w.Header().Set("Access-Control-Expose-Headers", reqlog.Header)
		if s.corsOrigin != "*" {
			// Lets the mavi_role / mavi_actor session cookies travel from the web origin.
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requestLog gives every request its id and writes its access line. The id
// is the caller's X-Request-ID when it sends a usable one, otherwise new; it
// is returned in the same header, carried by the request's context onto
// every line logged for the request, and sent on to the AI service with any
// call the request makes (internal/reqlog), which is how one request is
// followed across both services. The health probe and CORS preflights are
// logged at debug.
func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(reqlog.Header)
		if !reqlog.Valid(id) {
			id = reqlog.NewID()
		}
		ctx := reqlog.WithID(r.Context(), id)
		w.Header().Set(reqlog.Header, id)
		rw := &responseLog{ResponseWriter: w, ctx: ctx}
		r = r.WithContext(ctx)
		start := time.Now()
		next.ServeHTTP(rw, r)

		level := slog.LevelInfo
		if r.Pattern == healthRoute || r.Method == http.MethodOptions {
			level = slog.LevelDebug
		}
		if rw.status == 0 {
			rw.status = http.StatusOK // a handler that wrote nothing
		}
		// route is the pattern the mux matched; "" for a path it does not serve.
		slog.Log(ctx, level, "request", "method", r.Method, "path", r.URL.Path, "route", r.Pattern,
			"status", rw.status, "duration_ms", time.Since(start).Milliseconds(), "role", r.Header.Get(auth.RoleHeader))
	})
}

// responseLog is the ResponseWriter of a logged request: it notes the status
// for the access line, and holds the request's context so that a helper with
// only the writer in hand (fail) logs with the request id.
type responseLog struct {
	http.ResponseWriter
	ctx    context.Context
	status int
}

func (w *responseLog) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseLog) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the writer underneath.
func (w *responseLog) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// requestCtx is the context of the request w answers, for logging; the
// background context when w is not a logged request's writer.
func requestCtx(w http.ResponseWriter) context.Context {
	if rw, ok := w.(*responseLog); ok {
		return rw.ctx
	}
	return context.Background()
}

// ---------------------------------------------------------------------------
// request / response helpers
// ---------------------------------------------------------------------------

// validationError is a 422 with per-field messages.
type validationError struct {
	Fields map[string]string
}

func (v *validationError) Error() string {
	parts := make([]string, 0, len(v.Fields))
	for k, msg := range v.Fields {
		parts = append(parts, k+": "+msg)
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

func (v *validationError) add(field, msg string) {
	if v.Fields == nil {
		v.Fields = map[string]string{}
	}
	if _, dup := v.Fields[field]; !dup {
		v.Fields[field] = msg
	}
}

func (v *validationError) err() error {
	if len(v.Fields) == 0 {
		return nil
	}
	return v
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, contract.Error{Error: msg})
}

// fail maps store and validation errors onto status codes.
func fail(w http.ResponseWriter, err error) {
	var ve *validationError
	var refused *store.RefusedError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusUnprocessableEntity, contract.Error{Error: "validation failed", Fields: &ve.Fields})
	case errors.As(err, &refused):
		writeError(w, http.StatusConflict, refused.Reason)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "already exists: "+constraint(err))
	case errors.Is(err, store.ErrInUse):
		writeError(w, http.StatusConflict, "cannot delete: still referenced by "+constraint(err))
	case errors.Is(err, store.ErrBadRef):
		writeJSON(w, http.StatusUnprocessableEntity, contract.Error{Error: "referenced row does not exist: " + constraint(err)})
	case errors.Is(err, context.Canceled):
		// client went away; nothing useful to write
	default:
		logf(requestCtx(w), "internal error: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// logf is the server's log line, written with the request id ctx carries; a
// variable so tests can silence or capture it.
var logf = func(ctx context.Context, format string, args ...any) {
	slog.WarnContext(ctx, fmt.Sprintf(format, args...))
}

// constraint names the violated constraint, for error messages.
func constraint(err error) string {
	var ce *store.ConstraintError
	if errors.As(err, &ce) {
		return ce.Constraint
	}
	return "a constraint"
}

// body is a decoded JSON object plus the set of keys it carried, so a PUT
// handler can tell an omitted field from an explicit null.
type body map[string]json.RawMessage

func (b body) has(key string) bool { _, ok := b[key]; return ok }

// set reports whether the body carried key with a non-null value.
func (b body) set(key string) bool { raw, ok := b[key]; return ok && string(raw) != "null" }

// decodeBody reads a JSON object into v and writes a 400 on failure,
// returning false when it did. Unknown fields are an error so a typo in a
// field name does not silently drop the value. An empty body is treated as
// "{}" when allowEmpty is set.
func decodeBody(w http.ResponseWriter, r *http.Request, v any, allowEmpty bool) (body, bool) {
	buf, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return nil, false
	}
	if allowEmpty && len(strings.TrimSpace(string(buf))) == 0 {
		return body{}, true
	}
	// Postgres stores no NUL in text or jsonb, so one would be a 500 at the write.
	if hasNUL(buf) {
		writeError(w, http.StatusBadRequest, "invalid JSON body: it contains a NUL character (\\u0000), which cannot be stored")
		return nil, false
	}
	var keys body
	if err := json.Unmarshal(buf, &keys); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(buf))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return nil, false
	}
	return keys, true
}

// hasNUL reports whether a JSON document holds a NUL character: a raw zero
// byte, or the escape \u0000 (and not the six characters of it spelled out
// with an escaped backslash).
func hasNUL(doc []byte) bool {
	for i := 0; i < len(doc); i++ {
		switch doc[i] {
		case 0:
			return true
		case '\\':
			if bytes.HasPrefix(doc[i+1:], []byte("u0000")) {
				return true
			}
			i++ // whatever is escaped is not the start of another escape
		}
	}
	return false
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func isUUID(s string) bool { return uuidPattern.MatchString(s) }

// uuidParam validates an optional id query parameter.
func uuidParam(v *validationError, r *http.Request, name string) string {
	val := strings.TrimSpace(r.URL.Query().Get(name))
	if val != "" && !isUUID(val) {
		v.add(name, "must be a UUID")
	}
	return val
}

func pageFrom(r *http.Request) store.Page {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	return store.Page{Limit: limit, Offset: offset}
}

func identity(r *http.Request) auth.Identity {
	id, _ := auth.FromContext(r.Context())
	return id
}

// ownsCandidate reports whether the caller may act on candidate id: ops
// always, talent only when X-Actor names that candidate. It writes the
// response when not.
func ownsCandidate(w http.ResponseWriter, r *http.Request, candidateID string) bool {
	id := identity(r)
	switch id.Role {
	case auth.Ops:
		return true
	case auth.Talent:
		if !talentActor(w, id) {
			return false
		}
		if !strings.EqualFold(id.Actor, candidateID) {
			// 404 rather than 403 so talent cannot probe for other ids.
			writeError(w, http.StatusNotFound, "not found")
			return false
		}
		return true
	}
	writeError(w, http.StatusForbidden, "role "+string(id.Role)+" may not access candidates")
	return false
}

// talentActor checks that a talent request names its candidate id; it
// writes the 403 and returns false when not.
func talentActor(w http.ResponseWriter, id auth.Identity) bool {
	if !isUUID(id.Actor) {
		writeError(w, http.StatusForbidden, "talent requests must set "+auth.ActorHeader+" to the candidate id")
		return false
	}
	return true
}

// enumOr is the string value of an optional enum field from a generated
// request type, or fallback when the field was omitted or null.
func enumOr[T ~string](p *T, fallback string) string {
	if p == nil {
		return fallback
	}
	return string(*p)
}

// validEnum validates value against a generated enum type. The members come
// from api/openapi.yaml, so a value added there is accepted here with no code
// change, and there is no second list to keep in step.
func validEnum[T interface {
	~string
	Valid() bool
}](v *validationError, field, value string) {
	if !T(value).Valid() {
		v.add(field, "is not a "+field+" the API contract allows")
	}
}

// resolveTerms maps free-text taxonomy values to canonical ids, recording
// anything unknown as a validation error on field.
func (s *Server) resolveTerms(v *validationError, field string, kind taxonomy.Kind, values []string) []string {
	ids, unknown := s.tax.ResolveAll(kind, values)
	if len(unknown) > 0 {
		v.add(field, "not in the taxonomy: "+strings.Join(unknown, ", "))
	}
	return ids
}

// validTimezone accepts nil or an IANA zone name.
func validTimezone(v *validationError, field string, tz *string) {
	if tz == nil || *tz == "" {
		return
	}
	if _, err := time.LoadLocation(*tz); err != nil || *tz == "Local" {
		v.add(field, "must be an IANA timezone name such as America/Chicago")
	}
}

// jsonObject validates an optional JSONB object field.
func jsonObject(v *validationError, field string, raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "{") {
		v.add(field, "must be a JSON object")
		return nil
	}
	return raw
}

func trimAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func strPtr(p *string) *string {
	if p == nil {
		return nil
	}
	s := strings.TrimSpace(*p)
	if s == "" {
		return nil
	}
	return &s
}
