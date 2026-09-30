package testdb

import (
	"context"
	"fmt"
)

// The negative-identity variant builders (5a, "Negative identity
// variants"). Every variant is a rejection fixture: it breaks the
// canonical provisioning shape in the disposable suite database, built by
// the bootstrap identity, pre-convergence or post-convergence as the
// package-1 validation case names it. The runner's canonical-ownership
// gate or the runtime identity assertion must reject the state; the
// harness never asserts a variant as a viable deployment and never
// converges from one.
//
// Fixture role names follow the package-1 validation text: G and M are
// harmless intermediate roles, H a harmless role that owns a migrated
// object, P a BYPASSRLS role, S a superuser, U an unreachable role. They
// are cluster-global fixture roles, created idempotently, and own nothing
// once the suite database is dropped.

// execAsBootstrap runs the statements sequentially against the suite
// database as the bootstrap identity. The bootstrap identity is used only
// for provisioning and test-only fixture DDL — never to exercise service
// code paths.
func (h *Harness) execAsBootstrap(ctx context.Context, stmts ...string) error {
	conn, err := h.BootstrapConn(ctx, DatabaseName)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s); err != nil {
			return fmt.Errorf("testdb: apply fixture: %w", err)
		}
	}
	return nil
}

// CreateRole creates a test fixture role with the named role attributes
// (e.g. "SUPERUSER", "BYPASSRLS", "LOGIN", "INHERIT"). It is idempotent:
// if the role already exists the call succeeds without error, so re-runs
// observe the same fixture shape. It uses a two-step check-then-create
// because PostgreSQL CREATE ROLE has no IF NOT EXISTS clause.
func (h *Harness) CreateRole(ctx context.Context, name string, attrs ...string) error {
	conn, err := h.BootstrapConn(ctx, h.cfg.Database)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	// Check existence first to avoid "role already exists" failure.
	var exists bool
	if err := conn.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = $1)", name).Scan(&exists); err != nil {
		return fmt.Errorf("testdb: check role %q existence: %w", name, err)
	}
	if exists {
		return nil
	}

	stmt := "CREATE ROLE " + qi(name)
	for _, a := range attrs {
		stmt += " " + a
	}
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("testdb: create role %q: %w", name, err)
	}
	return nil
}

// GrantMembership grants the named role to member with the PostgreSQL 18
// membership options set explicitly —
//
//	GRANT <role> TO <member> WITH INHERIT <b>, SET <b>, ADMIN <b>
//
// — as every package-1 membership fixture requires, so the rejection
// under test is isolated from the other options.
func (h *Harness) GrantMembership(ctx context.Context, role, member string, inherit, set, admin bool) error {
	stmt := fmt.Sprintf(
		"GRANT %s TO %s WITH INHERIT %t, SET %t, ADMIN %t",
		qi(role), qi(member), inherit, set, admin,
	)
	return h.execAsBootstrap(ctx, stmt)
}

// grantOptionStmt renders GRANT <privileges> ON <object> TO <grantee> WITH
// GRANT OPTION for the named object kind.
func grantOptionStmt(kind, privileges, object, grantee string) string {
	var ref string
	switch kind {
	case "database":
		ref = "DATABASE " + qi(object)
	case "schema":
		ref = "SCHEMA " + qi(object)
	default:
		ref = object // already a qualified object reference (table, column list, function, type)
	}
	return fmt.Sprintf("GRANT %s ON %s TO %s WITH GRANT OPTION", privileges, ref, qi(grantee))
}

// ---------------------------------------------------------------------
// Ownership variants (rejected by the runner's canonical-ownership gate)
// ---------------------------------------------------------------------

// PrecreateSchemaOwnedByRuntime pre-creates the named migrated schema
// owned by the runtime role (validation cases 1–2, pre-convergence
// variant): the canonical-ownership gate must reject the run before
// bootstrap, with no bootstrap DDL and no history write.
func (h *Harness) PrecreateSchemaOwnedByRuntime(ctx context.Context, schema string) error {
	return h.execAsBootstrap(ctx,
		"CREATE SCHEMA "+qi(schema)+" AUTHORIZATION "+qi(RuntimeRole),
	)
}

