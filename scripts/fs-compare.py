#!/usr/bin/env python3
"""Filesystem comparison: verify image matches distroless base + vector-service binary.

Usage: python3 fs-compare.py <base.tar> <image.tar>

Compares the two tar archives entry by entry, asserting:
- No base entries missing from image
- Extra entries are exactly the allowed set (usr/local/, usr/local/bin/, binary)
- Base entries match in type, mode, uid, gid, symlink target, and content SHA256
- Added paths have expected type and metadata
- Binary exists, is a regular file, has non-zero size, and has valid content
"""

import sys
import tarfile
import hashlib


def compute_sha256(tarinfo, tar):
    """Compute SHA256 of regular file content."""
    if not tarinfo.isfile():
        return None
    f = tar.extractfile(tarinfo)
    if f is None:
        return None
    h = hashlib.sha256()
    while True:
        chunk = f.read(65536)
        if not chunk:
            break
        h.update(chunk)
    return h.hexdigest()


def main():
    if len(sys.argv) != 3:
        print(f"usage: {sys.argv[0]} <base.tar> <image.tar>")
        sys.exit(1)

    base_entries = {}
    image_entries = {}

    with tarfile.open(sys.argv[1], "r") as base_tar:
        for ti in base_tar:
            h = compute_sha256(ti, base_tar)
            name = ti.name.rstrip("/")
            base_entries[name] = {
                "type": ti.type,
                "mode": ti.mode,
                "uid": ti.uid,
                "gid": ti.gid,
                "linkname": ti.linkname if ti.islnk() or ti.issym() else None,
                "isfile": ti.isfile(),
                "isdir": ti.isdir(),
                "issym": ti.issym(),
                "islnk": ti.islnk(),
                "size": ti.size,
                "sha256": h,
            }

    with tarfile.open(sys.argv[2], "r") as image_tar:
        for ti in image_tar:
            h = compute_sha256(ti, image_tar)
            name = ti.name.rstrip("/")
            image_entries[name] = {
                "type": ti.type,
                "mode": ti.mode,
                "uid": ti.uid,
                "gid": ti.gid,
                "linkname": ti.linkname if ti.islnk() or ti.issym() else None,
                "isfile": ti.isfile(),
                "isdir": ti.isdir(),
                "issym": ti.issym(),
                "islnk": ti.islnk(),
                "size": ti.size,
                "sha256": h,
            }

    # Allowed extra entries: the binary and its parent directories.
    allowed_extra = {
        "usr/local",
        "usr/local/bin",
        "usr/local/bin/vector-service",
    }

    # Check: no base entries missing from image.
    missing = set(base_entries.keys()) - set(image_entries.keys())
    if missing:
        print(f"FAIL: {len(missing)} base entries missing from image")
        for m in sorted(missing)[:10]:
            print(f"  missing: {m}")
        sys.exit(1)

    # Check: extra entries are exactly the allowed set.
    extra = set(image_entries.keys()) - set(base_entries.keys())
    if extra != allowed_extra:
        unexpected = extra - allowed_extra
        missing_allowed = allowed_extra - extra
        print("FAIL: extra entries do not match expected set")
        if unexpected:
            print(f"  unexpected ({len(unexpected)}):")
            for u in sorted(unexpected)[:10]:
                print(f"    {u}")
        if missing_allowed:
            print(f"  missing expected ({len(missing_allowed)}):")
            for m in sorted(missing_allowed):
                print(f"    {m}")
        sys.exit(1)

    # Check: base entries have matching metadata (type, mode, uid, gid, symlink target, content SHA256).
    mismatches = []
    for name, base_info in sorted(base_entries.items()):
        img_info = image_entries[name]
        diffs = []
        if base_info["type"] != img_info["type"]:
            diffs.append(f"type {base_info['type']}!={img_info['type']}")
        if base_info["mode"] != img_info["mode"]:
            diffs.append(f"mode {base_info['mode']}!={img_info['mode']}")
        if base_info["uid"] != img_info["uid"]:
            diffs.append(f"uid {base_info['uid']}!={img_info['uid']}")
        if base_info["gid"] != img_info["gid"]:
            diffs.append(f"gid {base_info['gid']}!={img_info['gid']}")
        if base_info["linkname"] != img_info["linkname"]:
            diffs.append(f"linkname {base_info['linkname']}!={img_info['linkname']}")
        if base_info["sha256"] != img_info["sha256"]:
            if base_info["isfile"]:
                diffs.append(
                    f"sha256 {base_info['sha256'][:16]}...!={img_info['sha256'][:16]}..."
                )
            elif base_info["sha256"] is not None or img_info["sha256"] is not None:
                diffs.append("sha256 mismatch for non-file")
        if diffs:
            mismatches.append((name, diffs))

    if mismatches:
        print(f"FAIL: {len(mismatches)} base entries have metadata mismatches")
        for name, diffs in mismatches[:10]:
            print(f"  {name}: {', '.join(diffs)}")
        sys.exit(1)

    # Check: added paths have expected type and metadata.
    # usr/local/ and usr/local/bin/ must be directories with expected
    # ownership (0:0, as Docker COPY parents are root-owned) and safe
    # expected modes (0755). Reject overly permissive modes and special
    # permission bits (suid/sgid/sticky).
    dir_checks = {
        "usr/local": {
            "expect": "dir",
            "reason": "usr/local parent directory",
            "expected_uid": 0,
            "expected_gid": 0,
            "expected_mode": 0o755,
        },
        "usr/local/bin": {
            "expect": "dir",
            "reason": "usr/local/bin parent directory",
            "expected_uid": 0,
            "expected_gid": 0,
            "expected_mode": 0o755,
        },
    }
    for path_name, check in dir_checks.items():
        info = image_entries.get(path_name)
        if info is None:
            print(f"FAIL: {check['reason']} ({path_name}) not found in image")
            sys.exit(1)
        if check["expect"] == "dir" and not info["isdir"]:
            print(
                f"FAIL: {check['reason']} ({path_name}) is not a directory "
                f"(isfile={info['isfile']}, issym={info['issym']})"
            )
            sys.exit(1)
        # Validate mode: reject special permission bits (suid/sgid/sticky) first.
        raw_mode = info["mode"]
        if raw_mode & 0o7000:
            print(
                f"FAIL: {path_name} has special permission bits set "
                f"(mode {oct(raw_mode)}): suid={bool(raw_mode & 0o4000)}, "
                f"sgid={bool(raw_mode & 0o2000)}, sticky={bool(raw_mode & 0o1000)}"
            )
            sys.exit(1)
        # Mode must be exactly the expected value (0755).
        if raw_mode != check["expected_mode"]:
            print(
                f"FAIL: {path_name} has mode {oct(raw_mode)}, "
                f"expected exactly {oct(check['expected_mode'])}"
            )
            sys.exit(1)
        # Check ownership: uid/gid must be 0:0 (root, as Docker COPY parents).
        if info["uid"] != check["expected_uid"]:
            print(
                f"FAIL: {path_name} has uid {info['uid']}, "
                f"expected {check['expected_uid']} (Docker COPY parent)"
            )
            sys.exit(1)
        if info["gid"] != check["expected_gid"]:
            print(
                f"FAIL: {path_name} has gid {info['gid']}, "
                f"expected {check['expected_gid']} (Docker COPY parent)"
            )
            sys.exit(1)

    # Check: binary exists, is a regular file, has non-zero size, and has content.
    binary_info = image_entries.get("usr/local/bin/vector-service")
    if binary_info is None:
        print("FAIL: binary usr/local/bin/vector-service not found in image")
        sys.exit(1)
    if not binary_info["isfile"]:
        print(
            f"FAIL: binary usr/local/bin/vector-service is not a regular file "
            f"(isdir={binary_info['isdir']}, issym={binary_info['issym']})"
        )
        sys.exit(1)
    if binary_info["size"] == 0:
        print("FAIL: binary usr/local/bin/vector-service has zero size")
        sys.exit(1)
    if binary_info["sha256"] is None:
        print("FAIL: binary usr/local/bin/vector-service has no content hash (empty or unreadable)")
        sys.exit(1)
    # Validate binary metadata: mode must be exactly 0755 (from Docker COPY).
    # Reject special permission bits (suid/sgid/sticky) and any deviation.
    binary_raw_mode = binary_info["mode"]
    if binary_raw_mode & 0o7000:
        print(
            f"FAIL: binary usr/local/bin/vector-service has special permission bits set "
            f"(mode {oct(binary_raw_mode)}): suid={bool(binary_raw_mode & 0o4000)}, "
            f"sgid={bool(binary_raw_mode & 0o2000)}, sticky={bool(binary_raw_mode & 0o1000)}"
        )
        sys.exit(1)
    if binary_raw_mode != 0o755:
        print(
            f"FAIL: binary usr/local/bin/vector-service has mode {oct(binary_raw_mode)}, "
            f"expected exactly {oct(0o755)}"
        )
        sys.exit(1)
    # Check ownership: binary should be owned by root (0:0) from Docker COPY.
    if binary_info["uid"] != 0:
        print(
            f"FAIL: binary usr/local/bin/vector-service has uid {binary_info['uid']}, "
            f"expected 0 (Docker COPY source)"
        )
        sys.exit(1)
    if binary_info["gid"] != 0:
        print(
            f"FAIL: binary usr/local/bin/vector-service has gid {binary_info['gid']}, "
            f"expected 0 (Docker COPY source)"
        )
        sys.exit(1)

    # Success: print summary.
    print(
        f"OK: base={len(base_entries)} image={len(image_entries)} "
        f"extra={len(extra)} mismatches={len(mismatches)}"
    )
    print(f"OK: binary sha256={binary_info['sha256']}")


if __name__ == "__main__":
    main()
