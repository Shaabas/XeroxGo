package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func setupTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "tx_test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, context.Background()
}

// 1. Payment record INSERT failure rolls back the transaction.
func TestCapturePayment_PaymentInsertFailure_RollsBack(t *testing.T) {
	s, ctx := setupTestStore(t)
	now := time.Now().UTC()
	kioskID := "kiosk-tx-1"
	orderID := "ord_tx_001"

	createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "1111", 1, contracts.JobStatusPendingPayment, now)
	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 10, 15*time.Minute, now)
	if err != nil {
		t.Fatalf("create session failed: %v", err)
	}

	// Inject failure on payments INSERT
	_, err = s.db.Exec(`CREATE TRIGGER fail_pay_ins BEFORE INSERT ON payments BEGIN SELECT RAISE(ABORT, 'injected payment insert failure'); END;`)
	if err != nil {
		t.Fatalf("create trigger failed: %v", err)
	}

	p := PaymentRecord{
		PaymentID:   "pay_tx_001",
		OrderID:     orderID,
		AmountPaise: 200,
		Status:      "captured",
		Provider:    "razorpay",
		SessionID:   sess.SessionID,
	}

	res, err := s.CapturePaymentAndCheckDispatch(ctx, p, now)
	if err == nil {
		t.Fatal("expected error from CapturePaymentAndCheckDispatch when payment insert fails, got nil")
	}
	if res != nil {
		t.Fatalf("expected nil result on failure, got %+v", res)
	}

	// Verify DB state: rolled back
	var payCount int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM payments WHERE payment_id = ?;`, p.PaymentID).Scan(&payCount)
	if payCount != 0 {
		t.Errorf("expected 0 payments recorded, got %d", payCount)
	}

	ord, err := s.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if ord.Status != contracts.JobStatusPendingPayment {
		t.Errorf("expected order status %s, got %s", contracts.JobStatusPendingPayment, ord.Status)
	}

	var sessStatus string
	err = s.db.QueryRow(`SELECT status FROM kiosk_sessions WHERE session_id = ?;`, sess.SessionID).Scan(&sessStatus)
	if err != nil {
		t.Fatalf("query session failed: %v", err)
	}
	if sessStatus != string(KioskSessionPaymentPending) {
		t.Errorf("expected session status %s, got %s", KioskSessionPaymentPending, sessStatus)
	}

	var intentCount int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM dispatch_intents WHERE order_id = ?;`, orderID).Scan(&intentCount)
	if intentCount != 0 {
		t.Errorf("expected 0 dispatch intents, got %d", intentCount)
	}
}

// 2. Session transition SQL failure does not authorize dispatch.
func TestCapturePayment_SessionTransitionFailure_DoesNotAuthorizeDispatch(t *testing.T) {
	s, ctx := setupTestStore(t)
	now := time.Now().UTC()
	kioskID := "kiosk-tx-2"
	orderID := "ord_tx_002"

	createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "2222", 1, contracts.JobStatusPendingPayment, now)
	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 10, 15*time.Minute, now)
	if err != nil {
		t.Fatalf("create session failed: %v", err)
	}

	// Inject failure on session UPDATE
	_, err = s.db.Exec(`CREATE TRIGGER fail_sess_upd BEFORE UPDATE ON kiosk_sessions BEGIN SELECT RAISE(ABORT, 'injected session update failure'); END;`)
	if err != nil {
		t.Fatalf("create trigger failed: %v", err)
	}

	p := PaymentRecord{
		PaymentID:   "pay_tx_002",
		OrderID:     orderID,
		AmountPaise: 200,
		Status:      "captured",
		Provider:    "razorpay",
		SessionID:   sess.SessionID,
	}

	res, err := s.CapturePaymentAndCheckDispatch(ctx, p, now)
	if err == nil {
		t.Fatal("expected error from CapturePaymentAndCheckDispatch when session update fails, got nil")
	}
	if res != nil {
		t.Fatalf("expected nil result on failure, got %+v", res)
	}

	// Verify DB state: rolled back
	ord, err := s.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if ord.Status != contracts.JobStatusPendingPayment {
		t.Errorf("expected order status %s, got %s", contracts.JobStatusPendingPayment, ord.Status)
	}

	var sessStatus string
	err = s.db.QueryRow(`SELECT status FROM kiosk_sessions WHERE session_id = ?;`, sess.SessionID).Scan(&sessStatus)
	if err != nil {
		t.Fatalf("query session failed: %v", err)
	}
	if sessStatus != string(KioskSessionPaymentPending) {
		t.Errorf("expected session status %s, got %s", KioskSessionPaymentPending, sessStatus)
	}

	var intentCount int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM dispatch_intents WHERE order_id = ?;`, orderID).Scan(&intentCount)
	if intentCount != 0 {
		t.Errorf("expected 0 dispatch intents, got %d", intentCount)
	}

	var payCount int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM payments WHERE payment_id = ?;`, p.PaymentID).Scan(&payCount)
	if payCount != 0 {
		t.Errorf("expected 0 payments recorded, got %d", payCount)
	}
}

