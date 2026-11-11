package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestStore_KioskSessionLifecycle(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "session_test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	now := time.Now().UTC()

	// 1. Create a test order
	cmd := contracts.PrintJobCommand{
		JobID:          "job_sess_001",
		KioskID:        "kiosk-sess-1",
		DownloadURL:    "https://storage.test/doc.pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages:     4,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
		DisplayCode:    "1234",
	}
	ord := Order{
		OrderID:      "ord_sess_001",
		Command:      cmd,
		Status:       contracts.JobStatusPendingPayment,
		PricePaise:   1500,
		PINExpiresAt: now.Add(15 * time.Minute),
	}
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("failed to create order: %v", err)
	}

	// 2. Claim order from kiosk-sess-1
	sess, err := s.CreateOrGetKioskSession(ctx, "kiosk-sess-1", "ord_sess_001", 5*time.Minute, now)
	if err != nil {
		t.Fatalf("failed to create kiosk session: %v", err)
	}
	if sess.SessionID == "" {
		t.Errorf("expected non-empty session ID")
	}
	if sess.Status != KioskSessionPaymentPending {
		t.Errorf("status = %s, want %s", sess.Status, KioskSessionPaymentPending)
	}
	if sess.PricePaise != 1500 {
		t.Errorf("price = %d, want 1500", sess.PricePaise)
	}

	// 3. Idempotent claim from same kiosk returns same active session
	sess2, err := s.CreateOrGetKioskSession(ctx, "kiosk-sess-1", "ord_sess_001", 5*time.Minute, now.Add(30*time.Second))
	if err != nil {
		t.Fatalf("expected idempotent claim to succeed, got %v", err)
	}
	if sess2.SessionID != sess.SessionID {
		t.Errorf("idempotent session ID = %s, want %s", sess2.SessionID, sess.SessionID)
	}

	// 4. Claim from another kiosk fails with ErrOrderAlreadyClaimed
	_, err = s.CreateOrGetKioskSession(ctx, "kiosk-sess-2", "ord_sess_001", 5*time.Minute, now.Add(1*time.Minute))
	if !errors.Is(err, ErrOrderAlreadyClaimed) {
		t.Errorf("expected ErrOrderAlreadyClaimed, got %v", err)
	}

	// 5. GetKioskSession retrieves session
	retrieved, err := s.GetKioskSession(ctx, sess.SessionID, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("failed to get kiosk session: %v", err)
	}
	if retrieved.SessionID != sess.SessionID {
		t.Errorf("retrieved ID = %s, want %s", retrieved.SessionID, sess.SessionID)
	}

	// 6. CompleteKioskSession marks session COMPLETED
	if err := s.CompleteKioskSession(ctx, "ord_sess_001", now.Add(3*time.Minute)); err != nil {
		t.Fatalf("failed to complete kiosk session: %v", err)
	}
	completed, err := s.GetKioskSession(ctx, sess.SessionID, now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("failed to get completed session: %v", err)
	}
	if completed.Status != KioskSessionCompleted {
		t.Errorf("status = %s, want %s", completed.Status, KioskSessionCompleted)
	}
}

func TestStore_KioskSessionCancellationAndReclaim(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "cancel_test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	now := time.Now().UTC()

	ord := Order{
		OrderID: "ord_cancel_001",
		Command: contracts.PrintJobCommand{
			JobID:          "job_cancel_001",
			KioskID:        "kiosk-can",
			DownloadURL:    "https://storage.test/doc.pdf",
			SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			TotalPages:     2,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
			DisplayCode:    "5678",
		},
		Status:       contracts.JobStatusPendingPayment,
		PricePaise:   800,
		PINExpiresAt: now.Add(15 * time.Minute),
	}
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("failed to create order: %v", err)
	}

	sess, err := s.CreateOrGetKioskSession(ctx, "kiosk-can", "ord_cancel_001", 5*time.Minute, now)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	// Cancel session
	if err := s.CancelKioskSession(ctx, sess.SessionID, "kiosk-can", now.Add(1*time.Minute)); err != nil {
		t.Fatalf("failed to cancel session: %v", err)
	}

	canceled, err := s.GetKioskSession(ctx, sess.SessionID, now.Add(1*time.Minute))
	if err != nil {
		t.Fatalf("failed to get canceled session: %v", err)
	}
	if canceled.Status != KioskSessionCanceled {
		t.Errorf("status = %s, want %s", canceled.Status, KioskSessionCanceled)
	}

	// Now that it's canceled, a new claim can succeed
	sessNew, err := s.CreateOrGetKioskSession(ctx, "kiosk-can", "ord_cancel_001", 5*time.Minute, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("failed to re-claim order after cancellation: %v", err)
	}
	if sessNew.SessionID == sess.SessionID {
		t.Errorf("expected new session ID after cancel, got same: %s", sessNew.SessionID)
	}
	if sessNew.Status != KioskSessionPaymentPending {
		t.Errorf("new session status = %s, want %s", sessNew.Status, KioskSessionPaymentPending)
	}
}

