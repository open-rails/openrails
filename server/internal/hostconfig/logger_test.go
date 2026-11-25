package hostconfig

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestLoggerConfigDefaults(t *testing.T) {
	var out bytes.Buffer
	h, err := LoggerConfig{}.Handler(&out)
	if err != nil {
		t.Fatalf("an unset logger section must start with the documented defaults: %v", err)
	}
	if h.Enabled(context.Background(), slog.LevelDebug) || !h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("unset logger.level must be info")
	}
	slog.New(h).Info("ready")
	if !strings.HasPrefix(strings.TrimSpace(out.String()), "{") {
		t.Fatalf("a log that is no terminal must be json, got %q", out.String())
	}
	if _, err := (LoggerConfig{Level: "verbose"}).Handler(&out); err == nil {
		t.Fatal("an unknown level must be refused")
	}
}
