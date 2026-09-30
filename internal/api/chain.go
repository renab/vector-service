package api

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"vector-service/internal/apierr"
	"vector-service/internal/auth"
	"vector-service/internal/dbctx"
)

// authnFunc is the authenticated-identity check of an auth middleware
// adapter. It receives the request's effective context (the deadline
// stage's WithCancel-derived context — the context every PostgreSQL
// operation runs under) and returns the authenticated application
// identity (the data plane) or nil (the admin plane authorizes but
// resolves no application). It returns the single typed *apierr.Error
// (401 unauthorized) on failure; infrastructure and settlement outcomes
// are returned as-is (they pass through Classify rule 1 / rule 2
// unchanged and render as 503 or nothing).
type authnFunc func(ctx context.Context, r *http.Request) (appID string, err error)

// Chain is the assembled middleware chain of the service (implementation
// package 4, section 2). It owns the two mount points:
//
//   - the full chain (all API routes): request ID → access logging →
//     operation deadline → recovery → operation (per-route) →
//     auth (data-plane | admin) → content-type / body-cap (body endpoints
//     only) → handler adapter;
//   - the base chain (/healthz and /readyz): request ID → access logging
//     → operation deadline → recovery. The deadline stage is not bypassed
//     on the base chain: every request — including probes — is bounded by
//     the operation deadline.
//
// The chain owns one request-ID stage, one access logger, one deadline
// stage, one recovery, and one content-type/body-cap implementation per
// chain. Route-specific operation names and the auth adapter per route are
// supplied at registration (the router, section 4 — out of scope here);
// BaseChain is fully assembled here because the health/readiness routes
// carry no per-route stage.
type Chain struct {
	// Deadline is the operation deadline stage (VEC_HTTP_REQUEST_TIMEOUT).
	Deadline *OperationDeadline
	// MaxBodyBytes is the body cap (VEC_HTTP_MAX_BODY_BYTES); 0 disables
	// the body-cap stage (no body endpoints registered).
	MaxBodyBytes int
}

// authStageKey carries the resolved authenticated application identity in
// the request context. The endpoint adapter (section 4) reads it when
// building HandlerInput; on admin routes it carries the empty (zero)
// identity: the admin stage authorizes but resolves no application.
type authStageKey struct{}

// AppIDFrom returns the authenticated application identity carried by the
// context ("" on admin routes and outside the chain).
func AppIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(authStageKey{}).(string)
	return id
}

// FullChain mounts the full chain over next for one route: the operation
// stage (the stable operation name, set before auth so the access line
// carries it even for auth failures and panics), the auth adapter (the
// data plane or the admin plane), and — when bodyCap is true — the
// content-type/body-cap stage (after auth: an unauthenticated oversized
// body receives 401, not 413).
func (c *Chain) FullChain(operation string, authn authnFunc, bodyCap bool, next Handler) http.Handler {
	var h http.Handler = c.handlerAdapter(next)
	if bodyCap {
		h = c.bodyStage(h)
	}
	h = c.authStage(authn, h)
	h = c.operationStage(operation, h)
	h = c.recovery(h)
	h = c.operationDeadline(h)
	h = c.accessLog(h)
	return c.requestID(h)
}

// BaseChain mounts the base chain over next: request ID → access logging
// → operation deadline → recovery. The health and readiness routes use it
// (no operation, auth, or body stage); the deadline stage is not bypassed.
func (c *Chain) BaseChain(next http.Handler) http.Handler {
	h := c.recovery(next)
	h = c.operationDeadline(h)
	h = c.accessLog(h)
	return c.requestID(h)
}

