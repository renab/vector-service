package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"vector-service/internal/apierr"
	"vector-service/internal/auth"
	"vector-service/internal/dbctx"
	"vector-service/internal/namespaces"
	"vector-service/internal/vectors"
)

// Classify is the single classification boundary of the service (overview
// error model, implementation package 4, section 3). Every failure from the
// service path is converted exactly once, here, into the typed *apierr.Error
// the handler contract returns. The order is normative:
//
//  1. the effective context is no longer active → the shared cause rule
//     (dbctx.Settle), whatever err is: client-initiated → the abort sentinel
//     (no response is rendered); service-initiated → unavailable (503);
//     a `response` outcome (committed status stands) → nil (nothing further
//     is rendered);
//  2. an already-typed *apierr.Error → pass through unchanged;
//  3. the SQLSTATE table (the overview's mapping, applied to
//     *pgconn.PgError and any typed service sentinel the owning flow maps
//     it to);
//  4. the P0001 message-prefix sub-classification (gated on
//     SQLSTATE == "P0001"; any other prefix → internal).
//
// On classifying a raw driver error it emits at most one structured
// diagnostic log record (emitDiagnostic), carrying only the safe allowlist
// fields: the decided catalog code and status, the SQLSTATE, the request
// ID, and — where set — the resolved application/namespace identifiers.
// The raw driver primary message is never among them: it is read only by
// the P0001 prefix gate for classification and never logged, for any
// SQLSTATE.
func Classify(ctx context.Context, err error) *apierr.Error {
	if err == nil {
		return nil
	}
	// Rule 1: the shared cause rule, whatever err is. The outcome of an
	// inactive effective context never depends on err.
	if ctx.Err() != nil {
		settled := dbctx.Settle(ctx)
		if settled == nil {
			// A `response` outcome: the committed status stands; nothing
			// further is rendered.
			return nil
		}
		if dbctx.IsAbort(settled) {
			// Client-initiated: the abort sentinel. Not a catalog code;
			// the renderer writes nothing.
			return abortSentinel
		}
		return apierr.ErrUnavailable
	}

	// Rule 2: already-typed values pass through unchanged.
	var typed *apierr.Error
	if errors.As(err, &typed) {
		return typed
	}

	// Rule 3a: the typed service sentinels — the catalog codes the owning
	// flow knows about.
	if code, ok := serviceErrorCode(err); ok {
		return apierr.New(code)
	}

	// Rule 3b: the SQLSTATE table.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		var code apierr.Code
		switch pgErr.Code {
		case "23505":
			// unique violation
			code = apierr.CodeConflict
		case "P0001":
			// The message-prefix sub-classification (rule 4), gated on the
			// SQLSTATE. The primary message is read ONLY here, for the
			// prefix gate; it is never logged (diagnostic-record policy).
			switch {
			case strings.HasPrefix(pgErr.Message, p0001SpaceNotFoundPrefix):
				code = apierr.CodeVectorSpaceNotFound
			case strings.HasPrefix(pgErr.Message, p0001SpaceDisabledPrefix):
				code = apierr.CodeVectorSpaceUnavailable
			case strings.HasPrefix(pgErr.Message, p0001DimensionMismatchPrefix):
				code = apierr.CodeInvalidVectorDimensions
			default:
				// Any other P0001 prefix: internal.
				code = apierr.CodeInternal
			}
		case "08006", "08001", "08003", "08004", "08007", "58000", "57P01", "57P02", "57014":
			// connection failure / pool exhaustion / acquisition or query
			// timeout (the context is active — rule 1 settled it otherwise)
			// → unavailable.
			code = apierr.CodeUnavailable
		default:
			// 23514 (CHECK), 22P02 (invalid vector — the server quotes
			// the malformed input in its detail; the typed validation
			// catches it first, and the trigger is defense in depth),
			// 23503, 55P03, anything else → the catalog code the owning
			// flow knows about (rule 3a), else internal.
			code = apierr.CodeInternal
		}
		// At most one diagnostic record per classified raw driver error.
		emitDiagnostic(buildDiagnostic(ctx, code, apierr.New(code).Status, pgErr.Code))
		return apierr.New(code)
	}

	// A transport body-cap overflow that bypassed the decode helper
	// (surfacing later in the request path) is still a caller-input
	// condition, not an infrastructure failure.
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return apierr.ErrBodyTooLarge
	}

	// Anything else is an unexpected server error.
	return apierr.ErrInternal
}

