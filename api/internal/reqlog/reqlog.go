// Package reqlog is the API's structured logging and the request id that
// runs through it.
//
// Every log line is one JSON object on stderr: time, level, service, msg, the
// request_id of the HTTP request or job attempt it was written for, and any
// fields attached to the context on the way (With). The id travels in the
// context, so a line needs only to be logged with its context to carry it,
// and the AI client sends it to the Python service as the X-Request-ID
// header, where the same id is on every line that service writes for the
// call (ai/app/logs.py). One id therefore finds a request, or a matching run,
// in both services' logs.
package reqlog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Header carries the request id: read from an incoming request, sent on a
// call to the AI service, and returned with every response.
const Header = "X-Request-ID"

// Service is the `service` field of every line, which tells the API's lines
// from the AI service's when the two are read together.
const Service = "api"

type ctxKey struct{}

// fields is what a context adds to the lines logged with it.
type fields struct {
	id    string
	attrs []slog.Attr
}

func from(ctx context.Context) fields {
	f, _ := ctx.Value(ctxKey{}).(fields)
	return f
}

// NewID returns a fresh request id: 16 hex characters.
func NewID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	return hex.EncodeToString(b[:])
}

// Valid reports whether id may be taken from outside as a request id: 1 to 64
// letters, digits and `. _ : -`. It is written to logs and echoed in a header.
func Valid(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.ContainsRune("._:-", c):
		default:
			return false
		}
	}
	return true
}

// WithID returns ctx carrying id as its request id.
func WithID(ctx context.Context, id string) context.Context {
	f := from(ctx)
	f.id = id
	return context.WithValue(ctx, ctxKey{}, f)
}

// ID is the request id ctx carries, or "" when it carries none.
func ID(ctx context.Context) string { return from(ctx).id }

// With returns ctx with fields added to every line logged with it, given as
// slog key-value pairs: reqlog.With(ctx, "role_id", id, "run_id", run).
func With(ctx context.Context, args ...any) context.Context {
	f := from(ctx)
	var r slog.Record
	r.Add(args...)
	attrs := make([]slog.Attr, 0, len(f.attrs)+r.NumAttrs())
	attrs = append(attrs, f.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	f.attrs = attrs
	return context.WithValue(ctx, ctxKey{}, f)
}

// handler adds the context's request id and fields to each record.
type handler struct{ slog.Handler }

func (h handler) Handle(ctx context.Context, r slog.Record) error {
	f := from(ctx)
	if f.id != "" {
		r.AddAttrs(slog.String("request_id", f.id))
	}
	r.AddAttrs(f.attrs...)
	return h.Handler.Handle(ctx, r)
}

func (h handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return handler{h.Handler.WithAttrs(attrs)}
}

func (h handler) WithGroup(name string) slog.Handler { return handler{h.Handler.WithGroup(name)} }

// NewLogger returns a logger that writes JSON lines to w at level and above,
// each with the request id and fields of the context it is logged with.
func NewLogger(w io.Writer, level slog.Leveler) *slog.Logger {
	base := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(handler{base.WithAttrs([]slog.Attr{slog.String("service", Service)})})
}

// Setup makes NewLogger on stderr the process's default logger, which is also
// where the standard log package then writes. LOG_LEVEL sets the level
// (debug, info, warn or error; default info): debug adds the health probes.
func Setup() {
	level := slog.LevelInfo
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		if err := level.UnmarshalText([]byte(v)); err != nil {
			level = slog.LevelInfo
		}
	}
	slog.SetDefault(NewLogger(os.Stderr, level))
}
