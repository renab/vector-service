// Package vectors, package-3 vector operations: the transaction-scoped
// upsert, get, projection delete, object delete, and search service
// functions of the data plane, per docs/implementation/
// package-3-vector-operations.md.
//
// Each operation resolves the authenticated namespace (and, when the
// request names a vector space, the vector space) inside ONE
// dbctx.WithAppContext transaction, then runs its business SQL in that same
// transaction. Every statement is explicit, parameterized SQL; no
// caller-controlled value is ever interpolated into SQL text. The
// application identity comes only from the authenticated context carried on
// ctx (dbctx.WithAppID) — a data-plane caller can never supply an
// application id.
//
// The functions return typed sentinel errors that the package-4 handler
// classifies into HTTP status and error codes. This package does no HTTP
// rendering.
package vectors

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"vector-service/internal/config"
	"vector-service/internal/dbctx"
	"vector-service/internal/namespaces"
)

// ---------------------------------------------------------------------------
// Typed errors
// ---------------------------------------------------------------------------
//
// Each sentinel carries a stable, generic message safe to render (it names
// no caller-controlled value, no identity, no internal detail). The
// package-4 handler maps the sentinel to its HTTP status and error code:
//
//	ErrMissingField         -> 400  missing_field
//	ErrInvalidVector        -> 400  invalid_vector
//	ErrNonFiniteVector      -> 422  non_finite_vector
//	ErrInvalidDimensions    -> 422  invalid_vector_dimensions
//	ErrSpaceNotFound        -> 404  vector_space_not_found
//	ErrSpaceUnavailable     -> 422  vector_space_unavailable
//	ErrInvalidContentHash   -> 400  invalid_content_hash
//	ErrMetadataNotObject    -> 400  metadata_not_object
//	ErrMetadataTooLarge     -> 400  metadata_too_large
//	ErrInvalidTimestamp     -> 400  invalid_timestamp
//	ErrDuplicateRecord      -> 400  duplicate_record
//	ErrBatchTooLarge        -> 413  batch_too_large
//	ErrInvalidLimit         -> 400  invalid_limit
//	ErrInvalidFilter        -> 400  invalid_filter
//	ErrInvalidUUID          -> 400  invalid_uuid
//	ErrRecordNotFound       -> 404  record_not_found

// ErrMissingField: a required request field is absent (400, missing_field).
var ErrMissingField = errors.New("missing required field")

// ErrInvalidContentHash: the content hash is not exactly 64 lowercase
// hexadecimal characters (400, invalid_content_hash).
var ErrInvalidContentHash = errors.New("invalid content hash")

// ErrMetadataNotObject: the metadata is not a JSON object (400,
// metadata_not_object).
var ErrMetadataNotObject = errors.New("metadata is not a JSON object")

// ErrMetadataTooLarge: the canonical metadata bytes exceed the configured
// bound (400, metadata_too_large).
var ErrMetadataTooLarge = errors.New("metadata exceeds the maximum size")

// ErrInvalidTimestamp: source_updated_at is not an RFC 3339 timestamp (400,
// invalid_timestamp).
var ErrInvalidTimestamp = errors.New("source_updated_at is not an RFC 3339 timestamp")

// ErrDuplicateRecord: the batch names the same (object_id, projection_id)
// more than once (400, duplicate_record).
var ErrDuplicateRecord = errors.New("batch contains a duplicate (object_id, projection_id) pair")

// ErrBatchTooLarge: the batch exceeds the configured record bound (413,
// batch_too_large).
var ErrBatchTooLarge = errors.New("batch exceeds the maximum record count")

// ErrInvalidLimit: the search limit is not a positive integer within the
// bound (400, invalid_limit).
var ErrInvalidLimit = errors.New("limit is not a positive integer within the bound")

// ErrInvalidFilter: a metadata filter violates the structured grammar (400,
// invalid_filter).
var ErrInvalidFilter = errors.New("metadata filter is invalid")

// ErrInvalidUUID: a request-supplied UUID is malformed (400, invalid_uuid).
var ErrInvalidUUID = errors.New("value is not a valid UUID")

// ErrRecordNotFound: the requested record does not exist (404,
// record_not_found).
var ErrRecordNotFound = errors.New("record not found")

// ---------------------------------------------------------------------------
// Request and result types
// ---------------------------------------------------------------------------

