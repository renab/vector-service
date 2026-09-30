// Vector-space resolution (package 3, section 1), inside the request
// transaction.
//
// ResolveInTx resolves a vector-space key against
// vector_control.vector_spaces inside an already-open application-scoped
// transaction — the same dbctx.WithAppContext transaction that resolved the
// namespace and in which the business SQL runs, so namespace, space, and
// data-plane statements share one transaction and one transaction-local
// application context. The runtime role holds only SELECT on
// vector_spaces (migration 0004); the service never writes space rows
// (invariant 5).
//
// Outcomes (package 3, section 1), for every operation that names the
// space:
//
//   - unknown key           -> ErrSpaceNotFound (404 vector_space_not_found)
//   - disabled space        -> ErrSpaceUnavailable (422 vector_space_unavailable)
//   - retired space, upsert -> ErrSpaceUnavailable (422 vector_space_unavailable)
//   - retired space, any
//     other operation       -> allowed (get, search, deletes; the
//     retire-then-purge flow)
//
// Operations that do not name a space (get by record ID, object delete
// without ?vector_space=) never call this function: records in disabled or
// retired spaces remain retrievable and deletable by ID.
//
// No row locking: the read-then-write in the request transaction is the
// documented bounded-concurrent behavior (package 3, section 1).
package vectors

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
)

// Distance metric names, mirroring the vector_control.distance_metric enum
// (migration 0001). The service never writes space rows, so these are read
// values, not inputs.
type Metric string

const (
	// Cosine is the cosine metric (pgvector <=>).
	Cosine Metric = "cosine"
	// L2 is the euclidean metric (pgvector <->).
	L2 Metric = "l2"
	// InnerProduct is the inner-product metric (pgvector <#>).
	InnerProduct Metric = "inner_product"
)

// IsCosine reports whether m is the cosine metric.
func (m Metric) IsCosine() bool { return m == Cosine }

// SpaceKeyGrammar renders the accepted vector-space-key grammar for
// diagnostic messages (migration 0001, vector_spaces_key_format).
const SpaceKeyGrammar = "^[a-z0-9][a-z0-9._-]{0,127}$"

// MaxSpaceKeyLength is the maximum accepted length of a vector-space key
// (one leading character plus at most 127 further characters).
const MaxSpaceKeyLength = 128

// spaceKeyPattern is the vector-space-key grammar: the same constraint as
// the schema's vector_spaces_key_format CHECK.
var spaceKeyPattern = regexp.MustCompile(SpaceKeyGrammar)

// ValidateSpaceKey checks a vector-space key against the grammar and length
// bound. An empty key, a key above MaxSpaceKeyLength, or a key containing a
// character outside [a-z0-9._-] (or not starting with [a-z0-9]) yields a
// typed error. The key is never logged on failure.
func ValidateSpaceKey(key string) error {
	if key == "" {
		return fmt.Errorf("vectors: %w: key is empty", ErrInvalidSpaceKey)
	}
	if len(key) > MaxSpaceKeyLength {
		return fmt.Errorf("vectors: %w: key length %d exceeds %d",
			ErrInvalidSpaceKey, len(key), MaxSpaceKeyLength)
	}
	if !spaceKeyPattern.MatchString(key) {
		return fmt.Errorf("vectors: %w: key does not match %s",
			ErrInvalidSpaceKey, SpaceKeyGrammar)
	}
	return nil
}

// ErrInvalidSpaceKey is the typed vector-space-key validation failure (400:
// the catalog has no dedicated space-key code, so a malformed key surfaces
// as a request-validation failure at the handler boundary). The key itself
// is not part of the error value; the message describes the shape of the
// failure without echoing the input.
var ErrInvalidSpaceKey = errors.New("invalid vector space key")

// Space is a resolved vector space: the compatibility contract the
// operation validates against (dimensions, metric) and the identity it
// binds as a SQL parameter. The state the gate consumed (enabled, retired)
// is not exposed: the operation has passed the gate and needs no more of
// it — re-deriving the gate from these fields would only restate the rule
// ResolveInTx already applied.
type Space struct {
	// ID is the space's UUID, bound as the vector_space_id parameter.
	ID string

	// Key is the resolved space key — the same key the request named.
	Key string

	// Dimensions is the space's dimensionality (1..16000 per the schema
	// CHECK), the target of the dimension check.
	Dimensions int

	// Metric is the space's distance metric.
	Metric Metric
}

// hiddenState is the raw resolution result: the bound fields plus the state
// fields (enabled, retired) the gate consumes before returning Space.
type hiddenState struct {
	Space
	Enabled bool
	Retired bool
}

// spaceSelectSQL resolves the named space row. The predicate is on the
// unique key only: unlike namespaces, a vector space is not application
// scoped (the vector_spaces table carries no application column), and the
// runtime role's SELECT grant on the table is the effective boundary.
const spaceSelectSQL = `
	SELECT id, vector_space_key, dimensions, distance_metric, enabled,
	       (retired_at IS NOT NULL)
	FROM vector_control.vector_spaces
	WHERE vector_space_key = $1`

// ResolveInTx resolves the vector-space key inside the request
// transaction, validates its state for the operation, and returns the
// resolved space. It requires the transaction to have already established
// the transaction-local application context (the dbctx.WithAppContext first
// statement) — the namespace resolved first in the same transaction
// requires it regardless.
//
// For a space in a disallowed state, the typed error carries no space
// identity beyond the condition itself; the key is not echoed.
func ResolveInTx(ctx context.Context, tx pgx.Tx, key string, write bool) (Space, error) {
	if err := ValidateSpaceKey(key); err != nil {
		return Space{}, err
	}
	var st hiddenState
	err := tx.QueryRow(ctx, spaceSelectSQL, key).
		Scan(&st.ID, &st.Key, &st.Dimensions, &st.Metric, &st.Enabled, &st.Retired)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
			return Space{}, ErrSpaceNotFound
		}
		return Space{}, err
	}
	switch {
	case !st.Enabled:
		// A disabled space is unavailable to every operation that
		// names it (upsert, search, projection delete, object delete
		// with ?vector_space=).
		return Space{}, ErrSpaceUnavailable
	case st.Retired && write:
		// A retired space accepts no upserts; get, search, and deletes
		// remain allowed (the retire-then-purge flow).
		return Space{}, ErrSpaceUnavailable
	}
	return st.Space, nil
}
