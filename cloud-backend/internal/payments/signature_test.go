package payments

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func testHMACSHA256(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// Test D — TestVerifyWebhookSignatureKnownVector
func TestVerifyWebhookSignatureKnownVector(t *testing.T) {
	secret := "key"
	payload := []byte("The quick brown fox jumps over the lazy dog")
	expectedSig := "f7bc83f430538424b13298e6aa6fb143ef4d59a14946175997479dbc2d1a3cd8"

	if !VerifyWebhookSignature(payload, expectedSig, secret) {
		t.Fatalf("VerifyWebhookSignature failed for standard known HMAC-SHA256 test vector")
	}
}

// Test E — TestVerifyWebhookSignatureRejectsInvalidSignature
func TestVerifyWebhookSignatureRejectsInvalidSignature(t *testing.T) {
	secret := "secret-key-123"
	payload := []byte(`{"event":"payment.captured","amount":2500}`)
	validSig := testHMACSHA256(payload, secret)

	// Verify valid baseline passes
	if !VerifyWebhookSignature(payload, validSig, secret) {
		t.Fatalf("valid signature failed baseline check")
	}

	// 1. Single character changed
	corrupted := []byte(validSig)
	if corrupted[0] == 'a' {
		corrupted[0] = 'b'
	} else {
		corrupted[0] = 'a'
	}
	if VerifyWebhookSignature(payload, string(corrupted), secret) {
		t.Fatalf("expected false for single-character corrupted signature")
	}

	// 2. Signature is empty
	if VerifyWebhookSignature(payload, "", secret) {
		t.Fatalf("expected false for empty signature")
	}

	// 3. Signature too short
	if VerifyWebhookSignature(payload, validSig[:30], secret) {
		t.Fatalf("expected false for truncated signature")
	}

	// 4. Signature too long
	if VerifyWebhookSignature(payload, validSig+"ab", secret) {
		t.Fatalf("expected false for extended signature")
	}

	// 5. Signature contains non-hex characters
	nonHex := validSig[:63] + "z"
	if VerifyWebhookSignature(payload, nonHex, secret) {
		t.Fatalf("expected false for non-hex signature")
	}

	// 6. Secret is empty
	if VerifyWebhookSignature(payload, validSig, "") {
		t.Fatalf("expected false for empty secret")
	}

	// 7. Signature of a different payload
	diffPayload := []byte(`{"event":"payment.failed","amount":2500}`)
	diffSig := testHMACSHA256(diffPayload, secret)
	if VerifyWebhookSignature(payload, diffSig, secret) {
		t.Fatalf("expected false when signature belongs to a different payload")
	}
}

// Test F — TestVerifyWebhookSignatureRawPayloadSensitivity
func TestVerifyWebhookSignatureRawPayloadSensitivity(t *testing.T) {
	secret := "webhook-secret-456"
	payload1 := []byte(`{"id":"evt_001","status":"captured"}`)
	payload2 := []byte(`{ "id": "evt_001", "status": "captured" }`)

	sig1 := testHMACSHA256(payload1, secret)

	// Original bytes must verify
	if !VerifyWebhookSignature(payload1, sig1, secret) {
		t.Fatalf("expected verification to succeed for exact original bytes")
	}

	// Different whitespace must fail
	if VerifyWebhookSignature(payload2, sig1, secret) {
		t.Fatalf("expected verification to fail for payload with different whitespace")
	}
}

// Test G — TestVerifyWebhookSignatureBinaryPayload
func TestVerifyWebhookSignatureBinaryPayload(t *testing.T) {
	secret := "binary-secret-789"
	binaryPayload := []byte{0x00, 0xFF, 0xFE, 0x01, 0x00, 0xAA, 0xBB, 0x00, 0xCC, 0xDD}

	sig := testHMACSHA256(binaryPayload, secret)

	if !VerifyWebhookSignature(binaryPayload, sig, secret) {
		t.Fatalf("expected verification to succeed for binary payload")
	}

	// Modify one byte
	tampered := make([]byte, len(binaryPayload))
	copy(tampered, binaryPayload)
	tampered[0] = 0x01

	if VerifyWebhookSignature(tampered, sig, secret) {
		t.Fatalf("expected verification to fail for tampered binary payload")
	}

	// Empty payload test
	emptyPayload := []byte{}
	emptySig := testHMACSHA256(emptyPayload, secret)
	if !VerifyWebhookSignature(emptyPayload, emptySig, secret) {
		t.Fatalf("expected verification to succeed for empty payload with valid HMAC")
	}
}

// Test H — TestVerifyWebhookSignatureCaseAndLength
func TestVerifyWebhookSignatureCaseAndLength(t *testing.T) {
	secret := "test-secret"
	payload := []byte("payload content")
	lowerSig := testHMACSHA256(payload, secret)

	// Lowercase hex succeeds
	if !VerifyWebhookSignature(payload, lowerSig, secret) {
		t.Fatalf("expected lowercase hex signature to succeed")
	}

	// Uppercase hex succeeds
	upperSig := strings.ToUpper(lowerSig)
	if !VerifyWebhookSignature(payload, upperSig, secret) {
		t.Fatalf("expected uppercase hex signature to succeed")
	}

	// Odd length hex (not valid hex byte pairs)
	oddSig := lowerSig[:63]
	if VerifyWebhookSignature(payload, oddSig, secret) {
		t.Fatalf("expected odd-length hex to be rejected")
	}

	// Correct hex, but wrong decoded length (e.g. 16 bytes = 32 hex chars)
	shortValidHex := lowerSig[:32]
	if VerifyWebhookSignature(payload, shortValidHex, secret) {
		t.Fatalf("expected short decoded length to be rejected")
	}
}
