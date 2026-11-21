package pricing

import (
	"strings"
	"testing"
)

func TestCalculateDocumentPricePaise_CanonicalMatrix(t *testing.T) {
	tariff := TariffConfig{
		BWSimplexPricePaise:    300,
		BWDuplexPricePaise:     500,
		ColorSimplexPricePaise: 800,
		ColorDuplexPricePaise:  1000,
		ColorEnabled:           true,
	}

	testCases := []struct {
		name     string
		pages    int
		isColor  bool
		isDuplex bool
		copies   int
		expected int64
	}{
		// 3-page odd duplex test cases: 5 for first 2 pages (1 full sheet), 3 for 3rd page = 8 (800 paise)
		{name: "3-page BW Duplex (1 copy)", pages: 3, isColor: false, isDuplex: true, copies: 1, expected: 800},
		{name: "3-page BW Duplex (2 copies)", pages: 3, isColor: false, isDuplex: true, copies: 2, expected: 1600},
		{name: "1-page BW Duplex (1 copy)", pages: 1, isColor: false, isDuplex: true, copies: 1, expected: 300},
		{name: "2-page BW Duplex (1 copy)", pages: 2, isColor: false, isDuplex: true, copies: 1, expected: 500},
		{name: "4-page BW Duplex (1 copy)", pages: 4, isColor: false, isDuplex: true, copies: 1, expected: 1000},
		{name: "5-page BW Duplex (1 copy)", pages: 5, isColor: false, isDuplex: true, copies: 1, expected: 1300}, // 5 + 5 + 3 = 13 (1300 paise)

		// Single-sided BW (1-5 pages)
		{name: "1-page BW Simplex", pages: 1, isColor: false, isDuplex: false, copies: 1, expected: 300},
		{name: "2-page BW Simplex", pages: 2, isColor: false, isDuplex: false, copies: 1, expected: 600},
		{name: "3-page BW Simplex", pages: 3, isColor: false, isDuplex: false, copies: 1, expected: 900},
		{name: "4-page BW Simplex", pages: 4, isColor: false, isDuplex: false, copies: 1, expected: 1200},
		{name: "5-page BW Simplex", pages: 5, isColor: false, isDuplex: false, copies: 1, expected: 1500},

		// Color Duplex (1-5 pages)
		{name: "1-page Color Duplex", pages: 1, isColor: true, isDuplex: true, copies: 1, expected: 800},
		{name: "2-page Color Duplex", pages: 2, isColor: true, isDuplex: true, copies: 1, expected: 1000},
		{name: "3-page Color Duplex", pages: 3, isColor: true, isDuplex: true, copies: 1, expected: 1800}, // 10 + 8 = 18
		{name: "4-page Color Duplex", pages: 4, isColor: true, isDuplex: true, copies: 1, expected: 2000},
		{name: "5-page Color Duplex", pages: 5, isColor: true, isDuplex: true, copies: 1, expected: 2800}, // 10 + 10 + 8 = 28

		// Color Simplex (1-5 pages)
		{name: "1-page Color Simplex", pages: 1, isColor: true, isDuplex: false, copies: 1, expected: 800},
		{name: "2-page Color Simplex", pages: 2, isColor: true, isDuplex: false, copies: 1, expected: 1600},
		{name: "3-page Color Simplex", pages: 3, isColor: true, isDuplex: false, copies: 1, expected: 2400},
		{name: "4-page Color Simplex", pages: 4, isColor: true, isDuplex: false, copies: 1, expected: 3200},
		{name: "5-page Color Simplex", pages: 5, isColor: true, isDuplex: false, copies: 1, expected: 4000},

		// Multi-copy tests
		{name: "5-page BW Duplex (3 copies)", pages: 5, isColor: false, isDuplex: true, copies: 3, expected: 3900},
		{name: "4-page Color Simplex (5 copies)", pages: 4, isColor: true, isDuplex: false, copies: 5, expected: 16000},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual, err := CalculateDocumentPricePaise(tariff, tc.pages, tc.isColor, tc.isDuplex, tc.copies)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if actual != tc.expected {
				t.Fatalf("got %d paise, want %d paise", actual, tc.expected)
			}
		})
	}
}

