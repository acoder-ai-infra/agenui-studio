package tenantadmin

import (
	"crypto/rand"
	"fmt"
)

// secretKeyAlphabet excludes visually ambiguous characters (0/O, 1/l/I) so a
// hand-typed 6-char login key is less error-prone.
const secretKeyAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789"

// SecretKeyLength is the fixed length of a tenant login key.
const SecretKeyLength = 6

// GenerateSecretKey returns a cryptographically-random SecretKeyLength-char key
// drawn from secretKeyAlphabet. Uniqueness across tenants is enforced by the
// store's UNIQUE(secret_key) constraint (with create-time retry on collision).
func GenerateSecretKey() (string, error) {
	buf := make([]byte, SecretKeyLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("tenantadmin: generate secret key: %w", err)
	}
	out := make([]byte, SecretKeyLength)
	for i, b := range buf {
		out[i] = secretKeyAlphabet[int(b)%len(secretKeyAlphabet)]
	}
	return string(out), nil
}