// PrecreateHistoryTableOwnedByRuntime pre-creates the canonical
// vector_control schema owned by the migration identity together with the
// migration-history table owned by the runtime role (validation case 3):
// the schemas remain canonically owned, so the gate's history check is
// what fires. The table is created with valid DDL and ownership is
// transferred via ALTER TABLE since CREATE TABLE does not accept an
// AUTHORIZATION clause.
func (h *Harness) PrecreateHistoryTableOwnedByRuntime(ctx context.Context) error {
	return h.execAsBootstrap(ctx,
		"CREATE SCHEMA vector_control AUTHORIZATION "+qi(MigrationRole),
		"CREATE TABLE vector_control.schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())",
		"ALTER TABLE vector_control.schema_migrations OWNER TO "+qi(RuntimeRole),
	)
}

// TransferSchemaOwnership transfers a migrated schema's ownership (the
// post-convergence form of cases 1–2): the next migrate pass must fail the
// canonical-ownership gate.
func (h *Harness) TransferSchemaOwnership(ctx context.Context, schema, owner string) error {
	return h.execAsBootstrap(ctx, "ALTER SCHEMA "+qi(schema)+" OWNER TO "+qi(owner))
}

// TransferTableOwnership transfers a migrated table's ownership
// (validation cases 4, 9a, 21): the gate rejects on pg_class.relowner.
func (h *Harness) TransferTableOwnership(ctx context.Context, table, owner string) error {
	return h.execAsBootstrap(ctx, "ALTER TABLE "+table+" OWNER TO "+qi(owner))
}

// TransferFunctionOwnership transfers a migrated function's ownership
// (validation cases 19a, 19b, 22): the gate rejects on pg_proc.proowner.
// function is the full qualified reference including its argument list,
// e.g. "vector_data.validate_vector_record()".
func (h *Harness) TransferFunctionOwnership(ctx context.Context, function, owner string) error {
	return h.execAsBootstrap(ctx, "ALTER FUNCTION "+function+" OWNER TO "+qi(owner))
}

// ---------------------------------------------------------------------
// Membership variants (rejected by the runtime identity assertion's
// zero-membership rule or membership-closure rule)
// ---------------------------------------------------------------------

// GrantMigrationIdentityToRuntime gives the runtime role direct membership
// in the migration identity with all options false (validation case 5):
// the closure reaches the migration identity.
func (h *Harness) GrantMigrationIdentityToRuntime(ctx context.Context) error {
	return h.GrantMembership(ctx, MigrationRole, RuntimeRole, false, false, false)
}

// GrantMigrationIdentityToRuntimeViaIntermediate gives the runtime role
// transitive membership in the migration identity through a fresh
// intermediate role (validation case 6), all options false.
func (h *Harness) GrantMigrationIdentityToRuntimeViaIntermediate(ctx context.Context, intermediate string) error {
	if err := h.CreateRole(ctx, intermediate); err != nil {
		return err
	}
	if err := h.GrantMembership(ctx, MigrationRole, intermediate, false, false, false); err != nil {
		return err
	}
	return h.GrantMembership(ctx, intermediate, RuntimeRole, false, false, false)
}

// GrantSetOptionMembershipToRuntime gives the runtime role a
// set_option-true membership in a harmless role (validation case 16):
// the zero-membership rule rejects the assumption right, direct form.
func (h *Harness) GrantSetOptionMembershipToRuntime(ctx context.Context, role string) error {
	if err := h.CreateRole(ctx, role); err != nil {
		return err
	}
	return h.GrantMembership(ctx, role, RuntimeRole, false, true, false)
}

// GrantSetOptionMembershipToRuntimeViaIntermediate is the two-hop form of
// the set_option-true fixture through a fresh intermediate role (case 16).
func (h *Harness) GrantSetOptionMembershipToRuntimeViaIntermediate(ctx context.Context, role, intermediate string) error {
	if err := h.CreateRole(ctx, role); err != nil {
		return err
	}
	if err := h.CreateRole(ctx, intermediate); err != nil {
		return err
	}
	if err := h.GrantMembership(ctx, role, intermediate, false, true, false); err != nil {
		return err
	}
	return h.GrantMembership(ctx, intermediate, RuntimeRole, false, true, false)
}

// GrantAdminOptionMembershipToRuntime gives the runtime role an
// admin_option-true membership in a harmless role (validation case 20,
// the release-critical ADMIN-only fixture): the edge conveys no privilege
// and permits no SET ROLE assumption, but the admin option is the regrant
// authority.
func (h *Harness) GrantAdminOptionMembershipToRuntime(ctx context.Context, role string) error {
	if err := h.CreateRole(ctx, role); err != nil {
		return err
	}
	return h.GrantMembership(ctx, role, RuntimeRole, false, false, true)
}

