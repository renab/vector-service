package db

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"vector-service/internal/privilege"
)

// Check names of the runtime identity assertion's subchecks, for structured
// errors. Every assertion failure names the check it failed.
const (
	checkCurrentDatabase = "current-database"
	checkCurrentUser     = "current-user"
	checkRoleAttributes  = "role-attributes"
	checkDatabaseOwner   = "database-owner"
	checkNoOwnership     = "no-ownership"
	checkMembership      = "membership-options"
	checkPrivilege       = "privilege-boundary"
	checkColumnPrivilege = "column-privilege-boundary"
	checkArrayPrivilege  = "array-privilege-view"
	checkProvenance      = "grant-provenance"
	checkGrantOption     = "grant-option"
	checkDefaultACL      = "default-acl"
	checkClosureRole     = "membership-closure"
)

// Schemas in scope: every migrated object lives in these two schemas. The
// sweeps and the privilege grid cover every in-scope object, and any future
// object these schemas acquire is covered automatically.
var inScopeSchemas = []string{"vector_control", "vector_data"}

// columnRelationKinds are the pg_class relkinds whose non-dropped columns
// carry column privileges: ordinary tables, partitioned tables, views,
// materialized views, and foreign tables. Every other in-scope relation kind
// (indexes, sequences, any future kind) has no column-level boundary of its
// own: its access rights are checked at the relation level.
var columnRelationKinds = map[string]bool{
	"r": true, // ordinary table
	"p": true, // partitioned table
	"v": true, // view
	"m": true, // materialized view
	"f": true, // foreign table
}

// zeroPrivileges is the empty canonical set, shared read-only.
var zeroPrivileges = privilege.Set{}

// AssertionError is a runtime identity assertion failure. Check names the
// failed subcheck; Detail names the object — the column, where applicable —
// the privilege, and the source of the grant (direct vector_api entry, a
// specific reachable member role, or PUBLIC), where applicable.
type AssertionError struct {
	Check  string
	Detail string
}

func (e *AssertionError) Error() string {
	return "runtime identity assertion failed: " + e.Check + ": " + e.Detail
}

func assert(check, format string, args ...any) error {
	return &AssertionError{Check: check, Detail: fmt.Sprintf(format, args...)}
}

// RuntimeBoundary asserts the complete effective privilege boundary of the
// runtime role on the connection: identity and role attributes, no
// ownership of the database or any in-scope object, the exact canonical
// grant boundary of migrations 0004-0006 (effective privileges, column
// level, true-array dependent privilege view, direct-grant provenance, grant
// options, default ACLs, and the membership-option rules), and the
// membership closure's privileged-role rule. It runs as the runtime role on
// every physical pool connection; any failure is fatal to the connection.
//
// runtimeRole, migrationRole, and databaseName are the configured canonical
// values (vector_api, the VEC_PG_MIGRATION_USER identity, and vector); they
// are compared against the connection's actual identity, never used to
// relax a check.
func RuntimeBoundary(ctx context.Context, q privilege.Querier, runtimeRole, migrationRole, databaseName string) error {
	st, err := gatherState(ctx, q)
	if err != nil {
		return err
	}
	if err := checkIdentity(st, runtimeRole, databaseName); err != nil {
		return err
	}
	if err := checkRuntimeNoOwnership(st); err != nil {
		return err
	}
	if err := checkGrantBoundary(ctx, q, st, runtimeRole); err != nil {
		return err
	}
	if err := checkMembershipClosure(st, migrationRole); err != nil {
		return err
	}
	return nil
}

// state is the complete catalog state the boundary check evaluates.
type state struct {
	roleName  string
	roleOID   int32
	role      privilege.RoleInfo
	roles     map[int32]privilege.RoleInfo
	roleNames map[string]int32
	names     func(int32) string
	subgraph  *privilege.Subgraph
	db        privilege.DatabaseInfo
	schemas   []privilege.SchemaInfo
	relations []privilege.RelationRow
	types     []privilege.TypeRow
	functions []privilege.FunctionRow
	columns   []privilege.ColumnRow
	defACLs   []privilege.DefaultACLRow

	// typeByOID indexes the in-scope types for array element resolution.
	typeByOID map[int32]privilege.TypeRow
	// relationByOID indexes the in-scope relations for column checks.
	relationByOID map[int32]privilege.RelationRow
}

