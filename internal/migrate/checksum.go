package migrate

import (
	"crypto/sha256"
	"encoding/hex"
)

// checksum returns the SHA-256 of the migration's original embedded bytes,
// hex-encoded. The checksum is always computed over the original file bytes;
// stripping the outer transaction framing for execution never changes it.
func checksum(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
