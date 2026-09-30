package dbctx

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The unit matrix of the transaction-helper control flow (package 2,
// Validation, "helper control flow against a stubbed transaction seam"):
// every failure point — acquire, BEGIN, set_config, the callback, COMMIT,
// ROLLBACK — is exercised through the private txFactory seam with a
// scripted transaction, so the full lifecycle (including the lease release
// on every exit path) runs without a database. RLS and the
// transaction-scoped set_config itself are integration territory (the 5a
// harness) and are never mocked here.
//
// The shared cause rule (Settle) is covered by its own table matrix
// (TestSettle), so the helper tests only record outcomes through their
// synchronization and assert the settled outcome.

// fakeTx is a scripted pgx.Tx for the helper unit matrix. Every statement
// the helper issues is recorded in order (sql + arguments); per-method
// outcomes are scripted. Commit and Rollback may fail at most once (the
// real tx is closed after either, so a second call is ErrTxClosed).
type fakeTx struct {
	sqls []string // every statement in order (set_config, the callback's,
	// then COMMIT/ROLLBACK)
	args [][]any

	fn          func() error // the callback scripted by the test (nil: ok)
	commitErr   error        // scripted COMMIT failure (returned at most once)
	rollbackErr error        // scripted ROLLBACK failure (returned at most once)

	commitCalls   int
	rollbackCalls int
	closed        bool
}

var _ pgx.Tx = (*fakeTx)(nil)

func (f *fakeTx) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	if f.closed {
		return pgconn.CommandTag{}, pgx.ErrTxClosed
	}
	f.sqls = append(f.sqls, sql)
	f.args = append(f.args, arguments)
	return pgconn.CommandTag{}, nil
}

func (f *fakeTx) Commit(ctx context.Context) error {
	if f.closed {
		return pgx.ErrTxClosed
	}
	f.commitCalls++
	f.closed = true
	if f.commitErr != nil {
		err := f.commitErr
		f.commitErr = nil
		return err
	}
	return nil
}

func (f *fakeTx) Rollback(ctx context.Context) error {
	if f.closed {
		return pgx.ErrTxClosed
	}
	f.rollbackCalls++
	f.closed = true
	if f.rollbackErr != nil {
		err := f.rollbackErr
		f.rollbackErr = nil
		return err
	}
	return nil
}

// The unexercised Tx methods. The helper's control flow issues exactly one
// Exec for set_config (WithAppContext only), the callback's own SQL, and
// Commit or Rollback; these must never be called, so they report an error
// (or fail loudly) if one ever is.
func (f *fakeTx) Begin(ctx context.Context) (pgx.Tx, error) {
	return nil, errors.New("fakeTx: Begin was called")
}

func (f *fakeTx) CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	return 0, errors.New("fakeTx: CopyFrom was called")
}

func (f *fakeTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	return nil
}

func (f *fakeTx) LargeObjects() pgx.LargeObjects { return pgx.LargeObjects{} }

func (f *fakeTx) Prepare(ctx context.Context, name, sql string) (*pgconn.StatementDescription, error) {
	return nil, errors.New("fakeTx: Prepare was called")
}

func (f *fakeTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, errors.New("fakeTx: Query was called")
}

func (f *fakeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	panic("fakeTx: QueryRow was called")
}

func (f *fakeTx) Conn() *pgx.Conn { return nil }

// fakeFactory is the scripted txFactory seam. It scripts the acquisition
// + BEGIN outcome, hands out the scripted transaction, and counts release
// calls so the lease invariant (released on every exit path, exactly
// once) can be asserted.
type fakeFactory struct {
	beginErr   error
	txs        []pgx.Tx
	releases   int
	beginCalls int
}

var _ txFactory = (*fakeFactory)(nil)

func (f *fakeFactory) begin(ctx context.Context) (pgx.Tx, func(), error) {
	f.beginCalls++
	if f.beginErr != nil {
		return nil, nil, f.beginErr
	}
	// The scripted transaction is the one the test pre-built in f.txs
	// (with its scripted callback and per-method outcomes); the factory
	// acquires and begins exactly one transaction per call.
	if len(f.txs) == 0 {
		f.txs = append(f.txs, &fakeTx{fn: func() error { return nil }})
	}
	tx := f.txs[0]
	return tx, func() { f.releases++ }, nil
}

