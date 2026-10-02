# Image Publishing

This document describes the GHCR image publishing workflow, its semantics, guarantees, and known limitations.

## Tags

| Tag | Purpose | Mutability |
|-----|---------|------------|
| `sha-<commit>` | Commit-addressed tag bound to a specific source commit | Mutable by any writer; protected against accidental workflow overwrite |
| `stable` | Mutable tag promoted to the latest successfully validated master commit | Mutable by any writer |
| `staging-<run_id>-<attempt>` | Unique tag used to initialize or verify GHCR package access after an ambiguous 404 | Persists after creation; harmless artifact |

## Publishing Flow

The publish job executes the following fail-closed best-effort flow:

### 1. Capture local image identity

The verified image from the `image` job is loaded and its identity captured.
`docker inspect --format '{{.Id}}'` returns the config blob digest
(`sha256:<hash>`), which is the authoritative artifact identity. This digest
includes the rootfs diff IDs — it is a stronger identity check than label
matching alone.

### 2. Log in to GHCR

Authenticate Docker CLI with `GITHUB_TOKEN` (packages:write scope) for
`docker push` operations.

### 3. Obtain OCI Registry bearer token

Exchange GitHub credentials for a scoped GHCR bearer token via the OCI
Registry V2 Token API:

```
GET https://ghcr.io/token?service=ghcr.io&scope=repository:<owner>/vector-service:pull,push
Authorization: Basic <base64(owner:GITHUB_TOKEN)>
```

The returned bearer token is used for all OCI Registry HTTP API calls
(manifest GET, blob GET). GITHUB_TOKEN is never sent directly as a Bearer
credential to the OCI Registry API.

### 4. Resolve existing SHA tag

Resolve the existing `sha-<commit>` tag via the authenticated OCI Registry
HTTP API:

```
GET /v2/<owner>/vector-service/manifests/sha-<commit>
Authorization: Bearer <ghcr-token>
Accept: application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json
```

The response is classified as follows:

| HTTP Status | Error Code | Meaning | Action |
|-------------|------------|---------|--------|
| 200 | — | Tag exists | Verify artifact identity (step 4a) |
| 404 | MANIFEST_UNKNOWN | Tag does not exist; repository is visible | Eligible to proceed to push (step 5) |
| 404 | NAME_UNKNOWN | Repository does not exist | Bootstrap with staging tag (step 4b), then resolve again |
| 404 | Empty body | GHCR may omit the error body for either an absent tag or package | Bootstrap with staging tag (step 4b), verify access, then resolve again; eligible to push only if the authenticated re-resolution is still empty |
| 401 | — | Authentication failed | **Fail** |
| 403 | — | Access denied | **Fail** |
| 429 | — | Rate limited | **Fail** |
| 5xx | — | Server error | **Fail** |
| Any other | — | Unexpected | **Fail** |

#### 4a. Verify existing artifact identity

When the tag exists (HTTP 200), the manifest is parsed and the config
descriptor is extracted. The manifest type is validated:

- **Accepted**: `application/vnd.docker.distribution.manifest.v2+json`
  (Docker schema2), `application/vnd.oci.image.manifest.v1+json` (OCI image)
- **Rejected**: `application/vnd.docker.distribution.manifest.list.v2+json`
  (Docker manifest list), `application/vnd.oci.image.index.v1+json`
  (OCI image index) — these cannot be verified as single-artifact identity

The remote manifest `.config.digest` is compared against the local verified
image config digest (`docker inspect --format '{{.Id}}'`). A mismatch
indicates the tag was overwritten with different content — the workflow fails.

The config blob is fetched via the OCI Registry API:

```
GET /v2/<owner>/vector-service/blobs/<config.digest>
Authorization: Bearer <ghcr-token>
```

The `org.opencontainers.image.revision` label is extracted from the config
blob and compared against the expected source SHA. Missing labels or parse
failures cause the workflow to fail.

Both checks must pass:
1. Config digest matches local verified artifact (rootfs identity)
2. Revision label matches expected commit SHA

If both match, the push is skipped.

#### 4b. Bootstrap with staging tag

When the SHA lookup returns `NAME_UNKNOWN` or an empty 404 body, package
visibility is ambiguous. The workflow pushes a unique staging tag:

```
staging-<run_id>-<attempt>
```

