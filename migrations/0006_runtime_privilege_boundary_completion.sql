BEGIN;

-------------------------------------------------------------------------------
-- Runtime privilege boundary completion
--
-- Migration 0005 established the PUBLIC baseline of the canonical
-- vector_api privilege boundary (implementation overview, invariant 4;
-- package 1): it revoked the PostgreSQL 18 PUBLIC defaults for the
-- database (TEMPORARY) and the migrated functions (EXECUTE), and
-- revoked PUBLIC USAGE on the migrated enum type
-- vector_control.distance_metric.
--
-- This migration completes the type half of that baseline. PostgreSQL 18
-- grants USAGE to PUBLIC by default on every type -- including the
-- implicit row (composite) type created for every table, whose type
-- name equals the table name and which lives in the table's schema.
-- Migration 0005 revoked the enum type only, leaving PUBLIC USAGE on
-- every migrated table's row type: the runtime role's effective
-- privileges then included PUBLIC USAGE on six row types that the
-- canonical boundary excludes.
--
-- This file revokes PUBLIC USAGE on every type in the two migrated
-- schemas: the migrated enum type (already revoked by 0005; the
-- statement is a no-op on a converged database and keeps this file
-- self-contained) and the implicit row type of every migrated table,
-- including the runner-bootstrapped migration-history table.
--
-- Nothing else in the migrated scope carries a PUBLIC default: schemas
-- and tables grant nothing to PUBLIC, so 0005 plus this file exhausts
-- the PUBLIC baseline. The required direct vector_api grants -- the
-- 0004 table grants and the 0005 EXECUTE / type USAGE grants -- are
-- untouched, and the service's SQL addresses table columns, not
-- whole-row values, so runtime operation is unaffected by the revokes.
--
-- REVOKE is idempotent and fully transactional: re-running this file
-- against a converged database changes nothing, and the runner
-- executes it in one transaction together with its history row.
-------------------------------------------------------------------------------

-------------------------------------------------------------------------------
-- PUBLIC baseline: revoke the default USAGE on every type in the two
-- migrated schemas -- the migrated enum type and the implicit row
-- (composite) type of every table
-------------------------------------------------------------------------------

REVOKE USAGE
ON TYPE vector_control.distance_metric
FROM PUBLIC;

REVOKE USAGE
ON TYPE vector_control.applications
FROM PUBLIC;

REVOKE USAGE
ON TYPE vector_control.namespaces
FROM PUBLIC;

REVOKE USAGE
ON TYPE vector_control.vector_spaces
FROM PUBLIC;

REVOKE USAGE
ON TYPE vector_control.application_credentials
FROM PUBLIC;

REVOKE USAGE
ON TYPE vector_control.schema_migrations
FROM PUBLIC;

REVOKE USAGE
ON TYPE vector_data.vector_records
FROM PUBLIC;

COMMIT;