func gatherState(ctx context.Context, q privilege.Querier) (*state, error) {
	roleName, roleOID, err := privilege.CurrentRole(ctx, q)
	if err != nil {
		return nil, err
	}
	roles, roleNames, err := privilege.Roles(ctx, q)
	if err != nil {
		return nil, err
	}
	role, ok := roles[roleOID]
	if !ok {
		return nil, assert(checkRoleAttributes, "role %q (OID %d) is absent from pg_roles", roleName, roleOID)
	}
	db, err := privilege.Database(ctx, q)
	if err != nil {
		return nil, err
	}
	schemas, err := privilege.Schemas(ctx, q, inScopeSchemas...)
	if err != nil {
		return nil, err
	}
	relations, err := privilege.Relations(ctx, q, inScopeSchemas...)
	if err != nil {
		return nil, err
	}
	types, err := privilege.Types(ctx, q, inScopeSchemas...)
	if err != nil {
		return nil, err
	}
	functions, err := privilege.Functions(ctx, q, inScopeSchemas...)
	if err != nil {
		return nil, err
	}
	columns, err := privilege.Columns(ctx, q, inScopeSchemas...)
	if err != nil {
		return nil, err
	}
	defACLs, err := privilege.DefaultACLs(ctx, q, inScopeSchemas...)
	if err != nil {
		return nil, err
	}
	edges, err := privilege.MembershipEdges(ctx, q)
	if err != nil {
		return nil, err
	}

	typeByOID := make(map[int32]privilege.TypeRow, len(types))
	for _, t := range types {
		typeByOID[t.OID] = t
	}
	relationByOID := make(map[int32]privilege.RelationRow, len(relations))
	for _, r := range relations {
		relationByOID[r.OID] = r
	}

	return &state{
		roleName:      roleName,
		roleOID:       roleOID,
		role:          role,
		roles:         roles,
		roleNames:     roleNames,
		names:         privilege.RoleNames(roles),
		subgraph:      privilege.MembershipSubgraph(roleOID, edges),
		db:            db,
		schemas:       schemas,
		relations:     relations,
		types:         types,
		functions:     functions,
		columns:       columns,
		defACLs:       defACLs,
		typeByOID:     typeByOID,
		relationByOID: relationByOID,
	}, nil
}

// checkIdentity is part 1: identity and attributes.
func checkIdentity(st *state, runtimeRole, databaseName string) error {
	if st.db.Name != databaseName {
		return assert(checkCurrentDatabase,
			"the connection is to database %q, which is not the canonical database %q",
			st.db.Name, databaseName)
	}
	if st.roleName != runtimeRole {
		return assert(checkCurrentUser,
			"current_user is %q, which is not the canonical runtime role %q",
			st.roleName, runtimeRole)
	}
	for _, attr := range []struct {
		name  string
		value bool
		want  bool
	}{
		{"rolsuper", st.role.Super, false},
		{"rolbypassrls", st.role.BypassRLS, false},
		{"rolcreatedb", st.role.CreateDB, false},
		{"rolcreaterole", st.role.CreateRole, false},
		{"rolinherit", st.role.Inherit, true},
	} {
		if attr.value != attr.want {
			want := "false"
			if attr.want {
				want = "true"
			}
			return assert(checkRoleAttributes,
				"role %q has attribute %s = %v, which must be %s",
				runtimeRole, attr.name, attr.value, want)
		}
	}
	if st.db.Owner == st.roleOID {
		return assert(checkDatabaseOwner,
			"the runtime role %q is the owner of the connected database %q; the runtime role must not own the database",
			runtimeRole, st.db.Name)
	}
	return nil
}

// checkRuntimeNoOwnership is part 2: the runtime role owns none of the
// connected database, the two migrated schemas, or any in-scope object —
// every pg_class row (relowner), every pg_type row (typowner), and every
// pg_proc row (proowner), including every migrated function.
func checkRuntimeNoOwnership(st *state) error {
	sweep := privilege.OwnershipSweep{
		Database:  st.db,
		Schemas:   st.schemas,
		Relations: st.relations,
		Types:     st.types,
		Functions: st.functions,
	}
	mismatches := privilege.CheckOwnership(sweep, st.roleOID, st.names, false)
	if len(mismatches) == 0 {
		return nil
	}
	var b strings.Builder
	const shown = 10
	for i, m := range mismatches {
		if i >= shown {
			fmt.Fprintf(&b, " and %d more", len(mismatches)-shown)
			break
		}
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(m.Error())
	}
	return assert(checkNoOwnership,
		"the runtime role %q owns an in-scope object: %s; the runtime role must own none of the database, the migrated schemas, or any object in them",
		st.roleName, b.String())
}