// GrantAdminOptionMembershipToRuntimeViaIntermediate is the two-hop form
// of the admin_option-true fixture through a fresh intermediate role
// (case 20).
func (h *Harness) GrantAdminOptionMembershipToRuntimeViaIntermediate(ctx context.Context, role, intermediate string) error {
	if err := h.CreateRole(ctx, role); err != nil {
		return err
	}
	if err := h.CreateRole(ctx, intermediate); err != nil {
		return err
	}
	if err := h.GrantMembership(ctx, role, intermediate, false, false, true); err != nil {
		return err
	}
	return h.GrantMembership(ctx, intermediate, RuntimeRole, false, false, true)
}

// GrantHarmlessRoleToRuntime gives the runtime role a membership in a
// harmless role that owns a migrated object (validation case 9a).
func (h *Harness) GrantHarmlessRoleToRuntime(ctx context.Context, role, table string) error {
	if err := h.CreateRole(ctx, role); err != nil {
		return err
	}
	if err := h.TransferTableOwnership(ctx, table, role); err != nil {
		return err
	}
	return h.GrantMembership(ctx, role, RuntimeRole, false, false, false)
}

// GrantBypassRLSRoleToRuntime creates a BYPASSRLS role and gives the
// runtime role a membership in it (validation case 9b).
func (h *Harness) GrantBypassRLSRoleToRuntime(ctx context.Context, role string) error {
	if err := h.CreateRole(ctx, role, "BYPASSRLS"); err != nil {
		return err
	}
	return h.GrantMembership(ctx, role, RuntimeRole, false, false, false)
}

// GrantSuperuserRoleToRuntime creates a superuser role and gives the
// runtime role a membership in it (validation case 9c).
func (h *Harness) GrantSuperuserRoleToRuntime(ctx context.Context, role string) error {
	if err := h.CreateRole(ctx, role, "SUPERUSER"); err != nil {
		return err
	}
	return h.GrantMembership(ctx, role, RuntimeRole, false, false, false)
}

// ---------------------------------------------------------------------
// Attribute variant
// ---------------------------------------------------------------------

// AlterRuntimeRoleBypassRLS sets BYPASSRLS on the runtime role
// (validation case 10): the identity assertion's role-attributes check
// fails.
func (h *Harness) AlterRuntimeRoleBypassRLS(ctx context.Context) error {
	return h.execAsBootstrap(ctx, "ALTER ROLE "+qi(RuntimeRole)+" WITH BYPASSRLS")
}

// ---------------------------------------------------------------------
// Extra direct grant variants (validation case 7)
// ---------------------------------------------------------------------

// GrantHistoryTableSelectToRuntime grants SELECT on the migration-history
// table, canonically none (case 7, first form).
func (h *Harness) GrantHistoryTableSelectToRuntime(ctx context.Context) error {
	return h.execAsBootstrap(ctx,
		"GRANT SELECT ON vector_control.schema_migrations TO "+qi(RuntimeRole),
	)
}

// GrantVectorRecordsTruncateToRuntime grants TRUNCATE on the migrated
// vector_records table, canonically absent (case 7, second form).
func (h *Harness) GrantVectorRecordsTruncateToRuntime(ctx context.Context) error {
	return h.execAsBootstrap(ctx,
		"GRANT TRUNCATE ON vector_data.vector_records TO "+qi(RuntimeRole),
	)
}

// GrantHistoryTableSelectToRole grants table-level SELECT on the
// migration-history table to a named (harmless) role, for the inherited
// grant fixture (case 8). The membership edge is granted separately via
// GrantMembership with INHERIT TRUE.
func (h *Harness) GrantHistoryTableSelectToRole(ctx context.Context, role string) error {
	return h.execAsBootstrap(ctx,
		"GRANT SELECT ON vector_control.schema_migrations TO "+qi(role),
	)
}

// ---------------------------------------------------------------------
// Column grant variants (validation cases 11–13)
// ---------------------------------------------------------------------

