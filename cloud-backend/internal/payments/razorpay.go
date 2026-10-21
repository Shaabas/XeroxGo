package payments

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

const maxInt64 = int64(1<<63 - 1)

// CalculatePricePaise computes the total price in integer paise for a given
// physical sheet count and configured price per sheet. It validates that both
// arguments are positive and protects against int64 overflow.
func CalculatePricePaise(totalSheets int, pricePerSheetPaise int64) (int64, error) {
	if totalSheets <= 0 {
		return 0, fmt.Errorf("calculate price: totalSheets must be > 0, got %d", totalSheets)
	}
	if pricePerSheetPaise <= 0 {
		return 0, fmt.Errorf("calculate price: pricePerSheetPaise must be > 0, got %d", pricePerSheetPaise)
	}

	// Check overflow before multiplication
	if int64(totalSheets) > maxInt64/pricePerSheetPaise {
		return 0, errors.New("calculate price: total price overflows int64")
	}

	return int64(totalSheets) * pricePerSheetPaise, nil
}

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
