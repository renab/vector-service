# Package 4 — HTTP API

## Objective

Deliver the complete HTTP surface: the endpoint handler contract, the
hardened middleware chain, strict JSON decoding, request IDs, access
logging, the error renderer and classifier (the single
classification boundary), route registration for every `docs/API.md`
endpoint, the four admin endpoints, `/healthz`, `/readyz`, and bounded
shutdown. After this package the service is fully operational: a real
client can authenticate, upsert, search, get, delete, and administer
applications over HTTP.

## Authority

- `docs/API.md` — base path `/v1`; `application/json`; error format
  `{"error": {"code", "message", "request_id"}}`; "Raw PostgreSQL
  errors must not be returned directly"; request IDs (service assigns
  or accepts; accepted values validated and bounded; returned in
  responses and logs; not a token); `/healthz` process-only, no auth;
  `/readyz` = configuration + applied migrations + usable PostgreSQL,
  no auth, 200 or 503; the admin endpoint request/response shapes and
  their conflict/idempotence rules; the C13 endpoint (see Unresolved).
- `docs/DEVELOPMENT.md` — stdlib HTTP; JSON request/response structs;
  reject malformed JSON and unknown fields on all JSON endpoints;
  bounded bodies; explicit server timeouts (never the unconfigured
  default server); `log/slog`; internal errors map to stable responses
  while logs may carry structured DB detail — the structured
  diagnostic-record fields (section 3), never the raw driver primary
  message.
- `docs/SECURITY.md` — the allowed and forbidden log fields; admin
  authentication distinct from data-plane.
- Package 1 — configuration, pool, migrations-converged flag, server
  timeout values.
- Package 2 — both auth middlewares, the helpers, the abort sentinel,
  namespace resolution.
- Package 3 — the five data-plane handler values and their error codes.
- Overview — error model, catalog, configuration.

## Scope

### 1. Handler contract (single definition)

Defined once in `internal/api`; packages 3 and 4's admin handlers
implement it; the endpoint adapter (section 4) is the only site that
converts it to `http.HandlerFunc`.

```go
type Handler func(ctx context.Context, w http.ResponseWriter,
    in *HandlerInput) *apierr.Error

type HandlerInput struct {
    Req   *http.Request      // raw request; the handler decodes its own body
    AppID uuid.UUID          // authenticated identity; zero UUID on admin routes
    Path  map[string]string  // raw path-segment values (unvalidated)
}
```

- The handler **owns its success response**: it writes the success
  status and JSON body to `w` itself, then returns `nil`.
- The success write goes through the **response gate** (section 2):
  the first status commit races atomically with the cancellation
  settlements in the settlement state. A success commit that lands
  first is preserved even if a deadline or shutdown settlement follows;
  a success commit attempted after a `service` settlement — or while
  the process-level shutdown marker is set — is **rejected**: no bytes
  reach the connection and `Write` returns an error. A handler must
  check its write results: a rejected or failed write is converted
  through the classifier (the inactive context settles by the shared
  cause rule) and returned as a typed error, exactly like any other
  failure. The deadline stage's finalization (section 2) guarantees the
  exactly-one-response invariant regardless of handler behavior.
- A failure path writes **nothing** to `w` and returns a typed
  `*apierr.Error` — each raw error from the service path converted
  **exactly once** via the classifier (section 3). The concrete pointer
  return type means a raw driver error cannot cross the boundary by
  construction (`return rawPgxErr` does not compile).
- Body endpoints begin by calling the strict-JSON decode helper
  (`api.DecodeJSON(in.Req, &RequestStruct)`) as the handler's first
  action; the decoded struct is handler-local and never arrives in
  `HandlerInput`.
- On admin routes `AppID` is the zero UUID: the admin stage authorizes
  but resolves no application; admin handlers resolve the `{application}`
  path key themselves (section 5).
- Path-segment grammar (UUID shape, non-emptiness) is validated by the
  owning handler (400 `invalid_uuid` / `missing_field`), not the router.

### 2. Middleware chain

Final order, normative (one per chain, never re-implemented):

```text
request ID → access logging → operation deadline → recovery →
operation (per-route) → auth (data-plane | admin | none) →
content-type / body-cap (body endpoints only) → handler
```

Two mount points: the **full chain** (all API routes) and the **base
chain** (request ID → access logging → operation deadline → recovery)
for `/healthz` and `/readyz`, which bypass operation, auth, and body
stages. The deadline stage is not bypassed on the base chain.

