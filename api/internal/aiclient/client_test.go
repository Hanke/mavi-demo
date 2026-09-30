package aiclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEmbedOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embed" || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"embedding":[0.1,0.2],"dim":2,"provider":"local"}`))
	}))
	defer srv.Close()

	got, err := New(srv.URL).Embed(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != 0.1 {
		t.Fatalf("embedding = %v", got)
	}
}

func TestErrorsAreClassified(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
		detail string
	}{
		{"validation", 422, `{"detail":[{"loc":["body","text"],"msg":"too short"}]}`, ErrBadRequest, `[{"loc":["body","text"],"msg":"too short"}]`},
		{"string detail", 400, `{"detail":"nope"}`, ErrBadRequest, "nope"},
		{"provider down", 503, `upstream down`, ErrUnavailable, "upstream down"},
		{"rate limited", 429, `{"detail":"slow down"}`, ErrUnavailable, "slow down"},
		{"crash", 500, `Internal Server Error`, ErrBadResponse, "Internal Server Error"},
		{"garbage", 200, `not json`, ErrBadResponse, ""},
		{"empty embedding", 200, `{"embedding":[]}`, ErrBadResponse, "empty embedding"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			_, err := New(srv.URL).Embed(context.Background(), "x")
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("err is %T, want *Error", err)
			}
			if e.StatusCode != tc.status || e.Detail != tc.detail {
				t.Fatalf("status/detail = %d/%q, want %d/%q", e.StatusCode, e.Detail, tc.status, tc.detail)
			}
			if !strings.Contains(err.Error(), "embed") {
				t.Fatalf("error message should name the op: %q", err.Error())
			}
		})
	}
}

func TestTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	c := New(srv.URL, WithTimeouts(20*time.Millisecond, 20*time.Millisecond))
	err := c.Health(context.Background())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

func TestUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	err := New(url).Health(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestCallerContextIsNotAServiceError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancel2 := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel2()

	for name, tc := range map[string]struct {
		ctx  context.Context
		want error
	}{
		"cancelled":       {cancelled, context.Canceled},
		"caller deadline": {expired, context.DeadlineExceeded},
	} {
		t.Run(name, func(t *testing.T) {
			err := New(srv.URL, WithTimeouts(time.Second, time.Second)).Health(tc.ctx)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			var e *Error
			if errors.As(err, &e) {
				t.Fatalf("caller's own context should not be wrapped as *Error, got %v", err)
			}
		})
	}
}
