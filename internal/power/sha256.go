package power

import (
	"crypto/sha256"
	"encoding/hex"
)

// sha256Hex is the SHA-256 variant of the digest hash function, used when the
// Shelly plug's challenge advertises algorithm=SHA-256.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