- **Request ID (outermost).** Accepts an inbound `X-Request-Id` that
  passes a bounded grammar check (printable, length-bounded); otherwise
  generates a UUID. The effective ID is echoed in responses
  (`X-Request-Id` header and the error body's `request_id`), in the
  access line, and in every log record for the request. It is not a
  secret and not a token.
- **Access logging.** Wraps the recovery stage (its response recorder
  is installed before the inner chain runs, and its deferred line is
  emitted after the inner chain — including recovery — returns), so the
  single access line observes a recovered 500. The line: request ID,
  method, path, `status_code`, `duration_ms`, plus the fields set on
  the per-request log state (below). `status_code` is the **first
  committed** status (the first `WriteHeader`, or the first `Write`
  implying 200) — commit is not proof of delivery, and no later
  disconnect can retroactively change a committed status — or `0` iff
  no status was ever committed, i.e. the write was suppressed because
  the transport's closure was already known at the render decision: a
  `client` settlement, or a `service` settlement rendered after the
  transport closed (overview cancellation contract).
- **Operation deadline (sole implementation).** The stage derives the
  effective context with `context.WithCancel(r.Context())` — never
  `context.WithTimeout`: a `WithTimeout` timer cancels on its own with
  no hook to settle the request first, and a cancel carries **no**
  classification information. `WithCancel` makes the cancel
  lifecycle-controlled: the effective context can become inactive only
  through the stage's timer fire path, the stage's deferred path
  (normal completion — it claims nothing new; the response is already
  committed, so nothing is ever classified), or a cancel of an ancestor
  (the serve root, section 7). The service-origin cancels — the fire
  path's and the serve root's — each settle the request **before**
  they cancel (below); the transport (client) cancel settles through
  **synchronous transport observation at the settlement points** (below
  — there is no asynchronous watcher). One timer per request arms the
  `VEC_HTTP_REQUEST_TIMEOUT` bound as a `time.AfterFunc` whose callback
  is the stage's **fire path**; the stage composes into both mount
  points.
  - **Fire path (timer goroutine):** a cause-resolution step under the
    settlement state's synchronization, in exact order: (1) an outcome
    already recorded → release, cancel, settle nothing (a late fire — a
    cancel never settles a request that is already settled); (2)
    otherwise observe the **transport** request context
    (`r.Context()`) — if it is already inactive, the client
    disconnected **before** the deadline fired: record the `client`
    outcome (the first cause) and cancel; (3) otherwise record the
    `service` outcome (cause `operation_deadline`) and cancel. The
    cancel itself never records or changes an outcome.
  - **No asynchronous watcher (first-cause rule).** The `client`
    outcome is recorded **only** at a settlement point that
    **synchronously** observes the transport context inactive — the
    fire path (step 2), the response gate's first commit, the shared
    cause rule (section 3 and the package-2 helpers), and the deferred
    path's finalization — each check running under the settlement
    state's synchronization **before** any outcome is recorded or
    applied. The first-cause guarantee therefore needs no goroutine
    that could run late: a transport cancellation that happens-before a
    service settlement is seen by that settlement's own check and
    settles the request as `client`; a service event can record
    `service` only while the transport is still active, and can
    therefore never reclassify an already-canceled transport request,
    regardless of scheduling. A true simultaneous race (neither order
    observable) resolves by whichever settlement point wins the claim
    first: deadline-before-disconnect commits the 503 (the transport
    was active at decision time); disconnect-before-deadline is an
    abort.
  - **Deferred path (request goroutine, every return):** run the
    **finalization step** (next bullet), then acquire the settlement
    state's synchronization, mark the request settled, release; stop
    the timer (best-effort — a fire already scheduled or running loses
    its claim against the committed outcome); invoke cancel
    (idempotent).
  - **Finalization step (request goroutine, after the inner chain
    returns, before settle):** read the settlement state once under
    its synchronization, then act:
    - a status was committed (`response` outcome) → do nothing; the
      first-committed status is unreplaceable, whatever else the state
      shows;
    - `client` outcome → do nothing (abort; no response);
    - `service` outcome → render the cancellation response through
      `api.Render` with typed `unavailable` — the centralized responder
      (section 3) commits exactly one 503 (access `status_code` 503;
      commit is not proof of delivery); if the transport closed after
      the claim and before the render, the write is suppressed and the
      access line carries 0 (the outcome stays `service` — it is never
      reclassified);
    - no outcome recorded → resolve it now, under the same
      synchronization, by the shared cause rule (section 3): the
      transport observed inactive → record `client`, do nothing
      (abort); the transport active and the shutdown marker set →
      record `service` (cause `shutdown`) and render the 503; the
      transport active with no marker → the inner chain returned
      without committing any status (a handler contract violation: it
      returned `nil` without writing a success) → render 500
      `internal` so a live connection always receives exactly one
      response. This is a defect backstop, not a normal path.
  - **Settlement invariant:** each outcome is claimed at most once and
    only while no outcome is recorded; the `service` claim is released
    under the settlement state's synchronization **before** the fire
    path cancels; the process-level marker is set under its own
    synchronization **before** the serve root is canceled; and the
    `client` outcome is recorded only by a settlement point that
    synchronously observed the transport inactive under the same
    synchronization (first-cause rule, above). Because the recorded
    outcomes and the marker are the only classification sources, and
    the classifier, the gate, and the shared cause rule read them
    through the same synchronization, a request can never be observed
    inactive without a settled explanation — a recorded outcome, the
    marker (transport active), or the transport-inactive observation
    (client). A service event can never be the explanation for a
    request whose transport was already canceled: `service` is
    recorded only while the transport was observed active, and the
    marker is honored only then.
  - The stage itself classifies nothing and emits no log record. The
    derived context is the request's effective context for auth,
    decoding, handlers, and every PostgreSQL operation. When the
    deadline fires with the transport still active the request settles
    as **service-initiated**: exactly one stable 503 `unavailable` is
    committed (section 3), and in-flight PostgreSQL work cancels
    through the ordinary pgx context path. When the transport was
    already canceled, the fire path settles it as `client` instead
    (first-cause rule) — an abort, no response.
- **Settlement state (synchronized; distinct from log state).** One
  small mutable struct per request, allocated by the access-logging
  stage together with its recorder (the response gate, next bullet)
  and carried in the request context; the deadline stage reads it from
  the context. The per-request log state remains plain and
  unsynchronized (below), and the access logger never reads the
  settlement state — it reads only the recorder's committed status.
  The settlement state is the **sole** entry point the shared cause
  rule (overview; implemented once, shared with the package-2 helpers)
  uses to observe settlement, and it holds exactly:
  - the outcome: `none`, `client`, `service`, or `response` — the
    first claim wins; later claims are no-ops and can never
    reclassify the request,
  - the service cause (`operation_deadline` from the fire path, or
    `shutdown` when a no-outcome state is resolved through the
    marker),
  - the committed status (recorded with the `response` claim by the
    response gate),
  - the settled flag (set only by the deferred path),
  - a reference to the **transport** request context (`r.Context()`),
    which the settlement points observe synchronously under the
    state's synchronization (first-cause rule, above), and
  - a reference to the **process-level shutdown marker** (one per
    serve process; section 7).
  The state's own fields are read or written only under the state's
  own synchronization; the referenced process-level marker is read
  through its own synchronization; the transport context's
  active/inactive state is read by the settlement points at their
  defined moments (the context is always eventually canceled, so the
  observation is always well-defined). It is deliberately small: no
  generic fields, no error detail, no log fields.
- **Response gate (the access-logging recorder).** The access-logging
  stage wraps `w` once; that single recorder is also the **response
  gate**, the only `http.ResponseWriter` the inner chain ever sees. It
  remains the access line's status source — the **first** committed
  status, or `0` when none was committed — and additionally enforces
  the settlement rule at the first status commit:
  - **First commit** (the first `WriteHeader`, or the first `Write`
    that implies 200): the rule below is evaluated under the
    settlement state's synchronization, in this order:
    - a status was already committed (`response` outcome) → the call
      is a subsequent write: pass through (below);
    - the outcome is `client`, **or** the transport context is
      observed inactive (first-cause rule: in the none-outcome case,
      record the `client` outcome before rejecting; a `service`
      outcome is never reclassified) → the commit is **rejected**:
      no response at all is the client outcome, and nothing —
      including an error body or the cancellation 503 — may be written
      to a transport whose closure is already known;
    - the outcome is `service`, or the outcome is none with the
      shutdown marker set (transport active, by the previous rule) →
      the commit is accepted **only if the status is 503**: by the
      handler contract only the renderer writes 503, so this
      authorizes exactly the centralized cancellation response. Any
      other status — a handler success write after the deadline fired
      or shutdown began — is **rejected**: no status is recorded, no
      bytes reach the connection, and the write returns `0` with an
      error so a handler that checks its writes surfaces the failure
      to the classifier;
    - otherwise (no outcome, no marker, transport active) → claim
      `response` with the commit's status and write it (the success
      commit, or the first-rendered error status while the request is
      still open).
    Rejected commits record no status, deliver 0 bytes, and return an
    error.
  - **Subsequent writes** after a committed status always pass
    through (the committed response body continues; the first status
    is unreplaceable). Writes after a rejected first commit are
    re-evaluated as first commits and stay rejected under a winning
    cancellation, so no partial body can leak.
  - The gate never writes response bytes itself; it only authorizes or
    rejects the first commit. The renderer (section 3) is the only
    code that produces response bytes, and the finalization step (above)
    is the only code that invokes it after the handler has returned.
