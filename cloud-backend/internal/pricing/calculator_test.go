package pricing

import (
	"math"
	"testing"
)

var sheetRates = Tariff{
	Revision: 2, BWSimplexPaise: 300, BWDuplexSheetPaise: 500,
	ColorSimplexPaise: 800, ColorDuplexSheetPaise: 1000, ColorEnabled: true,
}

func TestCalculateOrderSheetPricing(t *testing.T) {
	cases := []struct {
		name         string
		docs         []Document
		price, sides int64
	}{
		{"bw simplex", []Document{{3, 1, false, false}}, 900, 3},
		{"bw duplex 1 side", []Document{{1, 1, true, false}}, 300, 1},
		{"bw duplex 2 sides", []Document{{2, 1, true, false}}, 500, 2},
		{"bw duplex 3 sides", []Document{{3, 1, true, false}}, 800, 3},
		{"bw duplex 4 sides", []Document{{4, 1, true, false}}, 1000, 4},
		{"bw duplex 5 sides", []Document{{5, 1, true, false}}, 1300, 5},
		{"color duplex odd", []Document{{3, 1, true, true}}, 1800, 3},
		{"color simplex", []Document{{3, 1, false, true}}, 2400, 3},
		{"copies", []Document{{3, 2, true, false}}, 1600, 6},
		{"2-up imposed", []Document{{2, 1, true, false}}, 500, 2},
		{"4-up imposed", []Document{{1, 1, true, false}}, 300, 1},
		{"6-up imposed", []Document{{1, 1, false, true}}, 800, 1},
		{"mixed multi-file order",
			[]Document{{3, 1, true, false}, {2, 2, false, true}, {1, 1, true, true}}, 4800, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := CalculateOrder(sheetRates, tc.docs)
			if err != nil {
				t.Fatal(err)
			}
			if q.TotalPaise != tc.price || q.BilledSides != tc.sides || q.TariffRevision != 2 {
				t.Fatalf("quote = %+v, expected price %d sides %d revision 2", q, tc.price, tc.sides)
			}
		})
	}
}

func TestCalculateOrderRejectsInvalidInputs(t *testing.T) {
	cases := []struct {
		name string
		cfg  Tariff
		docs []Document
	}{
		{"unconfigured", Tariff{}, []Document{{1, 1, false, false}}},
		{"zero rate", Tariff{Revision: 1}, []Document{{1, 1, false, false}}},
		{"oversized tariff", Tariff{Revision: 1, BWSimplexPaise: MaxRatePaise + 1, BWDuplexSheetPaise: 500, ColorSimplexPaise: 800, ColorDuplexSheetPaise: 1000}, []Document{{1, 1, false, false}}},
		{"no documents", sheetRates, nil},
		{"zero copies", sheetRates, []Document{{2, 0, true, false}}},
		{"too many copies", sheetRates, []Document{{2, 1000, true, false}}},
		{"zero imposed sides", sheetRates, []Document{{0, 1, true, false}}},
		{"unsupported color", Tariff{Revision: 1, BWSimplexPaise: 300, BWDuplexSheetPaise: 500, ColorSimplexPaise: 800, ColorDuplexSheetPaise: 1000}, []Document{{2, 1, true, true}}},
		{"overflow", sheetRates, []Document{{math.MaxInt64, 1, false, false}}},
		{"overflow sides", sheetRates, []Document{{math.MaxInt64, 2, true, false}}},
		{"overflow order sum", sheetRates, []Document{{math.MaxInt64 / 300, 1, false, false}, {math.MaxInt64 / 300, 1, false, false}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := CalculateOrder(tc.cfg, tc.docs); err == nil {
				t.Fatal("expected a validation or overflow error")
			}
		})
	}
}

func TestCalculateOrderDoesNotModifyTariff(t *testing.T) {
	q, err := CalculateOrder(sheetRates, []Document{{2, 1, true, false}})
	if err != nil || q.TotalPaise != 500 {
		t.Fatalf("quote=%+v err=%v", q, err)
	}
	if sheetRates.BWDuplexSheetPaise != 500 {
		t.Fatal("tariff mutated")
	}
}
