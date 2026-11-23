// Package logx is how OpenRails logs: through the *slog.Logger a host injects,
// with request and job attributes carried by the context, and errors redacted.
// Only Or reads slog.Default; nothing here changes it.
package logx

import (
	"context"
	"log/slog"
	"slices"
)

// Or is l, or slog.Default() when l is nil: read once, where a logger enters
// OpenRails.
func Or(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}

// New is Or(l) with every attribute With put on a context added to the
// records logged with that context.
func New(l *slog.Logger) *slog.Logger {
	l = Or(l)
	if _, ok := l.Handler().(contextHandler); ok {
		return l
	}
	return slog.New(contextHandler{l.Handler()})
}

type attrsKey struct{}

// With is ctx carrying args, key-value pairs or slog.Attrs, for a logger from
// New to add to each record logged with it. A key given again replaces the
// earlier value.
func With(ctx context.Context, args ...any) context.Context {
	prev, _ := ctx.Value(attrsKey{}).([]slog.Attr)
	attrs := slices.Clone(prev)
	for _, a := range slog.Group("", args...).Value.Group() {
		if i := slices.IndexFunc(attrs, func(b slog.Attr) bool { return b.Key == a.Key }); i >= 0 {
			attrs[i] = a
		} else {
			attrs = append(attrs, a)
		}
	}
	return context.WithValue(ctx, attrsKey{}, attrs)
}

type contextHandler struct{ slog.Handler }

// Handle adds the context's attributes the record does not set itself.
func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs, _ := ctx.Value(attrsKey{}).([]slog.Attr); len(attrs) > 0 {
		set := make(map[string]bool, r.NumAttrs())
		r.Attrs(func(a slog.Attr) bool { set[a.Key] = true; return true })
		r = r.Clone()
		for _, a := range attrs {
			if !set[a.Key] {
				r.AddAttrs(a)
			}
		}
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}

// Err is err as the "error" attribute, its text redacted; a nil err adds
// nothing.
func Err(err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}
	return slog.String("error", Redact(err.Error()))
}
