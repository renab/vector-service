# Release tag trust boundary (Principal Reviewer F-1)

## Objective

Document the actual scope of the existing GHCR SHA-tag safeguard without changing publishing behavior.

## Authority

`.github/workflows/ci.yml` (serialized master publishing; pre-push SHA-tag lookup and config-digest/revision checks; post-push read-back) and `docs/PUBLISHING.md` (publishing flow and digest-pinned production deployments).

## Required behavior and invariants

- Serialization prevents concurrent publishers **within this workflow**, not external package writers. Before pushing the `sha-<commit>` tag, the workflow verifies any existing SHA tag's config digest against the verified local image and its revision label against the expected commit; a mismatch fails. An ambiguous initial lookup may first require a push of a unique staging tag to establish registry visibility; this staging push is not preceded by the existing-SHA-tag check. Following a SHA-tag push, the workflow reads back and verifies config digest and revision again. These checks prevent non-concurrent accidental workflow overwrite and establish what was observed at verification time, not future tag identity.
- GHCR supplies no atomic create-only protection for this publish flow. Another authorized package writer can change a SHA tag between the check and push, between push and read-back, or after read-back. Neither `sha-<commit>` nor `stable` is immutable; never claim registry-enforced immutability or protection against external races.
- Production trust is the **verified build's manifest content digest**, not its mutable SHA tag or stable tag. Production deployments must pin `ghcr.io/<owner>/vector-service@sha256:<manifest-digest>`. The image config digest (`docker inspect .Id` / manifest `.config.digest`) used for workflow verification is not the deployable manifest digest. Capture the manifest digest while associating it with the verified build; do not resolve a mutable tag later and assume it still represents that build. If provenance is uncertain, reverify before deployment.

## Scope

Align the guarantee and deployment wording in `docs/PUBLISHING.md` and, if necessary, top-level comments in `.github/workflows/ci.yml` with these limits. Preserve all publishing mechanics, action/Dockerfile pins, and existing uncommitted changes.

## Exact wording for existing release documentation

Use this concise statement in `docs/PUBLISHING.md` under **Guarantees** or **Known Limitations** (and align the workflow's introductory SHA-tag comment if needed):

> The SHA-tag guarantee is limited to serialized publishers in this workflow: any existing SHA tag is checked against the verified local image's config digest and expected revision before pushing that SHA tag, and a newly pushed SHA tag is checked again by read-back. An ambiguous initial lookup may require pushing a unique staging tag first to establish registry visibility; the existing-SHA-tag check does not precede that staging push. These checks prevent non-concurrent accidental workflow overwrite and detect mismatches at verification time. GHCR does not provide atomic create-only tag publication; other authorized package writers are not serialized and can race with these checks or mutate either tag afterward. Neither `sha-<commit>` nor `stable` is immutable.

Use this statement in `docs/PUBLISHING.md` under **Production deployments**, replacing guidance that suggests a digest copied from workflow logs or an unverified later tag inspection is sufficient:

> Production trusts the verified build's **manifest content digest**, not any later resolution of a mutable tag. Record a manifest digest associated with the image whose config digest and revision were verified against that build; deploy `ghcr.io/<owner>/vector-service@sha256:<manifest-digest>` only. The config digest printed by the workflow is not the deployable manifest digest. If the manifest digest's association with the verified build cannot be established, reverify it before deployment; never treat `sha-<commit>` or `stable` as a production pin.

## Validation

Orchestrator owns validation: review documentation and workflow comments for contradictory promises; verify there are no behavior changes and no changes to existing pins.

## Out of scope

Changing registry API calls, publish logic, workflow triggers, Dockerfile, or image verification mechanics.