// requestID is the request-ID stage (outermost). It accepts an inbound
// X-Request-Id that passes the bounded grammar check (printable,
// length-bounded); otherwise it generates a UUID. The effective ID is
// echoed in responses (X-Request-Id header and the error body's
// request_id), in the access line, and in every log record for the
// request. It is not a secret and not a token.
func (c *Chain) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ctx := ResolveRequestID(r.Context(), r)
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// accessLog is the access-logging stage. It allocates the per-request log
// state and the synchronized settlement state (together with the recorder
// / response gate, which it wraps around w), installs all of them in the
// context, and emits the single access line AFTER the inner chain —
// including recovery — returns, so the line observes a recovered 500.
//
// The line carries: request ID, method, path, status_code, duration_ms,
// plus the fields set on the per-request log state. status_code is the
// FIRST committed status (the first WriteHeader, or the first Write
// implying 200) — or 0 iff no status was ever committed (the write was
// suppressed because the transport's closure was already known at the
// render decision: a client settlement, or a service settlement rendered
// after the transport closed). The access logger never reads the
// settlement state — it reads only the recorder's committed status.
func (c *Chain) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		st := &dbctx.SettlementState{}
		logSt := &LogState{}
		rec := &responseGate{
			ResponseWriter: w,
			st:             st,
			transport:      r.Context(),
		}
		ctx := r.Context()
		ctx = dbctx.WithSettlement(ctx, st)
		ctx = WithLogState(ctx, logSt)
		ctx = context.WithValue(ctx, recorderKey{}, rec)
		next.ServeHTTP(rec, r.WithContext(ctx))

		attrs := []any{
			slog.String("request_id", dbctx.RequestID(ctx)),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status_code", rec.Status()),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		}
		if logSt.ApplicationID != "" {
			attrs = append(attrs, slog.String("application_id", logSt.ApplicationID))
		}
		if logSt.NamespaceID != "" {
			attrs = append(attrs, slog.String("namespace_id", logSt.NamespaceID))
		}
		if logSt.NamespaceKey != "" {
			attrs = append(attrs, slog.String("namespace_key", logSt.NamespaceKey))
		}
		if logSt.Operation != "" {
			attrs = append(attrs, slog.String("operation", logSt.Operation))
		}
		if logSt.VectorSpaceKey != "" {
			attrs = append(attrs, slog.String("vector_space_key", logSt.VectorSpaceKey))
		}
		if logSt.ApplicationKey != "" {
			attrs = append(attrs, slog.String("application_key", logSt.ApplicationKey))
		}
		if logSt.ResultCount != nil {
			attrs = append(attrs, slog.Int("result_count", *logSt.ResultCount))
		}
		if logSt.Limit != nil {
			attrs = append(attrs, slog.Int("limit", *logSt.Limit))
		}
		if logSt.FilterCount != nil {
			attrs = append(attrs, slog.Int("filter_count", *logSt.FilterCount))
		}
		if logSt.Upserted != nil {
			attrs = append(attrs, slog.Int("upserted", *logSt.Upserted))
		}
		if logSt.Unchanged != nil {
			attrs = append(attrs, slog.Int("unchanged", *logSt.Unchanged))
		}
		slog.Info("access", attrs...)
	})
}

// recorderKey carries the access-logging recorder (the response gate) in
// the request context. The deadline stage reads it (it is the only
// http.ResponseWriter the inner chain ever sees — the renderer writes
// through it, so every commit passes the gate).
type recorderKey struct{}

// recorderOf returns the response gate carried by the context, or nil
// when none is carried (a context outside the access-logging stage).
func recorderOf(ctx context.Context) *responseGate {
	rec, _ := ctx.Value(recorderKey{}).(*responseGate)
	return rec
}

// operationDeadline composes the operation deadline stage into the chain.
func (c *Chain) operationDeadline(next http.Handler) http.Handler {
	return c.Deadline.WithOperationDeadline(next)
}

// recovery is the recovery stage. A panic before anything is written → 500
// internal through the recorder, plus a separate stack-trace log record
// (request ID, operation if set, a panic marker) — the stack is logged,
// never written to the response. A panic after a status was committed
// (explicit WriteHeader or implicit 200) → the first-committed status is
// preserved and unreplaceable: no second status, no second body; the
// separate panic record is still emitted.
func (c *Chain) recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			logSt := LogStateOf(r.Context())
			var operation string
			if logSt != nil {
				operation = logSt.Operation
			}
			// The separate stack-trace log record: the stack is logged,
			// never written to the response.
			slog.Error("panic recovered",
				slog.String("request_id", dbctx.RequestID(r.Context())),
				slog.String("operation", operation),
				slog.String("panic", "true"),
				slog.String("stack", string(debug.Stack())),
			)
			// Render the 500 through the recorder: a panic after a status
			// was committed is a no-op on the wire (the first-committed
			// status is unreplaceable — Render checks the recorder), so the
			// same call covers the pre-commit and post-commit cases.
			Render(r.Context(), recorderOf(r.Context()), apierr.ErrInternal)
		}()
		next.ServeHTTP(w, r)
	})
}

// operationStage is the operation stage: a per-route closure attached at
// registration, before auth, that sets the stable operation name on the
// per-request log state (in place — the mutation-in-place contract) so the
// access line carries it even for auth failures and panics.
func (c *Chain) operationStage(name string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if logSt := LogStateOf(r.Context()); logSt != nil {
			logSt.Operation = name
		}
		next.ServeHTTP(w, r)
	})
}

// authStage is an auth middleware adapter. dataPlane and admin are built
// by DataPlaneAuth and AdminAuth; the adapter is the single site that
// runs the check and, on success, places the resolved application
// identity on the context (the endpoint adapter reads it when building
// HandlerInput — "" on the admin plane). On failure it renders the typed
// error (401) and stops the chain.
func (c *Chain) authStage(authn authnFunc, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appID, err := authn(r.Context(), r)
		if err != nil {
			Render(r.Context(), recorderOf(r.Context()), Classify(r.Context(), err))
			return
		}
		ctx := context.WithValue(r.Context(), authStageKey{}, appID)
		// Propagate the resolved application identity into dbctx so data-plane
		// handlers (and any downstream SQL scoping that reads it) see the same
		// application ID that HandlerInput.AppID carries.
		if appID != "" {
			ctx = dbctx.WithAppID(ctx, appID)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bodyStage is the content-type / body-cap stage (body endpoints only,
// after auth). The content type must be application/json (a charset
// parameter is allowed) → else 400 invalid_content_type. The body is
// capped by wrapping in http.MaxBytesReader at MaxBodyBytes; the cap
// mechanism writes nothing — the overflow surfaces as a
// *http.MaxBytesError from a later read and is classified by the decode
// stage (413 body_too_large), only while the request context is active.
// Because this stage sits after auth, an unauthenticated oversized body
// receives 401, not 413.
func (c *Chain) bodyStage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := CheckContentType(r); err != nil {
			Render(r.Context(), recorderOf(r.Context()), err)
			return
		}
		if c.MaxBodyBytes > 0 {
			r = r.WithContext(context.WithValue(r.Context(), bodyCapKey{}, c.MaxBodyBytes))
			CapBody(r, int64(c.MaxBodyBytes))
		}
		next.ServeHTTP(w, r)
	})
}