This tag is derived from the GitHub Actions run ID and attempt number,
making it unique per workflow execution. After the staging tag is pushed,
the workflow reads it back using the scoped registry token and checks its
config digest against the locally verified image. The push confirms write
permission and the authenticated read-back confirms package visibility.
Only then does the workflow resolve the SHA tag again. If it exists, its
identity is verified as described in step 4a; if the SHA lookup returns
`MANIFEST_UNKNOWN`, or an empty 404 after this unique staging tag was pushed
and its authenticated read-back proved package visibility, the tag is
considered absent and may be pushed. An initial empty 404 alone is never
eligible for push. Any contradictory package error fails closed.

The staging tag persists after creation. It is a harmless artifact of package
initialization or access verification. It may be manually deleted via the
GitHub Packages UI if desired.

### 5. Push SHA tag

If the tag does not exist (`MANIFEST_UNKNOWN`, or an empty 404 only after the
staging push and authenticated read-back described in step 4b), push the
`sha-<commit>` tag. After push, read back the tag via the OCI Registry API and verify:

1. The manifest config digest matches the local verified image config digest
2. The config blob revision label matches the expected commit SHA

Both checks must pass. If either fails, the workflow fails.

### 6. Promote stable tag

Push the `stable` tag from the verified local artifact. After push, read
back the stable tag via the OCI Registry API and verify:

