// Package logger configures Genie's structured logging. Logs go to
// $XDG_CACHE_HOME/genie/genie.log (rotated via lumberjack) at Debug
// level. Falls back to a no-op logger if the cache dir can't be
// created — Genie should never crash because it couldn't open a log
// file.
//
// Pattern lifted from /home/mrdon/dev/sx/internal/logger.
//
// Why a file rather than stderr: Genie typically runs as an MCP
// stdio subprocess of an agent (Claude Code, Claude Desktop, etc.).
// Those agents don't surface a child's stderr in a useful place;
// tailing a file is the only reliable way to follow what happened.
// Stderr stays available for crash output but day-to-day logs go
// to the file.
package logger

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	// EnvDir overrides the cache directory when set. Mirrors the
	// flag genie-wide.
	EnvDir = "GENIE_CACHE_DIR"
	// LogFileName is the file Genie writes to inside the cache dir.
	LogFileName = "genie.log"
)

var (
	defaultLogger *slog.Logger
	once          sync.Once
)

// Get returns the global logger, initialising it on first call.
func Get() *slog.Logger {
	once.Do(func() {
		defaultLogger = initLogger()
	})
	return defaultLogger
}

// SetDefault wires the file-backed logger up as slog.Default so the
// existing slog.Info / slog.Warn call sites pick it up unchanged.
func SetDefault() {
	slog.SetDefault(Get())
}

// LogPath returns the absolute path of the active log file, or ""
// if logging fell back to io.Discard.
func LogPath() string {
	dir, err := resolveCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, LogFileName)
}

func initLogger() *slog.Logger {
	dir, err := resolveCacheDir()
	if err != nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	logPath := filepath.Join(dir, LogFileName)

	w := &lumberjack.Logger{
		Filename:   logPath,
		MaxSize:    1, // MB; with MaxBackups=0 we cap at this.
		MaxBackups: 0,
		MaxAge:     0,
		Compress:   false,
	}
	h := slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	return slog.New(h)
}

func resolveCacheDir() (string, error) {
	if v := os.Getenv(EnvDir); v != "" {
		return v, nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "genie"), nil
}
