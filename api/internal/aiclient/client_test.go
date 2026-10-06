package aiclient

import (
	"context"
	"errors"
	"github.com/colehanke/mavi-demo/api/internal/reqlog"
	"io"
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

func TestEmbedBatch(t *testing.T) {
	var body string
	answer := `{"embeddings":[[0.1,0.2],[0.3,0.4]],"texts":["hello","Role: Controller"],"dim":2,"provider":"local"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embed-batch" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()

	items := []EmbedItem{{Text: "hello"}, {Requirements: []byte(`{"title":"Controller"}`)}}
	got, err := New(srv.URL).EmbedBatch(context.Background(), items)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"inputs":[{"text":"hello"},{"requirements":{"title":"Controller"}}]}`; body != want {
		t.Fatalf("request body = %s, want %s", body, want)
	}
	if len(got.Embeddings) != 2 || got.Embeddings[1][0] != 0.3 || got.Texts[1] != "Role: Controller" || got.Provider != "local" {
		t.Fatalf("response = %+v", got)
	}

	// One vector short, or an empty one, is a bad response, not a partial result.
	for _, answer = range []string{`{"embeddings":[[0.1]]}`, `{"embeddings":[[0.1],[]]}`} {
		if _, err := New(srv.URL).EmbedBatch(context.Background(), items); !errors.Is(err, ErrBadResponse) {
			t.Fatalf("answer %s: err = %v, want ErrBadResponse", answer, err)
		}
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

func TestExtractTextSendsTheFileAsTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/extract-text" || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/octet-stream" {
			t.Errorf("unexpected request %s %s (%s)", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		switch string(raw) {
		case "%PDF-1.7 resume":
			_, _ = w.Write([]byte(`{"text":"Ada Okafor\nCPA","kind":"pdf","pages":2}`))
		case "%PDF-1.7 scanned":
			_, _ = w.Write([]byte(`{"text":"  ","kind":"pdf","pages":1}`))
		default:
			w.WriteHeader(http.StatusUnsupportedMediaType)
			_, _ = w.Write([]byte(`{"detail":"unsupported file type: only PDF and DOCX files are accepted"}`))
		}
	}))
	defer srv.Close()
	c := New(srv.URL)

	got, err := c.ExtractText(context.Background(), []byte("%PDF-1.7 resume"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "Ada Okafor\nCPA" || got.Kind != "pdf" || got.Pages == nil || *got.Pages != 2 {
		t.Fatalf("response = %+v", got)
	}
	// The service's refusal keeps its status and its reason.
	_, err = c.ExtractText(context.Background(), []byte("GIF89a"))
	var e *Error
	if !errors.As(err, &e) || e.StatusCode != http.StatusUnsupportedMediaType || !strings.Contains(e.Detail, "only PDF and DOCX") {
		t.Fatalf("err = %v, want the 415 with its detail", err)
	}
	if _, err = c.ExtractText(context.Background(), []byte("%PDF-1.7 scanned")); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("blank text: err = %v, want ErrBadResponse", err)
	}
}

func TestParseResumeKeepsTheProfileAsSent(t *testing.T) {
	profile := `{"headline":"Senior Accountant","years_experience":8,"certifications":["cpa_us"],"other_software":[],"timezone":null}`
	answer := `{"contact":{"full_name":"Ada Okafor","email":null},"profile":` + profile + `,"provider":"fake"}`
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		if r.URL.Path != "/parse-resume" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()

	got, err := New(srv.URL).ParseResume(context.Background(), "Ada Okafor, CPA")
	if err != nil {
		t.Fatal(err)
	}
	if body != `{"text":"Ada Okafor, CPA"}` {
		t.Fatalf("request body = %s", body)
	}
	// The raw document keeps the nulls and empty lists the typed one drops.
	if string(got.ProfileJSON) != profile {
		t.Fatalf("ProfileJSON = %s", got.ProfileJSON)
	}
	if got.Profile.Headline == nil || *got.Profile.Headline != "Senior Accountant" || *got.Profile.YearsExperience != 8 ||
		len(got.Profile.Certifications) != 1 || got.Profile.Timezone != nil || got.Provider != "fake" || *got.Contact.FullName != "Ada Okafor" {
		t.Fatalf("decoded = %+v", got)
	}

	for _, answer = range []string{`{"contact":{},"provider":"fake"}`, `{"contact":{},"profile":["cpa"],"provider":"fake"}`} {
		if _, err := New(srv.URL).ParseResume(context.Background(), "x"); !errors.Is(err, ErrBadResponse) {
			t.Fatalf("answer %s: err = %v, want ErrBadResponse", answer, err)
		}
	}
}

func TestParseJDKeepsTheRequirementsAsSent(t *testing.T) {
	requirements := `{"title":"Senior Accountant","required_certifications":["cpa_us"],"must_haves":["Active CPA"],"nice_to_haves":[],"min_years_experience":5,"timezone":null}`
	answer := `{"company":"Northwind","requirements":` + requirements + `,"provider":"fake"}`
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		if r.URL.Path != "/parse-jd" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()

	got, err := New(srv.URL).ParseJD(context.Background(), "Senior Accountant at Northwind")
	if err != nil {
		t.Fatal(err)
	}
	if body != `{"text":"Senior Accountant at Northwind"}` {
		t.Fatalf("request body = %s", body)
	}
	// The raw document keeps the nulls and empty lists the typed one drops.
	if string(got.RequirementsJSON) != requirements {
		t.Fatalf("RequirementsJSON = %s", got.RequirementsJSON)
	}
	req := got.Requirements
	if req.Title == nil || *req.Title != "Senior Accountant" || *req.MinYearsExperience != 5 || len(req.RequiredCertifications) != 1 ||
		len(req.MustHaves) != 1 || req.Timezone != nil || got.Provider != "fake" || *got.Company != "Northwind" {
		t.Fatalf("decoded = %+v", got)
	}

	for _, answer = range []string{`{"company":null,"provider":"fake"}`, `{"requirements":["cpa"],"provider":"fake"}`} {
		if _, err := New(srv.URL).ParseJD(context.Background(), "x"); !errors.Is(err, ErrBadResponse) {
			t.Fatalf("answer %s: err = %v, want ErrBadResponse", answer, err)
		}
	}
}

func TestRerankChecksEveryCandidateIsAnsweredOnce(t *testing.T) {
	result := func(id string) string {
		dim := `{"level":3,"evidence":"e","quotes":["q"]}`
		return `{"id":"` + id + `","score":0.75,"reasons":["r"],"dimensions":{"must_have_coverage":` + dim + `,"experience_depth":` + dim +
			`,"software_fluency":{"level":null,"evidence":"none named","quotes":[]},"industry_fit":` + dim + `,"nice_to_haves":` + dim + `}}`
	}
	answer := `{"results":[` + result("b") + `,` + result("a") + `],"rubric_version":"1","provider":"fake"}`
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		if r.URL.Path != "/rerank" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()
	candidates := []RerankCandidate{{ID: "a", Text: "resume a"}, {ID: "b", Text: "resume b"}}

	got, err := New(srv.URL).Rerank(context.Background(), "Controller", candidates)
	if err != nil {
		t.Fatal(err)
	}
	if body != `{"candidates":[{"id":"a","text":"resume a"},{"id":"b","text":"resume b"}],"role":"Controller"}` {
		t.Fatalf("request body = %s", body)
	}
	if len(got.Results) != 2 || got.Results[0].ID != "b" || got.Results[0].Dimensions.SoftwareFluency.Level != nil ||
		got.Results[0].Dimensions.ExperienceDepth.Level != 3 || got.RubricVersion != "1" || got.Provider != "fake" {
		t.Fatalf("decoded = %+v", got)
	}

	// Somebody missing, somebody twice, somebody who was never sent.
	for _, answer = range []string{
		`{"results":[` + result("a") + `],"rubric_version":"1","provider":"fake"}`,
		`{"results":[` + result("a") + `,` + result("a") + `],"rubric_version":"1","provider":"fake"}`,
		`{"results":[` + result("a") + `,` + result("c") + `],"rubric_version":"1","provider":"fake"}`,
		// A score the matches table would refuse.
		`{"results":[` + result("a") + `,` + strings.Replace(result("b"), `"score":0.75`, `"score":1.25`, 1) + `],"rubric_version":"1","provider":"fake"}`,
		`{"results":[` + result("a") + `,` + strings.Replace(result("b"), `"score":0.75`, `"score":-0.1`, 1) + `],"rubric_version":"1","provider":"fake"}`,
	} {
		if _, err := New(srv.URL).Rerank(context.Background(), "Controller", candidates); !errors.Is(err, ErrBadResponse) {
			t.Fatalf("answer %s: err = %v, want ErrBadResponse", answer, err)
		}
	}
}

// The request id of the caller's context goes to the service with the call,
// which is what puts the same id on both services' log lines.
func TestRequestIDIsSentToTheService(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get(reqlog.Header))
		_, _ = w.Write([]byte(`{"status":"ok","llm_provider":"fake","embedding_provider":"local"}`))
	}))
	defer srv.Close()
	c := New(srv.URL)
	if err := c.Health(reqlog.WithID(context.Background(), "job-attempt-1")); err != nil {
		t.Fatal(err)
	}
	if err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "job-attempt-1" || got[1] != "" {
		t.Fatalf("X-Request-ID received = %q", got)
	}
}
