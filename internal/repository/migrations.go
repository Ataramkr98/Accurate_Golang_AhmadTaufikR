package repository

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationFiles is compiled into the API binary, so local, CI, and hosted
// environments always apply the same ordered schema changes.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockKey names the advisory lock that serialises migration runs.
// hashtext keeps the key stable across processes and releases; it only has to be
// unique among the locks this application takes.
const migrationLockKey = "tera_schema_migrations"

// MigrateTables applies each embedded SQL migration once, in filename order.
//
// Two instances starting at the same time (a rolling deploy, or a second
// container against the same database) would otherwise both see a migration as
// unapplied and both try to apply it: the loser gets a duplicate-object or
// duplicate-key error and the process dies during startup, which is a far worse
// symptom than waiting a few seconds. A session-level advisory lock makes the
// check-and-apply sequence atomic across processes. It is deliberately
// session-level rather than transaction-level so it also covers the
// schema_migrations bootstrap statement above the loop.
func MigrateTables(ctx context.Context, pool *pgxpool.Pool) error {
	// The lock must be taken on one connection and released on the same one:
	// acquiring a session lock through the pool and unlocking through another
	// connection would either fail or, worse, leak the lock.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		// Unlock with a fresh context: ctx may already be cancelled by the time
		// this runs, and leaving the lock held would block every later attempt
		// for the lifetime of the connection.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock(hashtext($1))`, migrationLockKey); err != nil {
			log.Printf("[Postgres] release migration lock: %v", err)
		}
	}()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version VARCHAR(255) PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		version := entry.Name()
		var applied bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if applied {
			continue
		}
		sql, err := migrationFiles.ReadFile("migrations/" + version)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", version, err)
		}
		// Each migration still runs in its own transaction: the advisory lock
		// serialises the run, this transaction keeps a single migration
		// atomic and rolls it back whole if any statement fails.
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", version, err)
		}
		if _, err = tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", version, err)
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", version, err)
		}
		log.Printf("[Postgres] Applied migration %s", version)
	}
	return nil
}