// errBegin is the default scripted acquisition/BEGIN failure.
var (
	errBegin     = errors.New("begin failed")
	errAcquire   = errors.New("acquire failed")
	errSetConfig = errors.New("set_config failed")
	errCommit    = errors.New("commit failed")
	errRollback  = errors.New("rollback failed")
	errBusiness  = errors.New("business error")
)

// wantOutcome describes one case's expected helper outcome.
type wantOutcome struct {
	isErr       error // errors.Is target, when the outcome is a sentinel
	isUnavail   bool  // the typed 503 outcome
	isBusiness  error // the business error must match via errors.Is
	hasRollback bool  // the outcome is a rollbackFailure wrapping something
	rollbackIs  error // errors.Is target for the attached rollback cause
	exactErr    string
	wantRelease int // expected release count (the lease invariant)
	wantSQL     []string
}

// runCase executes one helper matrix case through the seam and asserts the
// outcome, the lease release, and (when scripted) the statement order.
func runCase(t *testing.T, name string, appScope bool, appID string, f *fakeFactory, ctx context.Context, tc wantOutcome) {
	t.Helper()
	var err error
	if appScope {
		err = runTx(ctx, f, true, appID, callbackFn(f))
	} else {
		err = runTx(ctx, f, false, "", callbackFn(f))
	}
	assertOutcome(t, name, err, tc)
	if f.releases != tc.wantRelease {
		t.Errorf("%s: releases = %d, want %d (every exit path releases its lease exactly once)",
			name, f.releases, tc.wantRelease)
	}
	if tc.wantSQL != nil {
		var got []string
		for _, tx := range f.txs {
			if ftx, ok := tx.(*fakeTx); ok {
				got = append(got, ftx.sqls...)
			}
		}
		if len(got) != len(tc.wantSQL) {
			t.Errorf("%s: statements = %v, want %v", name, got, tc.wantSQL)
			return
		}
		for i := range tc.wantSQL {
			if got[i] != tc.wantSQL[i] {
				t.Errorf("%s: statement %d = %q, want %q", name, i, got[i], tc.wantSQL[i])
			}
		}
	}
}

// callbackFn wires the test's scripted callback into the transaction: it
// returns the scripted business error (nil when none is scripted).
func callbackFn(f *fakeFactory) func(pgx.Tx) error {
	return func(tx pgx.Tx) error {
		ftx, ok := tx.(*fakeTx)
		if !ok || ftx.fn == nil {
			return nil
		}
		return ftx.fn()
	}
}

func assertOutcome(t *testing.T, name string, err error, tc wantOutcome) {
	t.Helper()
	if tc.isErr != nil && !errors.Is(err, tc.isErr) {
		t.Errorf("%s: outcome = %v, want errors.Is %v", name, err, tc.isErr)
	}
	if tc.isUnavail {
		var u *Unavailable
		if !errors.As(err, &u) {
			t.Errorf("%s: outcome = %v, want the typed unavailable outcome", name, err)
			return
		}
		if u.Error() != "unavailable" {
			t.Errorf("%s: unavailable message = %q, want the stable message", name, u.Error())
		}
	}
	if tc.isBusiness != nil {
		if !errors.Is(err, tc.isBusiness) {
			t.Errorf("%s: outcome = %v, want the business error %v to match through Unwrap", name, err, tc.isBusiness)
		}
	}
	if tc.hasRollback {
		var rf *rollbackFailure
		if !errors.As(err, &rf) {
			t.Errorf("%s: outcome = %v, want a rollbackFailure", name, err)
			return
		}
		if tc.rollbackIs != nil && !errors.Is(rf.RollbackCause(), tc.rollbackIs) {
			t.Errorf("%s: rollback cause = %v, want errors.Is %v", name, rf.RollbackCause(), tc.rollbackIs)
		}
	}
	if tc.exactErr != "" && err.Error() != tc.exactErr {
		t.Errorf("%s: rendered message = %q, want %q", name, err.Error(), tc.exactErr)
	}
}