// pred is one engine has_* privilege comparison: expr is evaluated on the
// connection and must equal want; object, priv, and check name the failure.
type pred struct {
	object string
	priv   string
	want   bool
	expr   string
	check  string
}

// evalPreds evaluates every predicate of a grid in one statement, as the
// connection's role — the runtime role — so each predicate resolves exactly
// the effective privilege set: direct ACL entries for the runtime role, the
// entries of every role in its INHERIT closure, and PUBLIC.
func evalPreds(ctx context.Context, q privilege.Querier, preds []pred) error {
	if len(preds) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("SELECT ")
	for i, p := range preds {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(p.expr)
	}
	rows, err := q.Query(ctx, b.String())
	if err != nil {
		return fmt.Errorf("evaluate privilege predicates: %w", err)
	}
	if len(rows) != 1 {
		return fmt.Errorf("privilege predicates: query returned %d rows, want 1", len(rows))
	}
	for i, p := range preds {
		f := privilege.Field(rows, 0, i)
		if f == nil {
			return fmt.Errorf("privilege predicates: predicate for %s %s returned NULL", p.object, p.priv)
		}
		var got bool
		switch string(f) {
		case "t", "true", "TRUE", "True":
			got = true
		case "f", "false", "FALSE", "False":
			got = false
		default:
			return fmt.Errorf("privilege predicates: malformed boolean %q for %s %s", f, p.object, p.priv)
		}
		if got != p.want {
			return assert(p.check,
				"%s on %s is %t, want %t: the runtime role's effective privileges do not equal the canonical grant set",
				p.priv, p.object, got, p.want)
		}
	}
	return nil
}

// aclSource names the grant source an error detail reports: a direct entry
// for the runtime role, an entry for a specific reachable member role, or
// PUBLIC.
func (st *state) aclSource(e privilege.ACLEntry, runtimeRole string) string {
	switch {
	case e.Public:
		return "PUBLIC"
	case e.Grantee == runtimeRole || (e.GranteeOID != 0 && e.GranteeOID == st.roleOID):
		return "direct " + runtimeRole + " entry"
	default:
		return "reachable member role " + st.granteeName(e)
	}
}

// granteeName renders an entry's grantee for error messages: the role name
// when known, the recorded OID otherwise.
func (st *state) granteeName(e privilege.ACLEntry) string {
	if e.Grantee != "" && e.GranteeOID == 0 {
		return e.Grantee
	}
	oid, _ := st.granteeOID(e) // message rendering only; the OID fallback names the role
	return st.names(oid)
}

// granteeOID resolves an entry's grantee to a role OID: the recorded OID
// when PostgreSQL stored one, the name's pg_roles OID otherwise. An
// unresolvable name is fail-closed: a name-based entry that names no role is
// a malformed or unreadable catalog state.
func (st *state) granteeOID(e privilege.ACLEntry) (int32, error) {
	if e.GranteeOID != 0 {
		return e.GranteeOID, nil
	}
	if e.Grantee == "" { // PUBLIC
		return 0, nil
	}
	oid, ok := st.roleNames[e.Grantee]
	if !ok {
		return 0, fmt.Errorf("ACL entry grantee %q is absent from pg_roles", e.Grantee)
	}
	return oid, nil
}

// granteeInClosure reports whether an entry's grantee is a role in the
// runtime role's reachable membership subgraph.
func (st *state) granteeInClosure(e privilege.ACLEntry) (bool, error) {
	oid, err := st.granteeOID(e)
	if err != nil {
		return false, err
	}
	return oid != 0 && st.subgraph.Closure[oid], nil
}

