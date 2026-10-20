//go:build sqlite

package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure Go driver, no CGO

	"github.com/olive2bx/ricochet/internal/collector"
)

// Enabled reports whether SQLite support is compiled in.
const Enabled = true

const schema = `
CREATE TABLE IF NOT EXISTS events (
	id         INTEGER PRIMARY KEY,
	started_at TEXT NOT NULL,
	host       TEXT NOT NULL,
	data       TEXT NOT NULL -- JSON-encoded collector.Event
);`

type op struct {
	save  *collector.Event
	clear chan error
}

// DB persists events asynchronously: Save never blocks the proxy.
type DB struct {
	db      *sql.DB
	log     *slog.Logger
	maxRows int
	ops     chan op
	done    chan struct{}

	mu     sync.RWMutex // guards closed against sends on a closed ops
	closed bool
}

// Open opens (or creates) the database at path. maxRows <= 0 means
// DefaultMaxRows.
func Open(path string, maxRows int, logger *slog.Logger) (*DB, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if maxRows <= 0 {
		maxRows = DefaultMaxRows
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open history db: %w", err)
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, stmt := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", schema} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("init history db %s: %w", path, err)
		}
	}
	d := &DB{db: db, log: logger, maxRows: maxRows, ops: make(chan op, 4096), done: make(chan struct{})}
	go d.loop()
	return d, nil
}

// Save queues e for writing. Events are dropped if the queue is full.
func (d *DB) Save(e collector.Event) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return
	}
	select {
	case d.ops <- op{save: &e}:
	default:
		d.log.Warn("history db queue full, event not persisted", "id", e.ID)
	}
}

// Clear deletes every stored event, after pending writes.
func (d *DB) Clear() error {
	ch := make(chan error, 1)
	d.mu.RLock()
	if d.closed {
		d.mu.RUnlock()
		return errors.New("history db closed")
	}
	d.ops <- op{clear: ch}
	d.mu.RUnlock()
	return <-ch
}

// Load returns the last limit events, oldest first.
func (d *DB) Load(limit int) ([]collector.Event, error) {
	rows, err := d.db.Query(`SELECT data FROM (SELECT id, data FROM events ORDER BY id DESC LIMIT ?) ORDER BY id`, limit)
	if err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}
	defer rows.Close()
	var out []collector.Event
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("load history: %w", err)
		}
		var e collector.Event
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			d.log.Warn("skipping unreadable history row", "err", err)
			continue
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Close flushes pending writes and closes the database.
func (d *DB) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	close(d.ops)
	d.mu.Unlock()
	<-d.done
	return d.db.Close()
}

func (d *DB) loop() {
	defer close(d.done)
	var batch []collector.Event
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	flushes := 0
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := d.write(batch); err != nil {
			d.log.Warn("history db write", "err", err, "events", len(batch))
		}
		batch = batch[:0]
		if flushes++; flushes%50 == 0 {
			d.prune()
		}
	}
	for {
		select {
		case o, ok := <-d.ops:
			if !ok {
				flush()
				return
			}
			switch {
			case o.save != nil:
				batch = append(batch, *o.save)
				if len(batch) >= 200 {
					flush()
				}
			case o.clear != nil:
				batch = batch[:0]
				_, err := d.db.Exec(`DELETE FROM events`)
				o.clear <- err
			}
		case <-tick.C:
			flush()
		}
	}
}

func (d *DB) write(events []collector.Event) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO events (id, started_at, host, data) VALUES (?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, e := range events {
		data, err := json.Marshal(e)
		if err != nil {
			continue
		}
		if _, err := stmt.Exec(e.ID, e.StartedAt.UTC().Format(time.RFC3339Nano), e.Host, string(data)); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) prune() {
	if _, err := d.db.Exec(`DELETE FROM events WHERE id NOT IN (SELECT id FROM events ORDER BY id DESC LIMIT ?)`, d.maxRows); err != nil {
		d.log.Warn("history db prune", "err", err)
	}
}
