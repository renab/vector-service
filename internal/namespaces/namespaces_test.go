package namespaces

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"vector-service/internal/dbctx"
)

// The unit matrix of the namespace-resolution slice (no database,
// table-driven): the key grammar/size validation, the uniform 404 outcome
// for every lookup failure shape, the app-identity handling, and the
// result/context API. The real-PostgreSQL namespace isolation matrix (RLS
// active, foreign namespace, disabled namespace) is integration territory
// (resolve_integration_test.go, 5a harness) and is never mocked here.

const testAppID = "0199f31e-2000-7000-8000-0000000000a1"

// errInfra is the scripted database-infrastructure failure the passthrough
// case asserts on (a stable identity, so errors.Is matches it exactly).
var errInfra = errors.New("connection refused")

// ---------------------------------------------------------------------------
// Key grammar and size validation
// ---------------------------------------------------------------------------

func TestValidateKey(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"empty", "", true},
		{"lowercase letters", "project-alpha", false},
		{"with digits", "namespace-2", false},
		{"with underscore", "research_notes", false},
		{"with dash", "project-alpha", false},
		{"single letter", "a", false},
		{"max length 64", "a" + repeat("b", 63), false},
		{"over length 64", "a" + repeat("b", 64), true},
		{"uppercase", "Project", true},
		{"leading digit", "1alpha", true},
		{"leading dash", "-alpha", true},
		{"leading underscore", "_alpha", true},
		{"uppercase inside", "project-Alpha", true},
		{"space", "project alpha", true},
		{"dot", "project.alpha", true},
		{"slash", "project/alpha", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateKey(tc.key)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateKey(%q) error = %v, want error = %v", tc.key, err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("ValidateKey(%q) error = %v, want ErrInvalidKey", tc.key, err)
			}
		})
	}
}

// TestValidateKeyDoesNotEchoInput proves a failed key never appears in the
// error message (the key is caller-controlled request material; logging
// discipline forbids echoing arbitrary caller content).
func TestValidateKeyDoesNotEchoInput(t *testing.T) {
	secret := "UPPERCASE-should-not-appear"
	err := ValidateKey(secret)
	if err == nil {
		t.Fatal("ValidateKey accepted an invalid key")
	}
	if contains(err.Error(), secret) {
		t.Fatalf("error message echoes the input key: %q", err.Error())
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The uniform 404 outcome
// ---------------------------------------------------------------------------

// TestErrNotFoundUniform proves the single typed failure shape: the same
// value for every failure kind, the stable message, and no identity or
// enumeration in it.
func TestErrNotFoundUniform(t *testing.T) {
	if ErrNotFound.Error() != MessageNotFound {
		t.Fatalf("ErrNotFound message = %q, want %q", ErrNotFound.Error(), MessageNotFound)
	}
	if CodeNotFound != "namespace_not_found" {
		t.Fatalf("CodeNotFound = %q, want namespace_not_found", CodeNotFound)
	}
}

// ---------------------------------------------------------------------------
// In-transaction lookup (the primitive behind the composed Resolve)
// ---------------------------------------------------------------------------

// fakeTx is a scripted pgx.Tx for the resolution unit matrix: it records
// every statement in order and returns the scripted row or error from
// QueryRow.
type fakeTx struct {
	sqls []string
	args [][]any

	row    []any // the scripted row (nil: no rows)
	rowErr error // a scripted non-no-rows error
	calls  int
	closed bool
}

var _ pgx.Tx = (*fakeTx)(nil)

func (f *fakeTx) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	f.sqls = append(f.sqls, sql)
	f.args = append(f.args, arguments)
	return pgconn.CommandTag{}, nil
}

func (f *fakeTx) Commit(ctx context.Context) error {
	f.closed = true
	return nil
}

func (f *fakeTx) Rollback(ctx context.Context) error {
	f.closed = true
	return nil
}

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
	f.sqls = append(f.sqls, sql)
	f.args = append(f.args, args)
	f.calls++
	return fakeRow{row: f.row, err: f.rowErr}
}

func (f *fakeTx) Conn() *pgx.Conn { return nil }

type fakeRow struct {
	row []any
	err error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(r.row) == 0 {
		return pgx.ErrNoRows
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *string:
			*d, _ = r.row[i].(string)
		default:
			return errors.New("fakeRow: unsupported scan target")
		}
	}
	return nil
}

