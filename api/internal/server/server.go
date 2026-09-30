package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/aiclient"
)

// Pinger is the subset of *pgxpool.Pool the server needs, so tests can stub it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// AIHealth is the subset of *aiclient.Client the server needs.
type AIHealth interface {
	Health(ctx context.Context) error
}

var _ AIHealth = (*aiclient.Client)(nil)

type Server struct {
	db         Pinger
	ai         AIHealth
	corsOrigin string
}

func New(db Pinger, ai AIHealth, corsOrigin string) http.Handler {
	s := &Server{db: db, ai: ai, corsOrigin: corsOrigin}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	return s.cors(mux)
}

type healthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// handleHealth reports 200 only when both downstream dependencies are reachable,
// which is what the compose healthcheck and the acceptance criteria key off.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	resp := healthResponse{Status: "ok", Checks: map[string]string{}}
	check := func(name string, err error) {
		if err != nil {
			resp.Status = "degraded"
			resp.Checks[name] = "error: " + err.Error()
			return
		}
		resp.Checks[name] = "ok"
	}
	check("postgres", s.db.Ping(ctx))
	check("ai", s.ai.Health(ctx))

	code := http.StatusOK
	if resp.Status != "ok" {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, resp)
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", s.corsOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