// 3. Order transition SQL failure does not authorize dispatch.
func TestCapturePayment_OrderTransitionFailure_DoesNotAuthorizeDispatch(t *testing.T) {
	s, ctx := setupTestStore(t)
	now := time.Now().UTC()
	kioskID := "kiosk-tx-3"
	orderID := "ord_tx_003"

	createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "3333", 1, contracts.JobStatusPendingPayment, now)
	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 10, 15*time.Minute, now)
	if err != nil {
		t.Fatalf("create session failed: %v", err)
	}

	// Inject failure on order transition to DISPATCHED
	_, err = s.db.Exec(`CREATE TRIGGER fail_ord_disp BEFORE UPDATE OF status ON orders WHEN NEW.status = 'DISPATCHED' BEGIN SELECT RAISE(ABORT, 'injected order dispatch failure'); END;`)
	if err != nil {
		t.Fatalf("create trigger failed: %v", err)
	}

	p := PaymentRecord{
		PaymentID:   "pay_tx_003",
		OrderID:     orderID,
		AmountPaise: 200,
		Status:      "captured",
		Provider:    "razorpay",
		SessionID:   sess.SessionID,
	}

	res, err := s.CapturePaymentAndCheckDispatch(ctx, p, now)
	if err == nil {
		t.Fatal("expected error from CapturePaymentAndCheckDispatch when order transition fails, got nil")
	}
	if res != nil {
		t.Fatalf("expected nil result on failure, got %+v", res)
	}

	// Verify DB state
	ord, err := s.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if ord.Status != contracts.JobStatusPendingPayment {
		t.Errorf("expected order status %s, got %s", contracts.JobStatusPendingPayment, ord.Status)
	}

	var sessStatus string
	_ = s.db.QueryRow(`SELECT status FROM kiosk_sessions WHERE session_id = ?;`, sess.SessionID).Scan(&sessStatus)
	if sessStatus != string(KioskSessionPaymentPending) {
		t.Errorf("expected session status %s, got %s", KioskSessionPaymentPending, sessStatus)
	}

	var intentCount int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM dispatch_intents WHERE order_id = ?;`, orderID).Scan(&intentCount)
	if intentCount != 0 {
		t.Errorf("expected 0 dispatch intents, got %d", intentCount)
	}
}

// 4. Dispatch-intent DELETE failure rolls back payment/session/order changes.
func TestCapturePayment_DispatchIntentDeleteFailure_RollsBack(t *testing.T) {
	s, ctx := setupTestStore(t)
	now := time.Now().UTC()
	kioskID := "kiosk-tx-4"
	orderID := "ord_tx_004"

	createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "4444", 1, contracts.JobStatusPendingPayment, now)
	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 10, 15*time.Minute, now)
	if err != nil {
		t.Fatalf("create session failed: %v", err)
	}

	// Pre-populate a canceled dispatch intent for this order
	nowNS := now.UnixNano()
	_, err = s.db.Exec(`INSERT INTO dispatch_intents (order_id, job_id, kiosk_id, session_id, status, created_at_ns, updated_at_ns) VALUES (?, ?, ?, ?, 'CANCELED', ?, ?);`,
		orderID, "job_"+orderID, kioskID, "old_sess", nowNS, nowNS)
	if err != nil {
		t.Fatalf("insert old dispatch intent failed: %v", err)
	}

	// Inject failure on dispatch_intents DELETE
	_, err = s.db.Exec(`CREATE TRIGGER fail_disp_del BEFORE DELETE ON dispatch_intents BEGIN SELECT RAISE(ABORT, 'injected dispatch delete failure'); END;`)
	if err != nil {
		t.Fatalf("create trigger failed: %v", err)
	}

	p := PaymentRecord{
		PaymentID:   "pay_tx_004",
		OrderID:     orderID,
		AmountPaise: 200,
		Status:      "captured",
		Provider:    "razorpay",
		SessionID:   sess.SessionID,
	}

	res, err := s.CapturePaymentAndCheckDispatch(ctx, p, now)
	if err == nil {
		t.Fatal("expected error from CapturePaymentAndCheckDispatch when dispatch intent delete fails, got nil")
	}
	if res != nil {
		t.Fatalf("expected nil result on failure, got %+v", res)
	}

	// Verify DB state: pre-existing intent was NOT deleted, order and session unchanged
	var intentStatus string
	err = s.db.QueryRow(`SELECT status FROM dispatch_intents WHERE order_id = ?;`, orderID).Scan(&intentStatus)
	if err != nil {
		t.Fatalf("query dispatch intent failed: %v", err)
	}
	if intentStatus != "CANCELED" {
		t.Errorf("expected intent status CANCELED, got %s", intentStatus)
	}

	ord, err := s.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if ord.Status != contracts.JobStatusPendingPayment {
		t.Errorf("expected order status %s, got %s", contracts.JobStatusPendingPayment, ord.Status)
	}

	var sessStatus string
	_ = s.db.QueryRow(`SELECT status FROM kiosk_sessions WHERE session_id = ?;`, sess.SessionID).Scan(&sessStatus)
	if sessStatus != string(KioskSessionPaymentPending) {
		t.Errorf("expected session status %s, got %s", KioskSessionPaymentPending, sessStatus)
	}

	var payCount int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM payments WHERE payment_id = ?;`, p.PaymentID).Scan(&payCount)
	if payCount != 0 {
		t.Errorf("expected 0 payments recorded, got %d", payCount)
	}
}