- **Recovery.** A panic before anything is written → 500 `internal`
  through the recorder, plus a **separate** stack-trace log record
  (request ID, `operation` if set, a panic marker) — the stack is
  logged, never written to the response. A panic after a status was
  committed (explicit `WriteHeader` or implicit 200) → the
  first-committed status is preserved and unreplaceable: no second
  status, no second body; the separate panic record is still emitted.
  The same first-committed-status rule is what guarantees a single
  status under cancellation (section 3).
- **Per-request log state.** One fresh mutable struct per request,
  allocated by the access logger and stored in the context; inner
  layers mutate it in place on the request goroutine (no synchronization
  needed; `go test -race` clean). Closed typed field set:
  `application_id`, `namespace_id`, `namespace_key`, `operation`,
  `vector_space_key`, `result_count`, `limit`, `filter_count`,
  `upserted`, `unchanged`, and (admin plane only) `application_key`.
  There is no generic field and **no DB-error field**: DB error detail
  reaches the log only through separate diagnostic records (section 3 —
  which never carry the raw driver message), never through this state.
  A set value of `result_count` of 0 is emitted (an empty search result
  is an observable result).
- **Operation stage.** A per-route closure attached at registration,
  before auth, sets `operation` (stable names: `upsert`, `search`,
  `get_record`, `delete_projection`, `delete_object`,
  `register_application`, `register_namespace`, `create_credential`,
  `disable_credential`, `healthz`, `readyz`) so the access line carries
  it even for auth failures and panics.
- **Content-type / body-cap (body endpoints, after auth).** Content-
  type must be `application/json` (charset parameter allowed) → else
  400 `invalid_content_type`. The body is capped by wrapping in
  `http.MaxBytesReader` at `VEC_HTTP_MAX_BODY_BYTES`; the cap mechanism
  writes nothing — the overflow surfaces as a `*http.MaxBytesError`
  from a later read and is classified by the decode stage (413
  `body_too_large`), only while the request context is active. Because
  this stage sits after auth, an unauthenticated oversized body receives
  401, not 413. `DELETE …/records` carries a body and is checked like
  other body endpoints.

### 3. Error renderer and classifier (the single classification boundary)

- `api.Classify(ctx, err)` — the only classifier. Order: (1) effective
  context no longer active → the **shared cause rule** (overview error
  model), whatever `err` is. The rule reads the request's settlement
  state (section 2) **through its synchronization** — it never reads
  the outcome, the transport context, or the marker outside it — and
  settles by the recorded outcome: `client` → client-initiated;
  `service` → service-initiated; `response` → the committed status
  stands, so nothing further is rendered (the renderer's committed
  check enforces this on the wire). With **no outcome recorded**, the
  rule resolves the state in place, under the same synchronization:
  observe the **transport** context — inactive → record the `client`
  outcome (first-cause rule) → client-initiated; active → the
  referenced process-level shutdown marker, read through its own
  synchronization: set → record the `service` outcome (cause
  `shutdown`) → service-initiated; unset → record the `service`
  outcome (defensive branch — impossible by construction: the only
  other way the effective context can become inactive is the fire
  path's cancel, which always records its claim under the same
  synchronization first) → service-initiated, plus a separate warn
  record (defect backstop).
  Service-initiated → `unavailable` (503); client-initiated → the
  abort sentinel. Because every service-origin settlement — the
  `service` claim (section 2 fire path, recorded only while the
  transport is active) and the process-level marker (section 7,
  honored only while the transport is active) — is published **before**
  its cancel, a classifier that observes the effective context
  inactive and then reads the settlement state necessarily observes a
  complete explanation: a recorded outcome, the marker (transport
  active), or the transport-inactive observation (client). A request
  can never be observed inactive as a service settlement without its
  claim or marker, and a client-canceled transport request is never
  reclassified as service — the service events that could have
  reclassified it either recorded their outcomes only while the
  transport was still active, or lost the claim race to the `client`
  outcome.
  (2) already-typed
  `*apierr.Error` → pass through;
  (3) the overview SQLSTATE table; (4) the `P0001` message-prefix
  sub-classification
  (gated on `Code == "P0001"`; any other prefix → `internal`). On
  classifying a raw driver error it emits **at most one** separate
  diagnostic log record — the fields defined by the diagnostic-record
  policy (next bullet). The raw driver primary message is never among
  them: it is read only by the `P0001` prefix gate for classification
  and never logged, for any SQLSTATE.
