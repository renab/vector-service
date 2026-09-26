BEGIN;

CREATE SCHEMA IF NOT EXISTS vector_control;
CREATE SCHEMA IF NOT EXISTS vector_data;

-------------------------------------------------------------------------------
-- Applications
--
-- Applications are first-class isolation boundaries.
--
-- Examples:
--   bookshelf
--   vault
--   galaxy
--
-- Application identity is never inferred from namespace or caller metadata.
-------------------------------------------------------------------------------

CREATE TABLE vector_control.applications (
    id              uuid PRIMARY KEY,
    application_key text NOT NULL UNIQUE,
    display_name    text NOT NULL,
    enabled         boolean NOT NULL DEFAULT true,

    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT applications_key_format
        CHECK (
            application_key ~ '^[a-z][a-z0-9_-]{0,63}$'
        )
);

-------------------------------------------------------------------------------
-- Namespaces
--
-- Namespaces always belong to exactly one application.
--
-- There is deliberately no global/default namespace.
-------------------------------------------------------------------------------

CREATE TABLE vector_control.namespaces (
    id              uuid PRIMARY KEY,
    application_id  uuid NOT NULL
        REFERENCES vector_control.applications(id)
        ON DELETE RESTRICT,

    namespace_key   text NOT NULL,
    display_name    text NOT NULL,
    enabled         boolean NOT NULL DEFAULT true,

    metadata        jsonb NOT NULL DEFAULT '{}'::jsonb,

    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT namespaces_metadata_object
        CHECK (jsonb_typeof(metadata) = 'object'),

    CONSTRAINT namespaces_key_not_empty
        CHECK (length(namespace_key) > 0),

    CONSTRAINT namespaces_application_key_unique
        UNIQUE (application_id, namespace_key),

    CONSTRAINT namespaces_id_application_unique
        UNIQUE (id, application_id)
);

CREATE INDEX namespaces_application_idx
    ON vector_control.namespaces (application_id);

-------------------------------------------------------------------------------
-- Vector spaces
--
-- A vector space is the compatibility contract for embeddings.
--
-- Consumers must explicitly name a vector space when writing or searching.
-- Model/query semantics remain owned by Bookshelf, Vault, etc.; the shared
-- vector layer only records compatibility facts.
-------------------------------------------------------------------------------

CREATE TYPE vector_control.distance_metric AS ENUM (
    'cosine',
    'l2',
    'inner_product'
);

CREATE TABLE vector_control.vector_spaces (
    id                      uuid PRIMARY KEY,

    vector_space_key        text NOT NULL UNIQUE,

    embedding_model         text NOT NULL,
    embedding_model_version text NOT NULL,

    dimensions              integer NOT NULL,
    distance_metric         vector_control.distance_metric NOT NULL,

    enabled                 boolean NOT NULL DEFAULT true,

    created_at              timestamptz NOT NULL DEFAULT now(),
    retired_at              timestamptz,

    CONSTRAINT vector_spaces_key_format
        CHECK (
            vector_space_key ~ '^[a-z0-9][a-z0-9._-]{0,127}$'
        ),

    CONSTRAINT vector_spaces_dimensions_valid
        CHECK (
            dimensions > 0
            AND dimensions <= 16000
        ),

    CONSTRAINT vector_spaces_retirement_valid
        CHECK (
            retired_at IS NULL
            OR retired_at >= created_at
        )
);

-------------------------------------------------------------------------------
-- Vector records
--
-- This table is intentionally generic.
--
-- object_id and projection_id are opaque caller-owned identifiers.
--
-- Examples:
--
-- Vault:
--   object_id     = note/document identity
--   projection_id = chunk identity
--
-- Bookshelf:
--   object_id     = assertion/stream/etc. identity
--   projection_id = retrieval projection identity
--
-- The shared layer does not interpret either value.
-------------------------------------------------------------------------------

CREATE TABLE vector_data.vector_records (
    id              uuid PRIMARY KEY,

    application_id  uuid NOT NULL
        REFERENCES vector_control.applications(id)
        ON DELETE RESTRICT,

    namespace_id    uuid NOT NULL,

    vector_space_id uuid NOT NULL
        REFERENCES vector_control.vector_spaces(id)
        ON DELETE RESTRICT,

    object_id       text NOT NULL,
    projection_id   text NOT NULL,

    content_hash    bytea NOT NULL,

    source_updated_at timestamptz,

    metadata        jsonb NOT NULL DEFAULT '{}'::jsonb,

    embedding       vector NOT NULL,

    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT vector_records_namespace_application_fk
        FOREIGN KEY (namespace_id, application_id)
        REFERENCES vector_control.namespaces(id, application_id)
        ON DELETE RESTRICT,

    CONSTRAINT vector_records_object_id_not_empty
        CHECK (length(object_id) > 0),

    CONSTRAINT vector_records_projection_id_not_empty
        CHECK (length(projection_id) > 0),

    CONSTRAINT vector_records_sha256_length
        CHECK (octet_length(content_hash) = 32),

    CONSTRAINT vector_records_metadata_object
        CHECK (jsonb_typeof(metadata) = 'object'),

    CONSTRAINT vector_records_identity_unique
        UNIQUE (
            application_id,
            namespace_id,
            object_id,
            projection_id,
            vector_space_id
        )
);