// 5. Dispatch-intent INSERT failure rolls back payment/session/order changes.
func TestCapturePayment_DispatchIntentInsertFailure_RollsBack(t *testing.T) {
	s, ctx := setupTestStore(t)
	now := time.Now().UTC()
	kioskID := "kiosk-tx-5"
	orderID := "ord_tx_005"

	createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "5555", 1, contracts.JobStatusPendingPayment, now)
	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 10, 15*time.Minute, now)
	if err != nil {
		t.Fatalf("create session failed: %v", err)
	}

	// Inject failure on dispatch_intents INSERT
	_, err = s.db.Exec(`CREATE TRIGGER fail_disp_ins BEFORE INSERT ON dispatch_intents BEGIN SELECT RAISE(ABORT, 'injected dispatch insert failure'); END;`)
	if err != nil {
		t.Fatalf("create trigger failed: %v", err)
	}

	p := PaymentRecord{
		PaymentID:   "pay_tx_005",
		OrderID:     orderID,
		AmountPaise: 200,
		Status:      "captured",
		Provider:    "razorpay",
		SessionID:   sess.SessionID,
	}

	res, err := s.CapturePaymentAndCheckDispatch(ctx, p, now)
	if err == nil {
		t.Fatal("expected error from CapturePaymentAndCheckDispatch when dispatch intent insert fails, got nil")
	}
	if res != nil {
		t.Fatalf("expected nil result on failure, got %+v", res)
	}

	// Verify DB state
	ord, err := s.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if ord.Status != contracts.JobStatusPendingPayment {
		t.Errorf("expected order status %s, got %s", contracts.JobStatusPendingPayment, ord.Status)
	}

	var sessStatus string
	_ = s.db.QueryRow(`SELECT status FROM kiosk_sessions WHERE session_id = ?;`, sess.SessionID).Scan(&sessStatus)
	if sessStatus != string(KioskSessionPaymentPending) {
		t.Errorf("expected session status %s, got %s", KioskSessionPaymentPending, sessStatus)
	}

	var intentCount int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM dispatch_intents WHERE order_id = ?;`, orderID).Scan(&intentCount)
	if intentCount != 0 {
		t.Errorf("expected 0 dispatch intents, got %d", intentCount)
	}

	var payCount int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM payments WHERE payment_id = ?;`, p.PaymentID).Scan(&payCount)
	if payCount != 0 {
		t.Errorf("expected 0 payments recorded, got %d", payCount)
	}
}

