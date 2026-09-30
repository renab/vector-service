package migrate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// sentinelSecret is a unique, obviously-synthetic string injected into
// PgError fields to prove they do not leak into log output.
const sentinelSecret = "SENTINEL-SECRET-DO-NOT-LEAK-abcdef0123456789"

// sentinelPgErr builds a *pgconn.PgError with sentinel secret text in
// every field that must not reach log output: Message, Detail, Hint,
// Where, and Position (which may carry SQL fragments).
func sentinelPgErr() *pgconn.PgError {
	return &pgconn.PgError{
		Severity:       "ERROR",
		Code:           "42501",
		Message:        sentinelSecret,
		Detail:         sentinelSecret,
		Hint:           sentinelSecret,
		Where:          sentinelSecret,
		Position:       42, // numeric, not sensitive
		Routine:        "test_routine",
		SchemaName:     "test_schema",
		TableName:      "test_table",
		ColumnName:     "test_column",
		DataTypeName:   "test_type",
		ConstraintName: "test_constraint",
		File:           "test_file.c",
		Line:           123,
	}
}

// TestSanitizeError_PgErrorNoLeak asserts that a PgError's Message,
// Detail, Hint, Where, and Position never appear in the sanitized output.
// The sentinel secret text is injected into every text field; the output
// must contain only the SQLSTATE code (an allowlisted identifier).
func TestSanitizeError_PgErrorNoLeak(t *testing.T) {
	pgErr := sentinelPgErr()
	result := sanitizeError(pgErr)

	// The output must contain the SQLSTATE code.
	if !strings.Contains(result, "42501") {
		t.Errorf("sanitizeError: expected SQLSTATE 42501 in output, got: %q", result)
	}

	// The output must NOT contain any sentinel text.
	forbidden := []string{
		sentinelSecret,
		pgErr.Error(), // the full rendering
		pgErr.Message,
		pgErr.Detail,
		pgErr.Hint,
		pgErr.Where,
	}
	for _, f := range forbidden {
		if strings.Contains(result, f) {
			t.Errorf("sanitizeError: output contains forbidden text %q (result: %q)", f, result)
		}
	}
}

// TestSanitizeError_WrappedPgErrorNoLeak asserts that a PgError wrapped
// by fmt.Errorf still has its sensitive fields sanitized. The wrapper
// text is server-authored and safe; the inner PgError must be reduced
// to its SQLSTATE.
func TestSanitizeError_WrappedPgErrorNoLeak(t *testing.T) {
	pgErr := sentinelPgErr()
	wrapped := fmt.Errorf("connect as migration identity: %w", pgErr)
	result := sanitizeError(wrapped)

	// The output must contain the SQLSTATE code.
	if !strings.Contains(result, "42501") {
		t.Errorf("sanitizeError: expected SQLSTATE 42501 in output, got: %q", result)
	}

	// The output must NOT contain any sentinel text.
	forbidden := []string{
		sentinelSecret,
		pgErr.Error(),
		pgErr.Message,
		pgErr.Detail,
		pgErr.Hint,
		pgErr.Where,
	}
	for _, f := range forbidden {
		if strings.Contains(result, f) {
			t.Errorf("sanitizeError: output contains forbidden text %q (result: %q)", f, result)
		}
	}
}

// TestSanitizeError_ContextCancellation asserts that a canceled context
// is classified as "context:canceled" with no raw text.
func TestSanitizeError_ContextCancellation(t *testing.T) {
	result := sanitizeError(context.Canceled)
	if result != "context:canceled" {
		t.Errorf("sanitizeError(context.Canceled) = %q, want context:canceled", result)
	}
	if strings.Contains(result, sentinelSecret) {
		t.Error("sanitizeError: output contains sentinel text for context cancellation")
	}
}

// TestSanitizeError_ContextDeadline asserts that a deadline-exceeded
// context is classified as "context:deadline_exceeded" with no raw text.
func TestSanitizeError_ContextDeadline(t *testing.T) {
	result := sanitizeError(context.DeadlineExceeded)
	if result != "context:deadline_exceeded" {
		t.Errorf("sanitizeError(context.DeadlineExceeded) = %q, want context:deadline_exceeded", result)
	}
	if strings.Contains(result, sentinelSecret) {
		t.Error("sanitizeError: output contains sentinel text for context deadline")
	}
}

// TestSanitizeError_GenericError asserts that a generic error is
// classified as "error:unspecified" with no raw text.
func TestSanitizeError_GenericError(t *testing.T) {
	err := errors.New("generic failure: " + sentinelSecret)
	result := sanitizeError(err)

	if result != "error:unspecified" {
		t.Errorf("sanitizeError(generic) = %q, want error:unspecified", result)
	}
	if strings.Contains(result, sentinelSecret) {
		t.Errorf("sanitizeError: output contains sentinel text for generic error: %q", result)
	}
}

// TestSanitizeError_Nil asserts that a nil error returns a safe placeholder.
func TestSanitizeError_Nil(t *testing.T) {
	result := sanitizeError(nil)
	if result != "<nil>" {
		t.Errorf("sanitizeError(nil) = %q, want <nil>", result)
	}
}

