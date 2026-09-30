package api

import (
	"context"
	"log/slog"

	"vector-service/internal/apierr"
	"vector-service/internal/dbctx"
	"vector-service/internal/namespaces"
)

// Diagnostic-record policy (implementation package 4, section 3,
// "Diagnostic-record policy (the safe allowlist)").
//
// When Classify classifies a raw PostgreSQL driver error it emits at most
// one structured diagnostic log record. That record may contain only
// fields from this closed set:
//
//   - always present: the decided catalog code and status, the SQLSTATE,
//     and the request ID;
//   - where permitted: the application and namespace identifiers already
//     resolved on the per-request log state (the application identity
//     carried on the context by authentication, and the resolved namespace
//     carried on the context by resolution);
//   - only when explicitly allowlisted: a bounded, server-authored
//     `detail` value (see detailAllowlist below).
//
// The following are never present in a diagnostic record, in whole or in
// part: the driver's primary message or any message-derived text; SQLSTATE
// 22P02 detail (server-authored text that quotes the malformed vector
// input — caller data); P0001 trigger text (which embeds vector_space_id
// values; it is a classification input for the prefix gate, not a log
// input); arbitrary metadata; complete or partial vectors; credentials;
// credential digests; document/source content; and any other
// caller-controlled value.
//
// Suppression is at the log boundary only: the P0001 prefix gate still
// classifies from the message, and the rendered response keeps its stable
// catalog code and message.

// detailEntry is one row of the diagnostic-record detail allowlist. Each
// entry names its server-side source, carries a length bound, and must
// provably be incapable of carrying caller data. The allowlist is an
// explicit closed set in the diagnostic-record builder; adding an entry is
// a reviewed decision, not an implementation convenience.
type detailEntry struct {
	name  string // the slog field name for the value
	bound int    // the length bound in bytes (truncation length)
}

// detailAllowlist is the closed set of server-authored detail values the
// diagnostic record may carry. It is currently empty — no detail value is
// emitted today (package 4, section 3). Tests register a synthetic entry
// to exercise the allowlist mechanism (truncation of an over-long value,
// dropping of a non-allowlisted value) and remove it afterwards; production
// code never mutates the map.
var detailAllowlist = map[string]detailEntry{}

// RegisterDetail is a test seam: it registers a synthetic detail allowlist
// entry so a test can exercise the allowlist mechanism (truncation of an
// over-long value, dropping of a non-allowlisted value). It returns a
// function that removes the entry. Production code does not call it.
func RegisterDetail(name string, bound int) (unregister func()) {
	detailAllowlist[name] = detailEntry{name: name, bound: bound}
	return func() { delete(detailAllowlist, name) }
}

// allowedDetailValues returns the allowlisted detail values among the
// candidates: a candidate whose field name is on the allowlist is kept,
// truncated to its bound; a candidate whose name is not on the allowlist
// is dropped (not truncated). The result is the set of slog.Attr the
// diagnostic record may carry as detail.
func allowedDetailValues(candidates ...slog.Attr) []slog.Attr {
	var out []slog.Attr
	for _, a := range candidates {
		e, ok := detailAllowlist[a.Key]
		if !ok {
			// Not on the allowlist: dropped, not truncated.
			continue
		}
		v := a.Value.String()
		if len(v) > e.bound {
			v = v[:e.bound]
		}
		out = append(out, slog.String(a.Key, v))
	}
	return out
}

// diagnosticRecord is the complete field set of one diagnostic record. It
// exists so the test can assert the allowlist holds: every field the
// classifier builds is checked to be in the safe set, and no field value
// equals or contains the raw primary message, the P0001 trigger text, or
// any other supplied payload.
type diagnosticRecord struct {
	Code        string
	Status      int
	SQLSTATE    string
	RequestID   string
	Application string
	Namespace   string
	Detail      []slog.Attr
}

// buildDiagnostic assembles the diagnostic record for one classified raw
// driver error. The application and namespace identifiers are read through
// the packages' public context helpers (dbctx.AppID and
// namespaces.Namespace) — both are "already resolved on the per-request
// log state" by the time the classifier runs (authentication and namespace
// resolution precede the business SQL that produced the error). Neither
// the driver's primary message nor any of its detail fields is part of the
// record.
func buildDiagnostic(ctx context.Context, code apierr.Code, status int, sqlstate string) *diagnosticRecord {
	rec := &diagnosticRecord{
		Code:      string(code),
		Status:    status,
		SQLSTATE:  sqlstate,
		RequestID: dbctx.RequestID(ctx),
	}
	if appID := dbctx.AppID(ctx); appID != "" {
		rec.Application = appID
	}
	if ns, ok := namespaces.Namespace(ctx); ok && ns.NamespaceID != "" {
		rec.Namespace = ns.NamespaceID
	}
	// No detail value is emitted today: the allowlist is empty by
	// construction, so the detail field is always nil in production.
	rec.Detail = allowedDetailValues()
	return rec
}

// emitDiagnostic writes the single structured log record for a classified
// raw driver error (slog.Warn). It is called at most once per classified
// raw driver error, by Classify. The record carries only the safe
// allowlist fields built by buildDiagnostic — never the driver's primary
// message, the 22P02 detail, or the P0001 trigger text.
func emitDiagnostic(rec *diagnosticRecord) {
	var attrs []slog.Attr
	attrs = append(attrs,
		slog.String("event", "db_error"),
		slog.String("code", rec.Code),
		slog.Int("status", rec.Status),
		slog.String("sqlstate", rec.SQLSTATE),
	)
	if rec.RequestID != "" {
		attrs = append(attrs, slog.String("request_id", rec.RequestID))
	}
	if rec.Application != "" {
		attrs = append(attrs, slog.String("application_id", rec.Application))
	}
	if rec.Namespace != "" {
		attrs = append(attrs, slog.String("namespace_id", rec.Namespace))
	}
	attrs = append(attrs, rec.Detail...)
	// slog.Warn takes (msg string, args ...any); convert the []slog.Attr to
	// []any so the attribute slice is expanded as the variadic argument.
	args := make([]any, len(attrs))
	for i, a := range attrs {
		args[i] = a
	}
	slog.Warn("database error classified", args...)
}