// TestSettle is the shared cause rule matrix (overview error model): the
// rule reads the request's settlement state through its synchronization —
// a client outcome, or no outcome with the shutdown marker unset, yields
// the abort sentinel; a service outcome, or no outcome with the marker
// set, yields the typed unavailable outcome; a response outcome means the
// committed status stands (no further response). The marker never
// reclassifies a request whose per-request outcome already won.
func TestSettle(t *testing.T) {
	cases := []struct {
		name    string
		outcome Outcome
		marker  bool
		want    error // nil: the response outcome stands; ErrAbortOutcome; or the Unavailable marker
		wantUn  bool
	}{
		{"no state, no marker", OutcomeNone, false, ErrAbortOutcome, false},
		{"client outcome", OutcomeClient, false, ErrAbortOutcome, false},
		{"service outcome", OutcomeService, false, nil, true},
		{"response outcome stands", OutcomeResponse, false, nil, false},
		{"no outcome, marker set", OutcomeNone, true, nil, true},
		{"client outcome, marker set", OutcomeClient, true, ErrAbortOutcome, false},
		{"service outcome, marker set", OutcomeService, true, nil, true},
	}
	t.Cleanup(resetShutdown)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.marker {
				MarkShutdown()
			} else {
				resetShutdown()
			}
			ctx := context.Background()
			if tc.outcome != OutcomeNone {
				st := &SettlementState{}
				st.Claim(tc.outcome)
				ctx = WithSettlement(ctx, st)
			}
			got := Settle(ctx)
			if tc.wantUn {
				var u *Unavailable
				if !errors.As(got, &u) {
					t.Fatalf("Settle = %v, want the typed unavailable outcome", got)
				}
				return
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("Settle = %v, want nil (the committed status stands)", got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("Settle = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSettlementStateClaim is the first-claim-wins matrix: the first of the
// three outcome claims records the outcome; later claims are no-ops and
// never reclassify the request.
func TestSettlementStateClaim(t *testing.T) {
	st := &SettlementState{}
	if !st.Claim(OutcomeClient) {
		t.Fatal("first claim must win")
	}
	if st.Outcome() != OutcomeClient {
		t.Fatalf("outcome = %v, want client", st.Outcome())
	}
	if st.Claim(OutcomeService) {
		t.Fatal("a later claim must not win")
	}
	if st.Outcome() != OutcomeClient {
		t.Fatalf("outcome reclassified to %v; a claim never reclassifies", st.Outcome())
	}
}

// TestRunTxAcquire is the acquisition-failure matrix (package 2,
// Validation): while the effective context is no longer active the failure
// settles by the shared cause rule (client → abort sentinel; service →
// unavailable; no outcome with the marker set → unavailable; the markerless
// transport cancel → abort sentinel); while the context is still active the
// failure is the typed unavailable outcome. No transaction begins, so no
// lease is held and nothing is released.
func TestRunTxAcquire(t *testing.T) {
	cases := []struct {
		name    string
		cancel  bool
		outcome Outcome
		marker  bool
		want    wantOutcome
	}{
		{
			name:    "inactive, client outcome",
			cancel:  true,
			outcome: OutcomeClient,
			want:    wantOutcome{isErr: ErrAbortOutcome, wantRelease: 0},
		},
		{
			name:    "inactive, service outcome",
			cancel:  true,
			outcome: OutcomeService,
			want:    wantOutcome{isUnavail: true, wantRelease: 0},
		},
		{
			name:   "inactive, no outcome, marker set",
			cancel: true,
			marker: true,
			want:   wantOutcome{isUnavail: true, wantRelease: 0},
		},
		{
			name:   "inactive, no outcome, no marker (markerless transport cancel)",
			cancel: true,
			want:   wantOutcome{isErr: ErrAbortOutcome, wantRelease: 0},
		},
		{
			name: "active",
			want: wantOutcome{isUnavail: true, wantRelease: 0},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := ctxWithOutcome(t, tc)
			f := &fakeFactory{beginErr: errAcquire}
			runCase(t, tc.name, false, "", f, ctx, tc.want)
		})
	}
}

// TestRunTxBegin is the BEGIN-failure matrix: an active context yields the
// typed unavailable outcome; an inactive context settles by the shared
// cause rule. The lease is never acquired on a BEGIN failure, so nothing
// is released.
func TestRunTxBegin(t *testing.T) {
	cases := []struct {
		name    string
		cancel  bool
		outcome Outcome
		marker  bool
		want    wantOutcome
	}{
		{
			name: "active",
			want: wantOutcome{isUnavail: true, wantRelease: 0},
		},
		{
			name:    "inactive, client outcome",
			cancel:  true,
			outcome: OutcomeClient,
			want:    wantOutcome{isErr: ErrAbortOutcome, wantRelease: 0},
		},
		{
			name:    "inactive, service outcome",
			cancel:  true,
			outcome: OutcomeService,
			want:    wantOutcome{isUnavail: true, wantRelease: 0},
		},
		{
			name:   "inactive, no outcome, marker set",
			cancel: true,
			marker: true,
			want:   wantOutcome{isUnavail: true, wantRelease: 0},
		},
		{
			name:   "inactive, no outcome, no marker",
			cancel: true,
			want:   wantOutcome{isErr: ErrAbortOutcome, wantRelease: 0},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := ctxWithOutcome(t, tc)
			f := &fakeFactory{beginErr: errBegin}
			runCase(t, tc.name, false, "", f, ctx, tc.want)
		})
	}
}

// TestRunTxSetConfig is the set_config-failure matrix (WithAppContext
// only): an active context yields the typed unavailable outcome; an
// inactive context settles by the shared cause rule. The lease is released
// on every exit path.
func TestRunTxSetConfig(t *testing.T) {
	cases := []struct {
		name    string
		cancel  bool
		outcome Outcome
		marker  bool
		want    wantOutcome
	}{
		{
			name: "active",
			want: wantOutcome{isUnavail: true, wantRelease: 1},
		},
		{
			name:    "inactive, client outcome",
			cancel:  true,
			outcome: OutcomeClient,
			want:    wantOutcome{isErr: ErrAbortOutcome, wantRelease: 1},
		},
		{
			name:    "inactive, service outcome",
			cancel:  true,
			outcome: OutcomeService,
			want:    wantOutcome{isUnavail: true, wantRelease: 1},
		},
		{
			name:   "inactive, no outcome, marker set",
			cancel: true,
			marker: true,
			want:   wantOutcome{isUnavail: true, wantRelease: 1},
		},
		{
			name:   "inactive, no outcome, no marker",
			cancel: true,
			want:   wantOutcome{isErr: ErrAbortOutcome, wantRelease: 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := ctxWithOutcome(t, tc)
			f := &fakeFactory{}
			f.txs = append(f.txs, &failingExecTx{inner: &fakeTx{fn: func() error { return nil }}, failSQL: setConfigSQL, failErr: errSetConfig})
			runCase(t, tc.name, true, "0199f31e-2000-7000-8000-0000000000a1", f, ctx, tc.want)
		})
	}
}

// ctxWithOutcome builds a test context for a helper matrix case: the
// marker, the settlement outcome (claimed through the state's
// synchronization), and the active/inactive state.
func ctxWithOutcome(t *testing.T, tc struct {
	name    string
	cancel  bool
	outcome Outcome
	marker  bool
	want    wantOutcome
}) context.Context {
	t.Helper()
	resetShutdown()
	if tc.marker {
		MarkShutdown()
	}
	t.Cleanup(resetShutdown)

	ctx := context.Background()
	if tc.outcome != OutcomeNone {
		st := &SettlementState{}
		st.Claim(tc.outcome)
		ctx = WithSettlement(ctx, st)
	}
	if tc.cancel {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		cancel()
	}
	return ctx
}

// failingExecTx wraps a fakeTx and fails a specific SQL (the set_config
// statement) with a scripted error; all other statements pass through to
// the inner tx.
type failingExecTx struct {
	inner   *fakeTx
	failSQL string
	failErr error
}

var _ pgx.Tx = (*failingExecTx)(nil)

func (f *failingExecTx) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	if sql == f.failSQL {
		return pgconn.CommandTag{}, f.failErr
	}
	return f.inner.Exec(ctx, sql, arguments...)
}

func (f *failingExecTx) Commit(ctx context.Context) error { return f.inner.Commit(ctx) }

func (f *failingExecTx) Rollback(ctx context.Context) error { return f.inner.Rollback(ctx) }

func (f *failingExecTx) Begin(ctx context.Context) (pgx.Tx, error) { return f.inner.Begin(ctx) }

func (f *failingExecTx) CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	return f.inner.CopyFrom(ctx, tableName, columnNames, rowSrc)
}

