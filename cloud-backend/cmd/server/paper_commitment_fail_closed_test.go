package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

// TestPackage03_InitiatePayment_PaperCommitmentQueryError_FailsClosed asserts that
// payment initiation rejects requests and does NOT proceed or create a payment attempt
// when GetCommittedPaperSheets fails (e.g. database error).
func TestPackage03_InitiatePayment_PaperCommitmentQueryError_FailsClosed(t *testing.T) {
	_, srv, st, secret := setupPaperTestServer(t)
	kioskID := "kiosk_fail_closed_test"

	// 1. Connect kiosk with healthy printer and ample paper stock (500 sheets)
	_, cancelKiosk := connectKioskWithHealth(t, srv.URL, kioskID, contracts.PrinterHealth{
		Online:          true,
		State:           "IDLE",
		PaperTrayStatus: "OK",
		EstimatedSheets: 500,
		UpdatedAt:       time.Now().UTC(),
	})
	defer cancelKiosk()

	ctx := context.Background()
	now := time.Now().UTC()

	// 2. Create order needing 10 sheets
	orderID := "ord_p3_fail_closed_1"
	seedTestOrder(t, st, orderID, kioskID, "1234", 10, contracts.JobStatusPendingPayment, 500, now.Add(10*time.Minute))

	// 3. Create active session for the order
	sess, err := st.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 500, 120*time.Second, now)
	if err != nil {
		t.Fatalf("failed to create kiosk session: %v", err)
	}

	// 4. Inject simulated database failure on GetCommittedPaperSheets
	st.FaultInjectCommitmentQuery = errors.New("simulated database failure on paper commitment query")
	defer func() { st.FaultInjectCommitmentQuery = nil }()

	// 5. Initiate payment via canonical session-scoped HTTP API
	payURL := srv.URL + "/api/k/" + kioskID + "/sessions/" + sess.SessionID + "/initiate-payment"
	req, err := http.NewRequest(http.MethodPost, payURL, nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// 6. Must fail closed: HTTP 500 with DATABASE_ERROR
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 InternalServerError when paper commitment query fails, got: %d", resp.StatusCode)
	}

	var errResp map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp["code"] != "DATABASE_ERROR" {
		t.Fatalf("expected error code DATABASE_ERROR, got: %s", errResp["code"])
	}
	if errResp["error"] != "failed to verify paper commitments" {
		t.Fatalf("expected error message 'failed to verify paper commitments', got: %s", errResp["error"])
	}

	// 7. Verify no payment attempt was created in database
	attempt, err := st.GetActivePaymentAttempt(ctx, sess.SessionID, now)
	if err != nil {
		t.Fatalf("GetActivePaymentAttempt failed: %v", err)
	}
	if attempt != nil {
		t.Fatalf("expected no active payment attempt to be created on paper query error, got: %v", attempt)
	}

	// 8. Clear fault injection: subsequent payment initiation must succeed
	st.FaultInjectCommitmentQuery = nil

	req2, _ := http.NewRequest(http.MethodPost, payURL, nil)
	req2.Header.Set("Authorization", "Bearer "+secret)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 OK after fault injection cleared, got: %d", resp2.StatusCode)
	}

	attempt2, err := st.GetActivePaymentAttempt(ctx, sess.SessionID, now)
	if err != nil || attempt2 == nil {
		t.Fatalf("expected active payment attempt to be created successfully after fault cleared, got: %v (err=%v)", attempt2, err)
	}
}
