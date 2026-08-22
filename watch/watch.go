// Package watch tails agent session logs and folds what it finds into a live
// session index.
//
// The watcher polls rather than subscribing to filesystem events. Session logs
// are append-only and hv cares about them on a human timescale, so a periodic
// sweep is enough, and it avoids a dependency plus the per-platform event
// semantics that come with one.
package watch

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/satoricorp/demux/capture"
	"github.com/satoricorp/hv/parse"
	"github.com/satoricorp/hv/session"
)

// DefaultInterval is how often Run sweeps the log roots.
const DefaultInterval = 2 * time.Second

// Watcher tails every log file under the registered parsers' roots.
//
// It is safe for concurrent use: Sessions may be called while Run sweeps.
type Watcher struct {
	registry *parse.Registry
	interval time.Duration

	// MaxAge skips log files untouched for longer than this. Session log
	// roots accumulate for years - hundreds of files and hundreds of
	// megabytes - and reading all of it to answer "what is running now" is
	// waste. Zero reads everything.
	MaxAge time.Duration

	mu      sync.Mutex
	index   *session.Index
	offsets map[string]int64
}

// New returns a watcher over the given registry. A nil registry uses
// parse.Default; a non-positive interval uses DefaultInterval.
func New(registry *parse.Registry, interval time.Duration) *Watcher {
	if registry == nil {
		registry = parse.Default()
	}
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Watcher{
		registry: registry,
		interval: interval,
		index:    session.NewIndex(),
		offsets:  map[string]int64{},
	}
}

// Sessions returns the current session snapshot, most recently active first.
func (w *Watcher) Sessions() []session.Session {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.index.Sessions()
}

// Get returns one indexed session by ID.
func (w *Watcher) Get(id string) (session.Session, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.index.Get(id)
}

// Scan performs one sweep: it finds owned log files under every root, reads
// whatever has been appended since the last sweep, and folds the resulting
// events into the index. It returns the new events in file order.
//
// The first sweep reads each file whole, which is how an already-running
// session becomes visible to a watcher that started late.
func (w *Watcher) Scan() ([]capture.SessionEvent, error) {
	files, err := w.logFiles()
	if err != nil {
		return nil, err
	}
	fresh := []capture.SessionEvent{}
	for _, path := range files {
		events, err := w.tail(path)
		if err != nil {
			// A log that vanished or rotated mid-sweep is not a watcher
			// failure; the next sweep re-reads it from zero.
			continue
		}
		fresh = append(fresh, events...)
	}
	w.mu.Lock()
	for _, event := range fresh {
		w.index.Observe(event)
	}
	w.mu.Unlock()
	return fresh, nil
}

// Run sweeps until the context is cancelled, calling onEvent for every new
// event. A nil onEvent still updates the index.
func (w *Watcher) Run(ctx context.Context, onEvent func(capture.SessionEvent)) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		events, err := w.Scan()
		if err != nil {
			return err
		}
		if onEvent != nil {
			for _, event := range events {
				onEvent(event)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// tail reads the bytes appended to one log since the last sweep.
func (w *Watcher) tail(path string) ([]capture.SessionEvent, error) {
	parser, ok := w.registry.For(path)
	if !ok {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	offset := w.offsets[path]
	w.mu.Unlock()
	if info.Size() < offset {
		// Truncated or replaced: start over rather than reading garbage.
		offset = 0
	}
	if info.Size() == offset {
		return nil, nil
	}
	if _, err := file.Seek(offset, 0); err != nil {
		return nil, err
	}
	events, _, err := parse.ParseReader(parser, path, file)
	if err != nil {
		return nil, err
	}
	// Cursor transcripts carry no time at all, so their events are stamped
	// from the log file instead - without a stamp they sort as if they had
	// happened at epoch. This is deliberately limited to formats that declare
	// themselves untimed: a timed format's zero-timestamp records are records
	// that carry no time *on purpose* (a rename, a mode switch), and stamping
	// them would report a touched file as a working agent.
	if parse.IsUntimed(parser) {
		stamp := info.ModTime().UnixMilli()
		for index := range events {
			if events[index].TS == 0 {
				events[index].TS = stamp
			}
		}
	}
	// TODO(hv): a sweep can land mid-write and cut the final line in half.
	// Advance the offset only to the last newline actually consumed, so the
	// partial record is re-read whole on the next sweep.
	w.mu.Lock()
	w.offsets[path] = info.Size()
	w.mu.Unlock()
	return events, nil
}

// logFiles walks every root for files an owning parser recognizes.
func (w *Watcher) logFiles() ([]string, error) {
	paths := []string{}
	for _, root := range w.registry.Roots() {
		if root == "" {
			continue
		}
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				// An unreadable subtree is skipped, not fatal: roots for tools
				// that are not installed simply do not exist.
				if entry != nil && entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			if _, ok := w.registry.For(path); !ok {
				return nil
			}
			if w.MaxAge > 0 {
				info, err := entry.Info()
				if err != nil || time.Since(info.ModTime()) > w.MaxAge {
					return nil
				}
			}
			paths = append(paths, path)
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	sort.Strings(paths)
	return paths, nil
}