// TestResolveInTxUniform404 guards the primitive's contract directly:
// no row, a disabled row (modeled here as no row — the enabled predicate
// filters it in SQL), and a foreign row (modeled as no row — the
// application_id predicate filters it) all yield the uniform ErrNotFound,
// and the lookup binds the app identity and key as parameters (never
// interpolated).
func TestResolveInTxUniform404(t *testing.T) {
	appID := "0199f31e-2000-7000-8000-0000000000a1"
	cases := []struct {
		name string
		row  []any
	}{
		{"missing namespace", nil},
		{"disabled namespace", nil},
		{"foreign namespace", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := &fakeTx{row: tc.row}
			_, err := resolveInTx(context.Background(), tx, appID, "project-alpha")
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("resolveInTx error = %v, want ErrNotFound", err)
			}
			// The lookup must bind the application identity and the key
			// as parameters, never interpolate them.
			if len(tx.sqls) != 1 || len(tx.args) != 1 || len(tx.args[0]) != 2 {
				t.Fatalf("lookup statement = %v, args = %v; want one statement with two bound parameters",
					tx.sqls, tx.args)
			}
			if tx.args[0][0] != appID || tx.args[0][1] != "project-alpha" {
				t.Fatalf("lookup bound args = %v, want [%s project-alpha]", tx.args[0], appID)
			}
		})
	}
}

// TestResolveInTxNoRowsVariants proves every no-rows error shape (pgx and
// database/sql) settles to the uniform 404, while a database-infrastructure
// error passes through unchanged (the helper boundary classifies it).
func TestResolveInTxNoRowsVariants(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"pgx no rows", pgx.ErrNoRows, ErrNotFound},
		{"sql no rows", sql.ErrNoRows, ErrNotFound},
		{"infra error passes through", errInfra, errInfra},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := &fakeTx{rowErr: tc.err}
			_, err := resolveInTx(ctx, tx, testAppID, "project-alpha")
			if !errors.Is(err, tc.want) {
				t.Fatalf("resolveInTx error = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

// TestResolveInTxSuccess returns the row's UUID and key on a matching
// enabled row.
func TestResolveInTxSuccess(t *testing.T) {
	nsID := "0199f31e-3000-7000-8000-000000000007"
	tx := &fakeTx{row: []any{nsID, "project-alpha"}}
	r, err := resolveInTx(context.Background(), tx, testAppID, "project-alpha")
	if err != nil {
		t.Fatalf("resolveInTx error = %v, want nil", err)
	}
	if r.NamespaceID != nsID || r.Key != "project-alpha" {
		t.Fatalf("result = %+v, want {%s project-alpha}", r, nsID)
	}
}

// TestResolveAppIDMissing proves a context without a valid application
// identity fails with the typed programming error, before any SQL (invariant
// 1: identity flows only from the authenticated credential).
func TestResolveAppIDMissing(t *testing.T) {
	cases := []struct {
		name  string
		appID string
	}{
		{"no app id on context", ""},
		{"malformed app id", "not-a-uuid"},
		{"too short", "0199f31e"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.appID != "" {
				ctx = dbctx.WithAppID(ctx, tc.appID)
			}
			if _, _, err := Resolve(ctx, poolForTest(t), "project-alpha"); !errors.Is(err, ErrInvalidApp) {
				t.Fatalf("Resolve error = %v, want ErrInvalidApp", err)
			}
		})
	}
}

// TestResolveInvalidKey proves that a malformed key fails with the typed
// validation error before any database access.
func TestResolveInvalidKey(t *testing.T) {
	ctx := dbctx.WithAppID(context.Background(), testAppID)
	cases := []struct {
		name string
		key  string
	}{
		{"empty key", ""},
		{"uppercase key", "Project"},
		{"leading digit", "1alpha"},
		{"too long", "a" + repeat("b", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := Resolve(ctx, poolForTest(t), tc.key); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("Resolve(%q) error = %v, want ErrInvalidKey", tc.key, err)
			}
		})
	}
}

// poolForTest returns a pool that is never reached: both validation and
// app-identity failures settle before the transaction opens.
func poolForTest(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pc, err := pgxpool.ParseConfig("host=127.0.0.1 port=1 dbname=none user=none sslmode=disable connect_timeout=1")
	if err != nil {
		t.Fatalf("parse dummy pool config: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatalf("open dummy pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestContextRoundTrip proves the result/context API: a resolved namespace
// is attached to the context, Namespace returns it unchanged, and a context
// without one reports false.
func TestContextRoundTrip(t *testing.T) {
	r := Result{NamespaceID: "0199f31e-3000-7000-8000-000000000007", Key: "project-alpha"}

	got, ok := Namespace(context.Background())
	if ok || got != (Result{}) {
		t.Fatalf("Namespace on a bare context = (%+v, %v), want (zero, false)", got, ok)
	}

	ctx := WithContext(context.Background(), r)
	got, ok = Namespace(ctx)
	if !ok || got != r {
		t.Fatalf("Namespace on a resolved context = (%+v, %v), want (%+v, true)", got, ok, r)
	}

	// The app identity survives the context attachment (the operation
	// binds both the app and the namespace as parameters downstream).
	ctx2 := WithContext(dbctx.WithAppID(context.Background(), testAppID), r)
	if dbctx.AppID(ctx2) != testAppID {
		t.Fatalf("app identity lost through WithContext: %q", dbctx.AppID(ctx2))
	}
}
