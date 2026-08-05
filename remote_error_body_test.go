package openrails

import (
	"strings"
	"testing"
)

func TestExcerptErrorBodyCollapsesAndTruncates(t *testing.T) {
	// Multi-line "foreign" body (e.g. a proxy HTML page or a stack trace) must
	// become a single-line, bounded excerpt.
	raw := []byte("upstream error\n\tat foo()\r\n\tat bar()\n")
	got := excerptErrorBody(raw)
	if strings.ContainsAny(got, "\n\r\t") {
		t.Fatalf("excerpt still contains control whitespace: %q", got)
	}
	if got != "upstream error at foo() at bar()" {
		t.Fatalf("unexpected excerpt: %q", got)
	}
}

func TestExcerptErrorBodyBounded(t *testing.T) {
	raw := []byte(strings.Repeat("A", 5000))
	got := excerptErrorBody(raw)
	// Must not exceed the cap (+ a few bytes for the ellipsis rune).
	if len(got) > maxErrorMessageBytes+4 {
		t.Fatalf("excerpt not bounded: len=%d", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected truncation ellipsis, got %q", got[len(got)-8:])
	}
}

func TestStatusErrorFromBodyUsesExcerpt(t *testing.T) {
	// A non-envelope 400 body should be excerpted into the StatusError message.
	body := []byte("<html><body>Bad Gateway\n\n  detail  </body></html>")
	err := statusErrorFromBody(400, body)
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("expected *StatusError, got %T", err)
	}
	if strings.ContainsAny(se.Message, "\n\r\t") {
		t.Fatalf("message not sanitized: %q", se.Message)
	}
	if se.Message == "" {
		t.Fatalf("expected a non-empty excerpt message")
	}
}

// or#893 phase 7: ONE error representation. The canonical nested envelope is
// parsed; the retired top-level {code,message} shape is a foreign body now —
// excerpted, never mistaken for an OpenRails error code.
func TestStatusErrorFromBodyParsesOnlyTheCanonicalEnvelope(t *testing.T) {
	canonical := []byte(`{"error":{"type":"invalid_request_error","code":"parameter_invalid","message":"currency is required"}}`)
	se, ok := statusErrorFromBody(400, canonical).(*StatusError)
	if !ok {
		t.Fatalf("expected *StatusError")
	}
	if se.Code != "parameter_invalid" || se.Message != "currency is required" {
		t.Fatalf("canonical envelope not parsed: code=%q message=%q", se.Code, se.Message)
	}

	retired := []byte(`{"code":"parameter_invalid","message":"currency is required"}`)
	se, ok = statusErrorFromBody(400, retired).(*StatusError)
	if !ok {
		t.Fatalf("expected *StatusError")
	}
	if se.Code != "" {
		t.Fatalf("the retired top-level shape must not yield a code, got %q", se.Code)
	}
	if se.Message != `{"code":"parameter_invalid","message":"currency is required"}` {
		t.Fatalf("retired shape must be excerpted as an opaque body, got %q", se.Message)
	}
}
