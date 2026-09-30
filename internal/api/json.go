package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"vector-service/internal/apierr"
)

// DecodeJSON is the strict JSON body decoder (implementation package 4,
// section 1): a body endpoint calls it as the handler's first action,
// decoding the request body into the handler's request struct. It enforces
// the strict grammar:
//
//   - the body must be a well-formed JSON document (else CodeInvalidJSON);
//   - the document must be exactly one value — trailing content after it
//     is rejected (CodeInvalidJSON);
//   - unknown fields are rejected (CodeUnknownField): the decoder runs with
//     DisallowUnknownFields;
//   - a field with the wrong JSON type is rejected (CodeInvalidJSON): the
//     body is not the shape the endpoint expects;
//   - the body must be bounded: the decode stage (section 2,
//     content-type/body-cap stage) wraps r.Body in an http.MaxBytesReader
//     at VEC_HTTP_MAX_BODY_BYTES. An overflow surfaces as a
//     *http.MaxBytesError from a read and is classified CodeBodyTooLarge
//     (413), only while the request context is active.
//
// The decoded struct is handler-local; it never arrives in HandlerInput.
// The returned *apierr.Error is nil on success and the handler's typed
// failure on a grammar or overflow condition.
func DecodeJSON(r *http.Request, dst any) *apierr.Error {
	if r.Body == nil {
		return apierr.New(apierr.CodeInvalidJSON)
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	// Decode exactly one JSON value into dst.
	if err := dec.Decode(dst); err != nil {
		if mbErr, ok := err.(*http.MaxBytesError); ok {
			// The body crossed the MaxBytesReader cap. The overflow is a
			// caller-input condition (the body is over the bound); the
			// context-active gate is applied by the caller — an overflow
			// on an inactive context settles by the shared cause rule.
			_ = mbErr
			return apierr.New(apierr.CodeBodyTooLarge)
		}
		// Strict decoding: an unrecognized field is rejected by the
		// DisallowUnknownFields decoder. The error is a plain errorString
		// (not a *json.SyntaxError) whose message is `json: unknown field
		// "<name>"`. It is a distinct catalog code (CodeUnknownField) from
		// malformed JSON; the field name is never echoed in the response
		// (only the stable message). The "unknown field" prefix is unique
		// to DisallowUnknownFields — no other stdlib decoder path or
		// caller-supplied key can produce it.
		if strings.HasPrefix(err.Error(), "json: unknown field") {
			return apierr.New(apierr.CodeUnknownField)
		}
		switch err.(type) {
		case *json.SyntaxError, *json.UnmarshalTypeError:
			return apierr.New(apierr.CodeInvalidJSON)
		case *json.InvalidUnmarshalError:
			// dst is not a valid decode target: a programming error, not
			// a request failure.
			return apierr.New(apierr.CodeInternal)
		default:
			// Any other decode error (an io error from the capped body
			// that is not a MaxBytesError, or a non-typed json error) is
			// a malformed body.
			return apierr.New(apierr.CodeInvalidJSON)
		}
	}

	// Trailing content after the document is rejected: the body must be
	// exactly one JSON value.
	if dec.More() {
		return apierr.New(apierr.CodeInvalidJSON)
	}
	return nil
}

// CheckContentType enforces the JSON-endpoint content-type rule
// (implementation package 4, section 2, content-type stage): the
// Content-Type must be application/json (a charset parameter is allowed).
// Any other type — or a missing header — yields CodeInvalidContentType
// (400). This is called after auth, so an unauthenticated request with a
// bad content type receives 401 (auth fails first), not 400.
func CheckContentType(r *http.Request) *apierr.Error {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return apierr.New(apierr.CodeInvalidContentType)
	}
	// Split off any parameters (charset, etc.). The media type is the
	// token before the first ';', trimmed of surrounding whitespace and
	// lowercased for the comparison.
	media := ct
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		media = ct[:i]
	}
	media = strings.ToLower(strings.TrimSpace(media))
	if media != "application/json" {
		return apierr.New(apierr.CodeInvalidContentType)
	}
	return nil
}

// CapBody wraps the request body in an http.MaxBytesReader at maxBytes
// (implementation package 4, section 2, body-cap stage). The cap mechanism
// writes nothing; an overflow surfaces as a *http.MaxBytesError from a
// later read and is classified CodeBodyTooLarge (413) by DecodeJSON.
// maxBytes is VEC_HTTP_MAX_BODY_BYTES.
func CapBody(r *http.Request, maxBytes int64) {
	if r.Body != nil {
		r.Body = http.MaxBytesReader(nil, r.Body, maxBytes)
	}
}
