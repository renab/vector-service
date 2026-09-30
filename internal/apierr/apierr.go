// Package apierr is the stable error catalog of the Vector Service's HTTP
// API (implementation package 4, section 3; the v1 catalog of the
// implementation overview). It defines the typed error the handler contract
// returns, the closed set of stable catalog codes with their fixed HTTP
// statuses and fixed messages, and the construction helpers the handler
// boundary and the classifier use.
//
// The catalog is the complete set of codes that can appear in a response
// body (overview invariant 11: "Stable errors"). Every code has exactly one
// stable message and exactly one status; a message never interpolates
// caller input, path values, or driver text. The abort sentinel (client
// cancellation settlement) is NOT a catalog code (overview error model):
// it is carried by the dbctx abort outcome and never rendered to a response.
//
// This package does no classification. The single classification boundary
// is api.Classify (implementation package 4, section 3); this package only
// holds the catalog and the typed error it produces.
package apierr

import "net/http"

// Code is a stable machine-readable error code from the v1 catalog. It is
// the only code that can appear in a response body.
type Code string

// The v1 catalog: every stable code, its fixed HTTP status, and its fixed
// message. This is the complete set that can appear in a response body
// (overview, "Error code catalog (v1)").
const (
	// CodeUnauthorized: missing/unknown/invalid/expired/disabled
	// application credential, disabled application, or missing/invalid
	// admin token. Uniform — no enumeration of failure kinds.
	CodeUnauthorized Code = "unauthorized"

	// CodeInvalidJSON: malformed JSON body or trailing content after the
	// document.
	CodeInvalidJSON Code = "invalid_json"

	// CodeUnknownField: unrecognized request field (strict decoding).
	CodeUnknownField Code = "unknown_field"

	// CodeInvalidContentType: Content-Type not application/json on a JSON
	// endpoint.
	CodeInvalidContentType Code = "invalid_content_type"

	// CodeMissingField: required field absent (including an empty records
	// array).
	CodeMissingField Code = "missing_field"

	// CodeInvalidApplicationKey: application key fails
	// ^[a-z][a-z0-9_-]{0,63}$ (mirrors the schema constraint).
	CodeInvalidApplicationKey Code = "invalid_application_key"

	// CodeInvalidNamespaceKey: namespace key empty, contains / or control
	// characters, or too long.
	CodeInvalidNamespaceKey Code = "invalid_namespace_key"

	// CodeInvalidLimit: search limit outside 1..min(200, configured max)
	// or not an integer.
	CodeInvalidLimit Code = "invalid_limit"

	// CodeInvalidUUID: path/parameter value is not a valid UUID.
	CodeInvalidUUID Code = "invalid_uuid"

	// CodeInvalidTimestamp: not a valid RFC 3339 timestamp.
	CodeInvalidTimestamp Code = "invalid_timestamp"

	// CodeInvalidContentHash: not 64 lowercase hex characters.
	CodeInvalidContentHash Code = "invalid_content_hash"

	// CodeInvalidVector: vector is not a JSON array of numbers.
	CodeInvalidVector Code = "invalid_vector"

	// CodeNonFiniteVector: vector element is NaN or ±Infinity; zero-norm
	// vector in a cosine space (upsert or search).
	CodeNonFiniteVector Code = "non_finite_vector"

	// CodeInvalidVectorDimensions: vector length differs from the vector
	// space's dimensions.
	CodeInvalidVectorDimensions Code = "invalid_vector_dimensions"

	// CodeVectorSpaceNotFound: unknown vector_space key.
	CodeVectorSpaceNotFound Code = "vector_space_not_found"

	// CodeVectorSpaceUnavailable: disabled vector space (any operation that
	// names it) or retired vector space (upsert only).
	CodeVectorSpaceUnavailable Code = "vector_space_unavailable"

	// CodeNamespaceNotFound: namespace missing, disabled, or not owned by
	// caller (uniform, data plane).
	CodeNamespaceNotFound Code = "namespace_not_found"

	// CodeRecordNotFound: record not visible in the requested namespace.
	CodeRecordNotFound Code = "record_not_found"

	// CodeApplicationNotFound: admin plane: valid-grammar key with no
	// registered application.
	CodeApplicationNotFound Code = "application_not_found"

	// CodeCredentialNotFound: admin plane: credential ID unknown or not
	// owned by the named application.
	CodeCredentialNotFound Code = "credential_not_found"

	// CodeNotFound: unknown route (no matching API path).
	CodeNotFound Code = "not_found"

	// CodeMethodNotAllowed: known path, unsupported HTTP method.
	CodeMethodNotAllowed Code = "method_not_allowed"

	// CodeConflict: duplicate application key, namespace key, or
	// credential name.
	CodeConflict Code = "conflict"

	// CodeDuplicateRecord: upsert batch repeats the same (object_id,
	// projection_id); rejected before any write.
	CodeDuplicateRecord Code = "duplicate_record"

	// CodeBatchTooLarge: upsert batch exceeds the configured maximum.
	CodeBatchTooLarge Code = "batch_too_large"

	// CodeBodyTooLarge: HTTP body exceeds the configured maximum.
	CodeBodyTooLarge Code = "body_too_large"

	// CodeMetadataNotObject: metadata is not a JSON object.
	CodeMetadataNotObject Code = "metadata_not_object"

	// CodeMetadataTooLarge: metadata exceeds the configured size.
	CodeMetadataTooLarge Code = "metadata_too_large"

	// CodeInvalidFilter: unsupported operator, bad field grammar,
	// non-string value, or filter limit exceeded.
	CodeInvalidFilter Code = "invalid_filter"

	// CodeUnavailable: database unavailable, migrations incomplete, or
	// request abandoned by deadline/shutdown while the connection was still
	// usable.
	CodeUnavailable Code = "unavailable"

	// CodeInternal: unexpected server error.
	CodeInternal Code = "internal"
)

