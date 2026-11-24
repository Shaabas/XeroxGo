package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

// Scenario 1: PIN validity at 9:59 vs expiry at 10:00.
func TestPackage03_Scenario01_PINValidity_959_vs_1000(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	kioskID := "kiosk-p3-sc1"

	ord1 := sampleOrder("ord_sc1_valid", "job_sc1_valid")
	ord1.Command.KioskID = kioskID
	ord1.Command.DisplayCode = "1001"
	ord1.Status = contracts.JobStatusPendingPayment
	ord1.CreatedAt = t0
	ord1.PINExpiresAt = t0.Add(10 * time.Minute)
	if err := s.CreateOrder(ctx, ord1); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	// At t0 + 9m59s: order is valid and PIN is in use; claim succeeds
	t959 := t0.Add(9*time.Minute + 59*time.Second)
	inUse, err := s.IsPINInUse(ctx, kioskID, "1001", t959)
	if err != nil || !inUse {
		t.Fatalf("expected PIN to be in use at 9:59, got inUse=%v, err=%v", inUse, err)
	}
	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ord1.OrderID, 50, 120*time.Second, t959)
	if err != nil || sess == nil {
		t.Fatalf("claim at 9:59 must succeed, got: %v", err)
	}

	// Another order at exactly t0 + 10m00s: expired
	ord2 := sampleOrder("ord_sc1_expired", "job_sc1_expired")
	ord2.Command.KioskID = kioskID
	ord2.Command.DisplayCode = "1002"
	ord2.Status = contracts.JobStatusPendingPayment
	ord2.CreatedAt = t0
	ord2.PINExpiresAt = t0.Add(10 * time.Minute)
	if err := s.CreateOrder(ctx, ord2); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	t1000 := t0.Add(10 * time.Minute)
	inUse2, err := s.IsPINInUse(ctx, kioskID, "1002", t1000)
	if err != nil || inUse2 {
		t.Fatalf("expected PIN to NOT be in use at 10:00, got inUse=%v, err=%v", inUse2, err)
	}
	_, err = s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ord2.OrderID, 50, 120*time.Second, t1000)
	if !errors.Is(err, ErrOrderExpired) {
		t.Fatalf("expected ErrOrderExpired at 10:00, got: %v", err)
	}
}

// Scenario 2: Claim at 9:59 starts fresh 120s session and keeps paper committed beyond minute 10.
func TestPackage03_Scenario02_ClaimAt959_Starts120sSession_ExtendsPaperCommitment(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	kioskID := "kiosk-p3-sc2"

	ord := sampleOrder("ord_sc2_120s", "job_sc2_120s")
	ord.Command.KioskID = kioskID
	ord.Command.DisplayCode = "2002"
	ord.Command.TotalSheets = 8
	ord.TotalSheets = 8
	ord.Status = contracts.JobStatusPendingPayment
	ord.CreatedAt = t0
	ord.PINExpiresAt = t0.Add(10 * time.Minute)
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	// Claim order at 9:59 with 120-second session duration
	t959 := t0.Add(9*time.Minute + 59*time.Second)
	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ord.OrderID, 50, 120*time.Second, t959)
	if err != nil || sess == nil {
		t.Fatalf("claim failed: %v", err)
	}

	// Session expiration should be 120s from claim (t959 + 120s = t0 + 11m59s)
	expectedSessionExpiry := t959.Add(120 * time.Second)
	if !sess.ExpiresAt.Equal(expectedSessionExpiry) {
		t.Fatalf("expected session expiry %v, got %v", expectedSessionExpiry, sess.ExpiresAt)
	}

	// At minute 10:30 (past original 10m PIN deadline, but within 120s checkout session):
	t1030 := t0.Add(10*time.Minute + 30*time.Second)
	committed, err := s.GetCommittedPaperSheets(ctx, kioskID, "", t1030)
	if err != nil {
		t.Fatalf("GetCommittedPaperSheets failed: %v", err)
	}
	if committed != 8 {
		t.Fatalf("expected 8 sheets committed past minute 10 during active checkout, got %d", committed)
	}
}