// checkGrantBoundary is part 3: the exact effective grant boundary. The
// runtime role's effective privileges on every in-scope object must equal
// exactly the canonical grant set of migrations 0004-0006:
//
//  1. the zero-membership rule — no reachable membership edge carries the
//     SET option (an assumption right no privilege equality can bound) or
//     the ADMIN option (a regrant channel);
//  2. the engine's own has_* predicates — which resolve role membership and
//     PUBLIC internally, and run here as the non-superuser, non-owning
//     runtime role, so they measure grant-based privilege — must equal the
//     canonical row for every in-scope object and every applicable
//     privilege, at table level (the grid includes PostgreSQL 18's
//     MAINTAIN, canonically absent everywhere), at column level (exactly the
//     column-applicable subset of the table's canonical set on every
//     non-dropped column), and for the true array types' dependent
//     privilege view (the array's effective USAGE equals the element type's
//     canonical USAGE in both directions);
//  3. the provenance rules — each required canonical grant must exist as a
//     direct ACL entry for the runtime role in the applicable catalog, no
//     in-scope ACL entry may name a role in the runtime role's membership
//     subgraph as grantee (in the canonical zero-membership shape inherited
//     grants are absent), and PUBLIC carries at most the preserved default
//     CONNECT on the database — nothing on any other in-scope object;
//  4. the grant-option rejection — no in-scope ACL entry carries a grant
//     option for the runtime role, a closure role, or PUBLIC, across every
//     applicable ACL catalog;
//  5. the default-ACL rule — no pg_default_acl entry for the two schemas
//     grants to the runtime role, its closure, or PUBLIC.
func checkGrantBoundary(ctx context.Context, q privilege.Querier, st *state, runtimeRole string) error {
	// The zero-membership rule (1): any reachable membership edge with
	// set_option or admin_option true is rejected. A set_option-true edge
	// lets the member SET ROLE into the granted role — an assumption right
	// no privilege equality can bound; an admin_option-true edge is a
	// regrant channel for a membership the canonical shape does not have.
	if e, ok := st.subgraph.SetOptionEdge(); ok {
		return assert(checkMembership,
			"membership edge %s -> %s carries SET option: the runtime role may SET ROLE into a granted role, which no privilege equality can bound",
			st.names(e.Member), st.names(e.RoleID))
	}
	if e, ok := st.subgraph.AdminOptionEdge(); ok {
		return assert(checkMembership,
			"membership edge %s -> %s carries ADMIN option: the member may grant, revoke, or alter the membership, an unauthorized regrant channel",
			st.names(e.Member), st.names(e.RoleID))
	}

	// The effective-privilege grid (2), one engine predicate per object and
	// applicable privilege.
	var preds []pred

	// The in-scope database.
	addPreds := func(object string, exprs []string, want privilege.Set, check string) {
		for _, e := range exprs {
			// exprs are of the form "PRIV=expression".
			i := strings.IndexByte(e, '=')
			preds = append(preds, pred{
				object: object,
				priv:   e[:i],
				want:   want.Has(e[:i]),
				expr:   e[i+1:],
				check:  check,
			})
		}
	}
	dbPred := "database " + st.db.Name
	addPreds(dbPred, []string{
		"CONNECT=has_database_privilege(" + sqlString(st.db.Name) + ", 'CONNECT')",
		"CREATE=has_database_privilege(" + sqlString(st.db.Name) + ", 'CREATE')",
		"TEMPORARY=has_database_privilege(" + sqlString(st.db.Name) + ", 'TEMPORARY')",
	}, canonicalDatabase, checkPrivilege)

	// The in-scope schemas.
	for _, s := range st.schemas {
		addPreds("schema "+s.Name, []string{
			"USAGE=has_schema_privilege(" + sqlString(s.Name) + ", 'USAGE')",
			"CREATE=has_schema_privilege(" + sqlString(s.Name) + ", 'CREATE')",
		}, canonicalSchemas, checkPrivilege)
	}

	// The in-scope relations: every table-privilege relation (ordinary
	// tables, partitioned tables, views, materialized views, foreign tables,
	// and sequences) under the full table-privilege grid; index rows carry
	// no independent ACL (checked below as relacl IS NULL); any other
	// relation kind present is checked under the no-privilege rule.
	zero := privilege.Set{}
	for _, rel := range st.relations {
		switch {
		case tableRelationKinds[rel.Relkind] || rel.Relkind == "S":
			key := canonicalTablesKey(rel.Nsp, rel.Relname)
			want, ok := canonicalTables[key]
			if !ok {
				want = zero
			}
			ref := sqlString(privilege.QualifiedName(rel.Nsp, rel.Relname))
			addPreds("table "+rel.Nsp+"."+rel.Relname, tableGrid(ref), want, checkPrivilege)
		case indexRelationKinds[rel.Relkind]:
			if rel.RelACL != nil {
				return assert(checkPrivilege,
					"index %s.%s carries an independent ACL; PostgreSQL indexes carry no independent ACL, which must be NULL",
					rel.Nsp, rel.Relname)
			}
		default:
			ref := sqlString(privilege.QualifiedName(rel.Nsp, rel.Relname))
			addPreds("relation "+rel.Nsp+"."+rel.Relname, tableGrid(ref), zero, checkPrivilege)
		}
	}

	// The in-scope types. Independent types (enums, composites, domains,
	// base types) are checked against their canonical row — canonically none
	// except vector_control.distance_metric's USAGE. True array types of
	// in-scope element types are the dependent privilege view: the array's
	// effective USAGE must equal the element type's canonical USAGE in both
	// directions, and no direct-entry provenance is required for the array
	// itself (its ACL rules are below).
	for _, t := range st.types {
		ref := strconv.FormatInt(int64(t.OID), 10)
		key := canonicalTypesKey(t)
		switch {
		case isArrayType(t):
			elem, ok := st.typeByOID[t.Typeelem]
			if !ok {
				// An array type whose element type is not in scope is an
				// other in-scope type: the no-privilege rule.
				addPreds("type "+t.Nsp+"."+t.Typname, typeGrid(ref), zero, checkPrivilege)
				continue
			}
			// The element's canonical set defines the array's expected
			// effective USAGE.
			elemWant := canonicalTypes[canonicalTypesKey(elem)]
			if elemWant == nil {
				elemWant = zero
			}
			addPreds("array type "+t.Nsp+"."+t.Typname, typeGrid(ref), elemWant, checkArrayPrivilege)
		default:
			want := canonicalTypes[key]
			if want == nil {
				want = zero
			}
			addPreds("type "+t.Nsp+"."+t.Typname, typeGrid(ref), want, checkPrivilege)
		}
	}

	// The in-scope functions.
	for _, f := range st.functions {
		want := canonicalFunctions[canonicalFunctionsKey(f)]
		if want == nil {
			want = zero
		}
		addPreds("function "+f.Nsp+"."+f.Proname+"("+f.IdentityArgs+")",
			functionGrid(strconv.FormatInt(int64(f.OID), 10)), want, checkPrivilege)
	}

	// The column level (2, columns): every non-dropped column of every
	// in-scope table-privilege relation, for each column-applicable
	// privilege. The expected column set is not an independent row: it is
	// exactly the column-applicable subset of the table's canonical set and
	// nothing else — the migrations never grant per-column, and the engine
	// reports a table's privileges on each of its columns.
	for _, c := range st.columns {
		rel, ok := st.relationByOID[c.RelOID]
		if !ok || !columnRelationKinds[rel.Relkind] {
			continue
		}
		key := canonicalTablesKey(rel.Nsp, rel.Relname)
		tableWant := canonicalTables[key]
		if tableWant == nil {
			tableWant = zero
		}
		want := tableWant.ColumnSubset()
		tableRef := sqlString(privilege.QualifiedName(rel.Nsp, rel.Relname))
		columnRef := sqlString(c.Attname)
		addPreds("column "+rel.Nsp+"."+rel.Relname+"."+c.Attname, columnGrid(tableRef, columnRef), want, checkColumnPrivilege)
	}

	if err := evalPreds(ctx, q, preds); err != nil {
		return err
	}

	// The provenance, closure-grantee, and PUBLIC rules (3), over every
	// in-scope ACL catalog.
	if err := st.checkACLs(runtimeRole); err != nil {
		return err
	}

	// The grant-option rejection (4), over every in-scope ACL catalog.
	if err := st.checkGrantOptions(runtimeRole); err != nil {
		return err
	}

	// The default-ACL rule (5): no pg_default_acl entry for the two schemas
	// grants to the runtime role, its closure, or PUBLIC — default
	// privileges would silently extend the boundary to future objects.
	for _, d := range st.defACLs {
		entries, err := privilege.ParseACL(d.ACL, privilege.KindClass)
		if err != nil {
			return fmt.Errorf("decode pg_default_acl entry for schema %s (role %s, object type %s): %w", d.Nsp, d.Role, d.ObjType, err)
		}
		for _, e := range entries {
			if reachable, err := defaultACLGrantReachable(e, runtimeRole, st); err != nil {
				return err
			} else if reachable {
				return assert(checkDefaultACL,
					"pg_default_acl entry for schema %s (role %s, object type %s) grants %s to %s: default privileges would extend the runtime boundary to future objects",
					d.Nsp, d.Role, d.ObjType, e.Privileges.Sorted(), st.aclSource(e, runtimeRole))
			}
		}
	}
	return nil
}