// entry is one row of the catalog: the stable message and fixed status for
// a code.
type entry struct {
	status  int
	message string
}

// catalog is the complete v1 error-code catalog: code → (fixed status,
// stable message). It is the single source for both the classifier and the
// renderer (one classification boundary, one rendering site — package 4,
// section 3), so a code can never drift between the two.
var catalog = map[Code]entry{
	CodeUnauthorized:            {http.StatusUnauthorized, "unauthorized"},
	CodeInvalidJSON:             {http.StatusBadRequest, "malformed JSON body"},
	CodeUnknownField:            {http.StatusBadRequest, "unrecognized request field"},
	CodeInvalidContentType:      {http.StatusBadRequest, "content type must be application/json"},
	CodeMissingField:            {http.StatusBadRequest, "required field is missing"},
	CodeInvalidApplicationKey:   {http.StatusBadRequest, "application key is invalid"},
	CodeInvalidNamespaceKey:     {http.StatusBadRequest, "namespace key is invalid"},
	CodeInvalidLimit:            {http.StatusBadRequest, "limit is invalid"},
	CodeInvalidUUID:             {http.StatusBadRequest, "value is not a valid UUID"},
	CodeInvalidTimestamp:        {http.StatusBadRequest, "value is not a valid RFC 3339 timestamp"},
	CodeInvalidContentHash:      {http.StatusBadRequest, "content hash is invalid"},
	CodeInvalidVector:           {http.StatusBadRequest, "vector is not a JSON array of numbers"},
	CodeNonFiniteVector:         {http.StatusUnprocessableEntity, "vector contains a non-finite element"},
	CodeInvalidVectorDimensions: {http.StatusUnprocessableEntity, "vector dimensions do not match the vector space"},
	CodeVectorSpaceNotFound:     {http.StatusNotFound, "vector space not found"},
	CodeVectorSpaceUnavailable:  {http.StatusUnprocessableEntity, "vector space is unavailable"},
	CodeNamespaceNotFound:       {http.StatusNotFound, "namespace not found"},
	CodeRecordNotFound:          {http.StatusNotFound, "record not found"},
	CodeApplicationNotFound:     {http.StatusNotFound, "application not found"},
	CodeCredentialNotFound:      {http.StatusNotFound, "credential not found"},
	CodeNotFound:                {http.StatusNotFound, "not found"},
	CodeMethodNotAllowed:        {http.StatusMethodNotAllowed, "method not allowed"},
	CodeConflict:                {http.StatusConflict, "conflict"},
	CodeDuplicateRecord:         {http.StatusBadRequest, "batch contains a duplicate record"},
	CodeBatchTooLarge:           {http.StatusRequestEntityTooLarge, "batch exceeds the maximum record count"},
	CodeBodyTooLarge:            {http.StatusRequestEntityTooLarge, "request body exceeds the maximum size"},
	CodeMetadataNotObject:       {http.StatusBadRequest, "metadata is not a JSON object"},
	CodeMetadataTooLarge:        {http.StatusBadRequest, "metadata exceeds the maximum size"},
	CodeInvalidFilter:           {http.StatusBadRequest, "metadata filter is invalid"},
	CodeUnavailable:             {http.StatusServiceUnavailable, "service unavailable"},
	CodeInternal:                {http.StatusInternalServerError, "internal server error"},
}