// Scenario 3: Another order cannot reuse that PIN or overcommit paper during active checkout.
func TestPackage03_Scenario03_PINCollisionAndOvercommitmentDuringCheckout(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	kioskID := "kiosk-p3-sc3"

	ord1 := sampleOrder("ord_sc3_first", "job_sc3_first")
	ord1.Command.KioskID = kioskID
	ord1.Command.DisplayCode = "3333"
	ord1.Command.TotalSheets = 6
	ord1.TotalSheets = 6
	ord1.Status = contracts.JobStatusPendingPayment
	ord1.CreatedAt = t0
	ord1.PINExpiresAt = t0.Add(10 * time.Minute)
	if err := s.CreateOrder(ctx, ord1); err != nil {
		t.Fatalf("CreateOrder 1 failed: %v", err)
	}

	// Start 120s checkout session for ord1 at 9:59
	t959 := t0.Add(9*time.Minute + 59*time.Second)
	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ord1.OrderID, 10, 120*time.Second, t959)
	if err != nil || sess == nil {
		t.Fatalf("claim failed: %v", err)
	}

	// At minute 10:15 (during active checkout):
	// A new order attempting to use PIN "3333" on kioskID must fail with PIN collision
	t1015 := t0.Add(10*time.Minute + 15*time.Second)
	ord2 := sampleOrder("ord_sc3_second", "job_sc3_second")
	ord2.Command.KioskID = kioskID
	ord2.Command.DisplayCode = "3333"
	ord2.Command.TotalSheets = 2
	ord2.TotalSheets = 2
	ord2.Status = contracts.JobStatusPendingPayment
	ord2.CreatedAt = t1015
	ord2.PINExpiresAt = t1015.Add(10 * time.Minute)

	err = s.CreateOrder(ctx, ord2)
	if !errors.Is(err, ErrPINCollision) {
		t.Fatalf("expected ErrPINCollision during active checkout session lease, got: %v", err)
	}

	// A new order with a different PIN needing 5 sheets: with stock 10 and ord1 holding 6 sheets,
	// only 4 sheets available -> must fail with ErrInsufficientPaper!
	ord3 := sampleOrder("ord_sc3_third", "job_sc3_third")
	ord3.Command.KioskID = kioskID
	ord3.Command.DisplayCode = "4444"
	ord3.Command.TotalSheets = 5
	ord3.TotalSheets = 5
	ord3.Status = contracts.JobStatusPendingPayment
	ord3.CreatedAt = t1015
	ord3.PINExpiresAt = t1015.Add(10 * time.Minute)

	err = s.CreateOrderWithPaperCheck(ctx, ord3, 10)
	if !errors.Is(err, ErrInsufficientPaper) {
		t.Fatalf("expected ErrInsufficientPaper, got: %v", err)
	}
}

// Scenario 4: Esc before original expiry permits re-entry and a fresh session.
func TestPackage03_Scenario04_EscBeforeExpiry_AllowsReentryAndRetainsPaper(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	kioskID := "kiosk-p3-sc4"

	ord := sampleOrder("ord_sc4_esc", "job_sc4_esc")
	ord.Command.KioskID = kioskID
	ord.Command.DisplayCode = "4004"
	ord.Command.TotalSheets = 5
	ord.TotalSheets = 5
	ord.Status = contracts.JobStatusPendingPayment
	ord.CreatedAt = t0
	ord.PINExpiresAt = t0.Add(10 * time.Minute)
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	// Claim session at t0 + 2m
	tClaim := t0.Add(2 * time.Minute)
	sess1, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ord.OrderID, 20, 120*time.Second, tClaim)
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}

	// Cancel session at t0 + 2m30s (Esc before 10-minute expiry)
	tCancel := t0.Add(2*time.Minute + 30*time.Second)
	if err := s.CancelKioskSession(ctx, sess1.SessionID, kioskID, tCancel); err != nil {
		t.Fatalf("CancelKioskSession failed: %v", err)
	}

	// Paper reservation must remain held (5 sheets)
	committed, _ := s.GetCommittedPaperSheets(ctx, kioskID, "", tCancel)
	if committed != 5 {
		t.Fatalf("expected 5 sheets committed retained by PIN after cancel, got %d", committed)
	}

	// Re-enter PIN at t0 + 3m: succeeds with fresh session ID!
	tReclaim := t0.Add(3 * time.Minute)
	sess2, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ord.OrderID, 20, 120*time.Second, tReclaim)
	if err != nil {
		t.Fatalf("reclaim failed: %v", err)
	}
	if sess2.SessionID == sess1.SessionID {
		t.Fatalf("new session must have different ID than canceled session")
	}
}

