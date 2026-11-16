package pricing

import (
	"strings"
	"testing"

	"xeroxgo/pkg/contracts"
)

func TestCalculatePricePaise_Valid(t *testing.T) {
	cases := []struct {
		name      string
		sides     int
		unitPrice int64
		wantTotal int64
	}{
		{"single side 100 paise", 1, 100, 100},
		{"10 sides 250 paise", 10, 250, 2500},
		{"100 sides 150 paise", 100, 150, 15000},
		{"500 sides 200 paise", 500, 200, 100000},
		{"1200 sides (e.g. 400 pages x 3 copies) 150 paise", 1200, 150, 180000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CalculatePricePaise(tc.sides, tc.unitPrice)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantTotal {
				t.Fatalf("got %d, want %d", got, tc.wantTotal)
			}
		})
	}
}

func TestCalculatePricePaise_InvalidInputs(t *testing.T) {
	invalidCases := []struct {
		name       string
		sides      int
		unitPrice  int64
		wantErrSub string
	}{
		{"zero sides", 0, 200, "totalSides must be > 0"},
		{"negative sides", -1, 200, "totalSides must be > 0"},
		{"large negative sides", -500, 200, "totalSides must be > 0"},
		{"zero price", 10, 0, "pricePerSidePaise must be > 0"},
		{"negative price", 10, -50, "pricePerSidePaise must be > 0"},
		{"both negative", -5, -100, "totalSides must be > 0"},
	}

	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CalculatePricePaise(tc.sides, tc.unitPrice)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil result %d", tc.wantErrSub, got)
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("expected error containing %q, got %q", tc.wantErrSub, err.Error())
			}
		})
	}
}

func TestCalculatePricePaise_Overflow(t *testing.T) {
	// 2 * 2^62 = 2^63 which overflows signed int64
	_, err := CalculatePricePaise(2, int64(1<<62))
	if err == nil {
		t.Fatalf("expected overflow error for 2 * (1<<62), got nil")
	}

	// Large valid multiplication right below max
	divisor := int64(10)
	priceNearMax := MaxInt64 / divisor
	total, err := CalculatePricePaise(int(divisor), priceNearMax)
	if err != nil {
		t.Fatalf("valid large multiplication failed: %v", err)
	}
	expected := int64(divisor) * priceNearMax
	if total != expected {
		t.Fatalf("expected %d, got %d", expected, total)
	}

	// Just over maximum
	_, err = CalculatePricePaise(int(divisor)+1, priceNearMax+1)
	if err == nil {
		t.Fatalf("expected overflow error, got nil")
	}
}

func TestResolveUnitPrice_FourTiers(t *testing.T) {
	tariff := TariffConfig{
		BWSimplexPricePaise:    200,
		BWDuplexPricePaise:     150,
		ColorSimplexPricePaise: 1000,
		ColorDuplexPricePaise:  750,
		ColorEnabled:           true,
	}

	tests := []struct {
		name      string
		colorMode string
		duplex    contracts.DuplexMode
		wantPrice int64
	}{
		{"BW Simplex", "BW", contracts.DuplexSimplex, 200},
		{"BW Duplex LongEdge", "BW", contracts.DuplexLongEdge, 150},
		{"BW Duplex ShortEdge", "BW", contracts.DuplexShortEdge, 150},
		{"Color Simplex", "COLOR", contracts.DuplexSimplex, 1000},
		{"Color Duplex LongEdge", "COLOR", contracts.DuplexLongEdge, 750},
		{"Color Duplex ShortEdge", "COLOR", contracts.DuplexShortEdge, 750},
		{"Case-insensitive color string", "color", contracts.DuplexSimplex, 1000},
		{"Case-insensitive bw string", "bw", contracts.DuplexLongEdge, 150},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			price := ResolveUnitPrice(tariff, tc.colorMode, tc.duplex, 0)
			if price != tc.wantPrice {
				t.Fatalf("ResolveUnitPrice(%s, %s) = %d, want %d", tc.colorMode, tc.duplex, price, tc.wantPrice)
			}
		})
	}
}