// 6. Payment-attempt UPDATE failure is not silently ignored.
func TestCapturePayment_PaymentAttemptUpdateFailure_NotSilentlyIgnored(t *testing.T) {
	s, ctx := setupTestStore(t)
	now := time.Now().UTC()
	kioskID := "kiosk-tx-6"
	orderID := "ord_tx_006"

	createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "6666", 1, contracts.JobStatusPendingPayment, now)
	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 10, 15*time.Minute, now)
	if err != nil {
		t.Fatalf("create session failed: %v", err)
	}

	att := PaymentAttempt{
		AttemptID:   "att_tx_006",
		OrderID:     orderID,
		KioskID:     kioskID,
		SessionID:   sess.SessionID,
		AmountPaise: 200,
		Status:      "CREATED",
		ExpiresAt:   now.Add(15 * time.Minute),
	}
	if err := s.CreatePaymentAttempt(ctx, att); err != nil {
		t.Fatalf("CreatePaymentAttempt failed: %v", err)
	}

	// Inject failure on payment_attempts UPDATE
	_, err = s.db.Exec(`CREATE TRIGGER fail_att_upd BEFORE UPDATE ON payment_attempts BEGIN SELECT RAISE(ABORT, 'injected attempt update failure'); END;`)
	if err != nil {
		t.Fatalf("create trigger failed: %v", err)
	}

	p := PaymentRecord{
		PaymentID:   "pay_tx_006",
		OrderID:     orderID,
		AmountPaise: 200,
		Status:      "captured",
		Provider:    "razorpay",
		SessionID:   sess.SessionID,
	}

	res, err := s.CapturePaymentAndCheckDispatch(ctx, p, now)
	if err == nil {
		t.Fatal("expected error from CapturePaymentAndCheckDispatch when payment attempt update fails, got nil")
	}
	if res != nil {
		t.Fatalf("expected nil result on failure, got %+v", res)
	}

	// Verify DB state: attempt was NOT updated to PAID
	var attStatus string
	err = s.db.QueryRow(`SELECT status FROM payment_attempts WHERE attempt_id = ?;`, att.AttemptID).Scan(&attStatus)
	if err != nil {
		t.Fatalf("query attempt failed: %v", err)
	}
	if attStatus != "CREATED" {
		t.Errorf("expected attempt status CREATED, got %s", attStatus)
	}

	ord, err := s.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if ord.Status != contracts.JobStatusPendingPayment {
		t.Errorf("expected order status %s, got %s", contracts.JobStatusPendingPayment, ord.Status)
	}
}