1. The manifest config digest matches the local verified image config digest
   (same as the SHA tag's config digest)
2. The config blob revision label matches the expected commit SHA
3. `Docker-Content-Digest` is a valid SHA-256 digest and matches the SHA-256
   hash of the raw manifest response body

All checks must pass. If any fails, the workflow fails. The workflow records
the immutable manifest-digest deployment reference in `GITHUB_STEP_SUMMARY`
from this same verified post-push response.

## Guarantees

The following guarantees are provided:

1. **Exact verified artifact**: The published image is the exact artifact
   verified by `scripts/verify-image.sh`. No second rebuild occurs.

2. **Config digest identity verification**: The remote manifest config
   digest is compared against the local image's config digest
   (`docker inspect --format '{{.Id}}'`). This digest includes the rootfs
   diff IDs and is a stronger identity check than label matching alone.

3. **Revision label verification**: The config blob is fetched and the
   `org.opencontainers.image.revision` label is verified against the
   expected commit SHA. Missing labels or parse failures cause failure.

4. **Post-push read-back verification**: After pushing the SHA tag and
   promoting the stable tag, each is read back via the OCI Registry API
   and verified to have the correct config digest and revision label. The
   stable-tag response's manifest digest is syntax-checked and matched against
   the raw response body; its immutable deployment reference is recorded in
   the workflow step summary.

5. **Fail-closed on errors**: Any authentication, transport, or parse
   failure stops the workflow. An empty 404 is treated as "tag absent" only
   after a unique staging tag proves package write access and authenticated
   read-back proves package visibility.

6. **Serialized master publishing**: Concurrency configuration serializes
   master publishes, preventing concurrent workflow publishers from racing.

7. **Protected against accidental workflow overwrite**: The workflow resolves
   and verifies an existing SHA tag before pushing that SHA tag. An ambiguous
   lookup may first require pushing a unique staging tag to establish package
   visibility; that staging push is not preceded by the SHA-tag check.

8. **Limited SHA-tag guarantee**: These checks prevent non-concurrent
   accidental workflow overwrite and detect mismatches at verification time.
   GHCR does not provide atomic create-only tag publication; other authorized
   package writers are not serialized and can race with checks or mutate tags
   afterward. Neither `sha-<commit>` nor `stable` is immutable.

## Known Limitations

### No registry-side immutability

GHCR does not support immutable tags or atomic create-only manifest PUT.
Any authenticated writer with `packages:write` scope can mutate any tag at
any time, regardless of workflow safeguards.

Specifically:

- Repository owners can manually push a different image to `sha-<commit>`
  or `stable`.
- Other workflows or CI systems with `packages:write` scope can mutate tags.
- The workflow's check-then-push is not atomic; a race exists with outside
  writers.

### External tag mutation cannot be prevented

The workflow cannot prevent external GHCR tag mutation. The protection
provided is against accidental workflow overwrite (e.g., two workflow runs
racing to publish the same tag). Protection against external writers
requires deployment-side pinning.

### `docker manifest inspect` not used for verification

Post-push verification uses the OCI Registry HTTP API directly (manifest
GET + blob GET) rather than `docker manifest inspect`. This provides:

- Direct access to the config descriptor and config blob
- Precise HTTP status and error code handling
- Consistency with the pre-push existence check

### Best-effort workflow checks

The workflow checks are best-effort safeguards for a trusted publisher.
They do not provide cryptographic guarantees — they verify that the
published artifact matches the locally verified image at the time of
publishing. The checks can be bypassed by an external writer with
`packages:write` scope.

## Concurrency

Master publishes are serialized via the concurrency group:

```yaml
concurrency:
  group: ${{ github.workflow }}-${{ github.ref == 'refs/heads/master' && 'master' || github.ref }}
  cancel-in-progress: ${{ github.ref != 'refs/heads/master' }}
```

This prevents concurrent workflow publishers from racing on master.
However, a newer master push may supersede (cancel) a pending run.

### Concurrency semantics

- The concurrency group ensures only one master publish runs at a time.
- A newer push to master may supersede a pending (queued) run.
- The last eligible run that actually executes (is not superseded) publishes
  its SHA tag and promotes stable.
- Runs that are superseded never execute the publish step — they do not
  create a SHA tag.
- Every run that does execute the publish step creates its SHA tag.
- The `stable` tag points to the latest eligible successful master
  publication (the last run that actually executed publish).

This means: not every successful master push results in a published image.
If a push is superseded by a newer one before its publish step runs, it
never publishes. This is intentional — it avoids publishing intermediate
commits that have already been replaced.

### Ordinary CI

PR and branch runs retain cancellation (`cancel-in-progress: true`) for
faster feedback. They do not publish images.

## Deployment Recommendations

### Production deployments

Pin the verified build's manifest content digest, not a mutable tag:

```yaml
image: ghcr.io/renab/vector-service@sha256:<manifest-digest>
```

The workflow records the immutable deployment reference in the GitHub Actions
step summary. It captures `Docker-Content-Digest` from the authenticated
post-push stable-tag manifest response whose config digest and revision label
were verified, validates its SHA-256 syntax, and verifies it matches the hash
of the raw manifest response body. Use this recorded reference for production;
do not resolve a mutable SHA or `stable` tag later and assume it still
represents the verified build.

The config digest printed by `docker inspect --format '{{.Id}}'` is the image
config blob digest used for identity verification; it is not the deployable
manifest digest. If the recorded manifest digest's association with the
verified build cannot be established, reverify before deployment.

### Development/canary environments

The `stable` tag may be used for canary or development environments where
tracking the latest validated master commit is desirable.

## Required GitHub Settings

No special GitHub settings are required beyond the default `GITHUB_TOKEN`
with `packages:write` scope (granted by the workflow's `permissions`
declaration).

## Manual Publishing

To manually trigger publishing for the current master HEAD:

```bash
gh workflow run CI --ref master
```

Or use the GitHub Actions UI: Actions → CI → Run workflow → master branch.

## Troubleshooting

### "Tag already exists with different content" / config digest mismatch

The SHA tag exists but its config digest does not match the local verified
image. This can occur if:

1. An external writer pushed a different image to the same tag.
2. A previous workflow run was interrupted after push but before verification.
3. The commit SHA was reused (e.g., via `git replace` or force push).

Resolution: investigate the existing tag's content and determine the
appropriate action.

### "Could not retrieve config digest of pushed SHA tag"

The post-push verification failed to read back the pushed tag. This may
indicate a registry propagation delay or a network issue. Check the
workflow logs for details.

### "Registry authentication failed (HTTP 401)"

The bearer token exchange failed. This may indicate:

1. The `GITHUB_TOKEN` does not have `packages:write` scope.
2. The repository owner name is incorrect.
3. The token API endpoint is unreachable.

Verify the workflow's `permissions` declaration includes `packages: write`.

### "Missing org.opencontainers.image.revision label"

The config blob does not contain the expected revision label. This can
occur if:

1. The image was built without the `--label org.opencontainers.image.revision`
   flag.
2. The config blob was corrupted or replaced.

Resolution: verify the Dockerfile build step includes the revision label.

### "Unsupported manifest type"

The remote manifest is a manifest list or image index rather than a single-
platform image manifest. This can occur if:

1. A multi-arch image was pushed to the tag.
2. An external writer pushed a manifest list.

Resolution: investigate the existing tag's content.