// Status returns the fixed HTTP status for c. A code outside the catalog
// has no status and panics: the catalog is closed and every construction
// helper takes a catalog code, so an unknown code is a programming error,
// not a recoverable failure.
func (c Code) Status() int {
	e, ok := catalog[c]
	if !ok {
		panic("apierr: unknown code: " + string(c))
	}
	return e.status
}

// Message returns the stable message for c (never interpolating caller
// input). An unknown code panics (see Status).
func (c Code) Message() string {
	e, ok := catalog[c]
	if !ok {
		panic("apierr: unknown code: " + string(c))
	}
	return e.message
}

// CodeNames returns the complete closed set of catalog codes. It exists so
// the catalog test can pin completeness and so a tool can enumerate the
// response codes without re-deriving them.
func CodeNames() []Code {
	names := make([]Code, 0, len(catalog))
	for c := range catalog {
		names = append(names, c)
	}
	return names
}

// Error is the single typed error value of the handler contract
// (implementation package 4, section 1). A handler returns *Error on
// failure and nil on success; the concrete pointer type means a raw driver
// error cannot cross the handler boundary by construction (returning one
// does not compile).
//
// It carries exactly the catalog triple: a stable code, its fixed status,
// and its stable message. The request ID is not stored on the value: it
// comes from the request context (dbctx.RequestID) at render time, so the
// same typed error renders the same request's ID whether returned by a
// handler or by the settlement finalization.
type Error struct {
	Code    Code
	Status  int
	Message string
}

// New builds a typed error from a catalog code, filling the fixed status
// and stable message from the catalog.
func New(c Code) *Error {
	return &Error{Code: c, Status: c.Status(), Message: c.Message()}
}

// Error implements the error interface. The stable catalog message is
// returned, never caller- or driver-derived text.
func (e *Error) Error() string { return e.Message }