func TestCalculateDocumentPricePaise_ColorDisabledFallback(t *testing.T) {
	tariff := TariffConfig{
		BWSimplexPricePaise:    300,
		BWDuplexPricePaise:     500,
		ColorSimplexPricePaise: 800,
		ColorDuplexPricePaise:  1000,
		ColorEnabled:           false, // Hardware does not support color
	}

	// When ColorEnabled is false, requesting color MUST fall back to B&W rates
	cases := []struct {
		name     string
		pages    int
		isDuplex bool
		copies   int
		expected int64
	}{
		{"1-page Color Simplex with Color disabled -> BW Simplex", 1, false, 1, 300},
		{"3-page Color Duplex with Color disabled -> BW Duplex (5+3=8)", 3, true, 1, 800},
		{"4-page Color Duplex with Color disabled -> BW Duplex (2*5=10)", 4, true, 1, 1000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actual, err := CalculateDocumentPricePaise(tariff, tc.pages, true, tc.isDuplex, tc.copies)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if actual != tc.expected {
				t.Fatalf("got %d paise, want %d paise", actual, tc.expected)
			}
		})
	}
}

func TestCalculateDocumentPricePaise_RejectInvalidInputs(t *testing.T) {
	validTariff := DefaultTariff()

	invalidCases := []struct {
		name       string
		tariff     TariffConfig
		sides      int
		copies     int
		wantErrSub string
	}{
		{"zero sides", validTariff, 0, 1, "imposedSides must be > 0"},
		{"negative sides", validTariff, -3, 1, "imposedSides must be > 0"},
		{"zero copies", validTariff, 1, 0, "copies must be > 0"},
		{"negative copies", validTariff, 1, -2, "copies must be > 0"},
		{"zero BW simplex rate", TariffConfig{BWSimplexPricePaise: 0, BWDuplexPricePaise: 500, ColorSimplexPricePaise: 800, ColorDuplexPricePaise: 1000}, 1, 1, "all tariff rates must be > 0"},
		{"negative BW duplex rate", TariffConfig{BWSimplexPricePaise: 300, BWDuplexPricePaise: -500, ColorSimplexPricePaise: 800, ColorDuplexPricePaise: 1000}, 1, 1, "all tariff rates must be > 0"},
		{"zero Color simplex rate", TariffConfig{BWSimplexPricePaise: 300, BWDuplexPricePaise: 500, ColorSimplexPricePaise: 0, ColorDuplexPricePaise: 1000}, 1, 1, "all tariff rates must be > 0"},
		{"zero Color duplex rate", TariffConfig{BWSimplexPricePaise: 300, BWDuplexPricePaise: 500, ColorSimplexPricePaise: 800, ColorDuplexPricePaise: 0}, 1, 1, "all tariff rates must be > 0"},
	}

	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CalculateDocumentPricePaise(tc.tariff, tc.sides, false, false, tc.copies)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrSub)
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("expected error containing %q, got %q", tc.wantErrSub, err.Error())
			}
		})
	}
}

func TestCalculateDocumentPricePaise_Overflow(t *testing.T) {
	tariff := TariffConfig{
		BWSimplexPricePaise:    1000,
		BWDuplexPricePaise:     2000,
		ColorSimplexPricePaise: 1000,
		ColorDuplexPricePaise:  2000,
		ColorEnabled:           true,
	}

	// Multiplication by copies overflows MaxInt64
	_, err := CalculateDocumentPricePaise(tariff, 10, false, false, int(MaxInt64/5000))
	if err == nil {
		t.Fatalf("expected overflow error, got nil")
	}

	// Duplex full sheets overflow
	hugeSides := int(MaxInt64 / 1000)
	_, err = CalculateDocumentPricePaise(tariff, hugeSides, false, true, 2)
	if err == nil {
		t.Fatalf("expected overflow error for huge sides in duplex, got nil")
	}
}

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
	_, err := CalculatePricePaise(2, int64(1<<62))
	if err == nil {
		t.Fatalf("expected overflow error for 2 * (1<<62), got nil")
	}

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

	_, err = CalculatePricePaise(int(divisor)+1, priceNearMax+1)
	if err == nil {
		t.Fatalf("expected overflow error, got nil")
	}
}

func TestDefaultTariff(t *testing.T) {
	def := DefaultTariff()
	if def.BWSimplexPricePaise != 300 || def.BWDuplexPricePaise != 500 {
		t.Fatalf("unexpected BW canonical defaults: %+v", def)
	}
	if def.ColorSimplexPricePaise != 800 || def.ColorDuplexPricePaise != 1000 {
		t.Fatalf("unexpected Color canonical defaults: %+v", def)
	}
	if def.ColorEnabled != false {
		t.Fatalf("expected ColorEnabled false by default")
	}
}