func (f *failingExecTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	return f.inner.SendBatch(ctx, b)
}

func (f *failingExecTx) LargeObjects() pgx.LargeObjects { return f.inner.LargeObjects() }

func (f *failingExecTx) Prepare(ctx context.Context, name, sql string) (*pgconn.StatementDescription, error) {
	return f.inner.Prepare(ctx, name, sql)
}

func (f *failingExecTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return f.inner.Query(ctx, sql, args...)
}

func (f *failingExecTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return f.inner.QueryRow(ctx, sql, args)
}

func (f *failingExecTx) Conn() *pgx.Conn { return f.inner.Conn() }

// TestRunTxCallbackError is the business-error matrix: a business error
// returned by the callback passes through the helper unchanged (classified
// exactly once at the handler boundary) — errors.Is matches the original
// business error — while ROLLBACK runs and the lease is released.
func TestRunTxCallbackError(t *testing.T) {
	t.Run("WithAppContext", func(t *testing.T) {
		f := &fakeFactory{}
		f.txs = append(f.txs, &fakeTx{
			fn: func() error {
				// The callback's own business SQL (after set_config).
				f.txs[0].(*fakeTx).sqls = append(f.txs[0].(*fakeTx).sqls, "SELECT business")
				return errBusiness
			},
		})
		tc := wantOutcome{
			isBusiness:  errBusiness,
			wantRelease: 1,
			wantSQL:     []string{setConfigSQL, "SELECT business"},
		}
		runCase(t, "business error", true, "0199f31e-2000-7000-8000-0000000000a1", f, context.Background(), tc)
	})
	t.Run("WithShortTx", func(t *testing.T) {
		f := &fakeFactory{}
		f.txs = append(f.txs, &fakeTx{
			fn: func() error { return errBusiness },
		})
		tc := wantOutcome{
			isBusiness:  errBusiness,
			wantRelease: 1,
			wantSQL:     []string{},
		}
		runCase(t, "business error", false, "", f, context.Background(), tc)
	})
}

