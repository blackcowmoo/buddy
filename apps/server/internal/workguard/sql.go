package workguard

import (
	"context"
	"database/sql"
)

// SQLExecutor is the shared capability needed by short derived writes. Reusing
// the deletion fence's transaction both makes the write atomic and avoids
// exhausting the connection pool while every fence waits for a second connection.
type SQLExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type transactionKey struct{ db *sql.DB }

func WithTx(ctx context.Context, db *sql.DB, tx *sql.Tx) context.Context {
	return context.WithValue(ctx, transactionKey{db}, tx)
}

func Tx(ctx context.Context, db *sql.DB) *sql.Tx {
	tx, _ := ctx.Value(transactionKey{db}).(*sql.Tx)
	return tx
}

// Executor only reuses a transaction belonging to this exact database pool.
func Executor(ctx context.Context, db *sql.DB) SQLExecutor {
	if tx := Tx(ctx, db); tx != nil {
		return tx
	}
	return db
}
