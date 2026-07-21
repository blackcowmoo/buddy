// Package mysqlerr recognizes specific MySQL server error numbers, so
// callers can tell "this additive migration step was already applied" apart
// from a real failure. MySQL (unlike MariaDB) has no ADD COLUMN/INDEX IF NOT
// EXISTS across every 8.0 point release this app has run against, so
// idempotent startup migrations (internal/store's NewMySQL, internal/
// recording's NewS3) stay idempotent by swallowing these specific errors
// instead.
package mysqlerr

import (
	"errors"

	"github.com/go-sql-driver/mysql"
)

const (
	DupFieldName = 1060 // ER_DUP_FIELDNAME: ADD COLUMN against a column that already exists
	DupKeyName   = 1061 // ER_DUP_KEYNAME: ADD INDEX against an index name that already exists
)

// Is reports whether err is a *mysql.MySQLError carrying the given server
// error number.
func Is(err error, number uint16) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == number
}

// ApplyAdditive runs one additive migration statement (ADD COLUMN/ADD INDEX)
// via run, treating alreadyApplied as success instead of an error — see the
// package doc for why MySQL's lack of IF NOT EXISTS makes that necessary.
func ApplyAdditive(run func() error, alreadyApplied uint16) error {
	if err := run(); err != nil && !Is(err, alreadyApplied) {
		return err
	}
	return nil
}
