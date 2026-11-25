package hostconfig

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// LoggerConfig is the binary's logger: logger.level and logger.format.
type LoggerConfig struct {
	// Level is debug, info, warn or error; unset is info.
	Level string `yaml:"level"`
	// Format is json or text; unset is json.
	Format string `yaml:"format"`
}

// Handler is the handler c selects, writing to w.
func (c LoggerConfig) Handler(w io.Writer) (slog.Handler, error) {
	var level slog.Level
	switch l := strings.ToLower(strings.TrimSpace(c.Level)); l {
	case "":
		level = slog.LevelInfo
	case "debug", "info", "warn", "error":
		_ = level.UnmarshalText([]byte(l))
	default:
		return nil, fmt.Errorf("logger.level (LOGGER_LEVEL) %q: debug, info, warn or error", c.Level)
	}
	opts := &slog.HandlerOptions{Level: level}
	switch strings.ToLower(strings.TrimSpace(c.Format)) {
	case "", "json":
		return slog.NewJSONHandler(w, opts), nil
	case "text":
		return slog.NewTextHandler(w, opts), nil
	}
	return nil, fmt.Errorf("logger.format (LOGGER_FORMAT) %q: json or text", c.Format)
}