// Scenario 5: Esc after original expiry releases unpaid commitment and rejects re-entry.
func TestPackage03_Scenario05_EscAfterExpiry_ReleasesPaperAndRejectsReentry(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	kioskID := "kiosk-p3-sc5"

	ord := sampleOrder("ord_sc5_esc_late", "job_sc5_esc_late")
	ord.Command.KioskID = kioskID
	ord.Command.DisplayCode = "5005"
	ord.Command.TotalSheets = 7
	ord.TotalSheets = 7
	ord.Status = contracts.JobStatusPendingPayment
	ord.CreatedAt = t0
	ord.PINExpiresAt = t0.Add(10 * time.Minute)
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	// Claim at t0 + 9m50s
	tClaim := t0.Add(9*time.Minute + 50*time.Second)
	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ord.OrderID, 20, 120*time.Second, tClaim)
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}

	// Cancel session at t0 + 10m30s (past original 10-minute PIN expiry)
	tCancel := t0.Add(10*time.Minute + 30*time.Second)
	if err := s.CancelKioskSession(ctx, sess.SessionID, kioskID, tCancel); err != nil {
		t.Fatalf("CancelKioskSession failed: %v", err)
	}

	// Paper reservation must be released immediately (0 sheets)
	committed, _ := s.GetCommittedPaperSheets(ctx, kioskID, "", tCancel)
	if committed != 0 {
		t.Fatalf("expected 0 sheets committed after cancel past 10m, got %d", committed)
	}

	// Re-entry must be rejected with ErrOrderExpired
	_, err = s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ord.OrderID, 20, 120*time.Second, tCancel)
	if !errors.Is(err, ErrOrderExpired) {
		t.Fatalf("expected ErrOrderExpired after late cancel, got %v", err)
	}
}