// tableGrid renders the full table-privilege predicates for one relation,
// addressed by the given quoted qualified-name string constant.
func tableGrid(ref string) []string {
	var out []string
	for _, p := range tablePrivileges {
		out = append(out, p+"=has_table_privilege("+ref+", '"+p+"')")
	}
	return out
}

// typeGrid renders the single type-privilege predicate for one type,
// addressed by OID.
// isArrayType reports whether a pg_type row is a true array type.
// PostgreSQL 18 catalogs array types as base types (typtype 'b',
// typcategory 'A'); the cross-version invariant is that typelem names the
// element type and is 0 for every non-array type.
func isArrayType(t privilege.TypeRow) bool { return t.Typeelem != 0 }

func typeGrid(oid string) []string {
	return []string{"USAGE=has_type_privilege(" + oid + ", 'USAGE')"}
}

// functionGrid renders the single function-privilege predicate for one
// function, addressed by OID.
func functionGrid(oid string) []string {
	return []string{"EXECUTE=has_function_privilege(" + oid + ", 'EXECUTE')"}
}

// columnGrid renders the column-applicable privilege predicates for one
// column as the engine's three-argument has_column_privilege(table, column,
// privilege): PostgreSQL defines no two-argument column form. tableRef is
// the quoted qualified table name and columnRef the quoted column name,
// both string constants rendered from the service's own catalogs.
func columnGrid(tableRef, columnRef string) []string {
	var out []string
	for _, p := range privilege.ColumnPrivileges() {
		out = append(out, p+"=has_column_privilege("+tableRef+", "+columnRef+", '"+p+"')")
	}
	return out
}

