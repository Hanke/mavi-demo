package reqlog

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// A line logged with a context carries that context's request id and fields,
// next to the service name every line has.
func TestLinesCarryTheRequestIDAndFieldsOfTheirContext(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelInfo)
	ctx := With(WithID(context.Background(), "req-1"), "job_id", 7)
	run := With(ctx, "run_id", "r1")

	log.InfoContext(run, "reranked", "candidates", 3)
	log.InfoContext(ctx, "the job's own line")
	log.InfoContext(context.Background(), "no request")
	log.DebugContext(run, "below the level")

	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("not a JSON line: %s", raw)
		}
		lines = append(lines, line)
	}
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %s", len(lines), buf.String())
	}
	first := lines[0]
	if first["request_id"] != "req-1" || first["job_id"] != float64(7) || first["run_id"] != "r1" ||
		first["candidates"] != float64(3) || first["service"] != Service || first["msg"] != "reranked" || first["level"] != "INFO" {
		t.Errorf("first line = %v", first)
	}
	// With does not write to the context it was given.
	if _, has := lines[1]["run_id"]; has || lines[1]["request_id"] != "req-1" || lines[1]["job_id"] != float64(7) {
		t.Errorf("second line = %v", lines[1])
	}
	if _, has := lines[2]["request_id"]; has {
		t.Errorf("a line with no request has a request_id: %v", lines[2])
	}
}

func TestIDs(t *testing.T) {
	a, b := NewID(), NewID()
	if a == b || len(a) != 16 || !Valid(a) {
		t.Errorf("NewID gave %q then %q", a, b)
	}
	if ID(context.Background()) != "" || ID(WithID(context.Background(), a)) != a {
		t.Error("ID does not read back what WithID set")
	}
	for id, want := range map[string]bool{
		"abc-123_X.y:z":           true,
		"":                        false,
		"has space":               false,
		"line\nbreak":             false,
		`quote"`:                  false,
		strings.Repeat("a", 64):   true,
		strings.Repeat("a", 65):   false,
		"550e8400-e29b-41d4-a716": true,
	} {
		if Valid(id) != want {
			t.Errorf("Valid(%q) = %v, want %v", id, !want, want)
		}
	}
}