// 7. Successful payment with an active authorized session creates exactly one durable dispatch intent.
func TestCapturePayment_SuccessfulWithActiveSession_CreatesOneIntent(t *testing.T) {
	s, ctx := setupTestStore(t)
	now := time.Now().UTC()
	kioskID := "kiosk-tx-7"
	orderID := "ord_tx_007"

	createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "7777", 1, contracts.JobStatusPendingPayment, now)
	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 10, 15*time.Minute, now)
	if err != nil {
		t.Fatalf("create session failed: %v", err)
	}

	att := PaymentAttempt{
		AttemptID:   "att_tx_007",
		OrderID:     orderID,
		KioskID:     kioskID,
		SessionID:   sess.SessionID,
		AmountPaise: 200,
		Status:      "CREATED",
		ExpiresAt:   now.Add(15 * time.Minute),
	}
	if err := s.CreatePaymentAttempt(ctx, att); err != nil {
		t.Fatalf("CreatePaymentAttempt failed: %v", err)
	}

	p := PaymentRecord{
		PaymentID:   "pay_tx_007",
		OrderID:     orderID,
		AmountPaise: 200,
		Status:      "captured",
		Provider:    "razorpay",
		SessionID:   sess.SessionID,
	}

	res, err := s.CapturePaymentAndCheckDispatch(ctx, p, now)
	if err != nil {
		t.Fatalf("CapturePaymentAndCheckDispatch failed: %v", err)
	}
	if !res.DispatchEligible {
		t.Error("expected DispatchEligible to be true")
	}
	if res.OrderStatus != contracts.JobStatusDispatched {
		t.Errorf("expected OrderStatus DISPATCHED, got %s", res.OrderStatus)
	}

	// Verify DB state
	ord, err := s.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if ord.Status != contracts.JobStatusDispatched {
		t.Errorf("expected order status %s, got %s", contracts.JobStatusDispatched, ord.Status)
	}

	var sessStatus string
	_ = s.db.QueryRow(`SELECT status FROM kiosk_sessions WHERE session_id = ?;`, sess.SessionID).Scan(&sessStatus)
	if sessStatus != string(KioskSessionCompleted) {
		t.Errorf("expected session status COMPLETED, got %s", sessStatus)
	}

	var intentCount int
	var intentStatus, intentKiosk, intentSession string
	err = s.db.QueryRow(`SELECT status, kiosk_id, session_id FROM dispatch_intents WHERE order_id = ?;`, orderID).Scan(&intentStatus, &intentKiosk, &intentSession)
	if err != nil {
		t.Fatalf("query dispatch intent failed: %v", err)
	}
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM dispatch_intents WHERE order_id = ?;`, orderID).Scan(&intentCount)
	if intentCount != 1 {
		t.Errorf("expected exactly 1 dispatch intent, got %d", intentCount)
	}
	if intentStatus != "PENDING" {
		t.Errorf("expected intent status PENDING, got %s", intentStatus)
	}
	if intentKiosk != kioskID || intentSession != sess.SessionID {
		t.Errorf("expected intent kiosk %s and session %s, got %s, %s", kioskID, sess.SessionID, intentKiosk, intentSession)
	}

	var attStatus string
	_ = s.db.QueryRow(`SELECT status FROM payment_attempts WHERE attempt_id = ?;`, att.AttemptID).Scan(&attStatus)
	if attStatus != "PAID" {
		t.Errorf("expected attempt status PAID, got %s", attStatus)
	}
}

// 8. Payment without an active session remains PAID_WAITING.
func TestCapturePayment_WithoutActiveSession_RemainsPaidWaiting(t *testing.T) {
	s, ctx := setupTestStore(t)
	now := time.Now().UTC()
	kioskID := "kiosk-tx-8"
	orderID := "ord_tx_008"

	createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "8888", 1, contracts.JobStatusPendingPayment, now)

	// No session created
	p := PaymentRecord{
		PaymentID:   "pay_tx_008",
		OrderID:     orderID,
		AmountPaise: 200,
		Status:      "captured",
		Provider:    "razorpay",
		SessionID:   "",
	}

	res, err := s.CapturePaymentAndCheckDispatch(ctx, p, now)
	if err != nil {
		t.Fatalf("CapturePaymentAndCheckDispatch failed: %v", err)
	}
	if res.DispatchEligible {
		t.Error("expected DispatchEligible to be false without active session")
	}
	if res.OrderStatus != contracts.JobStatusPaidWaiting {
		t.Errorf("expected OrderStatus PAID_WAITING, got %s", res.OrderStatus)
	}

	// Verify DB state
	ord, err := s.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if ord.Status != contracts.JobStatusPaidWaiting {
		t.Errorf("expected order status %s, got %s", contracts.JobStatusPaidWaiting, ord.Status)
	}

	var intentCount int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM dispatch_intents WHERE order_id = ?;`, orderID).Scan(&intentCount)
	if intentCount != 0 {
		t.Errorf("expected 0 dispatch intents without active session, got %d", intentCount)
	}

	var payCount int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM payments WHERE payment_id = ?;`, p.PaymentID).Scan(&payCount)
	if payCount != 1 {
		t.Errorf("expected 1 payment recorded, got %d", payCount)
	}
}

// 9. Duplicate payment delivery never creates duplicate dispatch.
func TestCapturePayment_DuplicateDelivery_NeverCreatesDuplicateDispatch(t *testing.T) {
	s, ctx := setupTestStore(t)
	now := time.Now().UTC()
	kioskID := "kiosk-tx-9"
	orderID := "ord_tx_009"

	createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "9999", 1, contracts.JobStatusPendingPayment, now)
	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 10, 15*time.Minute, now)
	if err != nil {
		t.Fatalf("create session failed: %v", err)
	}

	p := PaymentRecord{
		PaymentID:   "pay_tx_009",
		OrderID:     orderID,
		AmountPaise: 200,
		Status:      "captured",
		Provider:    "razorpay",
		SessionID:   sess.SessionID,
	}

	// First payment delivery -> dispatches
	res1, err := s.CapturePaymentAndCheckDispatch(ctx, p, now)
	if err != nil {
		t.Fatalf("first capture failed: %v", err)
	}
	if !res1.DispatchEligible {
		t.Fatal("expected first capture to be dispatch eligible")
	}

	// Second payment delivery with same payment ID -> MUST NOT dispatch
	res2, err := s.CapturePaymentAndCheckDispatch(ctx, p, now)
	if err != nil {
		t.Fatalf("second capture failed: %v", err)
	}
	if res2.DispatchEligible {
		t.Error("duplicate payment must NOT be dispatch eligible")
	}
	if !res2.AlreadyProcessed {
		t.Error("expected AlreadyProcessed=true on duplicate payment")
	}

	// Verify dispatch intents table still has exactly 1 intent
	var intentCount int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM dispatch_intents WHERE order_id = ?;`, orderID).Scan(&intentCount)
	if intentCount != 1 {
		t.Errorf("expected exactly 1 dispatch intent, got %d", intentCount)
	}
}