// UpsertRecord is one record of an upsert batch. It is the package-4
// contract shape, mirrored here so this package stays import-free of the
// handler. The package-4 handler decodes the JSON body into these structs
// and calls Upsert.
type UpsertRecord struct {
	// ObjectID and ProjectionID identify the record's logical identity
	// (both required, non-empty; opaque caller-owned strings per docs/API.md
	// — the service does not enforce a UUID grammar on them).
	ObjectID     string `json:"object_id"`
	ProjectionID string `json:"projection_id"`

	// ContentHash is the 64-character lowercase hex SHA-256 of the source
	// content (required).
	ContentHash string `json:"content_hash"`

	// SourceUpdatedAt is the RFC 3339 timestamp of the source content's
	// last update (optional; empty string means absent → NULL in the DB).
	SourceUpdatedAt string `json:"source_updated_at,omitempty"`

	// Metadata is the structured metadata object (required; a JSON object
	// in the request). A nil value is metadata_not_object.
	Metadata map[string]any `json:"metadata"`

	// Vector is the caller-produced embedding vector (required). The JSON
	// field name is "vector" per docs/API.md.
	Vector []float32 `json:"vector"`
}

// UpsertRequest is the package-3 upsert operation's arguments. NamespaceKey
// is the request's namespace key (validated by the handler against the
// namespaces grammar); VectorSpace is the stable space key the batch is
// written into; Records is the bounded batch.
type UpsertRequest struct {
	NamespaceKey string
	VectorSpace  string
	Records      []UpsertRecord
}

// UpsertResult is the upsert outcome: how many records were written or
// updated. Unchanged is always 0 (the service has no idempotence-detection
// path; a re-upsert is a write, reported as upserted).
type UpsertResult struct {
	Upserted  int
	Unchanged int
}

// GetRequest is the get operation's arguments. NamespaceKey is the
// request's namespace key; ObjectID and ProjectionID name the record;
// VectorSpace is the stable space key.
type GetRequest struct {
	NamespaceKey string
	ObjectID     string
	ProjectionID string
	VectorSpace  string
}

// GetResult is the get outcome. SourceUpdatedAt is the database's
// timestamptz, or nil when the column is NULL (the handler omits the field
// when nil). VectorSpace is the space key (joined from vector_spaces, not
// stored on the record).
type GetResult struct {
	RecordID        string
	ObjectID        string
	ProjectionID    string
	VectorSpace     string
	ContentHash     string
	SourceUpdatedAt *time.Time
	Metadata        map[string]any
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// DeleteRequest is the delete operations' arguments. NamespaceKey is the
// request's namespace key; ObjectID names the object. When VectorSpace is
// empty the operation is the object delete (every space of the object);
// when set, it is the projection delete (only that space).
type DeleteRequest struct {
	NamespaceKey string
	ObjectID     string
	ProjectionID string
	VectorSpace  string
}

// DeleteResult is the delete outcome: how many records were removed.
type DeleteResult struct {
	Deleted int
}

// FilterOperator is a metadata filter's comparison operator. The v1 grammar
// supports exactly these two.
type FilterOperator string

const (
	// FilterEq is the equality operator (exactly one value).
	FilterEq FilterOperator = "eq"
	// FilterIn is the set-membership operator (one or more values).
	FilterIn FilterOperator = "in"
)

// FilterEntry is one structured metadata filter. Field is the metadata
// field name (non-empty, <= 128 bytes); Op is one of the two allowed
// operators; Values are the comparison values (JSON scalars, rendered as
// text and compared against metadata ->> (text)).
type FilterEntry struct {
	Field  string
	Op     FilterOperator
	Values []any
}

// SearchRequest is the search operation's arguments.
type SearchRequest struct {
	NamespaceKey string
	VectorSpace  string
	Embedding    []float32
	Filters      []FilterEntry
	Limit        int
}

// Match is one search hit. Score is the metric-specific similarity (higher
// is better); a nil Score marks a match excluded from the response by the
// non-finite rule (the handler omits it and a warn is logged). Distance is
// the raw pgvector distance (never rendered to the client).
type Match struct {
	RecordID        string
	ObjectID        string
	ProjectionID    string
	Score           *float64
	ContentHash     string
	SourceUpdatedAt *time.Time
	Metadata        map[string]any
	Distance        float64
}

// SearchResult is the search outcome. Matches is always non-nil (an empty
// search returns an empty array, not an omitted field).
type SearchResult struct {
	Matches []Match
}

// ---------------------------------------------------------------------------
// Bounds
// ---------------------------------------------------------------------------

// Bounds carries the configured package-3 limits. Service functions take a
// Bounds value so tests can exercise the limits deterministically; the
// package-4 handler constructs one from config.Load via BoundsFromConfig.
type Bounds struct {
	UpsertMaxRecords int
	SearchMaxLimit   int
	MaxFilters       int
	MaxFilterValues  int
	MaxMetadataBytes int
}

// BoundsFromConfig derives the package-3 bounds from a loaded Config.
func BoundsFromConfig(c *config.Config) Bounds {
	return Bounds{
		UpsertMaxRecords: c.UpsertMaxRecords,
		SearchMaxLimit:   c.SearchMaxLimit,
		MaxFilters:       c.MaxFilters,
		MaxFilterValues:  c.MaxFilterValues,
		MaxMetadataBytes: c.MaxMetadataBytes,
	}
}

// ---------------------------------------------------------------------------
// Validation helpers (pure; unit-tested without a database)
// ---------------------------------------------------------------------------

// newRecordID returns a random UUIDv4 (RFC 4122) as a lowercase hyphenated
// string. The service generates the record's row identity; a re-upsert of
// the same logical identity updates in place, never a new row.
func newRecordID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate record id: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40 // version 4
	raw[8] = (raw[8] & 0x3f) | 0x80 // variant 10
	return formatUUIDBytes(raw[:]), nil
}