// Is reports whether t is a typed error with the same catalog code. It
// makes the typed error identity-joinable (errors.Is) for callers that
// need to test for a specific catalog code through a wrapped chain.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// Sentinel values for the catalog codes the classifier and the handler
// boundary produce most often. They are catalog-carrying *Error values
// (not bare errors.New sentinels) so they participate in the typed return
// contract and in errors.Is code matching.
var (
	// ErrUnauthorized is the uniform authentication failure (401).
	ErrUnauthorized = New(CodeUnauthorized)

	// ErrInvalidJSON is the malformed-JSON failure (400).
	ErrInvalidJSON = New(CodeInvalidJSON)

	// ErrUnknownField is the unrecognized-field failure (400).
	ErrUnknownField = New(CodeUnknownField)

	// ErrInvalidContentType is the content-type failure (400).
	ErrInvalidContentType = New(CodeInvalidContentType)

	// ErrMissingField is the missing-required-field failure (400).
	ErrMissingField = New(CodeMissingField)

	// ErrInvalidApplicationKey is the application-key failure (400).
	ErrInvalidApplicationKey = New(CodeInvalidApplicationKey)

	// ErrInvalidNamespaceKey is the namespace-key failure (400).
	ErrInvalidNamespaceKey = New(CodeInvalidNamespaceKey)

	// ErrInvalidLimit is the limit failure (400).
	ErrInvalidLimit = New(CodeInvalidLimit)

	// ErrInvalidUUID is the UUID failure (400).
	ErrInvalidUUID = New(CodeInvalidUUID)

	// ErrInvalidTimestamp is the timestamp failure (400).
	ErrInvalidTimestamp = New(CodeInvalidTimestamp)

	// ErrInvalidContentHash is the content-hash failure (400).
	ErrInvalidContentHash = New(CodeInvalidContentHash)

	// ErrInvalidVector is the vector-structure failure (400).
	ErrInvalidVector = New(CodeInvalidVector)

	// ErrNonFiniteVector is the non-finite-vector failure (422).
	ErrNonFiniteVector = New(CodeNonFiniteVector)

	// ErrInvalidVectorDimensions is the dimension-mismatch failure (422).
	ErrInvalidVectorDimensions = New(CodeInvalidVectorDimensions)

	// ErrVectorSpaceNotFound is the unknown-space failure (404).
	ErrVectorSpaceNotFound = New(CodeVectorSpaceNotFound)

	// ErrVectorSpaceUnavailable is the unavailable-space failure (422).
	ErrVectorSpaceUnavailable = New(CodeVectorSpaceUnavailable)

	// ErrNamespaceNotFound is the uniform namespace-resolution failure
	// (404).
	ErrNamespaceNotFound = New(CodeNamespaceNotFound)

	// ErrRecordNotFound is the unknown-record failure (404).
	ErrRecordNotFound = New(CodeRecordNotFound)

	// ErrApplicationNotFound is the admin-plane unknown-application
	// failure (404).
	ErrApplicationNotFound = New(CodeApplicationNotFound)

	// ErrCredentialNotFound is the admin-plane unknown-credential failure
	// (404).
	ErrCredentialNotFound = New(CodeCredentialNotFound)

	// ErrNotFound is the unknown-route failure (404).
	ErrNotFound = New(CodeNotFound)

	// ErrMethodNotAllowed is the wrong-method failure (405).
	ErrMethodNotAllowed = New(CodeMethodNotAllowed)

	// ErrConflict is the duplicate-key failure (409).
	ErrConflict = New(CodeConflict)

	// ErrDuplicateRecord is the duplicate-record failure (400).
	ErrDuplicateRecord = New(CodeDuplicateRecord)

	// ErrBatchTooLarge is the oversized-batch failure (413).
	ErrBatchTooLarge = New(CodeBatchTooLarge)

	// ErrBodyTooLarge is the oversized-body failure (413).
	ErrBodyTooLarge = New(CodeBodyTooLarge)

	// ErrMetadataNotObject is the non-object-metadata failure (400).
	ErrMetadataNotObject = New(CodeMetadataNotObject)

	// ErrMetadataTooLarge is the oversized-metadata failure (400).
	ErrMetadataTooLarge = New(CodeMetadataTooLarge)

	// ErrInvalidFilter is the invalid-filter failure (400).
	ErrInvalidFilter = New(CodeInvalidFilter)

	// ErrUnavailable is the database-infrastructure failure (503).
	ErrUnavailable = New(CodeUnavailable)

	// ErrInternal is the unexpected-server-error failure (500).
	ErrInternal = New(CodeInternal)
)
