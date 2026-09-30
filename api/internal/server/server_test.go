package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type stub struct{ err error }

func (s stub) Ping(context.Context) error   { return s.err }
func (s stub) Health(context.Context) error { return s.err }

func TestHealthOK(t *testing.T) {
	h := New(stub{}, stub{}, "*")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body healthResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || body.Checks["postgres"] != "ok" || body.Checks["ai"] != "ok" {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestHealthDegradedWhenAIDown(t *testing.T) {
	h := New(stub{}, stub{err: errors.New("boom")}, "*")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