// Scenario 7: Expired/canceled QR submissions cannot newly capture or dispatch.
func TestPackage03_Scenario07_ExpiredOrCanceledSession_RejectsCapture(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	kioskID := "kiosk-p3-sc7"

	ord := sampleOrder("ord_sc7_cap", "job_sc7_cap")
	ord.Command.KioskID = kioskID
	ord.Command.DisplayCode = "7007"
	ord.PricePaise = 500
	ord.Status = contracts.JobStatusPendingPayment
	ord.CreatedAt = t0
	ord.PINExpiresAt = t0.Add(10 * time.Minute)
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ord.OrderID, 20, 120*time.Second, t0)
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}

	// 1. Canceled session capture rejection
	tCancel := t0.Add(30 * time.Second)
	_ = s.CancelKioskSession(ctx, sess.SessionID, kioskID, tCancel)

	_, err = s.CapturePaymentAndCheckDispatch(ctx, PaymentRecord{
		PaymentID:   "pay_canceled_attempt",
		OrderID:     ord.OrderID,
		KioskID:     kioskID,
		SessionID:   sess.SessionID,
		AmountPaise: 500,
		Provider:    "mock",
		Status:      "captured",
	}, tCancel)
	if !errors.Is(err, ErrSessionCanceled) {
		t.Fatalf("expected ErrSessionCanceled for payment against canceled session, got: %v", err)
	}

	// Verify order status was NOT changed to PAID_WAITING or DISPATCHED
	ordCheck, _ := s.GetOrder(ctx, ord.OrderID)
	if ordCheck.Status != contracts.JobStatusPendingPayment {
		t.Fatalf("order status must remain PENDING_PAYMENT, got %s", ordCheck.Status)
	}

	// 2. Expired session capture rejection
	ord2 := sampleOrder("ord_sc7_exp2", "job_sc7_exp2")
	ord2.Command.KioskID = kioskID
	ord2.Command.DisplayCode = "7008"
	ord2.PricePaise = 500
	ord2.Status = contracts.JobStatusPendingPayment
	ord2.CreatedAt = t0
	ord2.PINExpiresAt = t0.Add(10 * time.Minute)
	if err := s.CreateOrder(ctx, ord2); err != nil {
		t.Fatalf("CreateOrder 2 failed: %v", err)
	}

	sess2, _ := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ord2.OrderID, 20, 120*time.Second, t0)
	tExp := t0.Add(125 * time.Second) // 120s checkout session has expired

	_, err = s.CapturePaymentAndCheckDispatch(ctx, PaymentRecord{
		PaymentID:   "pay_expired_attempt",
		OrderID:     ord2.OrderID,
		KioskID:     kioskID,
		SessionID:   sess2.SessionID,
		AmountPaise: 500,
		Provider:    "mock",
		Status:      "captured",
	}, tExp)
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("expected ErrSessionExpired for payment against expired session, got: %v", err)
	}
}

// Scenario 8: Simultaneous Esc/payment submission has exactly one safe, idempotent outcome.
func TestPackage03_Scenario08_ConcurrentEscAndPayment_DeterministicSafeOutcome(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	kioskID := "kiosk-p3-sc8"

	ord := sampleOrder("ord_sc8_race", "job_sc8_race")
	ord.Command.KioskID = kioskID
	ord.Command.DisplayCode = "8008"
	ord.PricePaise = 500
	ord.Status = contracts.JobStatusPendingPayment
	ord.CreatedAt = t0
	ord.PINExpiresAt = t0.Add(10 * time.Minute)
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ord.OrderID, 20, 120*time.Second, t0)
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}

	tAction := t0.Add(30 * time.Second)
	var wg sync.WaitGroup
	wg.Add(2)

	var cancelErr, captureErr error
	var captureRes *PaymentCaptureResult

	go func() {
		defer wg.Done()
		cancelErr = s.CancelKioskSession(ctx, sess.SessionID, kioskID, tAction)
	}()

	go func() {
		defer wg.Done()
		captureRes, captureErr = s.CapturePaymentAndCheckDispatch(ctx, PaymentRecord{
			PaymentID:   "pay_sc8_sim",
			OrderID:     ord.OrderID,
			KioskID:     kioskID,
			SessionID:   sess.SessionID,
			AmountPaise: 500,
			Provider:    "mock",
			Status:      "captured",
		}, tAction)
	}()

	wg.Wait()

	// Exactly one of two clean outcomes:
	// Outcome A: Cancel won first -> capture rejected with ErrSessionCanceled.
	// Outcome B: Capture won first -> cancel returned cleanly without revoking the payment.
	ordFinal, err := s.GetOrder(ctx, ord.OrderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}

	if captureErr != nil {
		// Outcome A
		if !errors.Is(captureErr, ErrSessionCanceled) {
			t.Fatalf("expected ErrSessionCanceled when cancel won, got: %v", captureErr)
		}
		if ordFinal.Status != contracts.JobStatusPendingPayment {
			t.Fatalf("order status must remain PENDING_PAYMENT, got: %s", ordFinal.Status)
		}
	} else {
		// Outcome B
		if captureRes == nil {
			t.Fatal("expected non-nil capture result when capture succeeded")
		}
		if ordFinal.Status != contracts.JobStatusPaidWaiting && ordFinal.Status != contracts.JobStatusDispatched {
			t.Fatalf("order status must be PAID_WAITING or DISPATCHED, got: %s", ordFinal.Status)
		}
	}
	if cancelErr != nil {
		t.Fatalf("cancel should complete without error, got: %v", cancelErr)
	}
}