- `api.Render(ctx, w, err *apierr.Error)` — the only renderer, and the
  **centralized cancellation responder**; accepts exactly the typed
  error, never classifies. Writes
  `{"error": {"code", "message", "request_id"}}` with the mapped status;
  `message` is a stable string per code and never interpolates caller
  input or driver text.
  - **Observable wire semantics (real net/http):** the renderer does
    not detect a peer disconnect after `WriteHeader`, and it does not
    prove delivery — `status_code` is the first committed status, and a
    committed response may or may not reach the peer. The only
    pre-render knowledge of closure is the settlement state itself
    (transport observed inactive), which is what the rules below act
    on.
  - For the abort sentinel (client settlement) it writes **nothing**
    (no status, no body): the transport's closure was already known
    before the render decision, so the write is suppressed (the gate
    also rejects any first commit in that state), and the access line
    carries `status_code` 0.
  - For `unavailable` settled from a service-initiated cancellation
    (operation deadline or serve shutdown) it is the **only** writer
    of the cancellation response: it first checks the recorder for an
    already-committed status — a response the handler already committed
    (its success write) **preserves its first status**, and the 503 is
    dropped, never appended — and otherwise **commits** exactly one
    stable 503 `unavailable` (the catalog body): `WriteHeader(503)`
    commits the status, the body write follows it. The response gate
    authorizes that 503 as the request's first commit under a
    `service` settlement with the transport still active — no other
    first commit is accepted in that state (section 2) — so the
    cancellation 503 is the only status a settled request can still
    receive. If the commit is rejected because the transport closed
    after the `service` claim and before the render, the write is
    suppressed, nothing is delivered, and the access line carries
    `status_code` 0 — the recorded outcome stays `service` (never
    reclassified); the status simply was never committed. A body-write
    failure after a successful commit leaves the committed 503 in
    place (a later, unobserved disconnect never retroactively zeroes
    the status).
  - A handler cannot commit a success after a service cancellation has
    won: the gate rejects the success first commit under a `service`
    settlement (section 2) — the write returns `0` with an error and
    no bytes flow — and the deadline stage's finalization step commits
    the 503 even if the handler ignored the write error and returned
    `nil`. One status total per request: the first committed one; the
    cancellation 503 where the service settlement won before any
    commit and the transport was still active at the render; or
    nothing, where the write was suppressed (client settlement, or a
    service settlement rendered to a transport whose closure was
    already known).
- Raw PostgreSQL text never reaches a response body (overview
  invariant 11).
- **Diagnostic-record policy (the safe allowlist).** Raw PostgreSQL
  driver text — the primary message and any detail fields — is **never
  logged by default**. The at-most-one diagnostic record described
  above may contain only fields from this closed set:
  - **always present:** the decided catalog code and status, the
    SQLSTATE, and the request ID;
  - **where permitted:** the application and namespace identifiers
    already resolved on the per-request log state;
  - **only when explicitly allowlisted:** a bounded, server-authored
    `detail` value. The allowlist is an explicit closed set in the
    diagnostic-record builder: each entry names its server-side source,
    carries a length bound, and must provably be incapable of carrying
    caller data. The allowlist is **currently empty** — no `detail`
    value is emitted today — and adding an entry is a reviewed
    decision, not an implementation convenience.
  The following are never present in a diagnostic record, in whole or
  in part: the driver's primary message or any message-derived text;
  SQLSTATE `22P02` detail (server-authored text that quotes the
  malformed vector input — caller data); `P0001` trigger text (which
  embeds `vector_space_id` values; it is a classification input for the
  prefix gate, not a log input); arbitrary metadata; complete or
  partial vectors; credentials; credential digests; document/source
  content; and any other caller-controlled value.
  Suppression is at the log boundary only: the `P0001` prefix gate
  still classifies from the message, and the rendered response keeps
  its stable catalog code and message — stable API error rendering and
  `P0001` classification are unchanged by this policy.

### 4. Router and routes

- `net/http` `ServeMux` with Go 1.22+ method+path patterns; exact
  patterns for every endpoint; no subtree wildcards on API paths.
- **All route registration happens here, and only here**, each with its
  stable operation name and its auth stage:
  - data plane (package-2 data-plane auth stage):
    `PUT /v1/namespaces/{namespace}/records` (`upsert`),
    `POST /v1/namespaces/{namespace}/search` (`search`),
    `GET /v1/namespaces/{namespace}/records/{record_id}`
    (`get_record`), `DELETE /v1/namespaces/{namespace}/records`
    (`delete_projection`),
    `DELETE /v1/namespaces/{namespace}/objects/{object_id}`
    (`delete_object`);
  - admin plane (package-2 admin auth stage):
    `POST /v1/admin/applications` (`register_application`),
    `POST /v1/admin/applications/{application}/namespaces`
    (`register_namespace`),
    `POST /v1/admin/applications/{application}/credentials`
    (`create_credential`),
    `DELETE /v1/admin/applications/{application}/credentials/{credential_id}`
    (`disable_credential`);
  - base chain (no auth): `GET /healthz`, `GET /readyz`.
- **`GET /v1/vector-spaces` is not registered** (overview, Unresolved
  C13): requests to the path fall through to the unknown-route 404.
- Routing outcomes: unknown path → 404 `not_found`; known path with a
  wrong method → 405 `method_not_allowed` with the `Allow` header.
  Both are rendered by `api.Render` (context checked first).
- The **endpoint adapter** is the sole `Handler` → `http.HandlerFunc`
  conversion: it builds `HandlerInput` (copying `AppID` from the context
  the auth stage placed; zero UUID on admin routes), invokes the
  handler, and renders any non-nil return through `api.Render`. No
  second signature, input struct, or conversion site exists.

### 5. Admin endpoints

All under the admin auth stage. Each handler first resolves the
`{application}` path key: grammar check → 400 `invalid_application_key`;
lookup in `applications` → no row → 404 `application_not_found`. The
resolved UUID is set on the log state (`application_id`,
`application_key`).

- **Register application** (`WithShortTx`): body
  `{application_key, display_name}`; key grammar
  `^[a-z][a-z0-9_-]{0,63}$` → 400 `invalid_application_key`; insert
  into `applications` (no RLS on this table); duplicate key → `23505`
  → 409 `conflict`. Response: `id`, `application_key`, `display_name`,
  `enabled: true`. Keys are immutable after creation.
- **Register namespace** (**`WithAppContext(appID)`** — the forced
  RLS `WITH CHECK` on `namespaces` requires the app context, package 2):
  body `{namespace_key, display_name, metadata}`; key grammar (non-empty,
  no `/` or control characters, length-bounded) → 400
  `invalid_namespace_key`; metadata object rules (400
  `metadata_not_object` / `metadata_too_large`); duplicate
  `(application, namespace_key)` → 409 `conflict`. Response per API.md
  (`id`, `application_key`, `namespace_key`, `display_name`,
  `enabled: true`, `metadata`).
