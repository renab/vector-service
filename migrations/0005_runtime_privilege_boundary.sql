BEGIN;

-------------------------------------------------------------------------------
-- Runtime privilege boundary
--
-- Migrations 0001-0004 create the migrated objects, and migration 0004
-- grants vector_api its direct database, schema, and table privileges.
-- This migration completes the canonical vector_api privilege boundary
-- that the vector-service startup assertion enforces (implementation
-- overview, invariant 4; package 1). After it, the runtime role's
-- effective privileges -- direct grants, membership grants (none by
-- contract), and PUBLIC grants -- equal exactly:
--
--   - the direct grants of migration 0004
--   - the direct grants established here: EXECUTE on the migrated
--     functions and USAGE on the migrated enum type
--   - the preserved PUBLIC default: CONNECT on the database vector
--
-- and nothing else.
--
-- PostgreSQL 18 grants defaults within the migrated scope that the
-- canonical boundary excludes; they are revoked from PUBLIC here:
--
--   - TEMPORARY on database vector (granted by default; the service
--     never creates temporary tables)
--   - EXECUTE on every function in vector_control and vector_data
--     (PostgreSQL grants EXECUTE to PUBLIC on new functions)
--   - USAGE on every type in vector_control and vector_data (PostgreSQL
--     grants USAGE to PUBLIC on new types)
--
-- The one other default in scope, CONNECT on the database, is preserved:
-- it does not extend the runtime role's effective boundary beyond the
-- canonical set, because vector_api already holds CONNECT directly via
-- 0004. Schemas and tables grant nothing to PUBLIC by default, so there
-- is nothing else to preserve or revoke.
--
-- The service's own use of the migrated functions and type is granted
-- directly to vector_api instead of relying on PUBLIC:
--
--   - EXECUTE on vector_data.validate_vector_record() and
--     vector_control.set_updated_at(): the service's INSERT and UPDATE
--     statements fire these row triggers, and the canonical boundary
--     requires the runtime role's function privileges to be direct
--     grants, not PUBLIC-derived.
--   - USAGE on vector_control.distance_metric: the service reads the
--     vector_spaces.distance_metric column on every vector-space
--     resolution, and the canonical boundary requires the runtime
--     role's type privileges to be direct grants, not PUBLIC-derived.
--
-- GRANT and REVOKE are idempotent and fully transactional: re-running
-- this file against a converged database changes nothing, and the runner
-- executes it in one transaction together with its history row.
-------------------------------------------------------------------------------

-------------------------------------------------------------------------------
-- PUBLIC baseline: revoke the defaults the canonical boundary excludes
-------------------------------------------------------------------------------

REVOKE TEMPORARY
ON DATABASE vector
FROM PUBLIC;

REVOKE EXECUTE
ON FUNCTION vector_control.set_updated_at()
FROM PUBLIC;

REVOKE EXECUTE
ON FUNCTION vector_data.validate_vector_record()
FROM PUBLIC;

REVOKE USAGE
ON TYPE vector_control.distance_metric
FROM PUBLIC;

-------------------------------------------------------------------------------
-- Direct vector_api grants: the service's own use of migrated objects
-------------------------------------------------------------------------------

GRANT EXECUTE
ON FUNCTION vector_control.set_updated_at()
TO vector_api;

GRANT EXECUTE
ON FUNCTION vector_data.validate_vector_record()
TO vector_api;

GRANT USAGE
ON TYPE vector_control.distance_metric
TO vector_api;

COMMIT;