// GrantHistoryVersionColumnSelectToRuntime grants the runtime role the
// column-level SELECT on vector_control.schema_migrations.version, beyond
// the table's canonical set of none (case 11, first form).
func (h *Harness) GrantHistoryVersionColumnSelectToRuntime(ctx context.Context) error {
	return h.execAsBootstrap(ctx,
		"GRANT SELECT (version) ON vector_control.schema_migrations TO "+qi(RuntimeRole),
	)
}

// GrantRecordsObjectIDReferencesToRuntime grants the runtime role the
// column-level REFERENCES on vector_data.vector_records.object_id,
// canonically no REFERENCES on the table (case 11, second form).
func (h *Harness) GrantRecordsObjectIDReferencesToRuntime(ctx context.Context) error {
	return h.execAsBootstrap(ctx,
		"GRANT REFERENCES (object_id) ON vector_data.vector_records TO "+qi(RuntimeRole),
	)
}

// GrantHistoryVersionColumnSelectToRole grants the same column-level
// SELECT to a named (harmless) role, for the inherited-column-grant
// fixture (case 12). The membership edge is granted separately via
// GrantMembership with INHERIT TRUE.
func (h *Harness) GrantHistoryVersionColumnSelectToRole(ctx context.Context, role string) error {
	return h.execAsBootstrap(ctx,
		"GRANT SELECT (version) ON vector_control.schema_migrations TO "+qi(role),
	)
}

// GrantRecordsObjectIDReferencesToPublic grants the column-level
// REFERENCES on vector_data.vector_records.object_id to PUBLIC,
// canonically absent (case 13).
func (h *Harness) GrantRecordsObjectIDReferencesToPublic(ctx context.Context) error {
	return h.execAsBootstrap(ctx,
		"GRANT REFERENCES (object_id) ON vector_data.vector_records TO PUBLIC",
	)
}

// ---------------------------------------------------------------------
// Grant-option variants (validation case 14, one fixture per in-scope ACL
// catalog)
// ---------------------------------------------------------------------

// GrantOptionColumnHistorySelect is the column-catalog grant-option
// fixture (stored in pg_attribute.attacl, not pg_class.relacl).
func (h *Harness) GrantOptionColumnHistorySelect(ctx context.Context) error {
	return h.execAsBootstrap(ctx,
		"GRANT SELECT (version) ON vector_control.schema_migrations TO "+qi(RuntimeRole)+" WITH GRANT OPTION",
	)
}

// GrantOptionColumnApplicationsSelect is the pure column-catalog
// grant-option fixture on a canonically held privilege: the grant option
// rides on the runtime role's existing SELECT on vector_control.applications,
// so the effective set is unchanged and the grant-option rejection itself
// is what fires.
func (h *Harness) GrantOptionColumnApplicationsSelect(ctx context.Context) error {
	return h.execAsBootstrap(ctx,
		"GRANT SELECT (id) ON vector_control.applications TO "+qi(RuntimeRole)+" WITH GRANT OPTION",
	)
}

// GrantOptionDatabaseConnect is the database-catalog grant-option fixture.
func (h *Harness) GrantOptionDatabaseConnect(ctx context.Context) error {
	return h.execAsBootstrap(ctx, grantOptionStmt("database", "CONNECT", DatabaseName, RuntimeRole))
}

// GrantOptionSchemaUsage is the schema-catalog grant-option fixture.
func (h *Harness) GrantOptionSchemaUsage(ctx context.Context, schema string) error {
	return h.execAsBootstrap(ctx, grantOptionStmt("schema", "USAGE", schema, RuntimeRole))
}

// GrantOptionFunctionExecute is the function-catalog grant-option fixture.
func (h *Harness) GrantOptionFunctionExecute(ctx context.Context) error {
	return h.execAsBootstrap(ctx,
		"GRANT EXECUTE ON FUNCTION vector_data.validate_vector_record() TO "+qi(RuntimeRole)+" WITH GRANT OPTION",
	)
}

// GrantOptionTypeUsage is the type-catalog grant-option fixture.
func (h *Harness) GrantOptionTypeUsage(ctx context.Context) error {
	return h.execAsBootstrap(ctx,
		"GRANT USAGE ON TYPE vector_control.distance_metric TO "+qi(RuntimeRole)+" WITH GRANT OPTION",
	)
}

