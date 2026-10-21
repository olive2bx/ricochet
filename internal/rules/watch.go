package rules

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// debounce groups the bursts of events editors produce on save.
const debounce = 150 * time.Millisecond

// Watch reloads the rules file whenever it changes, until ctx is done.
// onReload (optional) is called after each reload attempt with its error.
// It watches the parent directory so that atomic saves (write + rename) are
// seen, and falls back to polling the file's mtime if fsnotify fails.
func (st *Store) Watch(ctx context.Context, logger *slog.Logger, onReload func(error)) {
	if st.path == "" {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	abs, err := filepath.Abs(st.path)
	if err != nil {
		abs = st.path
	}
	reload := func() {
		err := st.Reload()
		if onReload != nil {
			onReload(err)
		}
	}

	w, err := fsnotify.NewWatcher()
	if err == nil {
		if err = w.Add(filepath.Dir(abs)); err != nil {
			w.Close()
		}
	}
	if err != nil {
		logger.Warn("file watching unavailable, polling the rules file", "err", err)
		st.poll(ctx, time.Second, reload)
		return
	}
	defer w.Close()

	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if filepath.Clean(ev.Name) == abs && ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) != 0 {
				timer.Reset(debounce)
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			logger.Warn("rules watcher", "err", err)
		case <-timer.C:
			if _, err := os.Stat(abs); err != nil {
				// Removed or being replaced: wait for it to come back.
				continue
			}
			reload()
		}
	}
}

// poll reloads when the file's modification time or size changes.
func (st *Store) poll(ctx context.Context, every time.Duration, reload func()) {
	stamp := func() (time.Time, int64, bool) {
		fi, err := os.Stat(st.path)
		if err != nil {
			return time.Time{}, 0, false
		}
		return fi.ModTime(), fi.Size(), true
	}
	lastMod, lastSize, _ := stamp()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			mod, size, ok := stamp()
			if !ok || (mod.Equal(lastMod) && size == lastSize) {
				continue
			}
			lastMod, lastSize = mod, size
			reload()
		}
	}
}