// Scenario 10: Successful payment after original PIN expiry preserves paid entitlement.
func TestPackage03_Scenario10_PaymentAfterOriginalExpiry_PromotesPaidEntitlement(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	kioskID := "kiosk-p3-sc10"

	ord := sampleOrder("ord_sc10_paid_late", "job_sc10_paid_late")
	ord.Command.KioskID = kioskID
	ord.Command.DisplayCode = "1010"
	ord.Command.TotalSheets = 5
	ord.TotalSheets = 5
	ord.PricePaise = 500
	ord.Status = contracts.JobStatusPendingPayment
	ord.CreatedAt = t0
	ord.PINExpiresAt = t0.Add(10 * time.Minute)
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	// Claim order at minute 9:50 -> starts 120s checkout session (active until 11:50)
	tClaim := t0.Add(9*time.Minute + 50*time.Second)
	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ord.OrderID, 20, 120*time.Second, tClaim)
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}

	// Customer pays at minute 10:20 (past original 10m PIN expiry, but checkout session is active)
	tPay := t0.Add(10*time.Minute + 20*time.Second)
	capRes, err := s.CapturePaymentAndCheckDispatch(ctx, PaymentRecord{
		PaymentID:   "pay_sc10_late",
		OrderID:     ord.OrderID,
		KioskID:     kioskID,
		SessionID:   sess.SessionID,
		AmountPaise: 500,
		Provider:    "mock",
		Status:      "captured",
	}, tPay)
	if err != nil {
		t.Fatalf("capture during active checkout must succeed: %v", err)
	}
	if capRes == nil {
		t.Fatal("expected non-nil capture result")
	}

	// Verify order has been promoted to durable paid entitlement
	ordPaid, err := s.GetOrder(ctx, ord.OrderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if ordPaid.PINExpiresAt.Before(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("paid entitlement PIN expiry should be promoted to year 2100 deadline, got %v", ordPaid.PINExpiresAt)
	}

	// Even at 15 minutes later (well after checkout session expiry), paid order retains paper commitment!
	tMuchLater := t0.Add(15 * time.Minute)
	committed, err := s.GetCommittedPaperSheets(ctx, kioskID, "", tMuchLater)
	if err != nil {
		t.Fatalf("GetCommittedPaperSheets failed: %v", err)
	}
	if committed != 5 {
		t.Fatalf("expected 5 sheets committed for paid order after session expiry, got %d", committed)
	}
}