// GrantOptionTableSelect is the table-catalog grant-option fixture on
// vector_control.applications.
func (h *Harness) GrantOptionTableSelect(ctx context.Context) error {
	return h.execAsBootstrap(ctx,
		"GRANT SELECT ON vector_control.applications TO "+qi(RuntimeRole)+" WITH GRANT OPTION",
	)
}

// GrantOptionTableVectorRecords is the table-catalog grant-option fixture
// on the canonical vector_data.vector_records table.
func (h *Harness) GrantOptionTableVectorRecords(ctx context.Context) error {
	return h.execAsBootstrap(ctx,
		"GRANT UPDATE ON vector_data.vector_records TO "+qi(RuntimeRole)+" WITH GRANT OPTION",
	)
}

// GrantMaintainOnVectorRecords grants PostgreSQL 18's MAINTAIN on the
// migrated vector_records table, canonically absent everywhere (case 15).
func (h *Harness) GrantMaintainOnVectorRecords(ctx context.Context) error {
	return h.execAsBootstrap(ctx,
		"GRANT MAINTAIN ON vector_data.vector_records TO "+qi(RuntimeRole),
	)
}

// ---------------------------------------------------------------------
// Provenance-substitution variants (validation cases 17–18): the direct
// canonical grant is revoked and the same privilege is granted through a
// harmless role G or to PUBLIC, leaving the effective set — every has_*
// predicate — unchanged.
// ---------------------------------------------------------------------

// substituteInherited revokes the named privileges from the runtime role
// on the named object, grants them to a fresh harmless role, and gives
// the runtime role an INHERIT-true membership in that role. The role name
// is supplied by the caller so that each test invocation can use a unique,
// UUID-based fixture role that is cleaned up after the test.
func (h *Harness) substituteInherited(ctx context.Context, revoke, grant, role string) error {
	if err := h.CreateRole(ctx, role); err != nil {
		return err
	}
	if err := h.execAsBootstrap(ctx, revoke); err != nil {
		return err
	}
	if err := h.execAsBootstrap(ctx, grant+" TO "+qi(role)); err != nil {
		return err
	}
	return h.GrantMembership(ctx, role, RuntimeRole, true, false, false)
}

// SubstituteDatabaseConnectInherited substitutes the canonical database
// CONNECT direct grant via a harmless role (case 17, database form).
func (h *Harness) SubstituteDatabaseConnectInherited(ctx context.Context, role string) error {
	return h.substituteInherited(ctx,
		"REVOKE CONNECT ON DATABASE "+qi(DatabaseName)+" FROM "+qi(RuntimeRole),
		"GRANT CONNECT ON DATABASE "+qi(DatabaseName),
		role,
	)
}

// SubstituteSchemaUsageInherited substitutes the canonical schema USAGE
// direct grant via a harmless role (case 17, schema form).
func (h *Harness) SubstituteSchemaUsageInherited(ctx context.Context, schema, role string) error {
	return h.substituteInherited(ctx,
		"REVOKE USAGE ON SCHEMA "+qi(schema)+" FROM "+qi(RuntimeRole),
		"GRANT USAGE ON SCHEMA "+qi(schema),
		role,
	)
}

// SubstituteTablePrivilegesInherited substitutes the canonical table
// privilege direct grant via a harmless role (case 17, table form).
// table is the qualified table and privileges the table-level privilege
// list, e.g. ("vector_control.applications", "SELECT, INSERT, UPDATE").
func (h *Harness) SubstituteTablePrivilegesInherited(ctx context.Context, table, privileges, role string) error {
	return h.substituteInherited(ctx,
		"REVOKE "+privileges+" ON "+table+" FROM "+qi(RuntimeRole),
		"GRANT "+privileges+" ON "+table,
		role,
	)
}

// SubstituteColumnPrivilegesInherited substitutes the canonical
// column-level privileges (as granted at table level, which the engine
// reports on each column) via a harmless role (case 17, column form).
func (h *Harness) SubstituteColumnPrivilegesInherited(ctx context.Context, table, privileges, role string) error {
	return h.substituteInherited(ctx,
		"REVOKE "+privileges+" ON "+table+" FROM "+qi(RuntimeRole),
		"GRANT "+privileges+" ON "+table,
		role,
	)
}