// TestRunTxCommitFailure is the COMMIT-failure matrix: a COMMIT failure
// after a successful callback is itself the helper outcome (rule 1, then
// rule 2) — active context → the typed unavailable outcome; inactive →
// the settled outcome — while ROLLBACK still runs and the lease is
// released.
func TestRunTxCommitFailure(t *testing.T) {
	cases := []struct {
		name    string
		cancel  bool
		outcome Outcome
		marker  bool
		want    wantOutcome
	}{
		{
			name: "active",
			want: wantOutcome{isUnavail: true, wantRelease: 1},
		},
		{
			name:    "inactive, client outcome",
			cancel:  true,
			outcome: OutcomeClient,
			want:    wantOutcome{isErr: ErrAbortOutcome, wantRelease: 1},
		},
		{
			name:    "inactive, service outcome",
			cancel:  true,
			outcome: OutcomeService,
			want:    wantOutcome{isUnavail: true, wantRelease: 1},
		},
		{
			name:   "inactive, no outcome, marker set",
			cancel: true,
			marker: true,
			want:   wantOutcome{isUnavail: true, wantRelease: 1},
		},
		{
			name:   "inactive, no outcome, no marker",
			cancel: true,
			want:   wantOutcome{isErr: ErrAbortOutcome, wantRelease: 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := ctxWithOutcome(t, tc)
			f := &fakeFactory{}
			f.txs = append(f.txs, &fakeTx{
				fn:        func() error { return nil },
				commitErr: errCommit,
			})
			runCase(t, tc.name, false, "", f, ctx, tc.want)
		})
	}
}

