package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/colehanke/mavi-demo/api/internal/contract"
)

type stub struct{ err error }

func (s stub) Ping(context.Context) error   { return s.err }
func (s stub) Health(context.Context) error { return s.err }

func TestHealthOK(t *testing.T) {
	h := New(Config{DB: stub{}, AI: stub{}, CORSOrigin: "*"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body contract.HealthResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || body.Checks["postgres"] != "ok" || body.Checks["ai"] != "ok" {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestHealthDegradedWhenAIDown(t *testing.T) {
	h := New(Config{DB: stub{}, AI: stub{err: errors.New("boom")}, CORSOrigin: "*"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// Role gating does not need a database: the middleware answers before the
// handler runs, and a server without a store answers 503 for anything that
// gets through.
func TestRolesAreEnforcedBeforeHandlers(t *testing.T) {
	h := New(Config{DB: stub{}, AI: stub{}, CORSOrigin: "*"})
	cases := []struct {
		method, path, role string
		want               int
	}{
		{"GET", "/candidates", "", http.StatusUnauthorized},
		{"GET", "/candidates", "root", http.StatusUnauthorized},
		{"GET", "/candidates", "talent", http.StatusForbidden},
		{"GET", "/candidates", "employer", http.StatusForbidden},
		{"GET", "/candidates", "ops", http.StatusServiceUnavailable},
		{"POST", "/candidates", "employer", http.StatusForbidden},
		{"DELETE", "/candidates/x", "talent", http.StatusForbidden},
		{"POST", "/roles", "talent", http.StatusForbidden},
		{"DELETE", "/roles/x", "talent", http.StatusForbidden},
		{"POST", "/matches", "employer", http.StatusForbidden},
		{"POST", "/matches", "talent", http.StatusForbidden},
		{"PUT", "/matches/x", "employer", http.StatusForbidden},
		{"POST", "/matches/x/release", "employer", http.StatusForbidden},
		{"DELETE", "/matches/x", "employer", http.StatusForbidden},
		{"GET", "/matches", "employer", http.StatusServiceUnavailable},
		{"GET", "/health", "", http.StatusOK},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.role != "" {
			req.Header.Set("X-Role", tc.role)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s %s as %q: status = %d, want %d", tc.method, tc.path, tc.role, rec.Code, tc.want)
		}
	}
}

func TestCORSAllowsRoleHeaders(t *testing.T) {
	h := New(Config{DB: stub{}, AI: stub{}, CORSOrigin: "http://localhost:5173"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/candidates", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "Content-Type, X-Role, X-Actor" {
		t.Fatalf("allow-headers = %q", got)
	}
	if rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatal("cookie session needs Access-Control-Allow-Credentials for a specific origin")
	}

	rec = httptest.NewRecorder()
	New(Config{DB: stub{}, AI: stub{}, CORSOrigin: "*"}).ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/candidates", nil))
	if rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatal("credentials must not be allowed with a wildcard origin")
	}
}
