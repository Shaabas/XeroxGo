package pricing

import (
	"errors"
	"fmt"
	"math"
)

// MaxRatePaise is the upper bound for a single admin-configured billing unit.
// The validation limit also constrains corrupted or accidental tariff updates.
const MaxRatePaise int64 = 1_000_000

// Tariff is one immutable snapshot of the four configured billing rates.
// Duplex rates describe a FULL two-sided sheet; an odd final side uses simplex.
type Tariff struct {
	Revision              int64
	BWSimplexPaise        int64
	BWDuplexSheetPaise    int64
	ColorSimplexPaise     int64
	ColorDuplexSheetPaise int64
	ColorEnabled          bool
}

// Document quantities must originate from the canonical PDF normalizer.
// ImposedSidesPerCopy excludes artificial duplex blank padding.
type Document struct {
	ImposedSidesPerCopy int64
	Copies              int64
	Duplex              bool
	Color               bool
}

type Line struct {
	TotalPaise    int64
	BilledSides   int64
	SheetsPerCopy int64
}

type Quote struct {
	TariffRevision int64
	TotalPaise     int64
	BilledSides    int64
	Lines          []Line
}

func ValidateTariff(t Tariff) error {
	if t.Revision < 1 {
		return errors.New("tariff revision must be positive")
	}
	for _, entry := range []struct {
		name  string
		value int64
	}{
		{"bw simplex", t.BWSimplexPaise},
		{"bw duplex sheet", t.BWDuplexSheetPaise},
		{"color simplex", t.ColorSimplexPaise},
		{"color duplex sheet", t.ColorDuplexSheetPaise},
	} {
		if entry.value < 1 || entry.value > MaxRatePaise {
			return fmt.Errorf("%s rate outside allowed range 1..%d paise", entry.name, MaxRatePaise)
		}
	}
	return nil
}

func checkedAdd(a, b int64) (int64, error) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, errors.New("price addition overflow")
	}
	return a + b, nil
}

func checkedMul(a, b int64) (int64, error) {
	if a < 0 || b < 0 || (b != 0 && a > math.MaxInt64/b) {
		return 0, errors.New("price multiplication overflow")
	}
	return a * b, nil
}

// CalculateOrder uses exactly one tariff revision for all documents.
// It does not read configuration, touch PDFs, access the database, or charge.
func CalculateOrder(t Tariff, docs []Document) (Quote, error) {
	if err := ValidateTariff(t); err != nil {
		return Quote{}, err
	}
	if len(docs) == 0 {
		return Quote{}, errors.New("order has no documents")
	}
	if len(docs) > 100 {
		return Quote{}, errors.New("too many documents")
	}

	q := Quote{TariffRevision: t.Revision, Lines: make([]Line, 0, len(docs))}
	for i, d := range docs {
		if d.ImposedSidesPerCopy < 1 || d.Copies < 1 || d.Copies > 999 {
			return Quote{}, fmt.Errorf("document %d: invalid quantities", i+1)
		}
		if d.Color && !t.ColorEnabled {
			return Quote{}, fmt.Errorf("document %d: color unavailable", i+1)
		}

		simplex, duplex := t.BWSimplexPaise, t.BWDuplexSheetPaise
		if d.Color {
			simplex, duplex = t.ColorSimplexPaise, t.ColorDuplexSheetPaise
		}

		sheets := d.ImposedSidesPerCopy
		var perCopy int64
		var err error
		if d.Duplex {
			sheets = d.ImposedSidesPerCopy/2 + d.ImposedSidesPerCopy%2
			perCopy, err = checkedMul(d.ImposedSidesPerCopy/2, duplex)
			if err != nil {
				return Quote{}, fmt.Errorf("document %d: %w", i+1, err)
			}
			if d.ImposedSidesPerCopy%2 == 1 {
				perCopy, err = checkedAdd(perCopy, simplex)
			}
		} else {
			perCopy, err = checkedMul(d.ImposedSidesPerCopy, simplex)
		}
		if err != nil {
			return Quote{}, fmt.Errorf("document %d: %w", i+1, err)
		}

		linePrice, err := checkedMul(perCopy, d.Copies)
		if err != nil {
			return Quote{}, fmt.Errorf("document %d: %w", i+1, err)
		}
		billedSides, err := checkedMul(d.ImposedSidesPerCopy, d.Copies)
		if err != nil {
			return Quote{}, fmt.Errorf("document %d: %w", i+1, err)
		}
		q.TotalPaise, err = checkedAdd(q.TotalPaise, linePrice)
		if err != nil {
			return Quote{}, err
		}
		q.BilledSides, err = checkedAdd(q.BilledSides, billedSides)
		if err != nil {
			return Quote{}, err
		}
		q.Lines = append(q.Lines, Line{TotalPaise: linePrice, BilledSides: billedSides, SheetsPerCopy: sheets})
	}
	return q, nil
}