// Scenario 11: Multi-document reservations and remaining printed-sheet calculations are correct.
func TestPackage03_Scenario11_MultiDocumentPaperCommitment_AndProgress(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	kioskID := "kiosk-p3-sc11"
	orderID := "ord_sc11_multi"

	docs := []OrderDocument{
		{
			JobID:            "job_sc11_d1",
			OrderID:          orderID,
			DocumentIndex:    0,
			OriginalFilename: "report.pdf",
			DownloadURL:      "https://storage.test/report.pdf",
			SHA256Checksum:   "sha1",
			TotalPages:       3,
			TotalSheets:      3,
			Copies:           1,
			Duplex:           contracts.DuplexSimplex,
			PricePaise:       300,
		},
		{
			JobID:            "job_sc11_d2",
			OrderID:          orderID,
			DocumentIndex:    1,
			OriginalFilename: "thesis.pdf",
			DownloadURL:      "https://storage.test/thesis.pdf",
			SHA256Checksum:   "sha2",
			TotalPages:       4,
			TotalSheets:      4,
			Copies:           1,
			Duplex:           contracts.DuplexSimplex,
			PricePaise:       400,
		},
		{
			JobID:            "job_sc11_d3",
			OrderID:          orderID,
			DocumentIndex:    2,
			OriginalFilename: "slides.pdf",
			DownloadURL:      "https://storage.test/slides.pdf",
			SHA256Checksum:   "sha3",
			TotalPages:       5,
			TotalSheets:      5,
			Copies:           1,
			Duplex:           contracts.DuplexSimplex,
			PricePaise:       500,
		},
	}

	ord := Order{
		OrderID: orderID,
		Command: contracts.PrintJobCommand{
			JobID:          "job_sc11_d1",
			KioskID:        kioskID,
			DisplayCode:    "1111",
			TotalPages:     3,
			TotalSheets:    3,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			SHA256Checksum: "sha1",
			DownloadURL:    "https://storage.test/report.pdf",
		},
		TotalPages:   12,
		TotalSheets:  12, // 3 + 4 + 5 = 12 sheets across all docs
		PricePaise:   1200,
		Status:       contracts.JobStatusPendingPayment,
		CreatedAt:    t0,
		PINExpiresAt: t0.Add(10 * time.Minute),
		Documents:    docs,
	}

	// 1. Initial reservation must reserve full 12 sheets
	if err := s.CreateOrderWithPaperCheck(ctx, ord, 20); err != nil {
		t.Fatalf("CreateOrderWithPaperCheck failed: %v", err)
	}

	committed, err := s.GetCommittedPaperSheets(ctx, kioskID, "", t0)
	if err != nil {
		t.Fatalf("GetCommittedPaperSheets failed: %v", err)
	}
	if committed != 12 {
		t.Fatalf("expected 12 sheets committed for multi-doc order, got %d", committed)
	}

	// 2. Claim and mark as PAID_WAITING / PRINTING
	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 20, 120*time.Second, t0.Add(1*time.Minute))
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}
	_, err = s.CapturePaymentAndCheckDispatch(ctx, PaymentRecord{
		PaymentID:   "pay_sc11",
		OrderID:     orderID,
		KioskID:     kioskID,
		SessionID:   sess.SessionID,
		AmountPaise: 1200,
		Provider:    "mock",
		Status:      "captured",
	}, t0.Add(1*time.Minute+10*time.Second))
	if err != nil {
		t.Fatalf("capture failed: %v", err)
	}

	// 3. Document 1 completes (3 sheets) and Document 2 prints 2 sheets (total printed = 5)
	tPrint := t0.Add(2 * time.Minute)
	_ = s.UpdateJobProgress(ctx, contracts.JobProgressEvent{
		JobID:         "job_sc11_d1",
		KioskID:       kioskID,
		Status:        contracts.JobStatusCompleted,
		PagesPrinted:  3,
		SheetsPrinted: 3,
		Timestamp:     tPrint,
	})
	_ = s.UpdateJobProgress(ctx, contracts.JobProgressEvent{
		JobID:         "job_sc11_d2",
		KioskID:       kioskID,
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  2,
		SheetsPrinted: 2,
		Timestamp:     tPrint.Add(5 * time.Second),
	})

	// Remaining sheets must be 12 - 5 = 7 sheets
	committedRemaining, err := s.GetCommittedPaperSheets(ctx, kioskID, "", tPrint.Add(10*time.Second))
	if err != nil {
		t.Fatalf("GetCommittedPaperSheets failed: %v", err)
	}
	if committedRemaining != 7 {
		t.Fatalf("expected 7 sheets remaining (12 - 5), got %d", committedRemaining)
	}

	// 4. Paused error preserves remaining commitment (7 sheets)
	_ = s.UpdateJobProgress(ctx, contracts.JobProgressEvent{
		JobID:         "job_sc11_d2",
		KioskID:       kioskID,
		Status:        contracts.JobStatusPausedError,
		PagesPrinted:  2,
		SheetsPrinted: 2,
		ErrorReason:   "paper jam",
		Timestamp:     tPrint.Add(20 * time.Second),
	})
	committedPaused, _ := s.GetCommittedPaperSheets(ctx, kioskID, "", tPrint.Add(25*time.Second))
	if committedPaused != 7 {
		t.Fatalf("expected 7 sheets remaining during paused error, got %d", committedPaused)
	}

	// 5. All documents complete: commitment drops to 0
	_ = s.UpdateJobProgress(ctx, contracts.JobProgressEvent{
		JobID:         "job_sc11_d2",
		KioskID:       kioskID,
		Status:        contracts.JobStatusCompleted,
		PagesPrinted:  4,
		SheetsPrinted: 4,
		Timestamp:     tPrint.Add(30 * time.Second),
	})
	_ = s.UpdateJobProgress(ctx, contracts.JobProgressEvent{
		JobID:         "job_sc11_d3",
		KioskID:       kioskID,
		Status:        contracts.JobStatusCompleted,
		PagesPrinted:  5,
		SheetsPrinted: 5,
		Timestamp:     tPrint.Add(40 * time.Second),
	})

	committedDone, _ := s.GetCommittedPaperSheets(ctx, kioskID, "", tPrint.Add(50*time.Second))
	if committedDone != 0 {
		t.Fatalf("expected 0 sheets committed after all documents completed, got %d", committedDone)
	}
}