// 10. ReleasePaidOrder() rolls back when dispatch-intent deletion fails.
func TestReleasePaidOrder_DispatchIntentDeleteFailure_RollsBack(t *testing.T) {
	s, ctx := setupTestStore(t)
	now := time.Now().UTC()
	kioskID := "kiosk-tx-10"
	orderID := "ord_tx_010"

	createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "1010", 1, contracts.JobStatusPaidWaiting, now)
	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 10, 15*time.Minute, now)
	if err != nil {
		t.Fatalf("create session failed: %v", err)
	}

	// Pre-populate a canceled dispatch intent for this order
	nowNS := now.UnixNano()
	_, err = s.db.Exec(`INSERT INTO dispatch_intents (order_id, job_id, kiosk_id, session_id, status, created_at_ns, updated_at_ns) VALUES (?, ?, ?, ?, 'CANCELED', ?, ?);`,
		orderID, "job_"+orderID, kioskID, "old_sess", nowNS, nowNS)
	if err != nil {
		t.Fatalf("insert old dispatch intent failed: %v", err)
	}

	// Inject failure on dispatch_intents DELETE
	_, err = s.db.Exec(`CREATE TRIGGER fail_rel_del BEFORE DELETE ON dispatch_intents BEGIN SELECT RAISE(ABORT, 'injected release delete failure'); END;`)
	if err != nil {
		t.Fatalf("create trigger failed: %v", err)
	}

	released, err := s.ReleasePaidOrder(ctx, kioskID, orderID, sess.SessionID, now)
	if err == nil {
		t.Fatal("expected error from ReleasePaidOrder when dispatch intent delete fails, got nil")
	}
	if released != nil {
		t.Fatalf("expected nil released order, got %+v", released)
	}

	// Verify DB state: order remains PAID_WAITING, session remains PAYMENT_PENDING, intent remains CANCELED
	ord, err := s.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if ord.Status != contracts.JobStatusPaidWaiting {
		t.Errorf("expected order status %s, got %s", contracts.JobStatusPaidWaiting, ord.Status)
	}

	var sessStatus string
	_ = s.db.QueryRow(`SELECT status FROM kiosk_sessions WHERE session_id = ?;`, sess.SessionID).Scan(&sessStatus)
	if sessStatus != string(KioskSessionPaymentPending) {
		t.Errorf("expected session status %s, got %s", KioskSessionPaymentPending, sessStatus)
	}

	var intentStatus string
	err = s.db.QueryRow(`SELECT status FROM dispatch_intents WHERE order_id = ?;`, orderID).Scan(&intentStatus)
	if err != nil {
		t.Fatalf("query dispatch intent failed: %v", err)
	}
	if intentStatus != "CANCELED" {
		t.Errorf("expected intent status CANCELED, got %s", intentStatus)
	}
}

