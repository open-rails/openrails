package httpx

import (
	"errors"
	"strings"
	"testing"
)

func TestDecodeJSONLimited_OK(t *testing.T) {
	var out struct {
		Name string `json:"name"`
	}
	if err := DecodeJSONLimited(strings.NewReader(`{"name":"ok"}`), 1024, &out); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Name != "ok" {
		t.Fatalf("got %q want %q", out.Name, "ok")
	}
}

func TestDecodeJSONLimited_TooLarge(t *testing.T) {
	// Body larger than the 8-byte cap must be rejected before decoding.
	big := `{"name":"` + strings.Repeat("a", 100) + `"}`
	var out map[string]any
	err := DecodeJSONLimited(strings.NewReader(big), 8, &out)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("got %v want ErrResponseTooLarge", err)
	}
}

func TestDecodeJSONLimited_ExactlyAtLimit(t *testing.T) {
	body := `{"a":1}` // 7 bytes
	var out map[string]int
	if err := DecodeJSONLimited(strings.NewReader(body), int64(len(body)), &out); err != nil {
		t.Fatalf("body exactly at limit should decode: %v", err)
	}
	if out["a"] != 1 {
		t.Fatalf("got %v", out)
	}
}

func TestDecodeJSONLimited_DefaultCap(t *testing.T) {
	var out map[string]string
	if err := DecodeJSONLimited(strings.NewReader(`{"k":"v"}`), 0, &out); err != nil {
		t.Fatalf("default cap should allow small body: %v", err)
	}
}

func TestDecodeJSONLimited_NilReader(t *testing.T) {
	var out map[string]any
	if err := DecodeJSONLimited(nil, 0, &out); err == nil {
		t.Fatal("expected error for nil reader")
	}
}

func TestDecodeJSONLimited_BadJSON(t *testing.T) {
	var out map[string]any
	if err := DecodeJSONLimited(strings.NewReader(`{not json`), 1024, &out); err == nil {
		t.Fatal("expected decode error")
	}
}
