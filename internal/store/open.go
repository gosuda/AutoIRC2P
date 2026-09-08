package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

//go:embed migration2.sql
var migration2 string

//go:embed migration3.sql
var migration3 string

//go:embed migration4.sql
var migration4 string

const schemaVersion = 4

func NewSQLite(ctx context.Context, path string) (*sql.DB, *Queries, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, nil, err
	}
	if err = f.Close(); err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(err error) (*sql.DB, *Queries, error) { return nil, nil, errors.Join(err, db.Close()) }
	if _, err = db.ExecContext(ctx, "PRAGMA foreign_keys=ON; PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;"); err != nil {
		return fail(err)
	}
	var version int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fail(err)
	}
	if version < 0 || version > schemaVersion {
		return fail(fmt.Errorf("unsupported database version %d", version))
	}
	if version < schemaVersion {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fail(err)
		}
		migration := schema
		switch version {
		case 1:
			migration = migration2 + "\n" + migration3 + "\n" + migration4
		case 2:
			migration = migration3 + "\n" + migration4
		case 3:
			migration = migration4
		}
		if _, err = tx.ExecContext(ctx, migration+fmt.Sprintf("\nPRAGMA user_version=%d;", schemaVersion)); err != nil {
			return fail(errors.Join(err, tx.Rollback()))
		}
		if err = tx.Commit(); err != nil {
			return fail(err)
		}
	}
	if err = New(db).RecoverSends(ctx); err != nil {
		return fail(err)
	}
	return db, New(db), nil
}

func (q *Queries) GetTranslation(ctx context.Context, key string) (string, error) {
	return q.CachedTranslation(ctx, key)
}
func (q *Queries) PutTranslation(ctx context.Context, key, value string) error {
	return q.SaveTranslation(ctx, SaveTranslationParams{CacheKey: key, Translated: value})
}
