package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func getTestPostgresStore(t *testing.T) *Store {
	t.Helper()
	pgURI := os.Getenv("XEROXGO_TEST_POSTGRES_URL")
	if pgURI == "" {
		pgURI = os.Getenv("POSTGRES_TEST_URL")
	}
	if pgURI == "" {
		t.Log("[UNVERIFIED] Disposable PostgreSQL test database not configured (XEROXGO_TEST_POSTGRES_URL / POSTGRES_TEST_URL not set). Live PostgreSQL concurrency tests skipped.")
		t.Skip("skipping disposable Postgres concurrency tests (XEROXGO_TEST_POSTGRES_URL not set)")
		return nil
	}

	s, err := Open(pgURI)
	if err != nil {
		t.Fatalf("failed to connect to disposable PostgreSQL test store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if s.dialect != "postgres" {
		t.Fatalf("expected store dialect postgres, got: %s", s.dialect)
	}

	if err := s.EnsureActiveSessionConstraint(context.Background()); err != nil {
		t.Fatalf("failed to apply active session constraint on test database: %v", err)
	}

	return s
}

// TestPostgresConcurrency_SimultaneousUploads_PaperCheck tests simultaneous mobile uploads
// competing for limited paper stock on the same kiosk under PostgreSQL advisory locks.
// It verifies that physical paper is never overcommitted even under intense concurrency.
func TestPostgresConcurrency_SimultaneousUploads_PaperCheck(t *testing.T) {
	s := getTestPostgresStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	kioskID := fmt.Sprintf("kiosk-sim-up-%d", time.Now().UnixNano()%100000)
	availableStock := 15
	sheetsPerOrder := 6
	concurrency := 5 // 5 * 6 = 30 sheets requested > 15 available. Exactly 2 should succeed (12 sheets), 3 must fail.

	var wg sync.WaitGroup
	wg.Add(concurrency)

	errs := make([]error, concurrency)

	for i := 0; i < concurrency; i++ {
		idx := i
		ordID := fmt.Sprintf("ord_pg_up_%d_%d", time.Now().UnixNano(), i)
		pin := fmt.Sprintf("61%02d", i)

		ord := Order{
			OrderID:      ordID,
			PricePaise:   1000,
			Status:       contracts.JobStatusPendingPayment,
			TotalSheets:  sheetsPerOrder,
			TotalPages:   sheetsPerOrder,
			CreatedAt:    now,
			PINExpiresAt: now.Add(10 * time.Minute),
			Command: contracts.PrintJobCommand{
				JobID:         fmt.Sprintf("job_pg_up_%d_%d", time.Now().UnixNano(), i),
				KioskID:       kioskID,
				DisplayCode:   pin,
				TotalSheets:   sheetsPerOrder,
				TotalPages:    sheetsPerOrder,
				Copies:        1,
				Duplex:        contracts.DuplexSimplex,
				StartFromPage: 1,
			},
		}

		go func() {
			defer wg.Done()
			err := s.CreateOrderWithPaperCheck(ctx, ord, availableStock)
			errs[idx] = err
		}()
	}
	wg.Wait()

	var successCount int
	var insufficientCount int
	for _, err := range errs {
		if err == nil {
			successCount++
		} else if errors.Is(err, ErrInsufficientPaper) {
			insufficientCount++
		} else {
			t.Errorf("unexpected error during simultaneous upload: %v", err)
		}
	}

	if successCount != 2 {
		t.Fatalf("expected exactly 2 successful uploads, got %d", successCount)
	}
	if insufficientCount != 3 {
		t.Fatalf("expected exactly 3 insufficient paper rejections, got %d", insufficientCount)
	}

	// Verify authoritative paper commitments in PostgreSQL
	committed, err := s.GetCommittedPaperSheets(ctx, kioskID, "", now)
	if err != nil {
		t.Fatalf("GetCommittedPaperSheets failed: %v", err)
	}
	if committed != 12 {
		t.Fatalf("expected exactly 12 committed sheets in PostgreSQL, got %d", committed)
	}
}

// TestPostgresConcurrency_PaymentVsCancellation_Serialization tests concurrency race
// between payment capture and session cancellation on PostgreSQL.
// Exactly one operation must win atomically; subsequent or late cancellation must be
// an idempotent no-op that preserves payment and dispatch eligibility, while uncaptured
// payment against a canceled session must be rejected with ErrSessionCanceled.
func TestPostgresConcurrency_PaymentVsCancellation_Serialization(t *testing.T) {
	s := getTestPostgresStore(t)
	ctx := context.Background()

	// Run multiple trials to reliably explore both interleavings (payment wins vs cancel wins)
	trials := 8
	var paymentWonCount, cancelWonCount int

	for i := 0; i < trials; i++ {
		now := time.Now().UTC()
		kioskID := fmt.Sprintf("kiosk-race-%d-%d", time.Now().UnixNano()%100000, i)
		orderID := fmt.Sprintf("ord-race-%d-%d", time.Now().UnixNano(), i)
		pin := fmt.Sprintf("71%02d", i)

		ord := Order{
			OrderID:      orderID,
			PricePaise:   1000,
			Status:       contracts.JobStatusPendingPayment,
			TotalSheets:  2,
			TotalPages:   2,
			CreatedAt:    now,
			PINExpiresAt: now.Add(10 * time.Minute),
			Command: contracts.PrintJobCommand{
				JobID:         fmt.Sprintf("job-race-%d-%d", time.Now().UnixNano(), i),
				KioskID:       kioskID,
				DisplayCode:   pin,
				TotalSheets:   2,
				TotalPages:    2,
				Copies:        1,
				Duplex:        contracts.DuplexSimplex,
				StartFromPage: 1,
			},
		}

		if err := s.CreateOrder(ctx, ord); err != nil {
			t.Fatalf("trial %d: CreateOrder failed: %v", i, err)
		}

		sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 50, 120*time.Second, now)
		if err != nil {
			t.Fatalf("trial %d: CreateOrGetKioskSession failed: %v", i, err)
		}

		startBarrier := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)

		var payRes *PaymentCaptureResult
		var payErr error
		var cancelErr error

		// Goroutine 1: Capture payment
		go func() {
			defer wg.Done()
			<-startBarrier
			payRes, payErr = s.CapturePaymentAndCheckDispatch(ctx, PaymentRecord{
				PaymentID:   fmt.Sprintf("pay-race-%d-%d", time.Now().UnixNano(), i),
				OrderID:     orderID,
				KioskID:     kioskID,
				SessionID:   sess.SessionID,
				AmountPaise: 1000,
				Provider:    "mock",
				Status:      "captured",
			}, now.Add(1*time.Second))
		}()

		// Goroutine 2: Cancel session
		go func() {
			defer wg.Done()
			<-startBarrier
			cancelErr = s.CancelKioskSession(ctx, sess.SessionID, kioskID, now.Add(1*time.Second))
		}()

		// Trigger race
		close(startBarrier)
		wg.Wait()

		if cancelErr != nil {
			t.Fatalf("trial %d: CancelKioskSession returned unexpected error: %v", i, cancelErr)
		}

		// Check atomic outcome
		finalOrder, err := s.GetOrder(ctx, orderID)
		if err != nil {
			t.Fatalf("trial %d: GetOrder failed: %v", i, err)
		}
		finalSess, err := s.GetKioskSession(ctx, sess.SessionID, now.Add(2*time.Second))
		if err != nil {
			t.Fatalf("trial %d: GetKioskSession failed: %v", i, err)
		}
		pendingIntents, err := s.GetPendingDispatchOrders(ctx, kioskID)
		if err != nil {
			t.Fatalf("trial %d: GetPendingDispatchOrders failed: %v", i, err)
		}

		if payErr == nil {
			// Payment won!
			paymentWonCount++
			if !payRes.DispatchEligible {
				t.Fatalf("trial %d: expected DispatchEligible true when payment won", i)
			}
			if finalOrder.Status != contracts.JobStatusDispatched {
				t.Fatalf("trial %d: expected order status DISPATCHED, got: %s", i, finalOrder.Status)
			}
			if finalSess.Status != KioskSessionCompleted {
				t.Fatalf("trial %d: expected session status COMPLETED, got: %s", i, finalSess.Status)
			}
			if len(pendingIntents) != 1 {
				t.Fatalf("trial %d: expected exactly 1 pending dispatch intent, got: %d", i, len(pendingIntents))
			}
		} else {
			// Cancel won!
			cancelWonCount++
			if !errors.Is(payErr, ErrSessionCanceled) {
				t.Fatalf("trial %d: expected ErrSessionCanceled when cancel won, got: %v", i, payErr)
			}
			if finalOrder.Status != contracts.JobStatusPendingPayment {
				t.Fatalf("trial %d: expected order status PENDING_PAYMENT, got: %s", i, finalOrder.Status)
			}
			if finalSess.Status != KioskSessionCanceled {
				t.Fatalf("trial %d: expected session status CANCELED, got: %s", i, finalSess.Status)
			}
			if len(pendingIntents) != 0 {
				t.Fatalf("trial %d: expected 0 pending dispatch intents when cancel won, got: %d", i, len(pendingIntents))
			}
		}
	}

	t.Logf("Postgres concurrency serialization verified over %d trials: payment won %d times, cancel won %d times. Zero deadlocks or inconsistent states.", trials, paymentWonCount, cancelWonCount)
}