// defaultACLGrantReachable reports whether a pg_default_acl entry grants to
// the runtime role, a role in its membership closure, or PUBLIC.
func defaultACLGrantReachable(e privilege.ACLEntry, runtimeRole string, st *state) (bool, error) {
	if e.Public {
		return true, nil
	}
	if e.Grantee == runtimeRole || (e.GranteeOID != 0 && e.GranteeOID == st.roleOID) {
		return true, nil
	}
	return st.granteeInClosure(e)
}

// aclCatalog is one in-scope object's ACL under the provenance,
// closure-grantee, PUBLIC, array-type, and grant-option rules: the catalog
// text to decode, the privilege alphabet kind, the canonical set that must
// exist as direct runtime-role entries, and the object's special status.
type aclCatalog struct {
	object      string
	kind        privilege.Kind
	aclText     string
	required    privilege.Set
	isDatabase  bool
	isArrayType bool
}

// aclCatalogs enumerates every in-scope ACL catalog: the database, every
// schema, every table-privilege relation, every non-dropped column of a
// column-privilege relation, every function, and every type.
func (st *state) aclCatalogs() []aclCatalog {
	var out []aclCatalog
	out = append(out, aclCatalog{
		object:     "database " + st.db.Name,
		kind:       privilege.KindDatabase,
		aclText:    aclText(st.db.ACL),
		required:   canonicalDatabase,
		isDatabase: true,
	})
	for _, s := range st.schemas {
		out = append(out, aclCatalog{
			object:   "schema " + s.Name,
			kind:     privilege.KindNamespace,
			aclText:  aclText(s.ACL),
			required: canonicalSchemas,
		})
	}
	for _, rel := range st.relations {
		if indexRelationKinds[rel.Relkind] {
			continue // indexes carry no independent ACL; their relacl is NULL-checked
		}
		var kind privilege.Kind
		var required privilege.Set
		if rel.Relkind == "S" {
			kind = privilege.KindSequence
		} else {
			kind = privilege.KindClass
			if w, ok := canonicalTables[canonicalTablesKey(rel.Nsp, rel.Relname)]; ok {
				required = w
			}
		}
		out = append(out, aclCatalog{
			object:   "table " + rel.Nsp + "." + rel.Relname,
			kind:     kind,
			aclText:  aclText(rel.RelACL),
			required: required,
		})
	}
	for _, c := range st.columns {
		rel, ok := st.relationByOID[c.RelOID]
		if !ok || !columnRelationKinds[rel.Relkind] {
			continue
		}
		out = append(out, aclCatalog{
			object:  "column " + rel.Nsp + "." + rel.Relname + "." + c.Attname,
			kind:    privilege.KindClass,
			aclText: aclText(c.AttACL),
		})
	}
	for _, f := range st.functions {
		required := canonicalFunctions[canonicalFunctionsKey(f)]
		out = append(out, aclCatalog{
			object:   "function " + f.Nsp + "." + f.Proname + "(" + f.IdentityArgs + ")",
			kind:     privilege.KindFunction,
			aclText:  aclText(f.ProACL),
			required: required,
		})
	}
	for _, t := range st.types {
		key := canonicalTypesKey(t)
		if isArrayType(t) {
			if _, ok := st.typeByOID[t.Typeelem]; ok {
				// The dependent privilege view: no direct-entry
				// provenance, and no entry for the runtime role, any
				// closure role, or PUBLIC.
				out = append(out, aclCatalog{
					object:      "array type " + t.Nsp + "." + t.Typname,
					kind:        privilege.KindType,
					aclText:     aclText(t.TypACL),
					isArrayType: true,
				})
				continue
			}
		}
		required := canonicalTypes[key]
		out = append(out, aclCatalog{
			object:   "type " + t.Nsp + "." + t.Typname,
			kind:     privilege.KindType,
			aclText:  aclText(t.TypACL),
			required: required,
		})
	}
	return out
}