func TestResolveUnitPrice_ColorDisabledFallback(t *testing.T) {
	tariff := TariffConfig{
		BWSimplexPricePaise:    250,
		BWDuplexPricePaise:     180,
		ColorSimplexPricePaise: 1200,
		ColorDuplexPricePaise:  900,
		ColorEnabled:           false, // Hardware does not support color
	}

	// Requesting COLOR when ColorEnabled=false MUST fall back to BW rates
	simpPrice := ResolveUnitPrice(tariff, "COLOR", contracts.DuplexSimplex, 0)
	if simpPrice != 250 {
		t.Fatalf("expected BW simplex fallback 250, got %d", simpPrice)
	}

	dupPrice := ResolveUnitPrice(tariff, "COLOR", contracts.DuplexLongEdge, 0)
	if dupPrice != 180 {
		t.Fatalf("expected BW duplex fallback 180, got %d", dupPrice)
	}
}

func TestResolveUnitPriceFromConfig_NoPricing(t *testing.T) {
	tariff := TariffConfig{} // empty

	// When hasPricing is false, use fallbackUnitPrice if provided
	priceWithFallback := ResolveUnitPriceFromConfig(false, tariff, "BW", contracts.DuplexSimplex, 350)
	if priceWithFallback != 350 {
		t.Fatalf("expected fallback price 350, got %d", priceWithFallback)
	}

	// When fallbackUnitPrice is <= 0 and hasPricing is false, use DefaultBWSimplexPaise (200)
	defaultPrice := ResolveUnitPriceFromConfig(false, tariff, "BW", contracts.DuplexSimplex, 0)
	if defaultPrice != DefaultBWSimplexPaise {
		t.Fatalf("expected default price %d, got %d", DefaultBWSimplexPaise, defaultPrice)
	}
}

func TestResolveUnitPrice_ZeroOrNegativeTiersFallback(t *testing.T) {
	// Tariff with zero prices
	tariffZero := TariffConfig{
		BWSimplexPricePaise: 0,
		ColorEnabled:        false,
	}

	// Falls back to fallbackUnitPrice
	price := ResolveUnitPrice(tariffZero, "BW", contracts.DuplexSimplex, 300)
	if price != 300 {
		t.Fatalf("expected fallback 300, got %d", price)
	}

	// Falls back to DefaultBWSimplexPaise (200) when fallback is also 0
	priceDef := ResolveUnitPrice(tariffZero, "BW", contracts.DuplexSimplex, 0)
	if priceDef != DefaultBWSimplexPaise {
		t.Fatalf("expected default %d, got %d", DefaultBWSimplexPaise, priceDef)
	}
}

func TestComputeDuplexDiscount(t *testing.T) {
	// Standard 25% discount: (200 * 3) / 4 = 150
	d1 := ComputeDuplexDiscount(200, 150)
	if d1 != 150 {
		t.Fatalf("ComputeDuplexDiscount(200) = %d, want 150", d1)
	}

	// 1000 * 3 / 4 = 750
	d2 := ComputeDuplexDiscount(1000, 750)
	if d2 != 750 {
		t.Fatalf("ComputeDuplexDiscount(1000) = %d, want 750", d2)
	}

	// 350 * 3 / 4 = 262
	d3 := ComputeDuplexDiscount(350, 150)
	if d3 != 262 {
		t.Fatalf("ComputeDuplexDiscount(350) = %d, want 262", d3)
	}

	// Non-positive simplex falls back to default
	dZero := ComputeDuplexDiscount(0, 150)
	if dZero != 150 {
		t.Fatalf("ComputeDuplexDiscount(0) = %d, want fallback 150", dZero)
	}
}

func TestDefaultTariff(t *testing.T) {
	def := DefaultTariff()
	if def.BWSimplexPricePaise != 200 || def.BWDuplexPricePaise != 150 {
		t.Fatalf("unexpected BW defaults: %+v", def)
	}
	if def.ColorSimplexPricePaise != 1000 || def.ColorDuplexPricePaise != 750 {
		t.Fatalf("unexpected Color defaults: %+v", def)
	}
	if def.ColorEnabled != false {
		t.Fatalf("expected ColorEnabled false by default")
	}
	if def.BWPricePerSheetPaise != 200 || def.ColorPricePerSheetPaise != 1000 {
		t.Fatalf("unexpected compatibility sheet prices: %+v", def)
	}
}


