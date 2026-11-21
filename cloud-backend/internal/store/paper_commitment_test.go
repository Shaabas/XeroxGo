package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func makeTestOrderWithSheets(orderID, kioskID, pin string, sheets int, status contracts.JobStatus, now time.Time) Order {
	cmd := contracts.PrintJobCommand{
		JobID:          "job_" + orderID,
		KioskID:        kioskID,
		DownloadURL:    "https://storage.test/" + orderID + ".pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages:     sheets,
		TotalSheets:    sheets,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
		DisplayCode:    pin,
	}
	return Order{
		OrderID:      orderID,
		Command:      cmd,
		Status:       status,
		PricePaise:   int64(sheets * 200),
		PINExpiresAt: now.Add(30 * time.Minute),
	}
}

func createTestOrderWithSheets(t *testing.T, s *Store, ctx context.Context, orderID, kioskID, pin string, sheets int, status contracts.JobStatus, now time.Time) {
	t.Helper()
	if err := s.CreateOrder(ctx, makeTestOrderWithSheets(orderID, kioskID, pin, sheets, status, now)); err != nil {
		t.Fatalf("failed to create order %s: %v", orderID, err)
	}
}

func TestStore_PaperCommitments(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "commitment_test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	kioskID := "kiosk-commit-1"

	// 1. Initial commitments should be 0
	committed, err := s.GetCommittedPaperSheets(ctx, kioskID, "", now)
	if err != nil {
		t.Fatalf("initial GetCommittedPaperSheets failed: %v", err)
	}
	if committed != 0 {
		t.Errorf("committed = %d, want 0", committed)
	}

	// 2. Create Order 1 (10 sheets, PENDING_PAYMENT)
	createTestOrderWithSheets(t, s, ctx, "ord_c1", kioskID, "1001", 10, contracts.JobStatusPendingPayment, now)


	// PIN issuance reserves paper even before the kiosk creates a session.
	committed, _ = s.GetCommittedPaperSheets(ctx, kioskID, "", now)
	if committed != 10 {
		t.Errorf("unclaimed order committed = %d, want 10", committed)
	}

	// Claim Order 1 with 50 sheets in tray -> succeeds
	sess1, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, "ord_c1", 50, 5*time.Minute, now)
	if err != nil {
		t.Fatalf("claim ord_c1 failed: %v", err)
	}
	if sess1 == nil {
		t.Fatal("expected non-nil session")
	}

	// Now committed sheets should be 10
	committed, _ = s.GetCommittedPaperSheets(ctx, kioskID, "", now)
	if committed != 10 {
		t.Errorf("after claim ord_c1 committed = %d, want 10", committed)
	}

	// Excluding ord_c1 should report 0
	committedEx, _ := s.GetCommittedPaperSheets(ctx, kioskID, "ord_c1", now)
	if committedEx != 0 {
		t.Errorf("excluding ord_c1 committed = %d, want 0", committedEx)
	}

	// 3. Create Order 2 (15 sheets, PENDING_PAYMENT)
	createTestOrderWithSheets(t, s, ctx, "ord_c2", kioskID, "1002", 15, contracts.JobStatusPendingPayment, now)

	// Available stock is 20 sheets. With ord_c1 committing 10, effective available is 20 - 10 = 10 sheets.
	// ord_c2 needs 15 sheets -> should fail with ErrInsufficientPaper!
	_, err = s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, "ord_c2", 20, 5*time.Minute, now)
	if !errors.Is(err, ErrInsufficientPaper) {
		t.Fatalf("expected ErrInsufficientPaper, got %v", err)
	}

	// But if physical stock is 30 sheets, 30 - 10 = 20 >= 15 -> should succeed!
	sess2, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, "ord_c2", 30, 5*time.Minute, now)
	if err != nil {
		t.Fatalf("claim ord_c2 with stock 30 failed: %v", err)
	}
	if sess2 == nil {
		t.Fatal("expected non-nil session for ord_c2")
	}

	// Total commitments = 10 (ord_c1) + 15 (ord_c2) = 25
	committed, _ = s.GetCommittedPaperSheets(ctx, kioskID, "", now)
	if committed != 25 {
		t.Errorf("total committed = %d, want 25", committed)
	}


	// Session expiration does not release the mobile reservation.
	afterSessionExpiry := now.Add(6 * time.Minute)
	if got, _ := s.GetCommittedPaperSheets(ctx, kioskID, "", afterSessionExpiry); got != 25 {
		t.Errorf("expired session should retain PIN reservations: got %d, want 25", got)
	}
	// Only the unpaid PIN expiration releases it.
	later := now.Add(31 * time.Minute)
	committedLater, _ := s.GetCommittedPaperSheets(ctx, kioskID, "", later)
	if committedLater != 0 {
		t.Errorf("expired unpaid PINs committed %d sheets, want 0", committedLater)
	}

	// 5. Paid Order Preserves Commitment After Session Expiry
	// Mark ord_c1 as PAID_WAITING
	if err := s.UpdateOrderStatus(ctx, "ord_c1", contracts.JobStatusPaidWaiting); err != nil {
		t.Fatalf("failed to update ord_c1 to PAID_WAITING: %v", err)
	}

	// At later time, ord_c1 session is expired, but order is PAID_WAITING -> commits 10 sheets!
	committedPaid, _ := s.GetCommittedPaperSheets(ctx, kioskID, "", later)
	if committedPaid != 10 {
		t.Errorf("paid order after session expiry committed = %d, want 10", committedPaid)
	}

	// 6. Partial Print Reduces Commitment
	// ord_c1 prints 4 sheets
	progressEvt := contracts.JobProgressEvent{
		JobID:         "job_ord_c1",
		KioskID:       kioskID,
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  4,
		SheetsPrinted: 4,
		Timestamp:     later.Add(10 * time.Second),
	}
	if err := s.UpdateJobProgress(ctx, progressEvt); err != nil {
		t.Fatalf("failed to update job progress: %v", err)
	}
	// ord_c1 status is PRINTING, needs 10 - 4 = 6 sheets remaining
	committedPartial, _ := s.GetCommittedPaperSheets(ctx, kioskID, "", later.Add(11*time.Second))
	if committedPartial != 6 {
		t.Errorf("partial print committed = %d, want 6", committedPartial)
	}

	// 7. Paused Error Preserves Remaining Commitment
	pausedEvt := contracts.JobProgressEvent{
		JobID:         "job_ord_c1",
		KioskID:       kioskID,
		Status:        contracts.JobStatusPausedError,
		PagesPrinted:  4,
		SheetsPrinted: 4,
		ErrorReason:   "paper jam",
		Timestamp:     later.Add(20 * time.Second),
	}
	if err := s.UpdateJobProgress(ctx, pausedEvt); err != nil {
		t.Fatalf("failed to pause job: %v", err)
	}
	committedPaused, _ := s.GetCommittedPaperSheets(ctx, kioskID, "", later.Add(21*time.Second))
	if committedPaused != 6 {
		t.Errorf("paused error committed = %d, want 6", committedPaused)
	}

	// 8. Completed Order Releases All Commitment
	completeEvt := contracts.JobProgressEvent{
		JobID:         "job_ord_c1",
		KioskID:       kioskID,
		Status:        contracts.JobStatusCompleted,
		PagesPrinted:  10,
		SheetsPrinted: 10,
		Timestamp:     later.Add(30 * time.Second),
	}
	if err := s.UpdateJobProgress(ctx, completeEvt); err != nil {
		t.Fatalf("failed to complete job: %v", err)
	}
	committedDone, _ := s.GetCommittedPaperSheets(ctx, kioskID, "", later.Add(31*time.Second))
	if committedDone != 0 {
		t.Errorf("completed order committed = %d, want 0", committedDone)
	}
}

func TestStore_ConcurrentOvercommitmentGuard(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "concurrent_guard.db"))
	if err != nil { t.Fatal(err) }
	defer s.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	kioskID := "kiosk-race-1"
	const availableStock, orderCount = 10, 10

	var wg sync.WaitGroup
	var mu sync.Mutex
	var created, short, unexpected int
	for i := 0; i < orderCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ord := makeTestOrderWithSheets(
				fmt.Sprintf("ord_race_%d", i), kioskID,
				fmt.Sprintf("%04d", 2000+i), 6, contracts.JobStatusPendingPayment, now)
			err := s.CreateOrderWithPaperCheck(ctx, ord, availableStock)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil: created++
			case errors.Is(err, ErrInsufficientPaper): short++
			default:
				unexpected++
				t.Errorf("create reserved order: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if created != 1 || short != 9 || unexpected != 0 {
		t.Fatalf("expected 1 PIN issued, 9 stock rejections; got created=%d shortage=%d other=%d", created, short, unexpected)
	}
	if got, err := s.GetCommittedPaperSheets(ctx, kioskID, "", now); err != nil || got != 6 {
		t.Fatalf("reserved sheets=%d err=%v, want 6", got, err)
	}
}