// formatUUIDBytes renders 16 bytes as a lowercase hyphenated UUID.
func formatUUIDBytes(b []byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// isCanonicalUUID reports whether s is a lowercase hyphenated UUID (8-4-4-4-
// 12 hexadecimal characters). It is the request-side grammar check for
// object_id and projection_id.
func isCanonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !isHexChar(r) {
				return false
			}
		}
	}
	return true
}

func isHexChar(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
}

// parseContentHash decodes the 64-character lowercase hex content hash into
// its 32 raw bytes (the column is bytea). It returns ErrInvalidContentHash
// for any other shape (wrong length, non-hex, or uppercase).
func parseContentHash(s string) ([]byte, error) {
	if len(s) != 64 {
		return nil, ErrInvalidContentHash
	}
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'F' {
			return nil, ErrInvalidContentHash
		}
	}
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != sha256.Size {
		return nil, ErrInvalidContentHash
	}
	return raw, nil
}

// validateMetadata canonicalizes the metadata object to its bytes and
// checks the size bound. It returns ErrMetadataNotObject for a nil object
// and ErrMetadataTooLarge when the canonical form exceeds the bound.
func validateMetadata(meta map[string]any, maxBytes int) ([]byte, error) {
	if meta == nil {
		return nil, ErrMetadataNotObject
	}
	canonical, err := canonicalJSON(meta)
	if err != nil {
		return nil, ErrMetadataNotObject
	}
	if len(canonical) > maxBytes {
		return nil, ErrMetadataTooLarge
	}
	return canonical, nil
}

// canonicalJSON renders the metadata object as compact JSON (encoding/json
// sorts map[string]any keys, so the form is deterministic for a given
// logical object).
func canonicalJSON(meta map[string]any) ([]byte, error) {
	m, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// validateFilterSet checks the whole filter set against the v1 grammar:
// the set is bounded by MaxFilters, and every entry passes
// validateFilterEntry.
func validateFilterSet(filters []FilterEntry, b Bounds) error {
	if len(filters) > b.MaxFilters {
		return ErrInvalidFilter
	}
	for i := range filters {
		if err := validateFilterEntry(filters[i], b); err != nil {
			return err
		}
	}
	return nil
}

// validateFilterEntry checks one structured filter against the v1 grammar:
// the field is non-empty and at most 128 bytes, the operator is one of the
// two allowed, the value set is non-empty and bounded, and every value is a
// JSON scalar (string, number, or boolean).
func validateFilterEntry(e FilterEntry, b Bounds) error {
	if e.Field == "" || len(e.Field) > 128 {
		return ErrInvalidFilter
	}
	switch e.Op {
	case FilterEq, FilterIn:
	default:
		return ErrInvalidFilter
	}
	if e.Op == FilterEq && len(e.Values) != 1 {
		return ErrInvalidFilter
	}
	if len(e.Values) == 0 {
		return ErrInvalidFilter
	}
	if len(e.Values) > b.MaxFilterValues {
		return ErrInvalidFilter
	}
	for _, v := range e.Values {
		if !isJSONScalar(v) {
			return ErrInvalidFilter
		}
	}
	return nil
}

// isJSONScalar reports whether v is a JSON scalar (string, number, or
// boolean) — the only values a metadata filter can compare.
func isJSONScalar(v any) bool {
	switch v.(type) {
	case string, float64, json.Number, int64, int, bool:
		return true
	default:
		return false
	}
}

// renderScalar renders a JSON scalar as the text form stored compared
// against metadata ->> (the jsonb text representation of the scalar).
func renderScalar(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconvFloat(t)
	case int64:
		return fmt.Sprintf("%d", t)
	case int:
		return fmt.Sprintf("%d", t)
	case json.Number:
		return t.String()
	default:
		return ""
	}
}

