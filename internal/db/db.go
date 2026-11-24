package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

func Tx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return Tx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7412801)`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
			return err
		}
		files, err := fs.Glob(migrations, "migrations/*.sql")
		if err != nil {
			return err
		}
		for _, name := range files {
			var applied bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM schema_migrations WHERE name=$1)`, name).Scan(&applied); err != nil {
				return err
			}
			if applied {
				continue
			}
			body, err := migrations.ReadFile(name)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return fmt.Errorf("migration %s: %w", name, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(name) VALUES ($1)`, name); err != nil {
				return err
			}
		}
		return nil
	})
}

func Audit(ctx context.Context, tx pgx.Tx, actor, action, object string) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_logs(actor_id,action,object_id) VALUES(NULLIF($1,'')::uuid,$2,$3)`, actor, action, object)
	return err
}
