package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// sanitizeError reduces an error to server-authored, safe fields suitable
// for structured logging. It never returns the raw Error() text, a
// pgconn.PgError's Message, Detail, Hint, Where, or any connection string.
//
// The returned string carries only:
//   - a bounded operation/category label derived from the error chain
//   - the SQLSTATE code if a PgError is present in the chain and the code
//     is well-formed (exactly five uppercase alphanumeric characters)
//   - a context-cancel classification if the context is done
//
// This is the log-boundary policy: suppression is at the log site only —
// the original error is never weakened or replaced in the return chain.
func sanitizeError(err error) string {
	if err == nil {
		return "<nil>"
	}

	// Check for a PgError in the chain first — extract only the SQLSTATE
	// if it is well-formed.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if isValidSqlState(pgErr.Code) {
			return fmt.Sprintf("pg_error:sqlstate=%s", pgErr.Code)
		}
		return "pg_error:sqlstate=<invalid>"
	}

	// Check for a canceled or deadline-exceeded context.
	if errors.Is(err, context.Canceled) {
		return "context:canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context:deadline_exceeded"
	}

	// Everything else: drop all text, return only the class.
	return "error:unspecified"
}

// isValidSqlState checks that a PostgreSQL SQLSTATE code is exactly five
// uppercase alphanumeric characters. Malformed codes must not be emitted
// into log output, as they could carry attacker-controlled data.
func isValidSqlState(code string) bool {
	if len(code) != 5 {
		return false
	}
	for i := 0; i < 5; i++ {
		c := code[i]
		if !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return true
}
