import unittest

from ghcr_error_code import registry_error_code


class RegistryErrorCodeTests(unittest.TestCase):
    def test_recognizes_manifest_unknown(self):
        self.assertEqual(
            registry_error_code(b'{"errors":[{"code":"MANIFEST_UNKNOWN"}]}'),
            "MANIFEST_UNKNOWN",
        )

    def test_recognizes_name_unknown(self):
        self.assertEqual(
            registry_error_code(b'{"errors":[{"code":"NAME_UNKNOWN"}]}'),
            "NAME_UNKNOWN",
        )

    def test_classifies_empty_body(self):
        self.assertEqual(registry_error_code(b" \r\n"), "EMPTY")

    def test_rejects_malformed_or_unexpected_body(self):
        self.assertEqual(registry_error_code(b"not-json"), "INVALID")
        self.assertEqual(registry_error_code(b'{"errors":[]}'), "INVALID")
        self.assertEqual(registry_error_code(b'{"errors":[{}]}'), "INVALID")
        self.assertEqual(
            registry_error_code(b'{"errors":[{"code":"MANIFEST_UNKNOWN"}, {}]}'),
            "INVALID",
        )
        self.assertEqual(
            registry_error_code(b'{"errors":[{"code":"MANIFEST_UNKNOWN"}, null]}'),
            "INVALID",
        )
        self.assertEqual(
            registry_error_code(b'{"errors":[{"code": ["MANIFEST_UNKNOWN"]}]}'),
            "INVALID",
        )
        self.assertEqual(
            registry_error_code(
                b'{"errors":[{"code":"MANIFEST_UNKNOWN"}],'
                b'"errors":[{"code":"NAME_UNKNOWN"}]}'
            ),
            "INVALID",
        )

    def test_rejects_nonstandard_json_constants(self):
        for constant in (b"NaN", b"Infinity", b"-Infinity"):
            with self.subTest(constant=constant):
                response = (
                    b'{"errors":[{"code":"MANIFEST_UNKNOWN", "detail":'
                    + constant
                    + b"}]}"
                )
                self.assertEqual(registry_error_code(response), "INVALID")


if __name__ == "__main__":
    unittest.main()