func TestStore_KioskSessionExpiration(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "expiry_test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	now := time.Now().UTC()

	ord := Order{
		OrderID: "ord_exp_001",
		Command: contracts.PrintJobCommand{
			JobID:          "job_exp_001",
			KioskID:        "kiosk-exp",
			DownloadURL:    "https://storage.test/doc.pdf",
			SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
			DisplayCode:    "9900",
		},
		Status:       contracts.JobStatusPendingPayment,
		PricePaise:   500,
		PINExpiresAt: now.Add(10 * time.Minute),
	}
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("failed to create order: %v", err)
	}

	// Create session with 2 minute expiry
	sess, err := s.CreateOrGetKioskSession(ctx, "kiosk-exp", "ord_exp_001", 2*time.Minute, now)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	// Before expiry -> still PAYMENT_PENDING
	active, err := s.GetKioskSession(ctx, sess.SessionID, now.Add(1*time.Minute))
	if err != nil || active.Status != KioskSessionPaymentPending {
		t.Errorf("expected active before expiry, got %v (%s)", err, active.Status)
	}

	// After expiry (at 3 minutes) -> automatically marked EXPIRED
	expired, err := s.GetKioskSession(ctx, sess.SessionID, now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("failed to get session after expiry: %v", err)
	}
	if expired.Status != KioskSessionExpired {
		t.Errorf("status = %s, want %s", expired.Status, KioskSessionExpired)
	}

	// Re-claiming after expiry creates a new active session
	sess2, err := s.CreateOrGetKioskSession(ctx, "kiosk-exp", "ord_exp_001", 2*time.Minute, now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("failed to claim after expiry: %v", err)
	}
	if sess2.SessionID == sess.SessionID {
		t.Errorf("expected new session ID after expiry, got same %s", sess2.SessionID)
	}
}

func TestStore_ConcurrentKioskClaims(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "concurrent_claim_test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	now := time.Now().UTC()

	ord := Order{
		OrderID: "ord_race_001",
		Command: contracts.PrintJobCommand{
			JobID:          "job_race_001",
			KioskID:        "kiosk-race-1",
			DownloadURL:    "https://storage.test/doc.pdf",
			SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			TotalPages:     2,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
			DisplayCode:    "4455",
		},
		Status:       contracts.JobStatusPendingPayment,
		PricePaise:   1000,
		PINExpiresAt: now.Add(15 * time.Minute),
	}
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("failed to create order: %v", err)
	}

	// Concurrently attempt to claim order from kiosk-race-1 and other kiosks
	concurrency := 10
	var wg sync.WaitGroup
	wg.Add(concurrency)

	type claimResult struct {
		kioskID string
		sess    *KioskSession
		err     error
	}
	results := make([]claimResult, concurrency)

	for i := 0; i < concurrency; i++ {
		idx := i
		kid := "kiosk-race-1"
		if idx%2 == 1 {
			kid = "kiosk-race-2" // Mismatch / competing kiosk
		}
		go func() {
			defer wg.Done()
			sess, err := s.CreateOrGetKioskSession(ctx, kid, "ord_race_001", 5*time.Minute, now)
			results[idx] = claimResult{kioskID: kid, sess: sess, err: err}
		}()
	}
	wg.Wait()

	// All competing kiosk-race-2 attempts must fail with ErrKioskMismatch or ErrOrderAlreadyClaimed!
	var successfulClaims []*KioskSession
	for _, res := range results {
		if res.err == nil {
			successfulClaims = append(successfulClaims, res.sess)
		} else {
			if !errors.Is(res.err, ErrKioskMismatch) && !errors.Is(res.err, ErrOrderAlreadyClaimed) {
				t.Errorf("unexpected error on concurrent claim: %v", res.err)
			}
		}
	}

	// At least one claim succeeded, and all successful claims must share the EXACT SAME session ID!
	if len(successfulClaims) == 0 {
		t.Fatalf("expected at least 1 successful claim for kiosk-race-1")
	}
	firstSessID := successfulClaims[0].SessionID
	for _, sc := range successfulClaims {
		if sc.SessionID != firstSessID {
			t.Errorf("concurrent claims produced divergent sessions: %s vs %s", sc.SessionID, firstSessID)
		}
	}
}

