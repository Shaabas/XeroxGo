package payments

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

const maxInt64 = int64(1<<63 - 1)

// Authoritative default fixed pricing constants in paise (₹1 = 100 paise)
const (
	RateBWSimplexPaise    int64 = 300  // ₹3 for single side B&W
	RateBWDuplexPaise     int64 = 500  // ₹5 for double side B&W (2 pages)
	RateColorSimplexPaise int64 = 800  // ₹8 for single side Colour
	RateColorDuplexPaise  int64 = 1000 // ₹10 for double side Colour (2 pages)
)

// CalculateDocumentPricePaise calculates the total price in integer paise for a document
// based on default in-code rates:
// - 3 for single side b&w (300 paise)
// - 5 for double side b&w (500 paise for 2 pages)
// - 8 for single side colour (800 paise)
// - 10 for double side colour (1000 paise for 2 pages)
//
// For a double-sided order with an odd number of pages (e.g. 3 page pdf):
// - Every full 2-page sheet is charged at the double-side rate (₹5 B&W / ₹10 Colour).
// - The trailing single page is charged at the single-side rate (₹3 B&W / ₹8 Colour)
//   because there is no fourth page to print as double-sided.
//
// The total for one copy is multiplied by copies, protected against overflow.
func CalculateDocumentPricePaise(imposedPages int, isColor bool, isDuplex bool, copies int) (int64, error) {
	if imposedPages <= 0 {
		return 0, fmt.Errorf("calculate price: imposedPages must be > 0, got %d", imposedPages)
	}
	if copies <= 0 {
		copies = 1
	}

	var singleCopyPrice int64
	if isDuplex {
		fullSheets := int64(imposedPages / 2)
		hasOddPage := (imposedPages%2 != 0)
		if isColor {
			singleCopyPrice = fullSheets * RateColorDuplexPaise
			if hasOddPage {
				singleCopyPrice += RateColorSimplexPaise
			}
		} else {
			singleCopyPrice = fullSheets * RateBWDuplexPaise
			if hasOddPage {
				singleCopyPrice += RateBWSimplexPaise
			}
		}
	} else {
		if isColor {
			singleCopyPrice = int64(imposedPages) * RateColorSimplexPaise
		} else {
			singleCopyPrice = int64(imposedPages) * RateBWSimplexPaise
		}
	}

	if int64(copies) > maxInt64/singleCopyPrice {
		return 0, errors.New("calculate price: total price overflows int64")
	}

	return singleCopyPrice * int64(copies), nil
}

// CalculatePricePaise computes the total price in integer paise for a given
// printed sides count and configured price per side. It validates that both
// arguments are positive and protects against int64 overflow.
func CalculatePricePaise(totalSides int, pricePerSidePaise int64) (int64, error) {
	if totalSides <= 0 {
		return 0, fmt.Errorf("calculate price: totalSides must be > 0, got %d", totalSides)
	}
	if pricePerSidePaise <= 0 {
		return 0, fmt.Errorf("calculate price: pricePerSidePaise must be > 0, got %d", pricePerSidePaise)
	}

	// Check overflow before multiplication
	if int64(totalSides) > maxInt64/pricePerSidePaise {
		return 0, errors.New("calculate price: total price overflows int64")
	}

	return int64(totalSides) * pricePerSidePaise, nil
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
