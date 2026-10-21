//go:build !sqlite

package history

import (
	"errors"
	"log/slog"

	"github.com/olive2bx/ricochet/internal/collector"
)

// Enabled reports whether SQLite support is compiled in.
const Enabled = false

// DB is unavailable without the sqlite build tag.
type DB struct{}

// Open always fails without the sqlite build tag.
func Open(string, int, *slog.Logger) (*DB, error) {
	return nil, errors.New("persistent history needs a binary built with: go build -tags sqlite ./cmd/ricochet")
}

func (*DB) Save(collector.Event)                {}
func (*DB) Clear() error                        { return nil }
func (*DB) Load(int) ([]collector.Event, error) { return nil, nil }
func (*DB) Close() error                        { return nil }