- **Create credential** (`WithShortTx`): body `{credential_name}`;
  name unique per application → 409 `conflict`. Generates the opaque
  credential (package 2), persists only its digest, and returns
  `{credential_id, credential_name, credential}` — **the raw credential
  is returned exactly once** and is never logged.
- **Disable credential** (`WithShortTx`): `UPDATE
  application_credentials SET enabled = false WHERE id = $id AND
  application_id = $app` → `{"disabled": true}`, idempotent on repeat;
  unknown ID → 404 `credential_not_found`. The row is **never deleted**
  (the API semantics is disable; the row preserves historical
  credential identity) — regardless of the `DELETE` grant `0004`
  carries.

### 6. Health and readiness

- **`/healthz`** (base chain, no auth, no database): 200
  `{"status":"ok"}`. Process liveness only.
- **`/readyz`** (base chain, no auth): 200 `{"status":"ready"}` iff
  both hold — (1) the in-process **migrations-converged** flag set by
  the package-1 startup migration step (the runtime role has no grant
  on `vector_control.schema_migrations`, so readiness never reads
  migration history), and
  (2) a per-request `SELECT 1` on the runtime pool with a short bounded
  check (proposal: 2 s). The ping runs under its own nested bounded
  context; its expiry is a ping **failure** — the handler returns typed
  `unavailable` directly (the effective context is still active, so the
  shared cause rule is not consulted), which is the same 503
  `unavailable`
  (generic error shape; readiness is not a diagnostic endpoint —
  operators use logs) as any other readiness failure.
  A readiness request canceled mid-ping settles by the shared cause
  rule: an operation-deadline cancellation commits the 503
  `unavailable` (the transport was active at the render decision); a
  client-disconnect cancellation is the abort (the write is
  suppressed; access `status_code` 0).
  Asymmetry is intended: when the database dies after startup,
  `/healthz` stays 200 and `/readyz` flips to 503.
- Startup order (authoritative): config → connect → validate
  prerequisites → run/validate migrations → start serving → `/readyz`
  succeeds. A migration failure before listening is the package-1
  fail-fast path (non-zero exit, no port).

### 7. Server and bounded shutdown

- The `*http.Server` is built from package-1 configuration: explicit
  read-header, read, write, and idle timeouts; `VEC_LISTEN_ADDR`. The
  router is fully assembled and the migration step succeeded before the
  listener opens.
- **Serve root context.** The lifecycle creates
  `context.WithCancel(context.Background())` and sets it as the server's
  `BaseContext`, so every request context (and through it every
  PostgreSQL operation) is a descendant cancelable by the lifecycle.
- **Process-level shutdown marker (lifetime and ownership).** One
  marker per serve process. The lifecycle creates it together with the
  serve root context, before any request exists, and passes the
  reference into the chain assembly, where the access-logging stage
  carries it in every request's settlement state. The **signal handler
  is the sole writer**: it sets the marker exactly once, under the
  marker's own synchronization, **before** it cancels the serve root.
  The marker is never cleared: shutdown is terminal for the process
  (the process exits after the drain), so a clear after a set is a
  defect. The marker is a **process-global fallback**: it settles a
  request as service-initiated only while the request's settlement
  state records no outcome **and** the request's transport is still
  active (first-cause rule, section 2) — a transport whose closure is
  already known settles as `client` even during shutdown. It never
  reclassifies a request whose per-request outcome already won — a
  `client` outcome stays an abort (no response), a `response` outcome
  keeps its first status, and a `service` outcome stands on its own
  claim. Readers are the shared cause rule (package-4
  classifier/renderer and the package-2 helper boundary), the response
  gate's first-commit rule, the fire path's cause-resolution step, and
  the deferred path's finalization — all through the settlement state
  and the marker's own synchronization. A second signal sees the
  marker already set and does nothing (repeated signals are ignored;
  see below), so the write is exactly once by construction.
- **On the first SIGINT or SIGTERM**, in exact order:
  1. **Cancel in-flight work** (log a shutdown-start record): set the
     process-level shutdown marker (above), **then** cancel the serve
     root context. The order is normative and race-free: the marker
     is published before the cancel, so no request's classifier can
     observe the resulting cancellation without the marker. Every
     in-flight
     request's effective context becomes inactive; in-flight PostgreSQL
     work cancels through the pgx context path; open transactions roll
     back and their leases are released by their owners (package 2);
     each request settles through its settlement state: a request with
     no recorded outcome and a **still-active transport** settles as
     **service-initiated** — exactly one 503 `unavailable` is
     **committed** (access `status_code` 503; commit is not proof of
     delivery) — a request whose transport was already canceled (the
     client left before the shutdown processing observed it) settles
     as **client** by the first-cause rule: the marker never
     reclassifies it, the write is suppressed, and the access line
     carries `status_code` 0; a response already committed preserves
     its first status (section 3).
  2. **Stop accepting and drain:** `Server.Shutdown` under
     `VEC_HTTP_SHUTDOWN_GRACE` — an **independent** context (never
     derived from the just-canceled serve root). Closing the listener
     happens here. A grace-deadline return is a backstop, not a failure:
     it interrupts nothing and changes no exit status; the outstanding
     handlers (already context-canceled) complete on their own — their
     settled outcome (a committed 503, or a suppressed write) is
     reflected in the access lines as the drain proceeds.
  3. **Bounded pool close:** `DB.Close()` (blocks until every acquired
     lease is returned) in a goroutine, observed against the hard
     `VEC_HTTP_SHUTDOWN_CLOSE_TIMEOUT`.
- **Exit status:** 0 when the pool close completes within the hard
  deadline (the normal case); 1 with a structured
  `shutdown_pool_close_timeout` error when it does not. No other
  shutdown exit status exists; the process never hangs on a signal
  (worst-case stop = grace + close timeout; the config enforces this
  sum does not exceed 40 s).
  Repeated signals are ignored.
- **Startup failure (no drain):** if `serve` fails before listening,
  the constructed pool (if any) is closed directly, the startup error is
  logged, and the process exits non-zero; no signal path runs.

## Invariants and correctness constraints