// TestRunTxRollbackFailure is the rollback-failure matrix (the rollback-
// error bug): a ROLLBACK failure after a business error is attached to the
// original error — the business error's identity and stable rendering
// stand (errors.Is matches the business error; the rendered message is the
// business error's) — and the lease is released.
func TestRunTxRollbackFailure(t *testing.T) {
	t.Run("rollback failure after a business error: original error retained", func(t *testing.T) {
		f := &fakeFactory{}
		f.txs = append(f.txs, &fakeTx{
			fn:          func() error { return errBusiness },
			rollbackErr: errRollback,
		})
		tc := wantOutcome{
			isBusiness:  errBusiness, // the business error matches through Unwrap
			hasRollback: true,
			rollbackIs:  errRollback,
			exactErr:    "business error", // the stable original rendering stands
			wantRelease: 1,
		}
		runCase(t, "rollback failure after fn error", true, "0199f31e-2000-7000-8000-0000000000a1", f, context.Background(), tc)
	})
	t.Run("rollback failure after a set_config failure: infra outcome retained", func(t *testing.T) {
		f := &fakeFactory{}
		f.txs = append(f.txs, &failingExecTx{inner: &fakeTx{fn: func() error { return nil }, rollbackErr: errRollback}, failSQL: setConfigSQL, failErr: errSetConfig})
		ctx := context.Background()
		err := runTx(ctx, f, true, "0199f31e-2000-7000-8000-0000000000a1", callbackFn(f))
		var u *Unavailable
		if !errors.As(err, &u) {
			t.Fatalf("outcome = %v, want the typed unavailable outcome retained", err)
		}
		var rf *rollbackFailure
		if !errors.As(err, &rf) {
			t.Fatalf("outcome = %v, want the rollback failure attached", err)
		}
		if !errors.Is(rf.RollbackCause(), errRollback) {
			t.Fatalf("rollback cause = %v, want %v", rf.RollbackCause(), errRollback)
		}
		if f.releases != 1 {
			t.Fatalf("releases = %d, want 1", f.releases)
		}
	})
}

// TestRollbackFailureAttachment is the attachment semantics of
// rollbackFailure: the original failure's identity and stable rendering
// stand (errors.Is matches the original; the rendered message is the
// original's); the rollback cause is attached for diagnostics; and when
// the rollback failure stands alone (no preceding failure) it renders its
// own stable message.
func TestRollbackFailureAttachment(t *testing.T) {
	t.Run("business error + rollback failure", func(t *testing.T) {
		rf := &rollbackFailure{original: errBusiness, rollback: errRollback}
		if rf.Error() != "business error" {
			t.Fatalf("rendered = %q, want the business error's stable message", rf.Error())
		}
		if !errors.Is(rf, errBusiness) {
			t.Fatal("errors.Is(rf, business) must match through Unwrap")
		}
		if !errors.Is(rf, errRollback) {
			t.Fatal("errors.Is(rf, rollback) must match the attached cause")
		}
		if rf.Original() != errBusiness {
			t.Fatal("Original() must return the original failure")
		}
		if rf.RollbackCause() != errRollback {
			t.Fatal("RollbackCause() must return the rollback cause")
		}
	})
	t.Run("cleanup-only rollback failure stands alone", func(t *testing.T) {
		rf := &rollbackFailure{original: nil, rollback: errRollback}
		if rf.Error() != "transaction rollback failed" {
			t.Fatalf("rendered = %q, want the stable cleanup message", rf.Error())
		}
		if !errors.Is(rf, errRollback) {
			t.Fatal("errors.Is(rf, rollback) must match the attached cause")
		}
		if rf.Original() != nil {
			t.Fatal("Original() must be nil when the rollback failure stands alone")
		}
	})
}