// bodyCapKey carries the applied body cap (0 = not capped) in the request
// context, for diagnostics.
type bodyCapKey struct{}

// MaxBodyBytesFrom returns the body cap applied to the request by the
// body stage, or 0 when no cap stage ran (a non-body route).
func MaxBodyBytesFrom(ctx context.Context) int {
	n, _ := ctx.Value(bodyCapKey{}).(int)
	return n
}

// DataPlaneAuth is the data-plane auth middleware adapter: it parses the
// Authorization bearer credential and runs the data-plane lookup (the
// package-2 auth value). On success it returns the authenticated
// application identity (the only source of application identity for
// data-plane handlers); on any failure it returns the raw lookup error,
// which the auth stage classifies exactly once (401 unauthorized — the
// lookup never distinguishes failure kinds; 503 / nothing for the
// infrastructure or settlement outcomes — Classify rule 1 / rule 2).
func DataPlaneAuth(ctx context.Context, r *http.Request, lookup func(ctx context.Context, credential string) (string, error)) (string, error) {
	cred, err := auth.ParseBearer(r.Header.Get("Authorization"))
	if err != nil {
		return "", auth.ErrUnauthorized
	}
	return lookup(ctx, cred)
}

// AdminAuth is the admin-plane auth middleware adapter: it parses the
// Authorization bearer credential and runs the admin-token check
// (constant-time; no database access). It returns the empty (zero)
// application identity: the admin stage authorizes but resolves no
// application.
func AdminAuth(ctx context.Context, r *http.Request, adminToken string) (string, error) {
	cred, err := auth.ParseBearer(r.Header.Get("Authorization"))
	if err != nil {
		return "", auth.ErrUnauthorized
	}
	if err := auth.AdminPlane(adminToken, cred); err != nil {
		return "", auth.ErrUnauthorized
	}
	return "", nil
}

// handlerAdapter is the endpoint adapter: the sole Handler →
// http.HandlerFunc conversion. It builds HandlerInput (copying AppID from
// the context the auth stage placed — the zero identity on admin routes),
// invokes the handler, and renders any non-nil return through Render. No
// second signature, input struct, or conversion site exists.
func (c *Chain) handlerAdapter(h Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		in := &HandlerInput{
			Req:   r,
			AppID: AppIDFrom(r.Context()),
			Path:  pathValuesOf(r),
		}
		if err := h(r.Context(), w, in); err != nil {
			Render(r.Context(), recorderOf(r.Context()), Classify(r.Context(), err))
		}
	})
}

// pathValuesOf resolves the {key: value} path-segment map for a request
// served by the router (a Go 1.22+ ServeMux pattern). r.PathValue is a
// func(string) string method on *http.Request; it cannot be copied into a
// map field, so this helper iterates the known path keys for the route and
// builds the map. A base-chain route (no path keys) yields an empty map.
// The set of keys is derived from the request's pattern at serve time by
// the router (section 4); this helper reads the values through the
// method.
func pathValuesOf(r *http.Request) map[string]string {
	// The router (section 4) registers every route with an exact
	// method+path pattern and a closed set of path keys. The endpoint
	// adapter is the sole Handler conversion site; it builds the map by
	// iterating the request's pattern keys. Go 1.22+ net/http exposes the
	// resolved values through r.PathValue(key). The pattern keys are not
	// directly enumerable from *http.Request, so the adapter reads them
	// from the context the router placed at registration (one closed
	// string slice per route).
	keys, _ := r.Context().Value(routePathKeys{}).([]string)
	if len(keys) == 0 {
		return nil
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = r.PathValue(k)
	}
	return out
}

// routePathKeys carries the closed set of path keys for the route (e.g.
// ["namespace", "record_id"]) in the request context, placed by the router
// (section 4) at registration. The endpoint adapter reads it to build
// HandlerInput.Path.
type routePathKeys struct{}

// WithRoutePathKeys returns a context carrying the route's closed path-key
// set. The router calls this before mounting the endpoint adapter.
func WithRoutePathKeys(ctx context.Context, keys []string) context.Context {
	return context.WithValue(ctx, routePathKeys{}, keys)
}
