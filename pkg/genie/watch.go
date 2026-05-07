package genie

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// WatchConfig watches the config-file source for changes and calls
// Reload on each change. Blocks until ctx is cancelled.
//
// Watching the parent directory (not the file) is required because
// most editors and our own atomic-rename Save() replace the file
// rather than rewriting in place — fsnotify on the file itself
// would lose the watch after the first replace.
//
// Events are debounced: the reload fires 200ms after the last event
// in a burst, so a save that produces several CHMOD/WRITE events
// in quick succession only triggers one reload.
//
// Returns nil on a clean ctx cancellation; an error means the
// watcher couldn't start (e.g. config path empty, fsnotify init
// failed).
func (g *Genie) WatchConfig(ctx context.Context) error {
	if g.configPath == "" {
		return fmt.Errorf("WatchConfig requires a ConfigPath")
	}
	dir := filepath.Dir(g.configPath)
	target := filepath.Base(g.configPath)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify new: %w", err)
	}
	defer func() { _ = watcher.Close() }()

	if err := watcher.Add(dir); err != nil {
		return fmt.Errorf("fsnotify add %s: %w", dir, err)
	}
	slog.Info("watching config for changes", "path", g.configPath)

	const debounce = 200 * time.Millisecond
	var pending *time.Timer
	defer func() {
		if pending != nil {
			pending.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			slog.Warn("config watcher error", "err", err)
		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			slog.Debug("config watcher event", "name", ev.Name, "op", ev.Op.String())
			if filepath.Base(ev.Name) != target {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			slog.Debug("config changed; reload pending", "name", ev.Name, "op", ev.Op.String())
			if pending == nil {
				pending = time.AfterFunc(debounce, func() {
					if err := g.Reload(ctx); err != nil {
						slog.Warn("config reload failed", "err", err)
					}
				})
			} else {
				pending.Reset(debounce)
			}
		}
	}
}
