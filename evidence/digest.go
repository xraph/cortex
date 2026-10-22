package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// Digest returns the lowercase SHA-256 encoding of exact content bytes.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ValidDigest accepts only canonical lowercase SHA-256 encodings.
func ValidDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// BindingDigest hashes the complete versioned reference, including scope,
// retention and producer. Content hashes alone do not identify authority.
func (r Ref) BindingDigest() (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return Digest(data), nil
}
