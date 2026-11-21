package payments

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestFixtureSignatureRejectsTampering(t *testing.T) {
	const secret = "demo-fixture-secret"
	payload := []byte("test-capture")
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	sig := hex.EncodeToString(mac.Sum(nil))
	if !VerifyWebhookSignature(payload, sig, secret) { t.Fatal("valid fixture signature rejected") }
	if VerifyWebhookSignature([]byte("changed"), sig, secret) { t.Fatal("tampered fixture accepted") }
	if VerifyWebhookSignature(payload, sig, "") { t.Fatal("empty secret accepted") }
}
