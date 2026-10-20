// Package history persists finished events in a SQLite database (pure Go,
// only when built with -tags sqlite).
package history

// DefaultMaxRows bounds the number of events kept in the database.
const DefaultMaxRows = 100_000
