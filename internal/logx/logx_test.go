package logx

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"card 4111 1111 1111 1111 declined":              "card [card] declined",
		"pan=4111111111111111&cvv=867":                   "pan=[card]&cvv=[redacted]",
		`{"cvc":"867","card_number":"5555555555554444"}`: `{"cvc":"[redacted]","card_number":"[redacted]"}`,
		"order 20251123000001 for 29970000 micros":       "order 20251123000001 for 29970000 micros",
		"stripe key sk_test_critical refused":            "stripe key [redacted] refused",
		"Authorization: Bearer eyJhbGciOi.x.y":           "Authorization: [redacted]",
		"dial postgres://billing:hunter2@db:5432/x":      "dial postgres://[redacted]@db:5432/x",
		"security_key=abc123 gateway refused":            "security_key=[redacted] gateway refused",
		"whsec_critical_fixture did not verify":          "[redacted] did not verify",
	} {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
	if !Err(nil).Equal(slog.Attr{}) {
		t.Error("Err(nil) adds an attribute")
	}
}

func TestWith(t *testing.T) {
	var out bytes.Buffer
	l := New(slog.New(slog.NewTextHandler(&out, nil)))
	if New(l) != l {
		t.Error("New wraps twice")
	}
	ctx := With(With(context.Background(), "request_id", "a", "route", "/v1/x"), "request_id", "b")
	l.With("svc", "s").InfoContext(ctx, "hello", Err(errors.New("pan 4111111111111111")), "route", "/v1/y")
	line := out.String()
	for _, want := range []string{"svc=s", "request_id=b", "route=/v1/y", `error="pan [card]"`} {
		if !strings.Contains(line, want) {
			t.Errorf("%q lacks %s", line, want)
		}
	}
	if strings.Contains(line, "request_id=a") || strings.Contains(line, "route=/v1/x") {
		t.Errorf("%q kept a replaced attribute", line)
	}
}

type leaf struct{ Log *slog.Logger }

type tree struct {
	Leaf   *leaf
	Leaves []leaf
	ByName map[string]any
	self   *tree
}

func TestUnset(t *testing.T) {
	root := &tree{Leaf: &leaf{Log: slog.Default()}, Leaves: []leaf{{Log: nil}}, ByName: map[string]any{"x": &leaf{Log: nil}}}
	root.self = root
	got := Unset(root)
	if len(got) != 2 {
		t.Fatalf("Unset = %v, want the slice's and the map's leaf", got)
	}
	root.Leaves[0].Log, root.ByName["x"] = slog.Default(), &leaf{Log: slog.Default()}
	if got := Unset(root); len(got) != 0 {
		t.Fatalf("Unset = %v, want none", got)
	}
}