// These cases preserve Bibin's 50e1a24 pricing policy without silently
// migrating stored per-side tariffs.
func TestQuoteDocument_BibinSheetPolicy(t *testing.T) {
	tariff := TariffConfig{
		BWSimplexPricePaise:    300,
		BWDuplexPricePaise:     500,
		ColorSimplexPricePaise: 800,
		ColorDuplexPricePaise:  1000,
		ColorEnabled:           true,
	}
	cases := []struct {
		name          string
		sides, copies int
		mode          string
		duplex        contracts.DuplexMode
		want          int64
	}{
		{"one BW simplex", 1, 1, "BW", contracts.DuplexSimplex, 300},
		{"two BW duplex", 2, 1, "BW", contracts.DuplexLongEdge, 500},
		{"three BW duplex", 3, 1, "BW", contracts.DuplexLongEdge, 800},
		{"four BW duplex", 4, 1, "BW", contracts.DuplexLongEdge, 1000},
		{"five BW duplex", 5, 1, "BW", contracts.DuplexLongEdge, 1300},
		{"three BW duplex copies", 3, 2, "BW", contracts.DuplexShortEdge, 1600},
		{"three COLOR duplex", 3, 1, "COLOR", contracts.DuplexLongEdge, 1800},
		{"two COLOR duplex", 2, 1, "COLOR", contracts.DuplexLongEdge, 1000},
		{"one COLOR duplex", 1, 1, "COLOR", contracts.DuplexLongEdge, 800},
		{"three COLOR simplex", 3, 1, "COLOR", contracts.DuplexSimplex, 2400},
		{"two imposed sides (three original pages at two-up)", 2, 1, "BW", contracts.DuplexLongEdge, 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := QuoteDocument(tc.sides, tc.copies, tc.mode, tc.duplex, tariff, true, 0, PricingPerSheet)
			if err != nil {
				t.Fatal(err)
			}
			if got.TotalPaise != tc.want {
				t.Fatalf("total=%d want=%d", got.TotalPaise, tc.want)
			}
			if got.BilledSides != tc.sides*tc.copies {
				t.Fatalf("billed sides=%d", got.BilledSides)
			}
		})
	}
}

func TestQuoteDocument_LegacyPolicyUnchanged(t *testing.T) {
	tariff := TariffConfig{BWSimplexPricePaise: 200, BWDuplexPricePaise: 150}
	got, err := QuoteDocument(3, 2, "BW", contracts.DuplexLongEdge, tariff, true, 0, PricingPerSide)
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalPaise != 900 {
		t.Fatalf("legacy pricing changed: got %d want 900", got.TotalPaise)
	}
}

func TestQuoteDocument_RejectInvalidAndOverflow(t *testing.T) {
	tariff := TariffConfig{BWSimplexPricePaise: 300, BWDuplexPricePaise: 500}
	cases := []struct {
		sides, copies int
		mode          string
		policy        DuplexPricingPolicy
	}{
		{0, 1, "BW", PricingPerSheet},
		{1, 0, "BW", PricingPerSheet},
		{1, 1, "UNKNOWN", PricingPerSheet},
		{1, 1, "BW", "unknown"},
	}
	for _, tc := range cases {
		if _, err := QuoteDocument(tc.sides, tc.copies, tc.mode, contracts.DuplexLongEdge, tariff, true, 0, tc.policy); err == nil {
			t.Fatalf("accepted invalid %+v", tc)
		}
	}
	bigTariff := TariffConfig{BWSimplexPricePaise: MaxInt64, BWDuplexPricePaise: 500}
	if _, err := QuoteDocument(2, 1, "BW", contracts.DuplexSimplex, bigTariff, true, 0, PricingPerSheet); err == nil {
		t.Fatal("overflow accepted")
	}
	if _, err := QuoteDocument(3, 1, "BW", contracts.DuplexLongEdge, tariff, false, 0, PricingPerSheet); err == nil {
		t.Fatal("sheet mode without explicit tariffs accepted")
	}
}