- Single classification boundary: raw driver errors exist only between
  their origin and the handler layer (or the helper boundary for
  infrastructure failures, package 2); every failure response is
  rendered exactly once from a typed error (overview error model).
- Single implementations: one handler contract, one adapter, one
  classifier, one renderer, one deadline stage, one recovery and one
  access logger per chain, one `/healthz`, one `/readyz`, one signal
  handler, one `Server.Shutdown` call site. A duplicate is a defect —
  delete it.
- Cancellation settles exactly once per request: the settlement state
  (section 2) records the first outcome among `client`, `service`, and
  `response`, and later claims and later markers are no-ops. The
  renderer is the only writer of response bytes and the only writer of
  the 503 cancellation response; the finalization step is the only
  post-handler render site; the first-committed status is
  unreplaceable, so a request never receives a second status or body.
  The access line always asserts the committed status (the first
  committed status, or 0 when nothing was committed); 0 occurs only
  when the write was suppressed because the transport's closure was
  already known at the render decision — a committed status is never
  retroactively zeroed by a later, unobserved disconnect (commit is
  not proof of delivery, and delivery is not asserted).
- Settlements are synchronized before their consequences: the
  `service` claim lands under the settlement state's synchronization
  before the fire path cancels (and only while the transport is
  observed active), the process-level marker is set under its own
  synchronization before the serve root is canceled (and is honored
  only while the transport is observed active), the `client` outcome
  is recorded only by a settlement point that synchronously observed
  the transport inactive under the settlement synchronization (first-
  cause rule — there is no asynchronous claimer), and the `response`
  commit is a CAS that fails under any winning cancellation or a
  transport-inactive observation; the classifier, the gate, and the
  shared cause rule read the state only through its synchronization.
  Consequences: a service settlement is never the recorded outcome of
  a request whose transport was already canceled (a service event
  never reclassifies an already-canceled transport request), the
  shutdown marker never reclassifies a `client` or committed
  `response` request (the abort is never reclassified into a 503), a
  handler success commit cannot land after a service settlement, and
  all of this is race-free by construction (`go test -race` clean; the
  unit interleaving tests in Validation force the orders).
- Auth planes disjoint (overview invariants 1, and SECURITY.md):
  separate middleware, separate credential spaces, no shared path.
- Every request — including probes — is bounded by the operation
  deadline; no unconfigured default server.
- Bounded bodies before decode; the decoder never sees more than
  `VEC_HTTP_MAX_BODY_BYTES`.
- Request IDs are not secrets and not tokens.
- Data-plane 404 uniformity is untouched by routing: `not_found` /
  `method_not_allowed` apply to unknown *routes* only.
- Logging discipline (overview invariant 12): the access line carries
  no DB-error detail; diagnostic records are separate, bounded, and
  subject to the never-log list; and the raw driver primary message is
  never logged by default — a diagnostic record carries only the
  SQLSTATE, the stable classification/code/status, the permitted
  request and application/namespace identifiers, and (only when
  explicitly allowlisted) bounded server-authored detail; SQLSTATE
  `22P02` detail, `P0001` trigger text, caller metadata, vectors,
  credentials, and credential digests are never present.
- The raw credential appears in exactly one place ever: the
  `create_credential` response body.

## Validation

**Unit (no database), table-driven:**

- Chain composition (stub stages): stage order for a full-chain route
  and a base-chain route; exactly one recovery, one access logger, one
  deadline stage per chain; body stage only on body routes and after
  auth; health/readiness routes invoke no operation/auth/body stage.
- Request ID: valid inbound ID echoed (header + error body + access
  line); invalid/oversized inbound ID replaced by a generated UUID.
- Deadline: a stub handler blocking until context done with a short
  timeout → the fire path (transport still active) records
  `service` and the settlement state renders **exactly one** 503
  `unavailable` (catalog body), access `status_code` 503, no second
  write after the 503; the same handler canceled at the transport
  layer instead (client disconnect) → nothing written, access
  `status_code` 0 (the forced-order variants below pin the
  happens-before direction); a fast handler completes normally (its
  first commit claims `response`); the deferred path (finalize +
  settled + timer stop + cancel) is exercised on every return path
  (no timer leak, no late claim after settle, no stray goroutine —
  the goroutine count is asserted back to baseline on every path);
  the derived context is the same context a stub service receives
  (identity check); base-chain probe bounded identically.