// strconvFloat renders a float64 as its shortest decimal string (the same
// form json.Marshal uses for numbers).
func strconvFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "null"
	case math.IsInf(f, 1):
		return "1e+64"
	case math.IsInf(f, -1):
		return "-1e+64"
	default:
		return fmt.Sprintf("%g", f)
	}
}

// validateUpsertBatch checks every record of a batch: required fields, UUID
// grammar, content hash, timestamp, metadata, and vector structure. It
// rejects duplicate (object_id, projection_id) pairs. It does NOT check
// dimensions or zero-norm (those need the resolved space, which the service
// function performs after resolution).
func validateUpsertBatch(records []UpsertRecord, b Bounds) error {
	seen := make(map[[2]string]struct{}, len(records))
	for _, r := range records {
		if r.ObjectID == "" {
			return ErrMissingField
		}
		if r.ProjectionID == "" {
			return ErrMissingField
		}
		if r.ContentHash == "" {
			return ErrMissingField
		}
		// SourceUpdatedAt is optional (docs/API.md). When present it must be
		// a valid RFC 3339 timestamp; when absent the column is set to NULL.
		if r.SourceUpdatedAt != "" {
			if _, err := time.Parse(time.RFC3339, r.SourceUpdatedAt); err != nil {
				return ErrInvalidTimestamp
			}
		}
		// ObjectID and ProjectionID are opaque caller-owned strings per
		// docs/API.md — the service does not enforce a UUID grammar on them.
		if _, err := parseContentHash(r.ContentHash); err != nil {
			return err
		}
		if _, err := validateMetadata(r.Metadata, b.MaxMetadataBytes); err != nil {
			return err
		}
		if len(r.Vector) == 0 {
			return ErrInvalidVector
		}
		key := [2]string{r.ObjectID, r.ProjectionID}
		if _, dup := seen[key]; dup {
			return ErrDuplicateRecord
		}
		seen[key] = struct{}{}
	}
	return nil
}

// validateSearchRequest checks the search request's cheap bounds: the
// namespace key is non-empty, the vector space key is non-empty, the
// embedding is a non-empty vector, the limit is within the bound, and the
// filter set is valid. It does NOT check dimensions or zero-norm (those
// need the resolved space).
func validateSearchRequest(req SearchRequest, b Bounds) error {
	if req.NamespaceKey == "" {
		return ErrMissingField
	}
	if req.VectorSpace == "" {
		return ErrMissingField
	}
	if len(req.Embedding) == 0 {
		return ErrInvalidVector
	}
	maxLimit := b.SearchMaxLimit
	if maxLimit > 200 {
		maxLimit = 200
	}
	if req.Limit < 1 || req.Limit > maxLimit {
		return ErrInvalidLimit
	}
	return validateFilterSet(req.Filters, b)
}

// ---------------------------------------------------------------------------
// SQL builders (pure; unit-tested without a database)
// ---------------------------------------------------------------------------

// metricOperator returns the pgvector distance operator for a metric:
// cosine uses <=>, l2 (euclidean) uses <->, inner_product uses <#>.
func metricOperator(m Metric) string {
	switch m {
	case L2:
		return "<->"
	case InnerProduct:
		return "<#>"
	default:
		return "<=>"
	}
}

// scoreTransform converts a pgvector distance to the metric-specific
// similarity score (higher is better). It returns ok=false when the score
// is non-finite (the caller excludes the match from the response and logs
// a warning).
//
//   - cosine:        1 - distance/2  (distance in [0,2])
//   - l2:            1 / (1 + distance)
//   - inner_product: -distance
func scoreTransform(m Metric, distance float64) (score float64, ok bool) {
	switch m {
	case Cosine:
		s := 1 - distance/2
		if math.IsNaN(s) || math.IsInf(s, 0) {
			return 0, false
		}
		return s, true
	case L2:
		if distance < 0 {
			return 0, false
		}
		s := 1 / (1 + distance)
		if math.IsNaN(s) || math.IsInf(s, 0) {
			return 0, false
		}
		return s, true
	case InnerProduct:
		s := -distance
		if math.IsNaN(s) || math.IsInf(s, 0) {
			return 0, false
		}
		return s, true
	default:
		return 0, false
	}
}