// Scenario 12: Concurrent mobile orders cannot reserve more than available physical stock.
func TestPackage03_Scenario12_ConcurrentMobileUploads_CannotOvercommitPaper(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	kioskID := "kiosk-p3-sc12"
	availableStock := 15
	sheetsPerOrder := 6
	orderCount := 5

	var wg sync.WaitGroup
	wg.Add(orderCount)

	type uploadRes struct {
		orderID string
		err     error
	}
	results := make([]uploadRes, orderCount)

	for i := 0; i < orderCount; i++ {
		idx := i
		ordID := fmt.Sprintf("ord_sc12_%d", idx)
		pin := fmt.Sprintf("120%d", idx)
		go func(id, p string, index int) {
			defer wg.Done()
			cmd := contracts.PrintJobCommand{
				JobID:          "job_" + id,
				KioskID:        kioskID,
				DownloadURL:    "https://storage.test/" + id + ".pdf",
				SHA256Checksum: "sha",
				TotalPages:     sheetsPerOrder,
				TotalSheets:    sheetsPerOrder,
				Copies:         1,
				Duplex:         contracts.DuplexSimplex,
				StartFromPage:  1,
				DisplayCode:    p,
			}
			ord := Order{
				OrderID:      id,
				Command:      cmd,
				Status:       contracts.JobStatusPendingPayment,
				TotalPages:   sheetsPerOrder,
				TotalSheets:  sheetsPerOrder,
				PricePaise:   int64(sheetsPerOrder * 200),
				CreatedAt:    now,
				PINExpiresAt: now.Add(10 * time.Minute),
			}
			err := s.CreateOrderWithPaperCheck(ctx, ord, availableStock)
			results[index] = uploadRes{orderID: id, err: err}
		}(ordID, pin, idx)
	}
	wg.Wait()

	var successCount int
	var insufficientCount int
	for _, r := range results {
		if r.err == nil {
			successCount++
		} else if errors.Is(r.err, ErrInsufficientPaper) {
			insufficientCount++
		} else {
			t.Errorf("order %s unexpected error: %v", r.orderID, r.err)
		}
	}

	// Available stock is 15 sheets, each needs 6 sheets.
	// Floor(15 / 6) = 2 orders can be accepted. Exactly 2 must succeed, exactly 3 must receive ErrInsufficientPaper.
	if successCount != 2 {
		t.Fatalf("expected exactly 2 successful mobile paper reservations, got %d", successCount)
	}
	if insufficientCount != 3 {
		t.Fatalf("expected exactly 3 insufficient paper rejections, got %d", insufficientCount)
	}

	committed, err := s.GetCommittedPaperSheets(ctx, kioskID, "", now)
	if err != nil {
		t.Fatalf("GetCommittedPaperSheets failed: %v", err)
	}
	if committed != 12 {
		t.Fatalf("expected 12 sheets committed (2 * 6), got %d", committed)
	}
}
