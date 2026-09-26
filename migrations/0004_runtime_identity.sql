BEGIN;

-------------------------------------------------------------------------------
-- Application credentials
--
-- The API credential identifies the application.
--
-- Callers never provide or choose application_id themselves.
--
-- Credentials are assumed to be cryptographically random opaque values.
-- The raw credential is never stored. vector-service computes SHA-256 over
-- the credential and compares the resulting 32-byte digest.
-------------------------------------------------------------------------------

CREATE TABLE vector_control.application_credentials (
    id                  uuid PRIMARY KEY,

    application_id      uuid NOT NULL
                        REFERENCES vector_control.applications(id)
                        ON DELETE CASCADE,

    credential_name     text NOT NULL,

    credential_hash     bytea NOT NULL,

    enabled             boolean NOT NULL DEFAULT true,

    created_at          timestamptz NOT NULL DEFAULT now(),
    expires_at          timestamptz,
    last_used_at        timestamptz,

    CONSTRAINT application_credentials_hash_length
        CHECK (octet_length(credential_hash) = 32),

    CONSTRAINT application_credentials_name_not_empty
        CHECK (length(credential_name) > 0),

    CONSTRAINT application_credentials_expiry_valid
        CHECK (
            expires_at IS NULL
            OR expires_at > created_at
        ),

    CONSTRAINT application_credentials_name_unique
        UNIQUE (application_id, credential_name),

    CONSTRAINT application_credentials_hash_unique
        UNIQUE (credential_hash)
);

CREATE INDEX application_credentials_application_idx
    ON vector_control.application_credentials (application_id);

-------------------------------------------------------------------------------
-- vector_api privileges
-------------------------------------------------------------------------------

GRANT CONNECT
ON DATABASE vector
TO vector_api;

GRANT USAGE
ON SCHEMA vector_control
TO vector_api;

GRANT USAGE
ON SCHEMA vector_data
TO vector_api;

-------------------------------------------------------------------------------
-- Control plane reads/writes
-------------------------------------------------------------------------------

GRANT SELECT, INSERT, UPDATE
ON vector_control.applications
TO vector_api;

GRANT SELECT, INSERT, UPDATE
ON vector_control.namespaces
TO vector_api;

GRANT SELECT
ON vector_control.vector_spaces
TO vector_api;

GRANT SELECT, INSERT, UPDATE, DELETE
ON vector_control.application_credentials
TO vector_api;

-------------------------------------------------------------------------------
-- Vector record operations
-------------------------------------------------------------------------------

GRANT SELECT, INSERT, UPDATE, DELETE
ON vector_data.vector_records
TO vector_api;

COMMIT;