// checkACLs applies the provenance rules over every in-scope ACL catalog:
// each required canonical grant must exist as a direct ACL entry for the
// runtime role in the applicable catalog; no entry may name a role in the
// runtime role's membership subgraph as grantee (in the canonical
// zero-membership shape inherited grants are absent — a grant that reaches
// the runtime role only through a member role fails even when the has_*
// predicates resolve it as canonical); the true array types carry no entry
// for the runtime role, any closure role, or PUBLIC (no independent array
// ACL or PUBLIC grant — their effective USAGE is the element type's, and no
// direct-entry provenance applies to them); and PUBLIC carries at most the
// preserved default CONNECT on the database, nothing on any other in-scope
// object.
func (st *state) checkACLs(runtimeRole string) error {
	for _, c := range st.aclCatalogs() {
		entries, err := privilege.ParseACL(c.aclText, c.kind)
		if err != nil {
			return fmt.Errorf("decode %s ACL: %w", c.object, err)
		}

		// Direct-entry provenance: every required canonical grant must be
		// a direct entry for the runtime role in this catalog. The
		// dependent privilege-view array types are exempt: their effective
		// USAGE is the element type's canonical grant, and no direct entry
		// for the array itself is required (or allowed).
		if !c.isArrayType {
			direct := privilege.PrivilegesOf(privilege.EntriesFor(entries, runtimeRole, st.roleOID))
			for _, p := range c.required.Sorted() {
				if !direct.Has(p) {
					return assert(checkProvenance,
						"the canonical %s on %s has no direct %s ACL entry for the runtime role: the grant must be a direct %s entry, not inherited or PUBLIC",
						p, c.object, p, runtimeRole)
				}
			}
		}

		for _, e := range entries {
			// No entry may name a role in the runtime role's membership
			// subgraph as grantee, in any in-scope catalog, array types
			// included.
			if !e.Public {
				inClosure, err := st.granteeInClosure(e)
				if err != nil {
					return err
				}
				if inClosure {
					return assert(checkProvenance,
						"%s on %s is granted to %s, a role in the runtime role's membership subgraph: in the canonical zero-membership shape grants reach the runtime role directly, never through a member role",
						e.Privileges.Sorted(), c.object, st.granteeName(e))
				}
			}
			// The true array types carry no entry for the runtime role,
			// any closure role, or PUBLIC: no independent array ACL.
			if c.isArrayType && !e.Public {
				if oid, err := st.granteeOID(e); err != nil {
					return err
				} else if oid == st.roleOID {
					return assert(checkArrayPrivilege,
						"array type %s carries a direct %s entry: true array types have no independently mutable ACL, their effective USAGE is the element type's",
						c.object, e.Privileges.Sorted())
				}
			}
			// The PUBLIC rules: at most the preserved default CONNECT on
			// the database, nothing on any other in-scope object.
			if e.Public {
				switch {
				case c.isArrayType:
					return assert(checkProvenance,
						"array type %s carries a PUBLIC %s entry: true array types carry no independent ACL or PUBLIC grant",
						c.object, e.Privileges.Sorted())
				case c.isDatabase:
					for _, p := range e.Privileges.Sorted() {
						if p != privilege.PrivConnect {
							return assert(checkProvenance,
								"PUBLIC carries %s on %s: PUBLIC carries at most the preserved default CONNECT on the database",
								p, c.object)
						}
					}
				default:
					if len(e.Privileges) > 0 {
						return assert(checkProvenance,
							"PUBLIC carries %s on %s: no migrated object carries any PUBLIC privilege",
							e.Privileges.Sorted(), c.object)
					}
				}
			}
		}
	}
	return nil
}

