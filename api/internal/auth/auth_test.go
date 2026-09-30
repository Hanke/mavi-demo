package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func reject(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	_, _ = w.Write([]byte(msg))
}

func TestRequire(t *testing.T) {
	var seen Identity
	h := Require(reject, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}), Ops, Employer)

	cases := []struct {
		name   string
		header string
		cookie string
		actor  string
		want   int
	}{
		{"no role", "", "", "", http.StatusUnauthorized},
		{"unknown role", "admin", "", "", http.StatusUnauthorized},
		{"wrong role", "talent", "", "", http.StatusForbidden},
		{"allowed header", "ops", "", "ops@example.com", http.StatusOK},
		{"case insensitive", " Employer ", "", "", http.StatusOK},
		{"cookie fallback", "", "employer", "", http.StatusOK},
		{"header wins over cookie", "talent", "ops", "", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set(RoleHeader, tc.header)
			}
			if tc.actor != "" {
				req.Header.Set(ActorHeader, tc.actor)
			}
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: RoleCookie, Value: tc.cookie})
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusOK && tc.actor != "" && seen.Actor != tc.actor {
				t.Fatalf("actor = %q, want %q", seen.Actor, tc.actor)
			}
		})
	}
}
