package pricing

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"xeroxgo/pkg/contracts"
)

const (
	// Default tariff rates in integer paise (₹1 = 100 paise)
	DefaultBWSimplexPaise    int64 = 200  // ₹2.00
	DefaultBWDuplexPaise     int64 = 150  // ₹1.50 (discounted rate for duplex long/short edge)
	DefaultColorSimplexPaise int64 = 1000 // ₹10.00
	DefaultColorDuplexPaise  int64 = 750  // ₹7.50
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
	BWPricePerSheetPaise    int64     `json:"bw_price_per_sheet_paise"`
	ColorPricePerSheetPaise int64     `json:"color_price_per_sheet_paise"`
	BWPricePerSidePaise    int64     `json:"bw_price_per_side_paise,omitempty"`
	ColorPricePerSidePaise int64     `json:"color_price_per_side_paise,omitempty"`
	UpdatedAt               time.Time `json:"updated_at"`
}

// DefaultTariff returns a complete default 4-tier tariff configuration.
func DefaultTariff() TariffConfig {
	now := time.Now().UTC()
	return TariffConfig{
		BWSimplexPricePaise:    DefaultBWSimplexPaise,
		BWDuplexPricePaise:     DefaultBWDuplexPaise,
		ColorSimplexPricePaise: DefaultColorSimplexPaise,
		ColorDuplexPricePaise:  DefaultColorDuplexPaise,
		ColorEnabled:           DefaultColorEnabled,
		BWPricePerSheetPaise:    DefaultBWSimplexPaise,
		ColorPricePerSheetPaise: DefaultColorSimplexPaise,
		BWPricePerSidePaise:    DefaultBWSimplexPaise,
		ColorPricePerSidePaise: DefaultColorSimplexPaise,
		UpdatedAt:               now,
	}
}

// ComputeDuplexDiscount calculates the standard duplex discount (3/4 of simplex, 25% discount).
func ComputeDuplexDiscount(simplexPrice int64, defaultDuplex int64) int64 {
	duplex := (simplexPrice * 3) / 4
	if duplex <= 0 {
		return defaultDuplex
	}
	return duplex
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

// ResolveUnitPrice resolves the authoritative unit price in paise based on the 4-tier tariff matrix,
// requested color mode, duplex mode, and fallback options.
func ResolveUnitPrice(tariff TariffConfig, colorMode string, duplex contracts.DuplexMode, fallbackUnitPrice int64) int64 {
	isDuplex := duplex != contracts.DuplexSimplex
	var unitPrice int64

	if strings.EqualFold(colorMode, "COLOR") && tariff.ColorEnabled {
		if isDuplex {
			unitPrice = tariff.ColorDuplexPricePaise
		} else {
			unitPrice = tariff.ColorSimplexPricePaise
		}
	} else {
		if isDuplex {
			unitPrice = tariff.BWDuplexPricePaise
		} else {
			unitPrice = tariff.BWSimplexPricePaise
		}
	}

	if unitPrice <= 0 {
		unitPrice = fallbackUnitPrice
	}
	if unitPrice <= 0 {
		unitPrice = DefaultBWSimplexPaise
	}
	return unitPrice
}

// ResolveUnitPriceFromConfig resolves the unit price, properly handling cases where dynamic database
// pricing is unavailable (hasPricing == false).
func ResolveUnitPriceFromConfig(hasPricing bool, tariff TariffConfig, colorMode string, duplex contracts.DuplexMode, fallbackUnitPrice int64) int64 {
	if !hasPricing {
		if fallbackUnitPrice > 0 {
			return fallbackUnitPrice
		}
		return DefaultBWSimplexPaise
	}
	return ResolveUnitPrice(tariff, colorMode, duplex, fallbackUnitPrice)
}