// checkGrantOptions rejects any grant option (the ACL text's "+" marker) on
// an entry whose grantee is the runtime role, a closure role, or PUBLIC,
// across every in-scope ACL catalog — database, schemas, relations, columns,
// functions, and types. The canonical grants of 0004-0006 carry no grant
// option; any grant option for a reachable grantee is an unauthorized
// regrant channel.
func (st *state) checkGrantOptions(runtimeRole string) error {
	for _, c := range st.aclCatalogs() {
		entries, err := privilege.ParseACL(c.aclText, c.kind)
		if err != nil {
			return fmt.Errorf("decode %s ACL: %w", c.object, err)
		}
		for _, e := range entries {
			if !e.WithGrantOption {
				continue
			}
			var reachable bool
			switch {
			case e.Public:
				reachable = true
			case e.Grantee == runtimeRole || (e.GranteeOID != 0 && e.GranteeOID == st.roleOID):
				reachable = true
			default:
				reachable, err = st.granteeInClosure(e)
				if err != nil {
					return err
				}
			}
			if reachable {
				return assert(checkGrantOption,
					"%s on %s carries a grant option for %s: the canonical grants carry no grant option, and a grant option is an unauthorized regrant channel",
					e.Privileges.Sorted(), c.object, st.aclSource(e, runtimeRole))
			}
		}
	}
	return nil
}

// checkMembershipClosure is part 4: the membership closure's privileged-role
// rule. Every role in the runtime role's full reachable membership closure —
// the roleids of every reachable membership edge, whatever its options, any
// depth — must not be the configured migration identity and must not carry
// a privileged role attribute (superuser, BYPASSRLS, CREATEDB, or
// CREATEROLE) or own the connected database, a migrated schema, or any
// in-scope object (every pg_class row, every pg_type row, and every pg_proc
// row in the two schemas). The membership-option half of the zero-membership
// rule (no reachable set_option- or admin_option-true edge) is enforced by
// checkGrantBoundary. A closure role absent from the pg_roles snapshot is a
// fail-closed error: its attributes cannot be verified.
func checkMembershipClosure(st *state, migrationRole string) error {
	owner := func(o int32) bool {
		if o == st.roleOID {
			return false
		}
		_, inClosure := st.subgraph.Closure[o]
		return inClosure
	}
	// ownerships records, for each closure owner OID, the first in-scope
	// object it owns in sweep order, so the named violation is deterministic.
	ownerships := map[int32]string{}
	record := func(o int32, object string) {
		if owner(o) {
			if _, ok := ownerships[o]; !ok {
				ownerships[o] = object
			}
		}
	}
	record(st.db.Owner, "database "+st.db.Name)
	for _, s := range st.schemas {
		record(s.Owner, "schema "+s.Name)
	}
	for _, r := range st.relations {
		record(r.Owner, "relation "+r.Nsp+"."+r.Relname)
	}
	for _, t := range st.types {
		record(t.Owner, "type "+t.Nsp+"."+t.Typname)
	}
	for _, f := range st.functions {
		record(f.Owner, "function "+f.Nsp+"."+f.Proname+"("+f.IdentityArgs+")")
	}

	// Deterministic check order: closure OIDs ascending.
	closureOIDs := make([]int32, 0, len(st.subgraph.Closure))
	for oid := range st.subgraph.Closure {
		closureOIDs = append(closureOIDs, oid)
	}
	sort.Slice(closureOIDs, func(i, j int) bool { return closureOIDs[i] < closureOIDs[j] })
	for _, oid := range closureOIDs {
		info, ok := st.roles[oid]
		if !ok {
			return assert(checkClosureRole,
				"role %d in the runtime role's membership closure is absent from the pg_roles snapshot; the closure cannot be verified",
				oid)
		}
		if info.Name == migrationRole {
			return assert(checkClosureRole,
				"closure role %s is the configured migration identity %s: the runtime role must not reach the migration identity through membership",
				info.Name, migrationRole)
		}
		for _, attr := range []struct {
			name  string
			value bool
		}{
			{"rolsuper", info.Super},
			{"rolbypassrls", info.BypassRLS},
			{"rolcreatedb", info.CreateDB},
			{"rolcreaterole", info.CreateRole},
		} {
			if attr.value {
				return assert(checkClosureRole,
					"closure role %s has privileged attribute %s = true: no role in the runtime role's membership closure may carry a privileged attribute",
					info.Name, attr.name)
			}
		}
		if obj, ok := ownerships[oid]; ok {
			return assert(checkClosureRole,
				"closure role %s owns %s: no role in the runtime role's membership closure may own the database, a migrated schema, or any object in them",
				info.Name, obj)
		}
	}
	return nil
}

// sqlString renders a value read from the service's own catalogs as a
// single-quoted SQL string constant, with embedded single quotes doubled.
func sqlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// aclText dereferences a catalog ACL field, a nil (NULL) ACL being the empty
// text that ParseACL decodes to no entries.
func aclText(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