- **Settlement races** (forced orders, repeated many iterations each,
  every case under `go test -race`; the test seams expose the claim,
  publish, cancel, and write steps separately — which the fire path,
  the lifecycle, and the gate take in their respective orders in
  production — plus a **transport-context seam**: the test holds the
  transport context (a `WithCancel` pair standing in for `r.Context()`)
  and chooses exactly when it becomes canceled, so "transport
  cancellation first, service event delayed" is forced rather than
  hoped for):
  - *Transport cancel first, deadline fire delayed (forced order —
    first cause):* the test cancels the transport context, then
    invokes the fire path late. The fire path's cause-resolution step
    observes the transport inactive → records `client` (it never
    records `service`), cancels; the stub handler's blocked work
    returns, the handler's error is classified by the shared cause
    rule as the abort sentinel; nothing is written; access
    `status_code` 0; the state shows `client` with no `service`
    claim. Variant (a): the handler attempts its success write
    (`WriteHeader(200)` / `Write`) before returning → the gate
    rejects the first commit (transport observed inactive; `client`
    recorded) → 0 bytes, error returned, identical assertions.
    Variant (b): the handler ignores the write error and returns
    `nil` → finalization observes the `client` outcome → nothing
    written, access 0 (the exactly-one-response invariant does not
    depend on handler discipline).
  - *Transport cancel first, marker + root cancel delayed (forced
    order — no reclassification by shutdown):* the test cancels the
    transport context, then the lifecycle sets the shutdown marker
    and cancels the serve root (delayed). Finalization and the shared
    cause rule observe the transport inactive → record `client`; the
    marker never reclassifies: nothing is written, access
    `status_code` 0, the state shows `client`.
  - *Service claim first, transport cancel late (no reclassification
    of a service settlement):* the fire path claims `service`
    (production order: transport check, claim, then cancel — the
    transport is active at the check) and the test cancels the
    transport afterward; the claim stands. Variant (a) — close known
    before the render: the test models the closed transport at the
    render (the gate's transport check sees it inactive) → the 503
    commit is rejected, nothing is written, access `status_code` 0,
    the outcome stays `service`. Variant (b) — commit before the
    close is observable: the render runs while the transport is still
    observed active → the 503 commits; the test's writer then fails
    the body write (disconnect during the write) → the access line
    remains 503 (a committed status is never retroactively zeroed;
    commit is not proof of delivery).
  - *Service claim before cancel, then handler write (race: success
    after settlement):* the fire path claims `service` (transport
    active); the stub handler then attempts its success write
    (`WriteHeader(200)` / `Write`) → the write is rejected (0 bytes
    delivered, error returned), no 200 committed; the finalization
    renders **exactly one** 503 `unavailable` (catalog body), access
    `status_code` 503; the settlement state shows the `service`
    outcome and no `response` commit. Variant: the process-level
    shutdown marker set before the serve-root cancel (instead of a
    per-request claim, transport active) → identical committed
    outcome (exactly one 503, access 503).
  - *Cancellation versus simultaneous first success write (race: the
    commit):* the `service` claim (or the marker set) and the
    handler's first `Write` (implicit 200) are interleaved across many
    iterations so both orders occur: claim first → the write is
    rejected and exactly one 503 is committed (status 503) — or, when
    the transport is observed closed at the render, nothing is
    written (status 0, outcome unchanged); commit first → the 200 is
    preserved, the claim is a no-op, and any later error render
    writes no second status (status 200). The same sweep against the
    `client` outcome: first commit after the transport observed
    closed → rejected, nothing written (status 0); commit before the
    close is observable → the 200 is preserved. Per-iteration
    assertions: at most one committed status, the outcome is exactly
    one of {200, 503, nothing}, 200 and 503 never co-occur, and the
    access line equals the first committed status or 0.
  - *Transport cancel after settle (no reclassification of a
    committed success):* the transport context is canceled after the
    deferred path settled a normally-completed request (net/http
    post-completion cleanup) → the outcome remains `response`, the
    committed success stands, access `status_code` 2xx.
  - *Marker guard:* the serve root is canceled with the marker set,
    the transport **active**, and no other outcome recorded → no
    `client` is recorded (the transport check at every settlement
    point sees it active), the request settles through the marker
    fallback as `service`, exactly one 503 committed (access 503).
    Concurrent variant: the transport is canceled in the same instant
    → the sweep over iterations covers both {503 committed, nothing
    written}, with the access line always consistent with the
    first-cause rule.
  - *Late fire after the deferred path settled:* the fire path runs
    after settle (response already committed) → its claim is a no-op,
    no response is altered, the committed status stands.
  - *Boundary sweep:* deadline durations bracketing the stub
    handler's completion time (handler wins / deadline wins / both in
    the same tick) → each iteration's outcome is exactly one of
    {200, one committed 503, status 0}, asserted per iteration, never
    both 200 and 503 and never a mix within one request.
- Panic triad: pre-commit → exactly one access line with `status_code`
  500, catalog body, no stack in body, same effective request ID in
  header/body/log; post-commit explicit header → preserved 201, no
  second status or body; post-commit implicit 200 → preserved 200; the
  separate panic record present in all three cases.
- Log-state propagation: success line carries operation + identity +
  result fields (including a 0 `result_count`); auth-failure line
  carries `operation` but no identity; mutation-in-place is the only
  path (a stage that replaces the context object fails the assertions);
  the access logger is the only reader.
- Strict JSON: unknown field → `unknown_field`; malformed →
  `invalid_json`; trailing content → `invalid_json`; content-type
  variants (with/without charset) → `invalid_content_type`; oversized
  body (mid-document and trailing-read overflow points) → exactly one
  413 written only by the renderer; the same overflow on an inactive
  context settles by the cause rule (service-initiated → 503,
  client-initiated → nothing written); unauthenticated oversized body →
  401.
- Classifier: the full overview table — every SQLSTATE row, the three
  `P0001` prefixes, an unrecognized `P0001`, a non-`P0001` error
  carrying a trigger-like prefix (SQLSTATE gates first), connection/
  timeout failures with active vs. inactive context, already-typed
  pass-through, `nil` → `nil` — with an assertion that no rendered body
  contains raw message or SQLSTATE text; at most one diagnostic record
  per classified error.
- Diagnostic-record policy (unit, no database): for a representative
  raw driver error per row — including a `22P02` whose message quotes
  the malformed vector input and a `P0001` whose message carries
  trigger text with a `vector_space_id` — capture the structured
  record(s) the classifier emits and assert the allowlist holds:
  `code`, `status`, `sqlstate`, and the request ID are present; the
  resolved application/namespace identifiers are present only where
  set; and **no field value** equals or contains the raw primary
  message, any trigger text, the quoted vector fragment, or any other
  supplied payload. The builder's allowlist mechanism itself is
  exercised with a test-registered entry: a deliberately over-long
  value is truncated to its bound, and a value that is not on the
  allowlist is dropped, not truncated.
- Renderer: exact body shape for a 4xx and the 503; abort sentinel
  (client outcome) → nothing written — the gate rejects every first
  commit under a `client` settlement (the transport was observed
  inactive), so the abort is the only no-response path and the access
  line carries `status_code` 0; `unavailable` settled from a
  service-initiated cancellation (the per-request `service` outcome,
  or the shutdown-marker fallback) → **exactly one 503 catalog body
  is committed** while the transport is still observed active at the
  render (the gate authorizes that 503 as the only acceptable first
  commit under the winning cancellation; access `status_code` 503),
  with two forced variants: (i) the closure is already known before
  the render decision (the transport observed inactive at the render)
  → the write is suppressed, nothing is committed, access
  `status_code` 0, and the recorded outcome stays `service` (never
  reclassified); (ii) the commit succeeds and the body write then
  fails (a disconnect during the body) → the committed 503 stands
  and the access `status_code` stays 503 (a committed status is never
  retroactively zeroed; delivery is not asserted); an
  already-committed response → no second status or body (first status
  preserved; the 503 is dropped, never appended).
