package api

import "context"

// LogState is the per-request log state (implementation package 4,
// section 2, "Per-request log state"). One fresh mutable struct per
// request, allocated by the access logger and stored in the context; inner
// layers mutate it IN PLACE on the request goroutine — no synchronization
// is needed (go test -race clean) and a stage that replaces the context
// object fails the logging assertions (mutation in place is the only path).
//
// The field set is closed and typed:
//
//   - application_id, namespace_id, namespace_key, operation,
//     vector_space_key: the identifiers and operation name set by the
//     auth, operation, and owning-handler stages;
//   - result_count, limit, filter_count, upserted, unchanged: the
//     operation result fields set by the owning handler. A set value of
//     result_count of 0 is emitted (an empty search result is an
//     observable result);
//   - application_key: admin plane only.
//
// There is no generic field and NO DB-error field: DB error detail
// reaches the log only through separate diagnostic records (section 3 —
// which never carry the raw driver message), never through this state.
//
// A nil field means "not set"; a set value of 0 is still emitted
// (SetResultCount(0) records an empty search result). The Set* helpers
// update value and presence flag together on one call; identifier and
// key fields are plain strings (empty means not set).
type LogState struct {
	ApplicationID  string
	NamespaceID    string
	NamespaceKey   string
	Operation      string
	VectorSpaceKey string
	ApplicationKey string // admin plane only

	ResultCount *int
	Limit       *int
	FilterCount *int
	Upserted    *int
	Unchanged   *int
}

type logStateKey struct{}

// WithLogState returns a context carrying the per-request log state. It is
// installed by the access-logging stage and read only by that stage (the
// access logger is the only reader); inner layers mutate the struct in
// place through LogStateOf.
func WithLogState(ctx context.Context, st *LogState) context.Context {
	return context.WithValue(ctx, logStateKey{}, st)
}

// LogStateOf returns the per-request log state carried by the context, or
// nil when none is carried (a context outside the chain).
func LogStateOf(ctx context.Context) *LogState {
	st, _ := ctx.Value(logStateKey{}).(*LogState)
	return st
}
