package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"xeroxgo/cloud-backend/internal/pricing"
)

func sampleSheetTariff() pricing.Tariff {
	return pricing.Tariff{
		BWSimplexPaise: 300, BWDuplexSheetPaise: 500,
		ColorSimplexPaise: 800, ColorDuplexSheetPaise: 1000,
		ColorEnabled: false,
	}
}

func TestSheetTariffExplicitActivationAndPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pricing.sqlite")
	s, err := Open(path)
	if err != nil { t.Fatal(err) }
	if _, err := s.GetActiveSheetTariff(ctx); !errors.Is(err, ErrSheetTariffNotActive) {
		t.Fatalf("expected inactive tariff, got %v", err)
	}
	first, err := s.ActivateSheetTariff(ctx, 0, sampleSheetTariff(), "test-admin")
	if err != nil || first.Revision != 1 {
		t.Fatalf("activation=%+v err=%v", first, err)
	}
	active, err := s.GetActiveSheetTariff(ctx)
	if err != nil || active != first {
		t.Fatalf("active=%+v want=%+v err=%v", active, first, err)
	}
	if err := s.Close(); err != nil { t.Fatal(err) }
	reopened, err := Open(path)
	if err != nil { t.Fatal(err) }
	defer reopened.Close()
	active, err = reopened.GetActiveSheetTariff(ctx)
	if err != nil || active != first {
		t.Fatalf("reopened tariff=%+v want=%+v err=%v", active, first, err)
	}
}

func TestSheetTariffCASAndHistory(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "tariffs.sqlite"))
	if err != nil { t.Fatal(err) }
	defer s.Close()
	first, err := s.ActivateSheetTariff(ctx, 0, sampleSheetTariff(), "admin-one")
	if err != nil { t.Fatal(err) }
	if _, err := s.ActivateSheetTariff(ctx, 0, sampleSheetTariff(), "stale-admin"); !errors.Is(err, ErrSheetTariffRevisionConflict) {
		t.Fatalf("stale revision expected conflict; got %v", err)
	}
	changed := sampleSheetTariff()
	changed.BWSimplexPaise = 350
	second, err := s.ActivateSheetTariff(ctx, first.Revision, changed, "admin-two")
	if err != nil || second.Revision != 2 { t.Fatalf("next=%+v err=%v", second, err) }
	var oldPrice int64
	if err := s.DB().QueryRowContext(ctx, "SELECT bw_simplex_paise FROM sheet_tariff_versions WHERE revision=1").Scan(&oldPrice); err != nil || oldPrice != 300 {
		t.Fatalf("historical revision overwritten: price=%d err=%v", oldPrice, err)
	}
	var count int
	if err := s.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM sheet_tariff_versions").Scan(&count); err != nil || count != 2 {
		t.Fatalf("history count=%d err=%v", count, err)
	}
}

func TestSheetTariffSimultaneousAdminUpdates(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "concurrent.sqlite"))
	if err != nil { t.Fatal(err) }
	defer s.Close()
	if _, err := s.ActivateSheetTariff(ctx, 0, sampleSheetTariff(), "admin"); err != nil { t.Fatal(err) }
	const concurrent = 12
	var wg sync.WaitGroup
	results := make(chan error, concurrent)
	for i:=0; i<concurrent; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			next := sampleSheetTariff()
			next.BWSimplexPaise += int64(i)
			_, err := s.ActivateSheetTariff(ctx, 1, next, "admin")
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil { wins++ } else if !errors.Is(err, ErrSheetTariffRevisionConflict) {
			t.Fatalf("unexpected concurrent update error: %v", err)
		}
	}
	if wins != 1 { t.Fatalf("successful concurrent updates=%d, want 1", wins) }
	active, err := s.GetActiveSheetTariff(ctx)
	if err != nil || active.Revision != 2 { t.Fatalf("active=%+v err=%v", active, err) }
}

func TestSheetTariffInvalidUpdateLeavesActiveUnchanged(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "invalid.sqlite"))
	if err != nil { t.Fatal(err) }
	defer s.Close()
	if _, err := s.ActivateSheetTariff(ctx, 0, sampleSheetTariff(), "admin"); err != nil { t.Fatal(err) }
	invalid := sampleSheetTariff()
	invalid.ColorDuplexSheetPaise = 0
	if _, err := s.ActivateSheetTariff(ctx, 1, invalid, "admin"); err == nil {
		t.Fatal("invalid tariff accepted")
	}
	if _, err := s.ActivateSheetTariff(ctx, 1, sampleSheetTariff(), " "); err == nil {
		t.Fatal("empty actor accepted")
	}
	active, err := s.GetActiveSheetTariff(ctx)
	if err != nil || active.Revision != 1 { t.Fatalf("active=%+v err=%v", active, err) }
}
