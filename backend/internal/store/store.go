// Package store owns the SQLite connection and the schema.
//
// The connection topology is not incidental — it is spec §7.1. SQLite permits
// exactly one writer, and the default *sql.DB pool will happily open several
// connections and then deadlock them against each other with SQLITE_BUSY. So
// writes go through a pool pinned to a single connection, and reads go through
// a separate pool that WAL lets run concurrently alongside it.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"net/url"
	"sort"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so the binary cross-compiles to the VPS
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type Store struct {
	// W is the writer. Pinned to one connection so writes serialize in Go
	// rather than colliding inside SQLite.
	W *sql.DB
	// R is the reader. WAL allows these to run while a write is in flight.
	R *sql.DB
}

func Open(path string) (*Store, error) {
	// _txlock=immediate takes the write lock when the transaction begins
	// instead of when it first writes, which removes the deferred-to-exclusive
	// upgrade that is the usual source of SQLITE_BUSY under concurrency.
	writePragmas := []string{"busy_timeout(5000)", "foreign_keys(1)", "synchronous(1)"}
	writeDSN := dsn(path, writePragmas, "_txlock", "immediate")
	readDSN := dsn(path, []string{"busy_timeout(5000)"})

	w, err := sql.Open("sqlite", writeDSN)
	if err != nil {
		return nil, fmt.Errorf("open writer: %w", err)
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)

	if err := w.Ping(); err != nil {
		w.Close()
		return nil, fmt.Errorf("ping writer: %w", err)
	}

	// journal_mode is a property of the database file rather than a connection,
	// so it is set once here; the per-connection pragmas ride in on the DSN
	// above, which is the only way to reach every connection in a pool.
	var mode string
	if err := w.QueryRow("PRAGMA journal_mode = WAL").Scan(&mode); err != nil {
		w.Close()
		return nil, fmt.Errorf("set journal_mode: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		w.Close()
		return nil, fmt.Errorf("journal_mode is %q, not WAL: concurrent reads would block on every write", mode)
	}

	r, err := sql.Open("sqlite", readDSN)
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("open reader: %w", err)
	}
	r.SetMaxOpenConns(4)
	r.SetMaxIdleConns(4)

	s := &Store{W: w, R: r}
	if err := s.migrate(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	var first error
	for _, db := range []*sql.DB{s.W, s.R} {
		if db == nil {
			continue
		}
		if err := db.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Healthy reports whether the database answers a trivial query.
func (s *Store) Healthy(ctx context.Context) error {
	var one int
	return s.R.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

// ---------------------------------------------------------------- migrations

func (s *Store) migrate() error {
	if _, err := s.W.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		name       TEXT PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var seen int
		if err := s.W.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).Scan(&seen); err != nil {
			return err
		}
		if seen > 0 {
			continue
		}

		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}

		// Each migration is one transaction: a half-applied schema is worse
		// than a failed startup.
		tx, err := s.W.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations (name, applied_at) VALUES (?, unixepoch())`, name,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

// dsn builds a modernc.org/sqlite connection string. Pragmas repeat as separate
// _pragma parameters; extra key/value pairs follow as driver options.
func dsn(path string, pragmas []string, kv ...string) string {
	q := url.Values{}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	return "file:" + path + "?" + q.Encode()
}