// TestRunTxFnOrder is the first-statement ordering matrix (invariant 3):
// in WithAppContext the set_config statement is the FIRST statement of the
// transaction — it precedes the callback's SQL and any COMMIT/ROLLBACK —
// and in WithShortTx no set_config is executed. The UUID is bound as a
// parameter (never interpolated).
func TestRunTxFnOrder(t *testing.T) {
	t.Run("WithAppContext: set_config is the first statement", func(t *testing.T) {
		f := &fakeFactory{}
		f.txs = append(f.txs, &fakeTx{
			fn: func() error {
				// The callback's business SQL runs after set_config.
				f.txs[0].(*fakeTx).sqls = append(f.txs[0].(*fakeTx).sqls, "SELECT business")
				return nil
			},
		})
		err := runTx(context.Background(), f, true, "0199f31e-2000-7000-8000-0000000000a1", callbackFn(f))
		if err != nil {
			t.Fatalf("runTx = %v, want nil (success path)", err)
		}
		tx := f.txs[0].(*fakeTx)
		if len(tx.sqls) < 2 {
			t.Fatalf("statements = %v, want at least [set_config, business]", tx.sqls)
		}
		if tx.sqls[0] != setConfigSQL {
			t.Fatalf("first statement = %q, want the set_config statement (invariant 3)", tx.sqls[0])
		}
		if tx.sqls[len(tx.sqls)-1] != "SELECT business" {
			t.Fatalf("last Exec statement = %q, want the callback's business SQL on the success path", tx.sqls[len(tx.sqls)-1])
		}
		if tx.commitCalls != 1 {
			t.Fatalf("commitCalls = %d, want 1 (the helper commits on the success path)", tx.commitCalls)
		}
		if tx.rollbackCalls != 0 {
			t.Fatalf("rollbackCalls = %d, want 0 (no rollback on the success path)", tx.rollbackCalls)
		}
		// The UUID is bound as a parameter, never interpolated.
		setArgs := tx.args[0]
		if len(setArgs) != 1 || setArgs[0] != "0199f31e-2000-7000-8000-0000000000a1" {
			t.Fatalf("set_config arguments = %v, want the app UUID bound as a single parameter", setArgs)
		}
	})
	t.Run("WithShortTx: no set_config is executed", func(t *testing.T) {
		f := &fakeFactory{}
		f.txs = append(f.txs, &fakeTx{
			fn: func() error {
				f.txs[0].(*fakeTx).sqls = append(f.txs[0].(*fakeTx).sqls, "SELECT business")
				return nil
			},
		})
		err := runTx(context.Background(), f, false, "", callbackFn(f))
		if err != nil {
			t.Fatalf("runTx = %v, want nil (success path)", err)
		}
		tx := f.txs[0].(*fakeTx)
		for _, s := range tx.sqls {
			if s == setConfigSQL {
				t.Fatal("WithShortTx executed set_config; no set_config may run (no app context)")
			}
		}
	})
}