// serviceErrorCode maps a typed service sentinel (packages 2 and 3) to its
// catalog code — "the catalog code the owning flow knows about" in the
// SQLSTATE table. The sentinels are errors.New values matched by identity
// (errors.Is), so a wrapped occurrence classifies identically.
func serviceErrorCode(err error) (apierr.Code, bool) {
	switch {
	case errors.Is(err, namespaces.ErrNotFound):
		return apierr.CodeNamespaceNotFound, true
	case errors.Is(err, namespaces.ErrInvalidKey):
		return apierr.CodeInvalidNamespaceKey, true
	case errors.Is(err, namespaces.ErrInvalidApp):
		return apierr.CodeInternal, true
	case errors.Is(err, auth.ErrUnauthorized):
		return apierr.CodeUnauthorized, true
	case auth.IsUnavailable(err):
		return apierr.CodeUnavailable, true
	case isDBUnavailable(err):
		return apierr.CodeUnavailable, true
	case errors.Is(err, dbctx.ErrAbort{}):
		return abortCode, true
	case errors.Is(err, vectors.ErrInvalidVector):
		return apierr.CodeInvalidVector, true
	case errors.Is(err, vectors.ErrNonFiniteVector):
		return apierr.CodeNonFiniteVector, true
	case errors.Is(err, vectors.ErrInvalidDimensions):
		return apierr.CodeInvalidVectorDimensions, true
	case errors.Is(err, vectors.ErrSpaceNotFound):
		return apierr.CodeVectorSpaceNotFound, true
	case errors.Is(err, vectors.ErrSpaceUnavailable):
		return apierr.CodeVectorSpaceUnavailable, true
	case errors.Is(err, vectors.ErrInvalidSpaceKey):
		return apierr.CodeInvalidJSON, true
	case errors.Is(err, vectors.ErrMissingField):
		return apierr.CodeMissingField, true
	case errors.Is(err, vectors.ErrInvalidContentHash):
		return apierr.CodeInvalidContentHash, true
	case errors.Is(err, vectors.ErrMetadataNotObject):
		return apierr.CodeMetadataNotObject, true
	case errors.Is(err, vectors.ErrMetadataTooLarge):
		return apierr.CodeMetadataTooLarge, true
	case errors.Is(err, vectors.ErrInvalidTimestamp):
		return apierr.CodeInvalidTimestamp, true
	case errors.Is(err, vectors.ErrDuplicateRecord):
		return apierr.CodeDuplicateRecord, true
	case errors.Is(err, vectors.ErrBatchTooLarge):
		return apierr.CodeBatchTooLarge, true
	case errors.Is(err, vectors.ErrInvalidLimit):
		return apierr.CodeInvalidLimit, true
	case errors.Is(err, vectors.ErrInvalidFilter):
		return apierr.CodeInvalidFilter, true
	case errors.Is(err, vectors.ErrInvalidUUID):
		return apierr.CodeInvalidUUID, true
	case errors.Is(err, vectors.ErrRecordNotFound):
		return apierr.CodeRecordNotFound, true
	}
	return "", false
}

// isDBUnavailable reports whether err is the dbctx package's typed
// infrastructure outcome (*dbctx.Unavailable, 503). It is distinct from
// auth.IsUnavailable (the auth package's own 503) and from the abort
// sentinel; both settle to CodeUnavailable, but they are different typed
// outcomes returned by different helper packages. A wrapped occurrence
// (e.g. attached to a rollback failure) classifies identically.
func isDBUnavailable(err error) bool {
	var u *dbctx.Unavailable
	return errors.As(err, &u)
}

// abortCode is a sentinel code standing in for the abort outcome. The
// renderer (Render) settles the abort by writing nothing; the code never
// appears in a response body. It is deliberately not a catalog code
// (overview error model: the abort sentinel is not a catalog code) so the
// closed catalog cannot carry a non-catalog value.
const abortCode apierr.Code = "abort"

// P0001 trigger prefixes (the vector_records_validate trigger in migration
// 0001). They are the classification input of rule 4 and are never logged.
const (
	p0001SpaceNotFoundPrefix     = "unknown vector_space_id: "
	p0001SpaceDisabledPrefix     = "vector space is disabled: "
	p0001DimensionMismatchPrefix = "embedding dimension mismatch: "
)

// abortSentinel is the typed abort outcome the classifier returns for a
// client-initiated settlement. Render writes nothing for it. It is not a
// catalog code (overview error model): it carries no status or message and
// is identified by identity, not by code.
var abortSentinel = &apierr.Error{Code: abortCode}
