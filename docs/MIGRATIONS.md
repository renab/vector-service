# Database Migrations

## Ownership

Vector Service owns its application schema migrations.

Migrations live in this repository:

```text
migrations/
```

They do not belong in the deployment/GitOps repository.

Deployment infrastructure provisions:

- PostgreSQL
- the `vector` database
- database ownership
- runtime database roles
- pgvector availability

Vector Service owns everything above that infrastructure boundary.

---

# Principles

Migrations must be:

- ordered
- deterministic
- forward-only
- immutable after release
- safe under controlled concurrent startup
- recorded in migration history
- applied before code requiring the new schema becomes ready

---

# Naming

Use monotonically ordered migration filenames.

Example:

```text
0001_shared_vector_schema.sql
0002_initial_vector_space.sql
0003_initial_hnsw_index.sql
0004_runtime_identity.sql
0005_add_example_feature.sql
```

The descriptive suffix should explain the purpose of the migration.

---

# Released Migrations Are Immutable

Once a migration has been:

- merged to the main branch
- included in a released service image
- or applied to a persistent environment

do not edit it to change behavior.

Create another migration.

Incorrect:

```text
edit 0003 after production has applied it
```

Correct:

```text
0005_correct_hnsw_definition.sql
```

---

# Migration History

The migration runner must maintain a schema history table.

Recommended conceptual structure:

```text
vector_control.schema_migrations
```

Fields should include at minimum:

```text
version
name
checksum
applied_at
```

A migration's checksum should be validated against previously applied migrations.

If an applied migration has changed unexpectedly, startup/migration should fail rather than silently accepting drift.

---

# Concurrency

Multiple Vector Service replicas may start simultaneously.

Exactly one instance must perform migrations at a time.

Use PostgreSQL advisory locking or an equivalent database-native mechanism.

Conceptual sequence:

```text
connect
  ↓
acquire migration advisory lock
  ↓
read migration history
  ↓
validate checksums
  ↓
apply pending migrations
  ↓
release lock
```

Other replicas should block or wait within a bounded startup period.

---

# Transactions

Prefer running each migration in a transaction where PostgreSQL supports it.

Example:

```sql
BEGIN;

-- migration operations

COMMIT;
```

Some operations may require special handling outside transactions.

If so, the migration must document why.

---

# Startup Behavior

The service must not report ready while required migrations are pending or failed.

Recommended startup order:

```text
load configuration
  ↓
connect to PostgreSQL
  ↓
validate database prerequisites
  ↓
run/validate migrations
  ↓
start serving
  ↓
readyz succeeds
```

Migration failure should prevent readiness.

---

# Embedded Migrations

Migrations should be embedded into the compiled Go binary.

This ensures the running artifact contains exactly the migrations expected by that code version.

Go's standard:

```text
embed
```

package is suitable.

Do not depend on a mutable external filesystem path for production migrations unless explicitly designed.

---

# Schema Preconditions

Before application migrations run, infrastructure is expected to have provided:

- PostgreSQL database
- pgvector extension binaries
- runtime connection identity
- required TLS connectivity

Application migrations may create:

- schemas
- tables
- functions
- indexes
- triggers
- policies
- grants required by the service

---

# pgvector Extension

Infrastructure makes the pgvector extension available to PostgreSQL.

Application-owned database configuration may ensure:

```sql
CREATE EXTENSION vector;
```

has been applied through the established infrastructure/database contract.

Do not attempt to install operating-system extension packages from migrations.

---

# Vector-Space Migrations

Vector spaces should be introduced through explicit migrations when they are part of the deployed platform contract.

Example:

```text
0002_initial_vector_space.sql
```

Existing vector-space definitions are immutable.

A model migration should add a new vector space.

Do not update the meaning of an existing row.

---

# ANN Index Migrations

ANN indexes should be explicit migrations.

Example:

```text
0003_initial_hnsw_index.sql
```

Index definitions must specify the compatible vector space.

For mixed-dimension storage, use vector-space-specific expression/partial indexes.

Do not create one global ANN index that assumes all rows are compatible.

---

# Large Index Changes

Future large indexes may take significant time.

When corpus size eventually warrants it, migration strategy may need to use:

```text
CREATE INDEX CONCURRENTLY
```

or an explicit operational migration process.

Do not prematurely add that complexity to small initial deployments.

If non-transactional/concurrent index creation is introduced, document the recovery behavior for partial failure.

---

# Destructive Changes

Prefer additive migrations.

Destructive operations require extra care.

Examples:

- dropping columns
- dropping vector spaces
- dropping indexes still used by active service versions
- rewriting large record sets
- deleting application data

A destructive migration must account for rolling deployments where old and new service versions may briefly coexist.

---

# Compatibility Window

Schema changes should support normal rolling deployment where practical.

Preferred sequence:

```text
1. add compatible schema
2. deploy code capable of using it
3. migrate data if required
4. remove obsolete behavior in later release
5. remove obsolete schema only after compatibility window closes
```

Avoid migrations that require every running process to switch behavior atomically.

---

# Rollback Philosophy

Database migrations are forward-only.

Application rollback should generally mean:

```text
deploy older application version
```

only when the current schema remains backward compatible.

Do not depend on automatic down-migrations.

If a migration causes a problem, create a forward corrective migration.

---

# Derived Vector Data

Remember that vector records are derived state.

If an embedding representation changes incompatibly:

- register a new vector space
- rebuild projections into the new vector space
- migrate consumers
- retire the old vector space
- delete old derived records later if desired

Do not rewrite existing embeddings in place and pretend they remain the same vector space.

---

# Testing Migrations

CI should test at minimum:

1. empty database → latest schema
2. previous released schema → latest schema
3. migration history checksum validation
4. repeated startup with no pending migrations
5. concurrent migration attempts
6. failure behavior for invalid migrations

Migration tests should use a real supported PostgreSQL + pgvector environment where practical.

---

# Manual Database Changes

Avoid manual schema changes in persistent environments.

If an emergency manual fix is required:

1. record exactly what changed
2. create a matching forward migration immediately
3. ensure migration history and actual schema converge
4. document the incident

Git/source-controlled migrations remain the intended schema history.

---

# Database Drift

The service should detect migration-file drift through recorded checksums.

It is not required to perform a complete declarative diff of the entire PostgreSQL schema in v1.

Operational schema inspection may be added separately.

---

# Migration Review Checklist

Before merging a migration, verify:

- migration number is new
- prior migration files are unchanged
- ownership is correct
- RLS remains effective
- runtime privileges remain minimal
- application and namespace relationships remain enforced
- vector-space compatibility is preserved
- destructive changes have a compatibility plan
- migration can recover safely from failure
- tests cover upgrade from the previous released schema