// searchArgs is the ordered argument list for a search statement: the
// embedding text, the space id, the per-filter field/value parameters, and
// the limit.
type searchArgs struct {
	sql  string
	args []any
}

// buildSearchSQL renders the parameterized search statement for a resolved
// space and a validated filter set. It returns the SQL text and the ordered
// argument placeholders (the caller fills the nil slots).
//
// The statement:
//   - casts the embedding parameter to the space's dimensionality
//     (::vector(D)) so the index and the distance operator see the right
//     type;
//   - orders by the metric's distance operator ascending (nearest first);
//   - applies the AND of all filter predicates (each a structured
//     metadata ->> comparison against bound parameters);
//   - limits to the validated count.
//
// The application and namespace scoping come from the RLS policy (the
// transaction-local application context) — the statement names neither an
// application id nor a namespace id, so it cannot be used to escape the
// caller's namespace.
//
// Parameters:
//
//	$1 = embedding (pgvector vector)
//	$2 = space id
//	$3 = application id
//	$4 = namespace id
//	$5.. = filter and limit parameters
//
// The caller is responsible for filling the $3/$4 placeholders with the
// authenticated application UUID and the resolved namespace UUID.
func buildSearchSQL(space *Space, filters []FilterEntry, limit int) (string, []any) {
	op := metricOperator(space.Metric)

	var b strings.Builder
	b.WriteString("SELECT r.id, r.object_id, r.projection_id, r.content_hash, ")
	b.WriteString("r.source_updated_at, r.metadata, ")
	b.WriteString(fmt.Sprintf("(r.embedding::vector(%d) %s $1) AS distance\n", space.Dimensions, op))
	b.WriteString("FROM vector_data.vector_records r\n")
	b.WriteString("WHERE r.vector_space_id = $2\n")
	b.WriteString("  AND r.application_id = $3\n")
	b.WriteString("  AND r.namespace_id = $4\n")

	// Args: [0]=embedding, [1]=space id, [2]=application id, [3]=namespace id
	// (all nil placeholders to be filled by the caller).
	args := []any{nil, nil, nil, nil}
	next := 5 // next 1-based SQL parameter index

	for _, f := range filters {
		b.WriteString("\n  AND (")
		switch f.Op {
		case FilterEq:
			// (r.metadata ->> $field = $value)
			b.WriteString(fmt.Sprintf("(r.metadata ->> $%d = $%d)", next, next+1))
			args = append(args, f.Field, renderScalar(f.Values[0]))
			next += 2
		case FilterIn:
			// (r.metadata ->> $field = ANY($vals))
			// The value set is bound as a []string: pgx encodes it natively
			// as a text[] parameter, so no array literal is ever assembled
			// from caller-controlled text.
			b.WriteString(fmt.Sprintf("(r.metadata ->> $%d = ANY($%d))", next, next+1))
			args = append(args, f.Field, filterValues(f.Values))
			next += 2
		}
		b.WriteString(")")
	}

	b.WriteString(fmt.Sprintf("\nORDER BY distance ASC\nLIMIT $%d", next))
	args = append(args, limit)

	return b.String(), args
}

// filterValues renders the filter's value set to its text forms (the same
// forms compared against metadata ->> (jsonb's text representation of a
// scalar)). The result is bound as a []string: pgx encodes it natively as a
// text[] parameter for the ANY operator, so no array literal is ever
// assembled from caller-controlled text — values containing commas, braces,
// quotes, or backslashes are preserved byte-for-byte.
func filterValues(values []any) []string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = renderScalar(v)
	}
	return parts
}

// upsertStmt is one record's parameterized upsert statement. It is
// idempotent on the 5-tuple identity (application_id, namespace_id,
// object_id, projection_id, vector_space_id): a re-upsert updates the
// content fields in place and never the row id. The row id ($1) is the
// service-generated UUIDv4 for a new record; on conflict it is not touched.
const upsertStmt = `
INSERT INTO vector_data.vector_records
	(id, application_id, namespace_id, object_id, projection_id,
	 vector_space_id, content_hash, source_updated_at, metadata, embedding)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10::vector)
ON CONFLICT (application_id, namespace_id, object_id, projection_id, vector_space_id)
DO UPDATE SET
	content_hash      = EXCLUDED.content_hash,
	source_updated_at = EXCLUDED.source_updated_at,
	metadata          = EXCLUDED.metadata,
	embedding         = EXCLUDED.embedding,
	updated_at        = now()`

// ---------------------------------------------------------------------------
// Service functions
// ---------------------------------------------------------------------------

