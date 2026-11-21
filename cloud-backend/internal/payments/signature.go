package payments

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// VerifyWebhookSignature verifies test-fixture HMAC signatures only. There is
// no network webhook endpoint or real payment provider in the demo application.
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
