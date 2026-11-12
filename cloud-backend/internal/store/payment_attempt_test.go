package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestPaymentAttempt_StoreLifecycle(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "attempts_test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	orderID := "ord_att_001"
	kioskID := "kiosk-01"
	sessID := "sess_att_001"
	pricePaise := int64(1500)

	// Seed order
	cmd := contracts.PrintJobCommand{
		JobID:          "job_" + orderID,
		KioskID:        kioskID,
		DownloadURL:    "https://storage.test/doc.pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages:     2,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
		DisplayCode:    "1234",
	}
	ord := Order{
		OrderID:      orderID,
		Command:      cmd,
		Status:       contracts.JobStatusPendingPayment,
		PricePaise:   pricePaise,
		PINExpiresAt: time.Now().Add(15 * time.Minute),
	}
	if err := st.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("failed to create order: %v", err)
	}

	// Create session
	sess, err := st.CreateOrGetKioskSession(ctx, kioskID, orderID, 15*time.Minute, time.Now())
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	sessID = sess.SessionID

	// 1. Create PaymentAttempt
	attemptID := "att_test_123"
	providerOrderID := "mock_qr_test_123"
	att := PaymentAttempt{
		AttemptID:       attemptID,
		OrderID:         orderID,
		KioskID:         kioskID,
		SessionID:       sessID,
		Provider:        "mock",
		ProviderOrderID: providerOrderID,
		AmountPaise:     pricePaise,
		Currency:        "INR",
		Status:          "CREATED",
		QRPayload:       "upi://pay?pa=mock@upi&am=15.00",
		QRImageURL:      "https://example.com/qr.png",
		DemoToken:       "dmo_test_attempt_123",
		ExpiresAt:       time.Now().Add(10 * time.Minute),
	}
	if err := st.CreatePaymentAttempt(ctx, att); err != nil {
		t.Fatalf("failed to create payment attempt: %v", err)
	}

	// 2. Retrieve active attempt (unexpired)
	active, err := st.GetActivePaymentAttempt(ctx, sessID, time.Now())
	if err != nil || active == nil {
		t.Fatalf("expected active payment attempt, got %v (err=%v)", active, err)
	}
	if active.AttemptID != attemptID {
		t.Errorf("expected attempt_id %s, got %s", attemptID, active.AttemptID)
	}
	if active.AmountPaise != pricePaise {
		t.Errorf("expected amount %d, got %d", pricePaise, active.AmountPaise)
	}
	if active.QRPayload != att.QRPayload {
		t.Errorf("expected payload %s, got %s", att.QRPayload, active.QRPayload)
	}

	// 3. Retrieve when expired -> should return nil
	future := time.Now().Add(20 * time.Minute)
	expired, err := st.GetActivePaymentAttempt(ctx, sessID, future)
	if err != nil {
		t.Fatalf("unexpected error checking expired attempt: %v", err)
	}
	if expired != nil {
		t.Errorf("expected nil for expired attempt, got %v", expired)
	}

	// 4. Retrieve by provider order ID
	byProv, err := st.GetPaymentAttemptByProviderOrder(ctx, "mock", providerOrderID)
	if err != nil || byProv == nil {
		t.Fatalf("failed to get payment attempt by provider order: %v", err)
	}
	if byProv.AttemptID != attemptID {
		t.Errorf("expected attempt_id %s, got %s", attemptID, byProv.AttemptID)
	}

	// 5. Retrieve by order & session
	byOrdSess, err := st.GetPaymentAttemptByOrderAndSession(ctx, orderID, sessID)
	if err != nil || byOrdSess == nil {
		t.Fatalf("failed to get payment attempt by order and session: %v", err)
	}
	if byOrdSess.AttemptID != attemptID {
		t.Errorf("expected attempt_id %s, got %s", attemptID, byOrdSess.AttemptID)
	}

	// 5b. Retrieve by demo token, session ID, or attempt ID
	byTok, err := st.GetPaymentAttemptByToken(ctx, "dmo_test_attempt_123")
	if err != nil || byTok == nil || byTok.AttemptID != attemptID {
		t.Errorf("failed to retrieve attempt by demo_token: %v", err)
	}
	bySessTok, err := st.GetPaymentAttemptByToken(ctx, sessID)
	if err != nil || bySessTok == nil || bySessTok.AttemptID != attemptID {
		t.Errorf("failed to retrieve attempt by session_id: %v", err)
	}
	byAttTok, err := st.GetPaymentAttemptByToken(ctx, attemptID)
	if err != nil || byAttTok == nil || byAttTok.AttemptID != attemptID {
		t.Errorf("failed to retrieve attempt by attempt_id: %v", err)
	}

	// 6. Transition to PAID
	if err := st.MarkPaymentAttemptPaid(ctx, attemptID, "pay_mock_captured_999"); err != nil {
		t.Fatalf("failed to mark attempt paid: %v", err)
	}

	// After marking PAID, it should no longer be returned as CREATED active attempt
	activeAfterPaid, err := st.GetActivePaymentAttempt(ctx, sessID, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if activeAfterPaid != nil {
		t.Errorf("expected nil active attempt after being marked PAID, got %v", activeAfterPaid)
	}
}

func TestPaymentAttempt_ValidationErrors(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "attempts_val_test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	badCases := []struct {
		name string
		att  PaymentAttempt
	}{
		{"empty attempt_id", PaymentAttempt{AttemptID: "", OrderID: "o", KioskID: "k", SessionID: "s", AmountPaise: 100}},
		{"empty order_id", PaymentAttempt{AttemptID: "a", OrderID: "", KioskID: "k", SessionID: "s", AmountPaise: 100}},
		{"empty kiosk_id", PaymentAttempt{AttemptID: "a", OrderID: "o", KioskID: "", SessionID: "s", AmountPaise: 100}},
		{"empty session_id", PaymentAttempt{AttemptID: "a", OrderID: "o", KioskID: "k", SessionID: "", AmountPaise: 100}},
		{"zero amount", PaymentAttempt{AttemptID: "a", OrderID: "o", KioskID: "k", SessionID: "s", AmountPaise: 0}},
		{"negative amount", PaymentAttempt{AttemptID: "a", OrderID: "o", KioskID: "k", SessionID: "s", AmountPaise: -50}},
	}

	for _, bc := range badCases {
		t.Run(bc.name, func(t *testing.T) {
			err := st.CreatePaymentAttempt(ctx, bc.att)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", bc.name)
			}
		})
	}
}
