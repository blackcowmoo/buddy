package mysqlerr

import (
	"errors"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func TestIsMatchesTheGivenErrorNumber(t *testing.T) {
	err := &mysql.MySQLError{Number: DupFieldName, Message: "Duplicate column name 'x'"}
	if !Is(err, DupFieldName) {
		t.Error("Is() = false, want true for a matching MySQLError")
	}
	if Is(err, DupKeyName) {
		t.Error("Is() = true, want false for a different error number")
	}
}

func TestIsFalseForNonMySQLErrors(t *testing.T) {
	if Is(errors.New("boom"), DupFieldName) {
		t.Error("Is() = true, want false for a non-MySQLError")
	}
	if Is(nil, DupFieldName) {
		t.Error("Is() = true, want false for a nil error")
	}
}

func TestIsUnwrapsWrappedErrors(t *testing.T) {
	err := &mysql.MySQLError{Number: DupKeyName, Message: "Duplicate key name 'idx'"}
	wrapped := errors.Join(errors.New("context"), err)
	if !Is(wrapped, DupKeyName) {
		t.Error("Is() = false, want true for a wrapped matching MySQLError")
	}
}