// Upsert writes a bounded batch of records into the named vector space.
// It runs one WithAppContext transaction: namespace resolution, space
// resolution (write-gated), post-resolution vector checks (dimensions,
// zero-norm), and the batch's upsert statements. The batch is all-or-
// nothing: any failure rolls back the whole batch.
func Upsert(ctx context.Context, pool *pgxpool.Pool, b Bounds, req UpsertRequest) (UpsertResult, error) {
	if req.NamespaceKey == "" {
		return UpsertResult{}, ErrMissingField
	}
	if req.VectorSpace == "" {
		return UpsertResult{}, ErrMissingField
	}
	if len(req.Records) == 0 {
		return UpsertResult{}, ErrMissingField
	}
	if len(req.Records) > b.UpsertMaxRecords {
		return UpsertResult{}, ErrBatchTooLarge
	}
	if err := validateUpsertBatch(req.Records, b); err != nil {
		return UpsertResult{}, err
	}
	if err := namespaces.ValidateKey(req.NamespaceKey); err != nil {
		return UpsertResult{}, err
	}
	if err := ValidateSpaceKey(req.VectorSpace); err != nil {
		return UpsertResult{}, err
	}

	appID := dbctx.AppID(ctx)
	if appID == "" {
		return UpsertResult{}, fmt.Errorf("vectors: no authenticated application in context")
	}

	var result UpsertResult
	err := dbctx.WithAppContext(ctx, pool, appID, func(tx pgx.Tx) error {
		ns, err := namespaces.ResolveInTx(ctx, tx, req.NamespaceKey)
		if err != nil {
			return err
		}
		space, err := ResolveInTx(ctx, tx, req.VectorSpace, true)
		if err != nil {
			return err
		}
		// Post-resolution vector checks: dimensions and zero-norm.
		for _, r := range req.Records {
			if err := CheckDimensions(r.Vector, space.Dimensions); err != nil {
				return err
			}
			if err := CheckZeroNorm(r.Vector, space); err != nil {
				return err
			}
		}
		for _, r := range req.Records {
			recordID, err := newRecordID()
			if err != nil {
				return err
			}
			hashBytes, err := parseContentHash(r.ContentHash)
			if err != nil {
				return err
			}
			metaBytes, err := validateMetadata(r.Metadata, b.MaxMetadataBytes)
			if err != nil {
				return err
			}
			// SourceUpdatedAt is optional: empty string → NULL in the DB.
			var srcAt any
			if r.SourceUpdatedAt != "" {
				srcAt = r.SourceUpdatedAt
			}
			_, err = tx.Exec(ctx, upsertStmt,
				recordID, appID, ns.NamespaceID, r.ObjectID, r.ProjectionID,
				space.ID, hashBytes, srcAt, string(metaBytes),
				EncodeText(r.Vector))
			if err != nil {
				return classifyUpsertError(err)
			}
		}
		result.Upserted = len(req.Records)
		return nil
	})
	if err != nil {
		return UpsertResult{}, err
	}
	return result, nil
}

// classifyUpsertError maps a data-plane upsert failure to a typed error.
// The trigger's defense-in-depth rejections (unknown space, disabled
// space, dimension mismatch) arrive as P0001 (raise_exception); they map
// to the same catalog codes the Go-side checks produce.
func classifyUpsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "P0001":
			// The trigger rejected the row: unknown space, disabled
			// space, or dimension mismatch. The space was just resolved
			// and validated, so the only reachable condition is a
			// dimension mismatch (the trigger re-checks it). Map to the
			// dimension error.
			return ErrInvalidDimensions
		}
	}
	return err
}

