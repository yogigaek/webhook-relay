package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The SQL files are compiled into the binary, so a deploy is one file with no migrations folder
// to copy alongside it.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLock is an arbitrary key for pg_advisory_lock: instances starting at the same time
// migrate one after another instead of racing on the same statements.
const migrationLock = 7_301_204

// Migrate applies every migration not yet recorded in schema_migrations, in file name order, each
// in its own transaction. A failed migration leaves the database as it was before that file.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	// An advisory lock belongs to a session, so everything runs on one connection.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer unlock(ctx, conn)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)

	for _, name := range names {
		if err := applyOne(ctx, conn.Conn(), name); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

// unlock frees the migration lock. It runs with a fresh context, so a cancelled start still frees the
// lock, but with a timeout of its own, so a database that stopped answering cannot hang the start.
// A failed unlock does not fail Migrate: closing the connection frees the lock anyway, and failing
// a start whose migrations all committed would only make the relay restart for nothing.
func unlock(ctx context.Context, conn *pgxpool.Conn) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, migrationLock); err != nil {
		// The session may still hold the lock. Closing the connection ends the session, which frees
		// it, and the pool drops a closed connection instead of handing it to the next Migrate.
		_ = conn.Conn().Close(ctx)
	}
}

func applyOne(ctx context.Context, conn *pgx.Conn, name string) error {
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		var applied bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, name).Scan(&applied); err != nil {
			return err
		}
		if applied {
			return nil
		}
		sql, err := migrationFiles.ReadFile(name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name)
		return err
	})
}
