package pricing

import (
	"errors"
	"fmt"
	"time"
)

const (
	// Canonical default rates in integer paise (₹1 = 100 paise)
	DefaultBWSimplexPaise    int64 = 300  // ₹3.00 per printed side
	DefaultBWDuplexPaise     int64 = 500  // ₹5.00 per complete two-sided sheet
	DefaultColorSimplexPaise int64 = 800  // ₹8.00 per printed side
	DefaultColorDuplexPaise  int64 = 1000 // ₹10.00 per complete two-sided sheet
	DefaultColorEnabled      bool  = false

	MaxInt64 = int64(1<<63 - 1)
)

// TariffConfig represents comprehensive pricing for monochrome and color prints (simplex and duplex)
// along with hardware capabilities.
type TariffConfig struct {
	BWSimplexPricePaise    int64     `json:"bw_simplex_price_paise"`
	BWDuplexPricePaise     int64     `json:"bw_duplex_price_paise"`
	ColorSimplexPricePaise int64     `json:"color_simplex_price_paise"`
	ColorDuplexPricePaise  int64     `json:"color_duplex_price_paise"`
	ColorEnabled           bool      `json:"color_enabled"`
	UpdatedAt              time.Time `json:"updated_at"`
}

// DefaultTariff returns a complete default 4-tier tariff configuration with canonical rates.
func DefaultTariff() TariffConfig {
	now := time.Now().UTC()
	return TariffConfig{
		BWSimplexPricePaise:    DefaultBWSimplexPaise,
		BWDuplexPricePaise:     DefaultBWDuplexPaise,
		ColorSimplexPricePaise: DefaultColorSimplexPaise,
		ColorDuplexPricePaise:  DefaultColorDuplexPaise,
		ColorEnabled:           DefaultColorEnabled,
		UpdatedAt:              now,
	}
}

// CalculateDocumentPricePaise calculates the total price in integer paise for a document
// based on canonical tariff configuration:
// - B&W simplex: tariff.BWSimplexPricePaise (300 paise / ₹3)
// - B&W duplex: tariff.BWDuplexPricePaise (500 paise / ₹5 per 2-sided sheet)
// - Color simplex: tariff.ColorSimplexPricePaise (800 paise / ₹8)
// - Color duplex: tariff.ColorDuplexPricePaise (1000 paise / ₹10 per 2-sided sheet)
//
// For duplex printing:
//   - Every complete two-sided sheet (floor(imposedSides/2)) is billed at the duplex sheet rate.
//   - Any odd trailing side (imposedSides%2 != 0) is billed at the simplex rate,
//     preserving historical odd-page behavior.
//
// For simplex printing:
// - Imposed sides are billed at the simplex rate.
//
// Both duplex-edge modes (long-edge and short-edge) have identical billing.
// Blank normalization padding is never billed as printed content (caller passes unpadded imposed sides).
// If color is requested while tariff.ColorEnabled is false, rates fall back to B&W rates.
//
// Rejects invalid quantities (imposedSides <= 0, copies <= 0), non-positive rates, and arithmetic overflow.
func CalculateDocumentPricePaise(tariff TariffConfig, imposedSides int, isColor bool, isDuplex bool, copies int) (int64, error) {
	if imposedSides <= 0 {
		return 0, fmt.Errorf("calculate price: imposedSides must be > 0, got %d", imposedSides)
	}
	if copies <= 0 {
		return 0, fmt.Errorf("calculate price: copies must be > 0, got %d", copies)
	}

	if tariff.BWSimplexPricePaise <= 0 || tariff.BWDuplexPricePaise <= 0 ||
		tariff.ColorSimplexPricePaise <= 0 || tariff.ColorDuplexPricePaise <= 0 {
		return 0, errors.New("calculate price: all tariff rates must be > 0")
	}

	useColor := isColor && tariff.ColorEnabled
	var simplexRate, duplexRate int64
	if useColor {
		simplexRate = tariff.ColorSimplexPricePaise
		duplexRate = tariff.ColorDuplexPricePaise
	} else {
		simplexRate = tariff.BWSimplexPricePaise
		duplexRate = tariff.BWDuplexPricePaise
	}

	var singleCopyPrice int64
	if isDuplex {
		fullSheets := int64(imposedSides / 2)
		hasOddPage := (imposedSides%2 != 0)

		if fullSheets > 0 && duplexRate > MaxInt64/fullSheets {
			return 0, errors.New("calculate price: total price overflows int64")
		}
		singleCopyPrice = fullSheets * duplexRate

		if hasOddPage {
			if singleCopyPrice > MaxInt64-simplexRate {
				return 0, errors.New("calculate price: total price overflows int64")
			}
			singleCopyPrice += simplexRate
		}
	} else {
		if int64(imposedSides) > MaxInt64/simplexRate {
			return 0, errors.New("calculate price: total price overflows int64")
		}
		singleCopyPrice = int64(imposedSides) * simplexRate
	}

	if int64(copies) > MaxInt64/singleCopyPrice {
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
	if int64(totalSides) > MaxInt64/pricePerSidePaise {
		return 0, errors.New("calculate price: total price overflows int64")
	}

	return int64(totalSides) * pricePerSidePaise, nil
}
