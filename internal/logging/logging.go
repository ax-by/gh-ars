// Package logging 은 `--log-level` / `--log-format` 를 log/slog 핸들러로 바꾼다. [§11, DESIGN §2]
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// 기본값. [§11]
const (
	DefaultLevel  = "info"
	DefaultFormat = "json"
)

// New 는 level(debug|info|warn|error)과 format(json|text)으로 로거를 만든다. [§11]
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "info", "":
		lv = slog.LevelInfo
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		return nil, fmt.Errorf("--log-level %q: debug|info|warn|error 중 하나여야 함", level)
	}
	opts := &slog.HandlerOptions{Level: lv}
	var h slog.Handler
	switch strings.ToLower(format) {
	case "json", "":
		h = slog.NewJSONHandler(w, opts)
	case "text":
		h = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("--log-format %q: json|text 중 하나여야 함", format)
	}
	return slog.New(h), nil
}
