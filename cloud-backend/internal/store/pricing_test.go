package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xeroxgo/cloud-backend/internal/pricing"
	"xeroxgo/pkg/contracts"
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

func TestPostgresSheetTariff_IntegrationSuite(t *testing.T) {
	pgURI := os.Getenv("XEROXGO_TEST_POSTGRES_URL")
	if pgURI == "" {
		pgURI = os.Getenv("POSTGRES_TEST_URL")
	}
	if pgURI == "" {
		t.Skip("skipping live Postgres tariff integration tests (XEROXGO_TEST_POSTGRES_URL/POSTGRES_TEST_URL not set)")
		return
	}

	rootStore, err := Open(pgURI)
	if err != nil {
		t.Fatalf("failed to open root postgres store: %v", err)
	}
	defer rootStore.Close()

	ctx := context.Background()
	testSchema := fmt.Sprintf("test_tariff_iso_%d", time.Now().UnixNano()%100000000)
	if _, err := rootStore.db.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s;", testSchema)); err != nil {
		t.Fatalf("failed to create isolated schema %s: %v", testSchema, err)
	}
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = rootStore.db.ExecContext(cleanCtx, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE;", testSchema))
	})

	separator := "?"
	if strings.Contains(pgURI, "?") {
		separator = "&"
	}
	isolatedURI := fmt.Sprintf("%s%ssearch_path=%s", pgURI, separator, testSchema)

	s, err := Open(isolatedURI)
	if err != nil {
		t.Fatalf("failed to open store in isolated schema: %v", err)
	}
	defer s.Close()

	t.Run("Postgres_InitialUnconfigured", func(t *testing.T) {
		_, err := s.GetActiveSheetTariff(ctx)
		if !errors.Is(err, ErrSheetTariffNotActive) {
			t.Fatalf("expected ErrSheetTariffNotActive on clean db, got: %v", err)
		}
	})

	var rev1 pricing.Tariff
	t.Run("Postgres_ActivationAndRetrieval", func(t *testing.T) {
		var err error
		rev1, err = s.ActivateSheetTariff(ctx, 0, sampleSheetTariff(), "test-admin")
		if err != nil || rev1.Revision != 1 {
			t.Fatalf("activation failed: rev=%+v err=%v", rev1, err)
		}
		active, err := s.GetActiveSheetTariff(ctx)
		if err != nil || active.Revision != 1 || active.BWDuplexSheetPaise != 500 {
			t.Fatalf("retrieved active mismatch: %+v, err: %v", active, err)
		}
	})

	t.Run("Postgres_StaleRevisionRejection", func(t *testing.T) {
		_, err := s.ActivateSheetTariff(ctx, 0, sampleSheetTariff(), "stale-admin")
		if !errors.Is(err, ErrSheetTariffRevisionConflict) {
			t.Fatalf("expected ErrSheetTariffRevisionConflict for stale expected revision, got: %v", err)
		}
	})

	t.Run("Postgres_ConcurrentAdminUpdates", func(t *testing.T) {
		const concurrent = 10
		var wg sync.WaitGroup
		errs := make(chan error, concurrent)

		for i := 0; i < concurrent; i++ {
			wg.Add(1)
			go func(delta int) {
				defer wg.Done()
				tCopy := sampleSheetTariff()
				tCopy.BWSimplexPaise += int64(delta)
				_, err := s.ActivateSheetTariff(ctx, 1, tCopy, fmt.Sprintf("admin-%d", delta))
				errs <- err
			}(i)
		}
		wg.Wait()
		close(errs)

		wins := 0
		for e := range errs {
			if e == nil {
				wins++
			} else if !errors.Is(e, ErrSheetTariffRevisionConflict) {
				t.Fatalf("unexpected concurrent activation error: %v", e)
			}
		}
		if wins != 1 {
			t.Fatalf("expected exactly 1 winner, got %d", wins)
		}
		active, err := s.GetActiveSheetTariff(ctx)
		if err != nil || active.Revision != 2 {
			t.Fatalf("expected active revision 2, got: %+v err=%v", active, err)
		}
	})

	t.Run("Postgres_OrderCreationWithRevisionGuard", func(t *testing.T) {
		now := time.Now().UTC()
		orderID := fmt.Sprintf("ord-pg-guard-%d", time.Now().UnixNano())
		jobID := fmt.Sprintf("job-pg-guard-%d", time.Now().UnixNano())
		kioskID := fmt.Sprintf("kiosk-pg-%d", time.Now().UnixNano()%1000)
		order := Order{
			OrderID: orderID,
			Command: contracts.PrintJobCommand{
				JobID:          jobID,
				KioskID:        kioskID,
				DisplayCode:    "1234",
				DownloadURL:    "https://example.com/test.pdf",
				SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
				TotalPages:     2,
				TotalSheets:    1,
				Copies:         1,
				Duplex:         contracts.DuplexLongEdge,
				StartFromPage:  1,
			},
			Status:     contracts.JobStatusNormalized,
			PricePaise: 500,
		}

		// 1. Creation with stale revision must fail
		errStale := s.CreateOrderWithTariffRevision(ctx, order, 1) // active is 2
		if !errors.Is(errStale, ErrSheetTariffRevisionConflict) {
			t.Fatalf("expected ErrSheetTariffRevisionConflict for stale order creation, got: %v", errStale)
		}

		// 2. Creation with active revision 2 must succeed
		errActive := s.CreateOrderWithTariffRevision(ctx, order, 2)
		if errActive != nil {
			t.Fatalf("CreateOrderWithTariffRevision failed with active revision: %v", errActive)
		}

		// 3. Verify order exists
		retrieved, err := s.GetOrder(ctx, orderID)
		if err != nil || retrieved == nil {
			t.Fatalf("GetOrder failed: %v", err)
		}
		if retrieved.OrderID != orderID || retrieved.PricePaise != 500 {
			t.Fatalf("retrieved order mismatch: %+v", retrieved)
		}

		_ = now
	})
}
