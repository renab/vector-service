package privilege

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier runs a single SELECT statement and returns its rows as raw text
// fields. NULL fields are nil. The two adapters cover the two connection
// types the assertions run on: *pgx.Conn (standalone probes, migration
// connection) and *pgconn.PgConn (pool connections, where the assertion runs
// from the connection's AfterConnect hook and no *pgx.Conn exists).
//
// The text contract is load-bearing: the catalog loaders parse field text
// (OIDs as decimals, booleans as "t"/"f", pg_type.typelem as the element
// type's plain OID decimal, 0 for non-arrays). A *pgx.Conn must therefore
// be configured to return text results, for example with
// DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol — pgx's default
// extended protocol returns binary results, which the loaders do not
// accept.
type Querier interface {
	Query(ctx context.Context, sql string) ([][][]byte, error)
}

// PgxQuerier adapts *pgx.Conn to Querier.
type PgxQuerier struct {
	Conn *pgx.Conn
}

// Query implements Querier.
func (q PgxQuerier) Query(ctx context.Context, sql string) ([][][]byte, error) {
	rows, err := q.Conn.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ncols := len(rows.FieldDescriptions())
	var out [][][]byte
	for rows.Next() {
		fields := make([][]byte, ncols)
		args := make([]any, ncols)
		for i := range fields {
			args[i] = &fields[i]
		}
		if err := rows.Scan(args...); err != nil {
			return nil, err
		}
		out = append(out, fields)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// PgconnQuerier adapts *pgconn.PgConn to Querier over the simple query
// protocol.
type PgconnQuerier struct {
	Conn *pgconn.PgConn
}

// Query implements Querier.
func (q PgconnQuerier) Query(ctx context.Context, sql string) ([][][]byte, error) {
	results, err := q.Conn.Exec(ctx, sql).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(results) != 1 {
		return nil, fmt.Errorf("privilege: query returned %d results, want 1", len(results))
	}
	return results[0].Rows, nil
}

// RoleInfo is one row of pg_roles.
type RoleInfo struct {
	OID         int32
	Name        string
	Super       bool
	BypassRLS   bool
	CreateDB    bool
	CreateRole  bool
	CanLogin    bool
	Replication bool
	Inherit     bool
}

// DatabaseInfo describes the connected database.
type DatabaseInfo struct {
	Name  string
	OID   int32
	Owner int32
	ACL   *string
}

// SchemaInfo describes one schema.
type SchemaInfo struct {
	OID   int32
	Name  string
	Owner int32
	ACL   *string
}

// RelationRow is one pg_class row in scope.
type RelationRow struct {
	OID     int32
	Nsp     string
	Relname string
	Relkind string
	Owner   int32
	RelACL  *string
	RelAM   *string
}

// TypeRow is one pg_type row in scope.
type TypeRow struct {
	OID      int32
	Nsp      string
	Typname  string
	Typtype  string
	Typeelem int32
	TypRelID int32
	Owner    int32
	TypACL   *string
}

// FunctionRow is one pg_proc row in scope.
type FunctionRow struct {
	OID          int32
	Nsp          string
	Proname      string
	IdentityArgs string
	Owner        int32
	ProACL       *string
}

// ColumnRow is one non-dropped column of an in-scope relation.
type ColumnRow struct {
	RelOID  int32
	Relname string
	Attname string
	Attnum  int32
	AttACL  *string
}

// DefaultACLRow is one pg_default_acl row for an in-scope schema.
// PostgreSQL 18 removed objsubid: default ACLs are object-level only, so a
// row is identified by its schema, its defining role, and its object type.
type DefaultACLRow struct {
	Nsp     string
	Role    string
	ObjType string
	ACL     string
}

// MembershipEdge is one pg_auth_members row. The direction is member ->
// roleid: member is a member of roleid, so privileges and attributes flow
// from roleid down to member, never the reverse.
type MembershipEdge struct {
	Member  int32
	RoleID  int32
	Admin   bool // admin_option: the member may grant, revoke, or alter the membership
	Inherit bool // inherit_option: roleid's object privileges are inherited into member
	Set     bool // set_option: the member may SET ROLE to roleid
}

// Field returns the raw field bytes of rows[row][col], or nil when the row,
// column, or value is absent. rows is a complete query result: a slice of
// rows, each a slice of field bytes, NULL a nil field.
func Field(rows [][][]byte, row, col int) []byte {
	if row >= 0 && row < len(rows) && col >= 0 && col < len(rows[row]) {
		return rows[row][col]
	}
	return nil
}

func text(rows [][][]byte, row, col int) string {
	if f := Field(rows, row, col); f != nil {
		return string(f)
	}
	return ""
}

func nullableText(rows [][][]byte, row, col int) *string {
	if f := Field(rows, row, col); f != nil {
		s := string(f)
		return &s
	}
	return nil
}

func oid(rows [][][]byte, row, col int) (int32, error) {
	f := Field(rows, row, col)
	if f == nil {
		return 0, fmt.Errorf("privilege: missing OID column %d in row %d", col, row)
	}
	n, err := strconv.ParseInt(string(f), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("privilege: malformed OID %q: %w", f, err)
	}
	return int32(n), nil
}

func flag(rows [][][]byte, row, col int) (bool, error) {
	f := Field(rows, row, col)
	if f == nil {
		return false, fmt.Errorf("privilege: missing boolean column %d in row %d", col, row)
	}
	switch string(f) {
	case "t", "true", "TRUE", "True":
		return true, nil
	case "f", "false", "FALSE", "False":
		return false, nil
	default:
		return false, fmt.Errorf("privilege: malformed boolean %q", rows[row][col])
	}
}

func inList(names ...string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = quoteIdentBare(n)
	}
	return strings.Join(q, ", ")
}

// quoteIdentBare renders a catalog name as a quoted SQL string constant.
func quoteIdentBare(name string) string {
	return "'" + strings.ReplaceAll(name, "'", "''") + "'"
}

// CurrentRole returns the name and OID of the role the connection is running
// as (current_user).
func CurrentRole(ctx context.Context, q Querier) (string, int32, error) {
	rows, err := q.Query(ctx, `SELECT current_user, oid FROM pg_roles WHERE rolname = current_user`)
	if err != nil {
		return "", 0, fmt.Errorf("privilege: read current role: %w", err)
	}
	if len(rows) != 1 {
		return "", 0, fmt.Errorf("privilege: current role has %d catalog rows, want exactly 1", len(rows))
	}
	roleOID, err := oid(rows, 0, 1)
	if err != nil {
		return "", 0, err
	}
	return text(rows, 0, 0), roleOID, nil
}

// Roles loads every role from pg_roles, indexed by OID and by name. pg_roles
// is readable by every role and carries the basic role attributes, which the
// closure checks need for every reachable role, not only the connection's.
func Roles(ctx context.Context, q Querier) (byOID map[int32]RoleInfo, byName map[string]int32, err error) {
	rows, err := q.Query(ctx, `SELECT oid, rolname, rolsuper, rolbypassrls, rolcreatedb, rolcreaterole, rolcanlogin, rolreplication, rolinherit FROM pg_roles`)
	if err != nil {
		return nil, nil, fmt.Errorf("privilege: read pg_roles: %w", err)
	}
	byOID = make(map[int32]RoleInfo, len(rows))
	byName = make(map[string]int32, len(rows))
	for i := range rows {
		var info RoleInfo
		info.OID, err = oid(rows, i, 0)
		if err != nil {
			return nil, nil, err
		}
		info.Name = text(rows, i, 1)
		if info.Name == "" {
			return nil, nil, fmt.Errorf("privilege: pg_roles row %d has no role name", i)
		}
		if _, dup := byOID[info.OID]; dup {
			return nil, nil, fmt.Errorf("privilege: duplicate pg_roles row for OID %d", info.OID)
		}
		if prev, dup := byName[info.Name]; dup {
			return nil, nil, fmt.Errorf("privilege: duplicate pg_roles row for role %q (OIDs %d and %d)", info.Name, prev, info.OID)
		}
		if info.Super, err = flag(rows, i, 2); err != nil {
			return nil, nil, err
		}
		if info.BypassRLS, err = flag(rows, i, 3); err != nil {
			return nil, nil, err
		}
		if info.CreateDB, err = flag(rows, i, 4); err != nil {
			return nil, nil, err
		}
		if info.CreateRole, err = flag(rows, i, 5); err != nil {
			return nil, nil, err
		}
		if info.CanLogin, err = flag(rows, i, 6); err != nil {
			return nil, nil, err
		}
		if info.Replication, err = flag(rows, i, 7); err != nil {
			return nil, nil, err
		}
		if info.Inherit, err = flag(rows, i, 8); err != nil {
			return nil, nil, err
		}
		byOID[info.OID] = info
		byName[info.Name] = info.OID
	}
	return byOID, byName, nil
}

// RoleNames resolves role OIDs to names for error messages, falling back to
// the numeric OID for a role absent from the snapshot.
func RoleNames(byOID map[int32]RoleInfo) func(int32) string {
	return func(o int32) string {
		if info, ok := byOID[o]; ok {
			return info.Name
		}
		return strconv.FormatInt(int64(o), 10)
	}
}

// Database reads the connected database's identity, owner, and ACL.
func Database(ctx context.Context, q Querier) (DatabaseInfo, error) {
	rows, err := q.Query(ctx, `SELECT current_database(), d.oid, d.datdba, d.datacl FROM pg_database d WHERE d.datname = current_database()`)
	if err != nil {
		return DatabaseInfo{}, fmt.Errorf("privilege: read current database: %w", err)
	}
	if len(rows) != 1 {
		return DatabaseInfo{}, fmt.Errorf("privilege: current database has %d catalog rows, want exactly 1", len(rows))
	}
	var db DatabaseInfo
	db.Name = text(rows, 0, 0)
	db.OID, err = oid(rows, 0, 1)
	if err != nil {
		return DatabaseInfo{}, err
	}
	db.Owner, err = oid(rows, 0, 2)
	if err != nil {
		return DatabaseInfo{}, err
	}
	db.ACL = nullableText(rows, 0, 3)
	return db, nil
}

// Schemas reads the named schemas' identity, owners, and ACLs.
func Schemas(ctx context.Context, q Querier, names ...string) ([]SchemaInfo, error) {
	rows, err := q.Query(ctx, `SELECT n.oid, n.nspname, n.nspowner, n.nspacl FROM pg_namespace n WHERE n.nspname IN (`+inList(names...)+`)`)
	if err != nil {
		return nil, fmt.Errorf("privilege: read schemas: %w", err)
	}
	out := make([]SchemaInfo, 0, len(rows))
	for i := range rows {
		var s SchemaInfo
		s.OID, err = oid(rows, i, 0)
		if err != nil {
			return nil, err
		}
		s.Name = text(rows, i, 1)
		s.Owner, err = oid(rows, i, 2)
		if err != nil {
			return nil, err
		}
		s.ACL = nullableText(rows, i, 3)
		out = append(out, s)
	}
	return out, nil
}

// Relations reads every pg_class row in the named schemas, for every relation
// kind.
func Relations(ctx context.Context, q Querier, schemas ...string) ([]RelationRow, error) {
	rows, err := q.Query(ctx, `SELECT c.oid, n.nspname, c.relname, c.relkind, c.relowner, c.relacl, c.relam
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname IN (`+inList(schemas...)+`)`)
	if err != nil {
		return nil, fmt.Errorf("privilege: read in-scope relations: %w", err)
	}
	out := make([]RelationRow, 0, len(rows))
	for i := range rows {
		var rel RelationRow
		rel.OID, err = oid(rows, i, 0)
		if err != nil {
			return nil, err
		}
		rel.Nsp = text(rows, i, 1)
		rel.Relname = text(rows, i, 2)
		rel.Relkind = text(rows, i, 3)
		rel.Owner, err = oid(rows, i, 4)
		if err != nil {
			return nil, err
		}
		rel.RelACL = nullableText(rows, i, 5)
		rel.RelAM = nullableText(rows, i, 6)
		out = append(out, rel)
	}
	return out, nil
}

// Types reads every pg_type row in the named schemas.
func Types(ctx context.Context, q Querier, schemas ...string) ([]TypeRow, error) {
	rows, err := q.Query(ctx, `SELECT t.oid, n.nspname, t.typname, t.typtype, t.typelem, t.typrelid, t.typowner, t.typacl
FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
WHERE n.nspname IN (`+inList(schemas...)+`)`)
	if err != nil {
		return nil, fmt.Errorf("privilege: read in-scope types: %w", err)
	}
	out := make([]TypeRow, 0, len(rows))
	for i := range rows {
		var t TypeRow
		t.OID, err = oid(rows, i, 0)
		if err != nil {
			return nil, err
		}
		t.Nsp = text(rows, i, 1)
		t.Typname = text(rows, i, 2)
		t.Typtype = text(rows, i, 3)
		// pg_type.typelem is a plain OID: the array's element type, 0 for
		// non-arrays.
		t.Typeelem, err = oid(rows, i, 4)
		if err != nil {
			return nil, err
		}
		t.TypRelID, err = oid(rows, i, 5)
		if err != nil {
			return nil, err
		}
		t.Owner, err = oid(rows, i, 6)
		if err != nil {
			return nil, err
		}
		t.TypACL = nullableText(rows, i, 7)
		out = append(out, t)
	}
	return out, nil
}

// Functions reads every pg_proc row in the named schemas, with each
// function's identity argument list for unambiguous naming.
func Functions(ctx context.Context, q Querier, schemas ...string) ([]FunctionRow, error) {
	rows, err := q.Query(ctx, `SELECT p.oid, n.nspname, p.proname, pg_get_function_identity_arguments(p.oid), p.proowner, p.proacl
FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
WHERE n.nspname IN (`+inList(schemas...)+`)`)
	if err != nil {
		return nil, fmt.Errorf("privilege: read in-scope functions: %w", err)
	}
	out := make([]FunctionRow, 0, len(rows))
	for i := range rows {
		var f FunctionRow
		f.OID, err = oid(rows, i, 0)
		if err != nil {
			return nil, err
		}
		f.Nsp = text(rows, i, 1)
		f.Proname = text(rows, i, 2)
		f.IdentityArgs = text(rows, i, 3)
		f.Owner, err = oid(rows, i, 4)
		if err != nil {
			return nil, err
		}
		f.ProACL = nullableText(rows, i, 5)
		out = append(out, f)
	}
	return out, nil
}

// Columns reads every non-dropped column of the named schemas' relations.
func Columns(ctx context.Context, q Querier, schemas ...string) ([]ColumnRow, error) {
	rows, err := q.Query(ctx, `SELECT a.attrelid, c.relname, a.attname, a.attnum, a.attacl
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname IN (`+inList(schemas...)+`) AND a.attnum > 0 AND NOT a.attisdropped`)
	if err != nil {
		return nil, fmt.Errorf("privilege: read in-scope columns: %w", err)
	}
	out := make([]ColumnRow, 0, len(rows))
	for i := range rows {
		var c ColumnRow
		c.RelOID, err = oid(rows, i, 0)
		if err != nil {
			return nil, err
		}
		c.Relname = text(rows, i, 1)
		c.Attname = text(rows, i, 2)
		c.Attnum, err = oid(rows, i, 3)
		if err != nil {
			return nil, err
		}
		c.AttACL = nullableText(rows, i, 4)
		out = append(out, c)
	}
	return out, nil
}

// DefaultACLs reads every pg_default_acl row for the named schemas.
func DefaultACLs(ctx context.Context, q Querier, schemas ...string) ([]DefaultACLRow, error) {
	rows, err := q.Query(ctx, `SELECT n.nspname, r.rolname, d.defaclobjtype, d.defaclacl
FROM pg_default_acl d
JOIN pg_namespace n ON n.oid = d.defaclnamespace
JOIN pg_roles r ON r.oid = d.defaclrole
WHERE n.nspname IN (`+inList(schemas...)+`)`)
	if err != nil {
		return nil, fmt.Errorf("privilege: read default ACLs: %w", err)
	}
	out := make([]DefaultACLRow, 0, len(rows))
	for i := range rows {
		var d DefaultACLRow
		d.Nsp = text(rows, i, 0)
		d.Role = text(rows, i, 1)
		d.ObjType = text(rows, i, 2)
		d.ACL = text(rows, i, 3)
		out = append(out, d)
	}
	return out, nil
}

// MembershipEdges reads the complete role membership graph from
// pg_auth_members. The view is readable by every role and carries the
// PostgreSQL 18 membership options: admin_option, inherit_option, set_option.
func MembershipEdges(ctx context.Context, q Querier) ([]MembershipEdge, error) {
	rows, err := q.Query(ctx, `SELECT member, roleid, admin_option, inherit_option, set_option FROM pg_auth_members`)
	if err != nil {
		return nil, fmt.Errorf("privilege: read pg_auth_members: %w", err)
	}
	out := make([]MembershipEdge, 0, len(rows))
	for i := range rows {
		var e MembershipEdge
		e.Member, err = oid(rows, i, 0)
		if err != nil {
			return nil, err
		}
		e.RoleID, err = oid(rows, i, 1)
		if err != nil {
			return nil, err
		}
		if e.Admin, err = flag(rows, i, 2); err != nil {
			return nil, err
		}
		if e.Inherit, err = flag(rows, i, 3); err != nil {
			return nil, err
		}
		if e.Set, err = flag(rows, i, 4); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}