// 11. ReleasePaidOrder() succeeds and creates PENDING intent.
func TestReleasePaidOrder_SuccessfulRelease_CreatesPendingIntent(t *testing.T) {
	s, ctx := setupTestStore(t)
	now := time.Now().UTC()
	kioskID := "kiosk-tx-11"
	orderID := "ord_tx_011"

	createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "1111", 1, contracts.JobStatusPaidWaiting, now)
	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 10, 15*time.Minute, now)
	if err != nil {
		t.Fatalf("create session failed: %v", err)
	}

	released, err := s.ReleasePaidOrder(ctx, kioskID, orderID, sess.SessionID, now)
	if err != nil {
		t.Fatalf("ReleasePaidOrder failed: %v", err)
	}
	if released.Status != contracts.JobStatusDispatched {
		t.Errorf("expected status DISPATCHED, got %s", released.Status)
	}

	// Verify DB state
	ord, err := s.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if ord.Status != contracts.JobStatusDispatched {
		t.Errorf("expected order status DISPATCHED, got %s", ord.Status)
	}

	var sessStatus string
	_ = s.db.QueryRow(`SELECT status FROM kiosk_sessions WHERE session_id = ?;`, sess.SessionID).Scan(&sessStatus)
	if sessStatus != string(KioskSessionCompleted) {
		t.Errorf("expected session status COMPLETED, got %s", sessStatus)
	}

	var intentStatus, intentKiosk, intentSession string
	err = s.db.QueryRow(`SELECT status, kiosk_id, session_id FROM dispatch_intents WHERE order_id = ?;`, orderID).Scan(&intentStatus, &intentKiosk, &intentSession)
	if err != nil {
		t.Fatalf("query dispatch intent failed: %v", err)
	}
	if intentStatus != "PENDING" {
		t.Errorf("expected intent status PENDING, got %s", intentStatus)
	}
	if intentKiosk != kioskID || intentSession != sess.SessionID {
		t.Errorf("expected intent kiosk %s and session %s, got %s, %s", kioskID, sess.SessionID, intentKiosk, intentSession)
	}
}