// SubstituteFunctionExecuteInherited substitutes the canonical function
// EXECUTE direct grant via a harmless role (case 17, function form).
func (h *Harness) SubstituteFunctionExecuteInherited(ctx context.Context, function, role string) error {
	return h.substituteInherited(ctx,
		"REVOKE EXECUTE ON FUNCTION "+function+" FROM "+qi(RuntimeRole),
		"GRANT EXECUTE ON FUNCTION "+function,
		role,
	)
}

// SubstituteTypeUsageInherited substitutes the canonical type USAGE
// direct grant via a harmless role (case 17, type form).
func (h *Harness) SubstituteTypeUsageInherited(ctx context.Context, typ, role string) error {
	return h.substituteInherited(ctx,
		"REVOKE USAGE ON TYPE "+typ+" FROM "+qi(RuntimeRole),
		"GRANT USAGE ON TYPE "+typ,
		role,
	)
}

// substitutePublic revokes the named privileges from the runtime role on
// the named object and grants them to PUBLIC.
func (h *Harness) substitutePublic(ctx context.Context, revoke, grant string) error {
	if err := h.execAsBootstrap(ctx, revoke); err != nil {
		return err
	}
	return h.execAsBootstrap(ctx, grant+" TO PUBLIC")
}

// SubstituteSchemaUsagePublic substitutes the canonical schema USAGE
// direct grant via PUBLIC (case 18, schema form — the spec's example).
func (h *Harness) SubstituteSchemaUsagePublic(ctx context.Context, schema string) error {
	return h.substitutePublic(ctx,
		"REVOKE USAGE ON SCHEMA "+qi(schema)+" FROM "+qi(RuntimeRole),
		"GRANT USAGE ON SCHEMA "+qi(schema),
	)
}

// SubstituteTablePrivilegesPublic substitutes the canonical table
// privilege direct grant via PUBLIC (case 18, table form).
func (h *Harness) SubstituteTablePrivilegesPublic(ctx context.Context, table, privileges string) error {
	return h.substitutePublic(ctx,
		"REVOKE "+privileges+" ON "+table+" FROM "+qi(RuntimeRole),
		"GRANT "+privileges+" ON "+table,
	)
}

// SubstituteColumnPrivilegesPublic substitutes the canonical column-level
// privileges (as granted at table level) via PUBLIC (case 18, column
// form).
func (h *Harness) SubstituteColumnPrivilegesPublic(ctx context.Context, table, privileges string) error {
	return h.substitutePublic(ctx,
		"REVOKE "+privileges+" ON "+table+" FROM "+qi(RuntimeRole),
		"GRANT "+privileges+" ON "+table,
	)
}

// SubstituteFunctionExecutePublic substitutes the canonical function
// EXECUTE direct grant via PUBLIC (case 18, function form).
func (h *Harness) SubstituteFunctionExecutePublic(ctx context.Context, function string) error {
	return h.substitutePublic(ctx,
		"REVOKE EXECUTE ON FUNCTION "+function+" FROM "+qi(RuntimeRole),
		"GRANT EXECUTE ON FUNCTION "+function,
	)
}

// SubstituteTypeUsagePublic substitutes the canonical type USAGE direct
// grant via PUBLIC (case 18, type form).
func (h *Harness) SubstituteTypeUsagePublic(ctx context.Context, typ string) error {
	return h.substitutePublic(ctx,
		"REVOKE USAGE ON TYPE "+typ+" FROM "+qi(RuntimeRole),
		"GRANT USAGE ON TYPE "+typ,
	)
}

// TransferFunctionOwnershipToUnreachableRole creates a fresh unreachable
// role U (no memberships, no privileges, no other ownership) and transfers
// the named migrated function to it (validation cases 19b/22 function
// form): the gate rejects on pg_proc.proowner — the rejection is the
// gate's, the unreachable owner is invisible to the runtime closure check.
func (h *Harness) TransferFunctionOwnershipToUnreachableRole(ctx context.Context, function string) error {
	if err := h.CreateRole(ctx, "U"); err != nil {
		return err
	}
	return h.TransferFunctionOwnership(ctx, function, "U")
}

// TransferTableOwnershipToUnreachableRole creates a fresh unreachable
// role U and transfers the named migrated table to it (validation case
// 21): the gate rejects on pg_class.relowner.
func (h *Harness) TransferTableOwnershipToUnreachableRole(ctx context.Context, table string) error {
	if err := h.CreateRole(ctx, "U"); err != nil {
		return err
	}
	return h.TransferTableOwnership(ctx, table, "U")
}
