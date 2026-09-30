package db

import (
	"vector-service/internal/privilege"
)

// The canonical grant table of migrations 0004, 0005, and 0006. The runtime
// role's effective privileges on every in-scope object must equal exactly
// this table: nothing more, nothing less, and every required grant must be a
// direct entry for the runtime role in the applicable catalog.
//
// Object classes, per kind:
//   - database:  CONNECT / CREATE / TEMPORARY
//   - schema:    USAGE / CREATE
//   - table:     SELECT / INSERT / UPDATE / DELETE / TRUNCATE / REFERENCES /
//     TRIGGER / MAINTAIN (PostgreSQL 18's table-privilege set includes
//     MAINTAIN; it is canonically absent everywhere)
//   - type:      USAGE
//   - function:  EXECUTE
//
// True array types (pg_type.typelem naming their element type) are not
// independent grant rows: a true array type has no independently mutable ACL,
// so the engine resolves has_type_privilege for the array by following the
// element type. The array row's effective USAGE must equal the element type's
// canonical USAGE in both directions, and the array type's pg_type.typacl
// carries no entry for the runtime role, any closure role, or PUBLIC.

// canonicalDatabase is the database row of the canonical table: CONNECT via
// the 0004 direct grant. No CREATE, no TEMPORARY.
var canonicalDatabase = privilege.NewSet(privilege.PrivConnect)

// canonicalSchemas is the schema row of the canonical table: USAGE via the
// 0004 direct grants. No CREATE.
var canonicalSchemas = privilege.NewSet(privilege.PrivUsage)

// canonicalTables is the table row of the canonical table, keyed by
// schema.table. vector_control.schema_migrations is deliberately absent: it
// is the only schema object with no runtime grants, so the runtime role can
// never read or write migration history. Every other in-scope table, view,
// sequence, materialized view, or future relation carries none.
var canonicalTables = map[string]privilege.Set{
	"vector_control.applications":            privilege.NewSet(privilege.PrivSelect, privilege.PrivInsert, privilege.PrivUpdate),
	"vector_control.namespaces":              privilege.NewSet(privilege.PrivSelect, privilege.PrivInsert, privilege.PrivUpdate),
	"vector_control.vector_spaces":           privilege.NewSet(privilege.PrivSelect),
	"vector_control.application_credentials": privilege.NewSet(privilege.PrivSelect, privilege.PrivInsert, privilege.PrivUpdate, privilege.PrivDelete),
	"vector_data.vector_records":             privilege.NewSet(privilege.PrivSelect, privilege.PrivInsert, privilege.PrivUpdate, privilege.PrivDelete),
}

// canonicalFunctions is the function row of the canonical table, keyed by
// schema.name(argument list). The two 0005 direct EXECUTE grants are the
// migrated trigger functions; the service's INSERT and UPDATE statements fire
// their row triggers, and the canonical boundary requires direct grants, not
// PUBLIC-derived privileges. Every other in-scope function carries none
// (0005 revoked the default PUBLIC EXECUTE on both, and nothing else is
// granted).
var canonicalFunctions = map[string]privilege.Set{
	"vector_control.set_updated_at()":      privilege.NewSet(privilege.PrivExecute),
	"vector_data.validate_vector_record()": privilege.NewSet(privilege.PrivExecute),
}

// canonicalTypes is the independent-type row of the canonical table, keyed by
// schema.typname. The distance_metric enum carries its 0005 direct USAGE
// grant. Every other in-scope type carries none: the implicit row (composite)
// types of the six migrated tables had their default PUBLIC USAGE revoked by
// 0006 and have no direct grant, and no other enum, composite, domain, or
// base type is granted. True array types are not in this map: they are the
// dependent privilege view, modeled through their element type.
var canonicalTypes = map[string]privilege.Set{
	"vector_control.distance_metric": privilege.NewSet(privilege.PrivUsage),
}

// tablePrivileges is the kindClass privilege grid applied to every
// table-privilege relation in scope.
var tablePrivileges = privilege.PrivilegesForKind(privilege.KindClass)

// schemaPrivileges is the privilege grid applied to every in-scope schema.
var schemaPrivileges = privilege.PrivilegesForKind(privilege.KindNamespace)

// databasePrivileges is the privilege grid applied to the in-scope database.
var databasePrivileges = privilege.PrivilegesForKind(privilege.KindDatabase)

// typePrivilege is the single privilege of a type.
var typePrivilege = privilege.PrivUsage

// functionPrivilege is the single privilege of a function or procedure.
var functionPrivilege = privilege.PrivExecute

// tableRelationKinds are the pg_class relkinds checked for table privileges:
// ordinary tables, partitioned tables, views, materialized views, and
// foreign tables.
var tableRelationKinds = map[string]bool{
	"r": true, // ordinary table
	"p": true, // partitioned table
	"v": true, // view
	"m": true, // materialized view
	"f": true, // foreign table
}

// indexRelationKinds are the pg_class relkinds that carry no independent ACL:
// indexes and partitioned indexes. Their access rights are exactly the
// owning table's, which is already checked, so the check asserts their
// relacl is NULL.
var indexRelationKinds = map[string]bool{
	"i": true, // index
	"I": true, // partitioned index
}

// canonicalTablesKey renders the schema.table key used by canonicalTables.
func canonicalTablesKey(schema, table string) string {
	return schema + "." + table
}

// canonicalFunctionsKey renders the schema.name(args) key used by
// canonicalFunctions.
func canonicalFunctionsKey(f privilege.FunctionRow) string {
	return f.Nsp + "." + f.Proname + "(" + f.IdentityArgs + ")"
}

// canonicalTypesKey renders the schema.typname key used by canonicalTypes.
func canonicalTypesKey(t privilege.TypeRow) string {
	return t.Nsp + "." + t.Typname
}