// 12. ReleasePaidOrder() refuses to delete or overwrite an acknowledged or completed dispatch intent.
func TestReleasePaidOrder_AcknowledgedOrCompletedIntent_RejectsRelease(t *testing.T) {
	for _, status := range []string{"ACKNOWLEDGED", "COMPLETED"} {
		status := status
		t.Run(status, func(t *testing.T) {
			s, ctx := setupTestStore(t)
			now := time.Now().UTC()
			kioskID := "kiosk-tx-12"
			orderID := "ord_tx_012_" + status

			createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "1212", 1, contracts.JobStatusPaidWaiting, now)
			sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 10, 15*time.Minute, now)
			if err != nil {
				t.Fatalf("create session failed: %v", err)
			}

			// Pre-populate an ACKNOWLEDGED or COMPLETED dispatch intent
			nowNS := now.UnixNano()
			_, err = s.db.Exec(`INSERT INTO dispatch_intents (order_id, job_id, kiosk_id, session_id, status, created_at_ns, updated_at_ns) VALUES (?, ?, ?, ?, ?, ?, ?);`,
				orderID, "job_"+orderID, kioskID, "ack_sess", status, nowNS, nowNS)
			if err != nil {
				t.Fatalf("insert dispatch intent failed: %v", err)
			}

			released, err := s.ReleasePaidOrder(ctx, kioskID, orderID, sess.SessionID, now)
			if err == nil {
				t.Fatalf("expected error when releasing order with %s dispatch intent, got nil", status)
			}
			if released != nil {
				t.Fatalf("expected nil released order, got %+v", released)
			}

			// Verify DB state: order remains PAID_WAITING, session remains PAYMENT_PENDING, intent status unchanged
			ord, err := s.GetOrder(ctx, orderID)
			if err != nil {
				t.Fatalf("GetOrder failed: %v", err)
			}
			if ord.Status != contracts.JobStatusPaidWaiting {
				t.Errorf("expected order status %s, got %s", contracts.JobStatusPaidWaiting, ord.Status)
			}

			var sessStatus string
			_ = s.db.QueryRow(`SELECT status FROM kiosk_sessions WHERE session_id = ?;`, sess.SessionID).Scan(&sessStatus)
			if sessStatus != string(KioskSessionPaymentPending) {
				t.Errorf("expected session status %s, got %s", KioskSessionPaymentPending, sessStatus)
			}

			var currentIntentStatus string
			err = s.db.QueryRow(`SELECT status FROM dispatch_intents WHERE order_id = ?;`, orderID).Scan(&currentIntentStatus)
			if err != nil {
				t.Fatalf("query dispatch intent failed: %v", err)
			}
			if currentIntentStatus != status {
				t.Errorf("expected intent status %s to be preserved, got %s", status, currentIntentStatus)
			}
		})
	}
}

// 13. Already-dispatched branch: payment insert failure rolls back before commit.
func TestCapturePayment_AlreadyDispatched_PaymentInsertFailure_RollsBack(t *testing.T) {
	s, ctx := setupTestStore(t)
	now := time.Now().UTC()
	kioskID := "kiosk-tx-13"
	orderID := "ord_tx_013"

	createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "1313", 1, contracts.JobStatusDispatched, now)

	// Inject failure on payments INSERT
	_, err := s.db.Exec(`CREATE TRIGGER fail_pay_ins_disp BEFORE INSERT ON payments BEGIN SELECT RAISE(ABORT, 'injected payment insert failure on dispatched'); END;`)
	if err != nil {
		t.Fatalf("create trigger failed: %v", err)
	}

	p := PaymentRecord{
		PaymentID:   "pay_tx_013",
		OrderID:     orderID,
		AmountPaise: 200,
		Status:      "captured",
		Provider:    "razorpay",
	}

	res, err := s.CapturePaymentAndCheckDispatch(ctx, p, now)
	if err == nil {
		t.Fatal("expected error from CapturePaymentAndCheckDispatch when payment insert fails on dispatched order, got nil")
	}
	if res != nil {
		t.Fatalf("expected nil result on failure, got %+v", res)
	}

	// Verify payment not recorded
	var payCount int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM payments WHERE payment_id = ?;`, p.PaymentID).Scan(&payCount)
	if payCount != 0 {
		t.Errorf("expected 0 payments recorded, got %d", payCount)
	}
}
