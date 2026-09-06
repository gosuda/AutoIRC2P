package store

import (
	"context"
	"database/sql"
	"errors"
)

var errTransactionUnavailable = errors.New("database does not support transactions")

func (q *Queries) Transaction(ctx context.Context, apply func(*Queries) error) error {
	db, ok := q.db.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	if !ok {
		return errTransactionUnavailable
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err = apply(q.WithTx(tx)); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}
