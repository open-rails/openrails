package hostconfig

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// LoggerConfig is the binary's logger: logger.level. It writes text to a
// terminal and JSON otherwise.
type LoggerConfig struct {
	// Level is debug, info, warn or error; unset is info.
	Level string `yaml:"level"`
}

// Handler is the handler c selects, writing to w: text when w is a
// terminal, JSON otherwise.
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
	if terminal(w) {
		return slog.NewTextHandler(w, opts), nil
	}
	return slog.NewJSONHandler(w, opts), nil
}

func terminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
