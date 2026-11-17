package payments

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// VerifyWebhookSignature verifies the HMAC-SHA256 signature of the raw webhook payload
// using constant-time comparison. It returns true if and only if the signature is valid.
func VerifyWebhookSignature(payload []byte, signature string, secret string) bool {
	if len(secret) == 0 {
		return false
	}
	if len(signature) == 0 {
		return false
	}

	provided, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	if len(provided) != sha256.Size {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write(payload); err != nil {
		return false
	}
	expected := mac.Sum(nil)

	return hmac.Equal(expected, provided)
}
