#!/usr/bin/env python3
"""Extract the OCI Registry error code from a response body."""

import json
import sys
from pathlib import Path


def _unique_object(pairs):
    """Build a JSON object, rejecting duplicate keys as ambiguous input."""
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def _reject_constant(value):
    """Reject non-standard constants accepted by Python's JSON decoder."""
    raise ValueError(f"non-standard JSON constant: {value}")


def registry_error_code(response: bytes) -> str:
    """Return a registry error code, EMPTY for blank bodies, or INVALID."""
    if not response.strip():
        return "EMPTY"

    try:
        payload = json.loads(
            response,
            object_pairs_hook=_unique_object,
            parse_constant=_reject_constant,
        )
    except (json.JSONDecodeError, UnicodeDecodeError, ValueError):
        return "INVALID"

    if not isinstance(payload, dict):
        return "INVALID"

    errors = payload.get("errors")
    # A response with multiple entries cannot safely identify one registry
    # outcome (in particular, do not infer a missing tag from the first one).
    if not isinstance(errors, list) or len(errors) != 1 or not isinstance(errors[0], dict):
        return "INVALID"

    code = errors[0].get("code")
    if not isinstance(code, str) or not code.strip():
        return "INVALID"
    return code


def main() -> int:
    if len(sys.argv) != 2:
        print("Usage: ghcr_error_code.py RESPONSE_FILE", file=sys.stderr)
        return 2

    try:
        response = Path(sys.argv[1]).read_bytes()
    except OSError:
        print("Could not read registry response body", file=sys.stderr)
        return 2

    code = registry_error_code(response)
    print(code)
    return 1 if code == "INVALID" else 0


if __name__ == "__main__":
    sys.exit(main())
