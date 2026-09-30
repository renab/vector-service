// Package vectors is the shared vector-validation and vector-space-resolution
// core of implementation package 3: the structural validation of caller
// vectors (JSON array of numbers, finite elements, float32 representability),
// the post-resolution checks (dimensionality against the resolved space, the
// deterministic zero-norm rule for cosine spaces), the pgvector text binding
// representation, and the in-transaction vector-space resolution with its
// state gate.
//
// The package sits below the HTTP handler boundary (overview error model):
// every function returns plain error values (typed sentinels, like
// namespaces.ErrNotFound). The package-4 handler is the single point that
// classifies them into catalog codes:
//
//	ErrInvalidVector        -> 400  invalid_vector
//	ErrNonFiniteVector      -> 422  non_finite_vector
//	ErrInvalidDimensions    -> 422  invalid_vector_dimensions
//	ErrSpaceNotFound        -> 404  vector_space_not_found
//	ErrSpaceUnavailable     -> 422  vector_space_unavailable
//
// Validation runs in Go before any data-plane SQL (package 3, section 2).
// The dimension check and the zero-norm check run after space resolution
// because they need the resolved space's dimensions and distance metric.
// The trigger in migration 0001 re-enforces the same conditions at write
// time (defense in depth); its P0001 rejections map to the same catalog
// codes.
package vectors

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ErrInvalidVector is the typed failure for a vector that is not a JSON
// array of numbers (package 3, section 2: "must be a JSON array of numbers
// -> else 400 invalid_vector"). An empty array is structurally valid: it
// fails the dimension check instead (every space has dimensions > 0).
var ErrInvalidVector = errors.New("invalid vector")

// ErrNonFiniteVector is the typed failure for a vector with a non-finite
// element (NaN or ±Infinity), or for an all-zero vector in a resolved
// cosine space (the zero-norm rule; package 3, section 2). Both map to 422
// non_finite_vector: cosine similarity is undefined at zero norm, and
// pgvector's cosine operator produces a non-finite distance against a zero
// vector.
var ErrNonFiniteVector = errors.New("non-finite vector")

// ErrInvalidDimensions is the typed failure for a vector whose length
// differs from the resolved space's dimensions (422
// invalid_vector_dimensions).
var ErrInvalidDimensions = errors.New("invalid vector dimensions")

// ErrSpaceNotFound is the typed failure for an unknown vector-space key
// (404 vector_space_not_found).
var ErrSpaceNotFound = errors.New("vector space not found")

// ErrSpaceUnavailable is the typed failure for a named space in a state
// that does not allow the operation: a disabled space (every operation
// that names it) or a retired space (upsert only) (422
// vector_space_unavailable).
var ErrSpaceUnavailable = errors.New("vector space unavailable")

// ParseVector validates the structural rules of a caller vector and converts
// it to float32 (the precision pgvector's text form carries). It is the
// first, database-free half of the shared validation (package 3, section 2):
//
//   - the input must be valid JSON, else the decoder's *json.SyntaxError;
//   - the top-level value must be a JSON array, else ErrInvalidVector;
//   - every element must be a JSON number, else ErrInvalidVector;
//   - every element must be finite and float32-representable, else
//     ErrNonFiniteVector.
//
// Decoding into []any (rather than []float64) is deliberate: encoding/json
// silently maps null to the zero value for typed slice elements, so a
// typed decode would accept [1,null] as [1,0]. Decoding into []any
// preserves the null as a nil interface element that we reject explicitly.
//
// Values that are not representable as float32 (for example a float64 that
// overflows the float32 range) round to ±Inf and are rejected by the
// finite rule with ErrNonFiniteVector.
func ParseVector(raw []byte) ([]float32, error) {
	// Decode to any: the decoder reports *json.SyntaxError for malformed
	// input and a Go value for well-formed input (nil for bare null,
	// float64 for numbers, []any for arrays, etc.).
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		if _, ok := err.(*json.SyntaxError); ok {
			return nil, err
		}
		return nil, fmt.Errorf("vectors: %w: not a JSON array of numbers", ErrInvalidVector)
	}
	// Top-level must be an array. A bare null decodes to a nil interface
	// value; a non-array JSON value (number, string, bool, object) decodes
	// to a non-nil, non-[]any value. Both are invalid_vector.
	arr, ok := doc.([]any)
	if !ok {
		return nil, fmt.Errorf("vectors: %w: not a JSON array of numbers", ErrInvalidVector)
	}
	// Each element must be a JSON number. encoding/json decodes JSON
	// numbers into float64 within an any. null decodes to a nil interface
	// element; strings, booleans, objects, and nested arrays decode to
	// their respective types. All non-float64 elements are invalid_vector.
	elems := make([]float64, len(arr))
	for i, el := range arr {
		f, ok := el.(float64)
		if !ok {
			return nil, fmt.Errorf("vectors: %w: not a JSON array of numbers", ErrInvalidVector)
		}
		elems[i] = f
	}
	out := make([]float32, len(elems))
	for i, e := range elems {
		if math.IsNaN(e) || math.IsInf(e, 0) {
			return nil, ErrNonFiniteVector
		}
		v := float32(e)
		if math.IsInf(float64(v), 0) {
			// The float64 input was finite but not representable as
			// float32: the conversion rounded it to ±Inf. The non-finite
			// outcome (section 2: every element must be representable as
			// float32 — the values travel in single precision).
			return nil, ErrNonFiniteVector
		}
		out[i] = v
	}
	return out, nil
}

// CheckDimensions rejects a vector whose length differs from the resolved
// space's dimensions (422 invalid_vector_dimensions). It runs after space
// resolution, which supplies the resolved dimensionality.
func CheckDimensions(v []float32, dimensions int) error {
	if len(v) != dimensions {
		return fmt.Errorf("vectors: %w: vector length %d does not match space dimensions %d",
			ErrInvalidDimensions, len(v), dimensions)
	}
	return nil
}

// IsZero reports the deterministic zero-norm test: every element of v is
// exactly 0.0 (after float32 conversion). There is no epsilon and no norm
// computation (package 3, section 2). Callers apply it only to the vectors
// of resolved cosine spaces.
func IsZero(v []float32) bool {
	for _, c := range v {
		if c != 0 {
			return false
		}
	}
	return true
}

// CheckZeroNorm enforces the zero-norm rule: in a resolved cosine space, an
// all-zero vector is rejected with ErrNonFiniteVector (422
// non_finite_vector). l2 and inner_product spaces accept zero vectors
// unchanged: IsCosine is false, so the rule does not apply.
//
// The rejection happens in Go before any data-plane SQL runs — for both
// upsert records' vectors and the search query vector (package 3, section 2
// and invariants).
func CheckZeroNorm(v []float32, space Space) error {
	if space.Metric == Cosine && IsZero(v) {
		return ErrNonFiniteVector
	}
	return nil
}

// EncodeText renders a float32 slice as pgvector's text form "[a,b,c,...]"
// (single precision) — the binding representation the package-1 text codec
// sends for every embedding parameter (package 3, sections 3 and 7:
// "embedding is bound through the package-1 text codec"; "$q is bound
// through the text codec").
//
// Components are rendered with the shortest decimal string that round-trips
// to the identical float32 bit pattern (the lossless float64 promotion of
// the float32, %g precision -1). The production codec never sees a NaN or
// ±Inf: ParseVector rejects non-finite elements before any encoding, so no
// non-finite token is rendered.
func EncodeText(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, c := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(c), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
