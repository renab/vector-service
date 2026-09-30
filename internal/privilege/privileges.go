// Package privilege implements the PostgreSQL catalog mechanics shared by the
// runtime identity assertion (internal/db) and the migration-side identity
// assertion and canonical ownership gate (internal/migrate): privilege name
// sets, the PostgreSQL ACL text format, role and membership catalog reads,
// and the ownership sweeps over the migrated schemas.
//
// All checks are fail-closed: an unreadable catalog, an unexpected row shape,
// or an ACL entry that cannot be decoded is an error, never a skipped check.
// No query in this package accepts a caller-controlled value: object names
// are read from the service's own catalogs and quoted with the standard
// identifier-quoting rules before they are used in a predicate. No query
// result in this package contains credentials or TLS material.
package privilege

import (
	"fmt"
	"strings"
)

// Privilege names, as spelled in PostgreSQL's ACL text format and in the
// GRANT/REVOKE syntax.
const (
	PrivConnect    = "CONNECT"
	PrivCreate     = "CREATE"
	PrivTemporary  = "TEMPORARY"
	PrivUsage      = "USAGE"
	PrivSelect     = "SELECT"
	PrivInsert     = "INSERT"
	PrivUpdate     = "UPDATE"
	PrivDelete     = "DELETE"
	PrivTruncate   = "TRUNCATE"
	PrivReferences = "REFERENCES"
	PrivTrigger    = "TRIGGER"
	PrivMaintain   = "MAINTAIN"
	PrivExecute    = "EXECUTE"
	PrivRead       = "READ"
	PrivWrite      = "WRITE"
)

// Kind is the class of a PostgreSQL object for privilege purposes. It selects
// the privilege letter alphabet of the object's ACL catalog.
type Kind int

// Object kinds.
const (
	// KindClass covers table-privilege relations: tables, views,
	// materialized views, and foreign tables, and their columns. PostgreSQL
	// 18's table privilege set includes MAINTAIN.
	KindClass Kind = iota
	// KindSequence covers sequences.
	KindSequence
	// KindDatabase covers databases.
	KindDatabase
	// KindNamespace covers schemas.
	KindNamespace
	// KindFunction covers functions and procedures.
	KindFunction
	// KindType covers types, including enums, domains, base types, and
	// composite and true-array types.
	KindType
	// KindLanguage covers procedural languages.
	KindLanguage
)

// kindPrivileges is the set of applicable privileges for a kind, exactly the
// set PostgreSQL defines for the object class in aclitem.c. MAINTAIN is part
// of the PostgreSQL 18 table-privilege set and is canonically absent on every
// in-scope object.
func kindPrivileges(k Kind) map[string]struct{} {
	switch k {
	case KindClass:
		return set(
			PrivSelect, PrivInsert, PrivUpdate, PrivDelete,
			PrivTruncate, PrivReferences, PrivTrigger, PrivMaintain,
		)
	case KindSequence:
		return set(PrivUsage, PrivSelect, PrivUpdate)
	case KindDatabase:
		return set(PrivConnect, PrivCreate, PrivTemporary)
	case KindNamespace:
		return set(PrivUsage, PrivCreate)
	case KindFunction:
		return set(PrivExecute)
	case KindType:
		return set(PrivUsage)
	case KindLanguage:
		return set(PrivUsage)
	default:
		panic(fmt.Sprintf("privilege: unknown kind %d", int(k)))
	}
}

// PrivilegesForKind returns the applicable privilege names for k.
func PrivilegesForKind(k Kind) []string {
	s := make([]string, 0, len(kindPrivileges(k)))
	for p := range kindPrivileges(k) {
		s = append(s, p)
	}
	return s
}

// columnPrivileges is the column-applicable subset of the table-privilege set:
// the privileges that may be granted or checked per column. The engine reports
// a table's privileges on each of its columns for exactly these.
var columnPrivileges = []string{
	PrivSelect, PrivInsert, PrivUpdate, PrivReferences,
}

// ColumnPrivileges returns the column-applicable privileges.
func ColumnPrivileges() []string {
	return append([]string(nil), columnPrivileges...)
}

func set(items ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(items))
	for _, s := range items {
		m[s] = struct{}{}
	}
	return m
}

// Set is an explicit set of privileges.
type Set map[string]struct{}

// NewSet builds a privilege set.
func NewSet(privs ...string) Set {
	return set(privs...)
}

// Clone returns a copy of the set.
func (s Set) Clone() Set {
	if s == nil {
		return nil
	}
	m := make(Set, len(s))
	for p := range s {
		m[p] = struct{}{}
	}
	return m
}

// Has reports whether the set contains p.
func (s Set) Has(p string) bool {
	_, ok := s[p]
	return ok
}

// Equal reports whether two sets contain exactly the same privileges.
func (s Set) Equal(other Set) bool {
	if len(s) != len(other) {
		return false
	}
	for p := range s {
		if !other.Has(p) {
			return false
		}
	}
	return true
}

// SubsetOf reports whether every privilege of s is in other.
func (s Set) SubsetOf(other Set) bool {
	for p := range s {
		if !other.Has(p) {
			return false
		}
	}
	return true
}

// Sorted returns the set's privileges in deterministic order.
func (s Set) Sorted() []string {
	items := make([]string, 0, len(s))
	for p := range s {
		items = append(items, p)
	}
	// Deterministic order: privilege-name sort.
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j] < items[j-1]; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
	return items
}

// ContainsAll reports whether every privilege in want is present in s.
func (s Set) ContainsAll(want Set) bool {
	return want.SubsetOf(s)
}

// columnSubset returns the column-applicable subset of a table's privilege
// set: exactly the intersection with ColumnPrivileges, nothing else.
func (s Set) ColumnSubset() Set {
	out := Set{}
	for _, p := range columnPrivileges {
		if s.Has(p) {
			out[p] = struct{}{}
		}
	}
	return out
}

// quoteIdent renders a catalog object name as a quoted SQL identifier, with
// embedded double quotes doubled. The names are values read from the
// service's own catalogs, never caller input.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// QualifiedName renders schema.name with identifier quoting.
func QualifiedName(schema, name string) string {
	return quoteIdent(schema) + "." + quoteIdent(name)
}

// ColumnName renders schema.table.column with identifier quoting.
func ColumnName(schema, table, column string) string {
	return quoteIdent(schema) + "." + quoteIdent(table) + "." + quoteIdent(column)
}

// FunctionName renders schema.name(args) with identifier quoting. args is the
// identity argument list, empty for a zero-argument function.
func FunctionName(schema, name, args string) string {
	return quoteIdent(schema) + "." + quoteIdent(name) + "(" + args + ")"
}