-------------------------------------------------------------------------------
-- Operational indexes
-------------------------------------------------------------------------------

CREATE INDEX vector_records_namespace_idx
    ON vector_data.vector_records (
        application_id,
        namespace_id,
        vector_space_id
    );

CREATE INDEX vector_records_object_idx
    ON vector_data.vector_records (
        application_id,
        namespace_id,
        object_id
    );

CREATE INDEX vector_records_content_hash_idx
    ON vector_data.vector_records (
        application_id,
        namespace_id,
        content_hash
    );

-------------------------------------------------------------------------------
-- Enforce vector-space dimensional compatibility.
--
-- vector is intentionally stored without vector(n), because the service may
-- support multiple embedding spaces with different dimensions over time.
--
-- pgvector permits mixed dimensions in a plain vector column. ANN indexes for
-- such data must be scoped to rows of one dimension/vector space.
-------------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION vector_data.validate_vector_record()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    expected_dimensions integer;
    space_enabled boolean;
BEGIN
    SELECT
        dimensions,
        enabled
    INTO
        expected_dimensions,
        space_enabled
    FROM vector_control.vector_spaces
    WHERE id = NEW.vector_space_id;

    IF expected_dimensions IS NULL THEN
        RAISE EXCEPTION
            'unknown vector_space_id: %',
            NEW.vector_space_id;
    END IF;

    IF NOT space_enabled THEN
        RAISE EXCEPTION
            'vector space is disabled: %',
            NEW.vector_space_id;
    END IF;

    IF vector_dims(NEW.embedding) <> expected_dimensions THEN
        RAISE EXCEPTION
            'embedding dimension mismatch: expected %, received %',
            expected_dimensions,
            vector_dims(NEW.embedding);
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER vector_records_validate
BEFORE INSERT OR UPDATE OF vector_space_id, embedding
ON vector_data.vector_records
FOR EACH ROW
EXECUTE FUNCTION vector_data.validate_vector_record();

-------------------------------------------------------------------------------
-- RLS
--
-- The vector service is expected to set:
--
--   SET LOCAL vector.application_id = '<uuid>';
--
-- at the beginning of every application-scoped transaction.
--
-- Missing application context means no vector records are visible or writable.
--
-- RLS is defense-in-depth against an accidentally unscoped SQL statement.
-- Authentication/authorization in vector-service remains authoritative.
-------------------------------------------------------------------------------

ALTER TABLE vector_data.vector_records
    ENABLE ROW LEVEL SECURITY;

ALTER TABLE vector_data.vector_records
    FORCE ROW LEVEL SECURITY;

CREATE POLICY vector_records_application_isolation
ON vector_data.vector_records
FOR ALL
USING (
    application_id =
        NULLIF(
            current_setting('vector.application_id', true),
            ''
        )::uuid
)
WITH CHECK (
    application_id =
        NULLIF(
            current_setting('vector.application_id', true),
            ''
        )::uuid
);

-------------------------------------------------------------------------------
-- Namespace RLS
--
-- This prevents accidental namespace discovery across applications after
-- application context has been established.
-------------------------------------------------------------------------------

ALTER TABLE vector_control.namespaces
    ENABLE ROW LEVEL SECURITY;

ALTER TABLE vector_control.namespaces
    FORCE ROW LEVEL SECURITY;

CREATE POLICY namespaces_application_isolation
ON vector_control.namespaces
FOR ALL
USING (
    application_id =
        NULLIF(
            current_setting('vector.application_id', true),
            ''
        )::uuid
)
WITH CHECK (
    application_id =
        NULLIF(
            current_setting('vector.application_id', true),
            ''
        )::uuid
);

-------------------------------------------------------------------------------
-- updated_at maintenance
-------------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION vector_control.set_updated_at()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$;

CREATE TRIGGER applications_set_updated_at
BEFORE UPDATE
ON vector_control.applications
FOR EACH ROW
EXECUTE FUNCTION vector_control.set_updated_at();

CREATE TRIGGER namespaces_set_updated_at
BEFORE UPDATE
ON vector_control.namespaces
FOR EACH ROW
EXECUTE FUNCTION vector_control.set_updated_at();

CREATE TRIGGER vector_records_set_updated_at
BEFORE UPDATE
ON vector_data.vector_records
FOR EACH ROW
EXECUTE FUNCTION vector_control.set_updated_at();

COMMIT;