// TestRunTxLeaseRelease is the lease invariant (every helper exit path
// releases its pool lease exactly once): the success path, the business-
// error path, the commit-failure path, the rollback-failure path, and the
// set_config-failure path all release once; the acquire and BEGIN failure
// paths release nothing (nothing was acquired).
func TestRunTxLeaseRelease(t *testing.T) {
	cases := []struct {
		name        string
		build       func() *fakeFactory
		appScope    bool
		wantRelease int
	}{
		{
			name: "success path",
			build: func() *fakeFactory {
				f := &fakeFactory{}
				f.txs = append(f.txs, &fakeTx{fn: func() error { return nil }})
				return f
			},
			appScope:    true,
			wantRelease: 1,
		},
		{
			name: "business error path",
			build: func() *fakeFactory {
				f := &fakeFactory{}
				f.txs = append(f.txs, &fakeTx{fn: func() error { return errBusiness }})
				return f
			},
			appScope:    true,
			wantRelease: 1,
		},
		{
			name: "commit failure path",
			build: func() *fakeFactory {
				f := &fakeFactory{}
				f.txs = append(f.txs, &fakeTx{fn: func() error { return nil }, commitErr: errCommit})
				return f
			},
			appScope:    true,
			wantRelease: 1,
		},
		{
			name: "rollback failure after business error",
			build: func() *fakeFactory {
				f := &fakeFactory{}
				f.txs = append(f.txs, &fakeTx{fn: func() error { return errBusiness }, rollbackErr: errRollback})
				return f
			},
			appScope:    true,
			wantRelease: 1,
		},
		{
			name: "set_config failure",
			build: func() *fakeFactory {
				f := &fakeFactory{}
				f.txs = append(f.txs, &failingExecTx{inner: &fakeTx{fn: func() error { return nil }}, failSQL: setConfigSQL, failErr: errSetConfig})
				return f
			},
			appScope:    true,
			wantRelease: 1,
		},
		{
			name: "acquire failure (no lease held)",
			build: func() *fakeFactory {
				return &fakeFactory{beginErr: errAcquire}
			},
			appScope:    true,
			wantRelease: 0,
		},
		{
			name: "BEGIN failure (no lease held)",
			build: func() *fakeFactory {
				return &fakeFactory{beginErr: errBegin}
			},
			appScope:    true,
			wantRelease: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.build()
			runTx(context.Background(), f, tc.appScope, "0199f31e-2000-7000-8000-0000000000a1", callbackFn(f))
			if f.releases != tc.wantRelease {
				t.Fatalf("releases = %d, want %d (every exit path releases exactly once; acquire/BEGIN failures hold no lease)",
					f.releases, tc.wantRelease)
			}
			if tc.wantRelease > 0 {
				// The transaction was settled exactly once: a commit that
				// succeeds sets committed=true (no rollback), and every
				// exit that did not commit runs exactly one rollback.
				for _, tx := range f.txs {
					if ftx, ok := tx.(*fakeTx); ok {
						total := ftx.commitCalls + ftx.rollbackCalls
						if total != 1 {
							t.Fatalf("%s: commit+rollback calls = %d, want exactly 1 (the helper settles the transaction exactly once)",
								tc.name, total)
						}
					}
				}
			}
		})
	}
}

// TestRunTxAcquireCancellationSettlement is the acquisition-cancellation
// settlement case: when the request context is canceled before acquisition
// and the settlement state records a client outcome, the abort sentinel is
// the outcome — the request is settled as a client-initiated cause, no
// response is written, and the lease is never held.
func TestRunTxAcquireCancellationSettlement(t *testing.T) {
	st := &SettlementState{}
	st.Claim(OutcomeClient)
	ctx := WithSettlement(context.Background(), st)
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	f := &fakeFactory{beginErr: errAcquire}
	err := runTx(cancelCtx, f, false, "", callbackFn(f))
	if !errors.Is(err, ErrAbortOutcome) {
		t.Fatalf("outcome = %v, want the abort sentinel (client settlement)", err)
	}
	if f.releases != 0 {
		t.Fatalf("releases = %d, want 0 (no lease was acquired)", f.releases)
	}
	if !IsAbort(err) {
		t.Fatal("IsAbort must report the abort sentinel")
	}
}

// TestNoSecretsInErrors verifies no error message in this package carries
// request or credential material: every typed outcome renders a stable,
// input-free message, and a rollback failure attached to a business error
// renders the business error's message (the attached cause is diagnostic
// only).
func TestNoSecretsInErrors(t *testing.T) {
	errs := []error{
		ErrAbortOutcome,
		&Unavailable{Cause: errors.New("connection refused by database")},
		&rollbackFailure{original: errors.New("business error"), rollback: errors.New("rollback failed: FATAL: password required")},
	}
	for _, err := range errs {
		msg := err.Error()
		if msg == "" {
			t.Fatal("an outcome rendered an empty message")
		}
	}
	u := &Unavailable{Cause: errors.New("connection refused by database")}
	if u.Error() != "unavailable" {
		t.Fatalf("Unavailable message = %q, want the stable message (the cause is diagnostic only)", u.Error())
	}
	rf := &rollbackFailure{original: errors.New("business error"), rollback: errors.New("rollback failed: FATAL: password required")}
	if rf.Error() != "business error" {
		t.Fatalf("rollbackFailure message = %q, want the original's stable message", rf.Error())
	}
}
