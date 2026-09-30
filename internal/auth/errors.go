package auth

import "errors"

// Error codes and stable messages of the authentication slice, from the
// error catalog of the implementation overview. Every authentication
// failure shape shares the single unauthorized code: the response never
// enumerates which kind of credential check failed (overview: "Uniform —
// no enumeration of failure kinds").

const (
	// CodeUnauthorized is the uniform authentication-failure code (401).
	CodeUnauthorized = "unauthorized"

	// MessageUnauthorized is the uniform authentication-failure message.
	MessageUnauthorized = "unauthorized"

	// CodeUnavailable is the typed database-infrastructure outcome (503):
	// the helper boundary's typed result for acquisition, BEGIN,
	// set_config, COMMIT, or ROLLBACK failure while the request context is
	// active.
	CodeUnavailable = "unavailable"

	// MessageUnavailable is the typed infrastructure-failure message.
	MessageUnavailable = "database unavailable"
)

// unauthorizedError is the typed, uniform authentication failure (401).
type unauthorizedError struct{}

func (unauthorizedError) Error() string { return MessageUnauthorized }

// ErrUnauthorized is the single typed error for every data-plane and
// admin authentication failure: missing, unknown, disabled, or expired
// credential, disabled application, or wrong admin token. It carries no
// credential material and distinguishes no failure kind.
var ErrUnauthorized = unauthorizedError{}

// unavailableError is the typed database-infrastructure outcome (503) the
// transaction helpers and the data-plane lookup return for failures that
// are not an unknown credential. The wrapped cause is diagnostic only: it
// is never rendered to a caller (raw PostgreSQL text never reaches a
// response).
type unavailableError struct {
	cause error
}

func (e *unavailableError) Error() string { return MessageUnavailable }

func (e *unavailableError) Unwrap() error { return e.cause }

// NewUnavailable wraps a database-infrastructure failure in the typed 503
// outcome.
func NewUnavailable(cause error) error { return &unavailableError{cause: cause} }

// IsUnavailable reports whether err is the typed 503 infrastructure
// outcome.
func IsUnavailable(err error) bool {
	var u *unavailableError
	return errors.As(err, &u)
}