// TestSanitizeError_DeeplyWrapped asserts that a deeply-wrapped PgError
// (multiple fmt.Errorf layers) still has its sensitive fields sanitized.
func TestSanitizeError_DeeplyWrapped(t *testing.T) {
	pgErr := sentinelPgErr()
	wrapped := fmt.Errorf("outer: %w",
		fmt.Errorf("middle: %w",
			fmt.Errorf("inner: %w", pgErr)))
	result := sanitizeError(wrapped)

	// The output must contain the SQLSTATE code.
	if !strings.Contains(result, "42501") {
		t.Errorf("sanitizeError: expected SQLSTATE 42501 in output, got: %q", result)
	}

	// The output must NOT contain any sentinel text.
	for _, f := range []string{sentinelSecret, pgErr.Message, pgErr.Detail, pgErr.Hint, pgErr.Where} {
		if strings.Contains(result, f) {
			t.Errorf("sanitizeError: output contains forbidden text %q (result: %q)", f, result)
		}
	}
}

// TestSanitizeError_ConnectionStringNoLeak asserts that a connection
// string error (which may carry host/port/user/dbname) is sanitized.
func TestSanitizeError_ConnectionStringNoLeak(t *testing.T) {
	err := errors.New("connection to server at \"10.0.0.5\", port 5432 failed: password required for user " + sentinelSecret)
	result := sanitizeError(err)

	if strings.Contains(result, sentinelSecret) {
		t.Errorf("sanitizeError: output contains sentinel text for connection string error: %q", result)
	}
	if strings.Contains(result, "10.0.0.5") {
		t.Errorf("sanitizeError: output contains IP address for connection string error: %q", result)
	}
}

// TestSanitizeError_WrappedContextCancellation asserts that a wrapped
// context cancellation is classified as "context:canceled".
func TestSanitizeError_WrappedContextCancellation(t *testing.T) {
	wrapped := fmt.Errorf("operation failed: %w", context.Canceled)
	result := sanitizeError(wrapped)
	if result != "context:canceled" {
		t.Errorf("sanitizeError(wrapped context.Canceled) = %q, want context:canceled", result)
	}
}

// TestSanitizeError_MalformedSqlState asserts that a PgError with a
// malformed Code (lowercase, too short, too long, with newlines, etc.)
// falls back to a safe classification without exposing the raw code.
func TestSanitizeError_MalformedSqlState(t *testing.T) {
	tests := []struct {
		name string
		code string
	}{
		{"lowercase", "57p01"},
		{"tooShort", "57P"},
		{"tooLong", "57P01X"},
		{"withNewline", "57P\n1"},
		{"withSpace", "57P 1"},
		{"specialChars", "57P!@"},
		{"mixedCase", "57pA1"},
		{"oversized", strings.Repeat("A", 100)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pgErr := &pgconn.PgError{
				Severity: "FATAL",
				Code:     tt.code,
				Message:  sentinelSecret,
			}
			result := sanitizeError(pgErr)

			// Must not contain the raw malformed code.
			if strings.Contains(result, tt.code) {
				t.Errorf("sanitizeError: output contains raw malformed code %q (result: %q)", tt.code, result)
			}

			// Must not contain sentinel text.
			if strings.Contains(result, sentinelSecret) {
				t.Errorf("sanitizeError: output contains sentinel text: %q", result)
			}

			// Must contain safe classification.
			if !strings.Contains(result, "pg_error:sqlstate=<invalid>") {
				t.Errorf("sanitizeError: expected safe classification for malformed code, got: %q", result)
			}
		})
	}
}

// TestSanitizeError_ValidSqlState asserts that a well-formed SQLSTATE
// code is emitted into the classification.
func TestSanitizeError_ValidSqlState(t *testing.T) {
	tests := []struct {
		name string
		code string
	}{
		{"standard", "57P01"},
		{"allNumeric", "00000"},
		{"allAlpha", "ABCDE"},
		{"mixed", "12ABC"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pgErr := &pgconn.PgError{
				Severity: "FATAL",
				Code:     tt.code,
				Message:  sentinelSecret,
			}
			result := sanitizeError(pgErr)

			// Must contain the valid SQLSTATE code.
			if !strings.Contains(result, tt.code) {
				t.Errorf("sanitizeError: expected SQLSTATE %q in output, got: %q", tt.code, result)
			}

			// Must not contain sentinel text.
			if strings.Contains(result, sentinelSecret) {
				t.Errorf("sanitizeError: output contains sentinel text: %q", result)
			}
		})
	}
}

// TestIsValidSqlState asserts the validator directly.
func TestIsValidSqlState(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"57P01", true},
		{"00000", true},
		{"ABCDE", true},
		{"12ABC", true},
		{"57p01", false},  // lowercase
		{"57P", false},    // too short
		{"57P01X", false}, // too long
		{"57P\n1", false}, // newline
		{"57P 1", false},  // space
		{"", false},       // empty
		{"57P!@", false},  // special chars
		{"12345", true},   // numeric only is valid
		{"57pA1", false},  // mixed case
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := isValidSqlState(tt.input)
			if got != tt.want {
				t.Errorf("isValidSqlState(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