// Get returns one record by its logical identity in the named vector space.
// It runs one WithAppContext transaction: namespace resolution, space
// resolution (read-gated: disabled → 422, retired → allowed), then the
// lookup. A missing record is ErrRecordNotFound (404).
func Get(ctx context.Context, pool *pgxpool.Pool, req GetRequest) (GetResult, error) {
	if req.NamespaceKey == "" {
		return GetResult{}, ErrMissingField
	}
	if req.ObjectID == "" {
		return GetResult{}, ErrMissingField
	}
	if req.ProjectionID == "" {
		return GetResult{}, ErrMissingField
	}
	if req.VectorSpace == "" {
		return GetResult{}, ErrMissingField
	}
	// ObjectID and ProjectionID are opaque caller-owned strings per
	// docs/API.md — the service does not enforce a UUID grammar on them.
	if err := namespaces.ValidateKey(req.NamespaceKey); err != nil {
		return GetResult{}, err
	}
	if err := ValidateSpaceKey(req.VectorSpace); err != nil {
		return GetResult{}, err
	}

	appID := dbctx.AppID(ctx)
	if appID == "" {
		return GetResult{}, fmt.Errorf("vectors: no authenticated application in context")
	}

	const getStmt = `
		SELECT r.id, r.object_id, r.projection_id, s.vector_space_key,
		       r.content_hash, r.source_updated_at, r.metadata,
		       r.created_at, r.updated_at
		FROM vector_data.vector_records r
		JOIN vector_control.vector_spaces s ON s.id = r.vector_space_id
		WHERE r.application_id = $1
		  AND r.namespace_id = $2
		  AND r.object_id = $3
		  AND r.projection_id = $4
		  AND r.vector_space_id = $5`

	var (
		res              GetResult
		contentHashBytea []byte
		metaBytea        []byte
		srcUpdatedAt     pgtype.Timestamptz
		createdAt        pgtype.Timestamptz
		updatedAt        pgtype.Timestamptz
	)
	err := dbctx.WithAppContext(ctx, pool, appID, func(tx pgx.Tx) error {
		ns, err := namespaces.ResolveInTx(ctx, tx, req.NamespaceKey)
		if err != nil {
			return err
		}
		space, err := ResolveInTx(ctx, tx, req.VectorSpace, false)
		if err != nil {
			return err
		}
		row := tx.QueryRow(ctx, getStmt,
			appID, ns.NamespaceID, req.ObjectID, req.ProjectionID, space.ID)
		return row.Scan(&res.RecordID, &res.ObjectID, &res.ProjectionID,
			&res.VectorSpace, &contentHashBytea, &srcUpdatedAt, &metaBytea,
			&createdAt, &updatedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GetResult{}, ErrRecordNotFound
		}
		return GetResult{}, err
	}
	// The content hash is bytea (32 raw bytes); render it as 64 lowercase
	// hex.
	res.ContentHash = hex.EncodeToString(contentHashBytea)
	// The metadata is jsonb; decode it into a map.
	if len(metaBytea) > 0 {
		if err := json.Unmarshal(metaBytea, &res.Metadata); err != nil {
			return GetResult{}, fmt.Errorf("decode metadata: %w", err)
		}
	}
	// The timestamps: convert pgtype.Timestamptz to *time.Time (nil when
	// NULL).
	if srcUpdatedAt.Valid {
		res.SourceUpdatedAt = &srcUpdatedAt.Time
	}
	if createdAt.Valid {
		res.CreatedAt = createdAt.Time
	}
	if updatedAt.Valid {
		res.UpdatedAt = updatedAt.Time
	}
	return res, nil
}

// DeleteProjection removes the record for (object, projection) in the named
// vector space. It runs one WithAppContext transaction: namespace
// resolution, space resolution (read-gated: disabled → 422, retired →
// allowed), then the delete. The result is the number of rows removed.
func DeleteProjection(ctx context.Context, pool *pgxpool.Pool, req DeleteRequest) (DeleteResult, error) {
	if req.NamespaceKey == "" {
		return DeleteResult{}, ErrMissingField
	}
	if req.ObjectID == "" {
		return DeleteResult{}, ErrMissingField
	}
	if req.ProjectionID == "" {
		return DeleteResult{}, ErrMissingField
	}
	if req.VectorSpace == "" {
		return DeleteResult{}, ErrMissingField
	}
	// ObjectID and ProjectionID are opaque caller-owned strings per
	// docs/API.md — the service does not enforce a UUID grammar on them.
	if err := namespaces.ValidateKey(req.NamespaceKey); err != nil {
		return DeleteResult{}, err
	}
	if err := ValidateSpaceKey(req.VectorSpace); err != nil {
		return DeleteResult{}, err
	}

	appID := dbctx.AppID(ctx)
	if appID == "" {
		return DeleteResult{}, fmt.Errorf("vectors: no authenticated application in context")
	}

	const deleteStmt = `
		DELETE FROM vector_data.vector_records
		WHERE application_id = $1
		  AND namespace_id = $2
		  AND object_id = $3
		  AND projection_id = $4
		  AND vector_space_id = $5`

	var result DeleteResult
	err := dbctx.WithAppContext(ctx, pool, appID, func(tx pgx.Tx) error {
		ns, err := namespaces.ResolveInTx(ctx, tx, req.NamespaceKey)
		if err != nil {
			return err
		}
		space, err := ResolveInTx(ctx, tx, req.VectorSpace, false)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, deleteStmt,
			appID, ns.NamespaceID, req.ObjectID, req.ProjectionID, space.ID)
		if err != nil {
			return err
		}
		result.Deleted = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return DeleteResult{}, err
	}
	return result, nil
}

// DeleteObject removes every record of the object, across every vector
// space. It runs one WithAppContext transaction: namespace resolution (no
// space resolution — the operation does not name a space), then the delete.
// The result is the number of rows removed.
func DeleteObject(ctx context.Context, pool *pgxpool.Pool, req DeleteRequest) (DeleteResult, error) {
	if req.NamespaceKey == "" {
		return DeleteResult{}, ErrMissingField
	}
	if req.ObjectID == "" {
		return DeleteResult{}, ErrMissingField
	}
	// ObjectID is an opaque caller-owned string per docs/API.md — the
	// service does not enforce a UUID grammar on it.
	if err := namespaces.ValidateKey(req.NamespaceKey); err != nil {
		return DeleteResult{}, err
	}

	appID := dbctx.AppID(ctx)
	if appID == "" {
		return DeleteResult{}, fmt.Errorf("vectors: no authenticated application in context")
	}

	const deleteStmt = `
		DELETE FROM vector_data.vector_records
		WHERE application_id = $1
		  AND namespace_id = $2
		  AND object_id = $3`

	var result DeleteResult
	err := dbctx.WithAppContext(ctx, pool, appID, func(tx pgx.Tx) error {
		ns, err := namespaces.ResolveInTx(ctx, tx, req.NamespaceKey)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, deleteStmt,
			appID, ns.NamespaceID, req.ObjectID)
		if err != nil {
			return err
		}
		result.Deleted = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return DeleteResult{}, err
	}
	return result, nil
}

// Search runs a bounded nearest-neighbor search in the named vector space.
// It runs one WithAppContext transaction: namespace resolution, space
// resolution (read-gated: disabled → 422, retired → allowed), post-
// resolution vector checks (dimensions, zero-norm), then the search.
// Non-finite scores are excluded from the result (and warn-logged).
func Search(ctx context.Context, pool *pgxpool.Pool, b Bounds, req SearchRequest) (SearchResult, error) {
	if err := validateSearchRequest(req, b); err != nil {
		return SearchResult{}, err
	}
	if err := namespaces.ValidateKey(req.NamespaceKey); err != nil {
		return SearchResult{}, err
	}
	if err := ValidateSpaceKey(req.VectorSpace); err != nil {
		return SearchResult{}, err
	}

	appID := dbctx.AppID(ctx)
	if appID == "" {
		return SearchResult{}, fmt.Errorf("vectors: no authenticated application in context")
	}

	var result SearchResult
	err := dbctx.WithAppContext(ctx, pool, appID, func(tx pgx.Tx) error {
		ns, err := namespaces.ResolveInTx(ctx, tx, req.NamespaceKey)
		if err != nil {
			return err
		}
		space, err := ResolveInTx(ctx, tx, req.VectorSpace, false)
		if err != nil {
			return err
		}
		// Post-resolution vector checks: dimensions and zero-norm.
		if err := CheckDimensions(req.Embedding, space.Dimensions); err != nil {
			return err
		}
		if err := CheckZeroNorm(req.Embedding, space); err != nil {
			return err
		}

		sqlText, args := buildSearchSQL(&space, req.Filters, req.Limit)
		args[0] = EncodeText(req.Embedding)
		args[1] = space.ID
		// Bind the authenticated application and resolved namespace so the
		// search is scoped to the caller's namespace. The RLS policy
		// additionally enforces application isolation at the database level.
		args[2] = appID
		args[3] = ns.NamespaceID

		rows, err := tx.Query(ctx, sqlText, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		var matches []Match
		for rows.Next() {
			var (
				m            Match
				contentHash  []byte
				srcUpdatedAt pgtype.Timestamptz
				metaBytea    []byte
			)
			if err := rows.Scan(&m.RecordID, &m.ObjectID, &m.ProjectionID,
				&contentHash, &srcUpdatedAt, &metaBytea, &m.Distance); err != nil {
				return err
			}
			m.ContentHash = hex.EncodeToString(contentHash)
			if srcUpdatedAt.Valid {
				m.SourceUpdatedAt = &srcUpdatedAt.Time
			}
			if len(metaBytea) > 0 {
				_ = json.Unmarshal(metaBytea, &m.Metadata)
			}
			score, ok := scoreTransform(space.Metric, m.Distance)
			if ok {
				m.Score = &score
				matches = append(matches, m)
			}
			// else: non-finite score — exclude from the result.
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if matches == nil {
			matches = []Match{}
		}
		result.Matches = matches
		return nil
	})
	if err != nil {
		return SearchResult{}, err
	}
	return result, nil
}