- **Response gate authorization matrix** (the recorder exercised in
  isolation, table-driven over every row × column combination, repeated
  under `go test -race`): rows are the settlement state's outcome ∈
  {`none`, `client`, `service`, `response`} crossed with the
  process-level shutdown marker ∈ {set, unset} **and with the
  transport context as observed under the gate's synchronization at
  the first commit** ∈ {active, inactive} (the test controls the
  transport through the same `WithCancel` seam the settlement-race
  tests use); columns are the first commit's status ∈ {200, 201, 404,
  500, 503}. Authorization rule:
  (`none`, unset, active) → every status authorized;
  (`none`, unset, inactive) → nothing authorized — the `client`
  outcome is recorded before the rejection (first-cause rule);
  (`none`, set, active) → only 503 authorized;
  (`none`, set, inactive) → nothing authorized — the `client` outcome
  is recorded (the marker is honored only while the transport is
  active; the transport is the first cause);
  (`service`, either, active) → only 503 authorized;
  (`service`, either, inactive) → nothing authorized — the write is
  suppressed and the outcome **stays** `service` (never
  reclassified);
  (`client`, either, either) → nothing authorized;
  (`response`, either, either) → no fresh first commit can occur
  (state unchanged; all subsequent writes pass through). A rejected
  commit delivers 0 bytes, returns a non-nil error, and records no
  status; later writes on the same writer are re-evaluated as first
  commits and stay rejected under the winning cancellation, so no
  partial body can leak; after an authorized commit every subsequent
  write passes through unchanged.
- Routing: every `docs/API.md` endpoint registered; `GET
  /v1/vector-spaces` **not** registered (request → 404 `not_found`);
  unknown path → 404; wrong method → 405 + `Allow`.
- Admin handlers (stubbed service): key grammars; duplicate → 409;
  credential creation returns the raw value exactly once; disable
  idempotence; unknown credential → 404; application lookup 404.
- `/healthz`: 200 with no pool, no flag, no readiness state involved.
- `/readyz`: flag set + ping ok → 200; ping failure → 503; flag unset →
  503; ping canceled by the operation deadline → exactly one 503
  `unavailable` is committed (the transport was active at the render
  decision; access `status_code` 503); ping canceled by a client
  disconnect → the write is suppressed, nothing is committed (access
  `status_code` 0).

**Integration (real PostgreSQL + pgvector, 5a harness; real server on a
test port):**

- Full data-plane path: register app + namespace + credential via the
  admin API; with the returned credential: upsert → get (field set, no
  `embedding`, hex hash) → search (match present, score ordering) →
  projection delete → object delete; request-ID echo throughout; one
  access line per request with the correct operation and identity
  fields.
- Auth planes: data-plane credential on an admin route → 401; admin
  token on a data-plane route → 401; missing header → 401 (uniform
  body).
- Error conformance: one 4xx and the 503 have exactly the catalog body
  shape; no SQLSTATE or driver text in any body; and, with structured
  logs captured for the battery, no log record carries the raw driver
  primary message, `P0001` trigger text, `22P02` detail, or any
  supplied payload (the diagnostic-record policy holds end-to-end — the
  release-critical regression for this is the 5b logging matrix).
- Readiness lifecycle: with the database stopped after startup,
  `/readyz` → 503 while `/healthz` → 200; restart → 200.
- Panic injection (test-only route): 500 `internal`, no stack in body,
  one access line `status_code` 500, same request ID everywhere.
- **Bounded shutdown:** (a) normal — signal while serving → no new
  connection accepted. First, a dedicated pre-signal case: one request
  is in flight, the test's client closes its connection, and the test
  waits until that request's access line is emitted — `status_code`
  0 with the `client` outcome (the abort is fully settled) — and only
  then sends the signal. The wave is then repeated several times
  against genuinely in-flight requests, and every request in the
  wave and in the pre-signal case is asserted against actual wire
  bytes and the access line. Commit is not proof of delivery, so the
  test asserts only what is observable: the committed status in the
  access line, and — for connections still open within the drain
  window — the bytes the client actually reads:
  - the pre-signal abort stands: no status is ever committed for that
    request (access `status_code` 0), and no 503 is committed either —
    a settled abort is never reclassified as service after the
    signal;
  - an in-flight request on a connection still open when the shutdown
    renders its settlement → **exactly one** 503 `unavailable` is
    committed (access `status_code` 503), and a client read within the
    drain window observes exactly the 503 catalog body (delivery is
    asserted on the live connection — never on a closed one);
  - a request that had already committed a success preserves that
    first status (its access line keeps that status; no second status
    or body on the wire);
  - a test route whose handler delays its success write until after
    the signal (the marker is published before the serve-root cancel,
    so the handler's 200 write then lands on the gate after the
    settlement) → the 200 first commit is rejected: no 200 is ever
    committed (the client, still open within the drain window, reads
    the 503 catalog body instead), and exactly one 503
    `unavailable` is committed (access `status_code` 503);
  - across the wave, access lines assert the first committed status:
    503 for settlements rendered while the transport was still active,
    0 only where the write was suppressed because the closure was
    already known at the render decision; a committed status is never
    retroactively zeroed;
  - pool close completes, process **exits 0** within grace + close
    bound (assert the exit status);
  (b) wedged lease — a held pool lease that is not released past both
  deadlines → process terminates on its own at ≈ grace + close bound,
  emits `shutdown_pool_close_timeout`, **exits 1** (the test's own
  longer timeout must not fire — no indefinite wait); (c) a second
  SIGTERM mid-shutdown changes nothing; (d) startup failure (failed
  migration step) → pool closed, non-zero exit, no listener ever
  opened.

## Out of scope

- In-process HTTP TLS termination (external reverse proxy owns it).
- Rate limiting, quotas, admission control beyond body caps.
- Metrics endpoints and tracing (not required for the first milestone).
- CORS configuration.
- API versioning mechanics beyond the fixed `/v1` base path.
- The C13 vector-spaces endpoint (blocked; 404 until the docs are
  revised).

## Open issues

- The default bound values (`VEC_HTTP_REQUEST_TIMEOUT` 30 s, shutdown
  30 s / 10 s, readiness ping 2 s) are proposals; they are
  configuration, so they can change without touching this design.
- Invalid inbound `X-Request-Id` → replace with a generated ID (rather
  than 400) is a proposal.
- `not_found` (404) and `method_not_allowed` (405) for unknown routes
  are new catalog entries not enumerated by `docs/API.md`; they are
  already in the overview catalog.
