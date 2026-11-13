package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

// Helper to open a test store: uses XEROXGO_TEST_POSTGRES_URL / POSTGRES_TEST_URL if provided, else temporary SQLite.
// Production environment variables (DATABASE_URL, SUPABASE_URI) are strictly ignored for safety.
func openConcurrencyTestStore(t *testing.T, prefix string) (*Store, func()) {
	t.Helper()
	pgURI := os.Getenv("XEROXGO_TEST_POSTGRES_URL")
	if pgURI == "" {
		pgURI = os.Getenv("POSTGRES_TEST_URL")
	}

	if pgURI != "" {
		s, err := Open(pgURI)
		if err != nil {
			t.Fatalf("failed to open postgres test store: %v", err)
		}
		if err := s.EnsureActiveSessionConstraint(context.Background()); err != nil {
			_ = s.Close()
			t.Fatalf("failed to ensure active session constraint on postgres test store: %v", err)
		}
		cleanup := func() {
			_ = s.Close()
		}
		return s, cleanup
	}

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, prefix+"_test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open sqlite test store: %v", err)
	}
	cleanup := func() {
		_ = s.Close()
	}
	return s, cleanup
}

// Test 1: Simultaneous claims of the exact same order.
// Invariant: Multiple concurrent claims of the same order must converge on the exact same session ID,
// and the underlying database table must contain strictly 1 active PAYMENT_PENDING row.
func TestConcurrencyBaseline_SimultaneousClaims_SameOrder(t *testing.T) {
	s, cleanup := openConcurrencyTestStore(t, "same_order")
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	kioskID := fmt.Sprintf("kiosk-concur-1-%d", time.Now().UnixNano()%1000000)
	orderID := fmt.Sprintf("ord_same_%d", time.Now().UnixNano())
	pin := "1234"

	createTestOrderWithSheets(t, s, ctx, orderID, kioskID, pin, 5, contracts.JobStatusPendingPayment, now)

	concurrency := 20
	var wg sync.WaitGroup
	wg.Add(concurrency)

	type result struct {
		sess *KioskSession
		err  error
	}
	results := make([]result, concurrency)

	for i := 0; i < concurrency; i++ {
		idx := i
		go func() {
			defer wg.Done()
			sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 100, 5*time.Minute, now)
			results[idx] = result{sess: sess, err: err}
		}()
	}
	wg.Wait()

	// Verify all returned results
	var firstSessID string
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("goroutine %d failed to claim order: %v", i, r.err)
		}
		if r.sess == nil {
			t.Fatalf("goroutine %d returned nil session without error", i)
		}
		if firstSessID == "" {
			firstSessID = r.sess.SessionID
		} else if r.sess.SessionID != firstSessID {
			t.Errorf("goroutine %d produced divergent session: got %s, want %s", i, r.sess.SessionID, firstSessID)
		}
	}

	// Verify actual database state directly via SQL
	var activeCount int
	err := s.db.QueryRowContext(ctx, s.rebind(`
SELECT COUNT(1) FROM kiosk_sessions 
WHERE order_id = ? AND status = 'PAYMENT_PENDING';
`), orderID).Scan(&activeCount)
	if err != nil {
		t.Fatalf("failed to query database for active sessions: %v", err)
	}
	if activeCount != 1 {
		t.Fatalf("database invariant violated: expected exactly 1 active session in database, found %d", activeCount)
	}

	var totalCount int
	err = s.db.QueryRowContext(ctx, s.rebind(`SELECT COUNT(1) FROM kiosk_sessions WHERE order_id = ?;`), orderID).Scan(&totalCount)
	if err != nil {
		t.Fatalf("failed to query total sessions: %v", err)
	}
	if totalCount != 1 {
		t.Fatalf("database invariant violated: expected exactly 1 total session row for order, found %d", totalCount)
	}
}

// Test 2: Competing claims from authorized vs unauthorized kiosks.
// Invariant: Concurrent claims from a competing/different kiosk must be rejected with ErrKioskMismatch
// or ErrOrderAlreadyClaimed, and no rogue session rows may be created.
func TestConcurrencyBaseline_CompetingKiosks(t *testing.T) {
	s, cleanup := openConcurrencyTestStore(t, "competing_kiosks")
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	ownerKiosk := fmt.Sprintf("kiosk-legit-%d", time.Now().UnixNano()%1000000)
	intruderKiosk := fmt.Sprintf("kiosk-intruder-%d", time.Now().UnixNano()%1000000)
	orderID := fmt.Sprintf("ord_compete_%d", time.Now().UnixNano())

	createTestOrderWithSheets(t, s, ctx, orderID, ownerKiosk, "5678", 3, contracts.JobStatusPendingPayment, now)

	concurrency := 10
	var wg sync.WaitGroup
	wg.Add(concurrency * 2)

	ownerResults := make([]error, concurrency)
	intruderResults := make([]error, concurrency)

	for i := 0; i < concurrency; i++ {
		idx := i
		go func() {
			defer wg.Done()
			_, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, ownerKiosk, orderID, 50, 5*time.Minute, now)
			ownerResults[idx] = err
		}()
		go func() {
			defer wg.Done()
			_, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, intruderKiosk, orderID, 50, 5*time.Minute, now)
			intruderResults[idx] = err
		}()
	}
	wg.Wait()

	// Owner claims must succeed
	for i, err := range ownerResults {
		if err != nil {
			t.Errorf("owner claim %d failed: %v", i, err)
		}
	}

	// Intruder claims must all fail
	for i, err := range intruderResults {
		if err == nil {
			t.Errorf("intruder claim %d unexpectedly succeeded!", i)
		} else if !errors.Is(err, ErrKioskMismatch) && !errors.Is(err, ErrOrderAlreadyClaimed) {
			t.Errorf("intruder claim %d failed with unexpected error: %v", i, err)
		}
	}

	// Database state verification: 0 sessions for intruderKiosk
	var intruderRows int
	_ = s.db.QueryRowContext(ctx, s.rebind(`SELECT COUNT(1) FROM kiosk_sessions WHERE kiosk_id = ?;`), intruderKiosk).Scan(&intruderRows)
	if intruderRows != 0 {
		t.Fatalf("database invariant violated: found %d intruder sessions in database", intruderRows)
	}
}

// Test 3: Simultaneous claims of different orders on the same kiosk with insufficient combined stock.
// Invariant: Available stock = 15 sheets. 5 orders each need 6 sheets (total needed: 30 sheets).
// At most floor(15 / 6) = 2 orders can be accepted. Exactly 2 must succeed; exactly 3 must receive ErrInsufficientPaper.
func TestConcurrencyBaseline_PaperOvercommitment_SimultaneousOrders(t *testing.T) {
	s, cleanup := openConcurrencyTestStore(t, "paper_overcommit")
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	kioskID := fmt.Sprintf("kiosk-paper-guard-%d", time.Now().UnixNano()%1000000)
	availableStock := 15
	sheetsPerOrder := 6
	orderCount := 5

	var orderIDs []string
	for i := 0; i < orderCount; i++ {
		ordID := fmt.Sprintf("ord_stock_%d_%d", time.Now().UnixNano(), i)
		pin := fmt.Sprintf("310%d", i)
		createTestOrderWithSheets(t, s, ctx, ordID, kioskID, pin, sheetsPerOrder, contracts.JobStatusPendingPayment, now)
		orderIDs = append(orderIDs, ordID)
	}

	var wg sync.WaitGroup
	wg.Add(orderCount)

	type claimRes struct {
		orderID string
		err     error
	}
	results := make([]claimRes, orderCount)

	for i := 0; i < orderCount; i++ {
		idx := i
		ordID := orderIDs[i]
		go func() {
			defer wg.Done()
			_, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ordID, availableStock, 5*time.Minute, now)
			results[idx] = claimRes{orderID: ordID, err: err}
		}()
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
			t.Errorf("order %s failed with unexpected error: %v", r.orderID, r.err)
		}
	}

	if successCount != 2 {
		t.Fatalf("expected exactly 2 successful claims for 15-sheet tray, got %d", successCount)
	}
	if insufficientCount != 3 {
		t.Fatalf("expected exactly 3 insufficient paper rejections, got %d", insufficientCount)
	}

	// Verify actual database commitments
	committed, err := s.GetCommittedPaperSheets(ctx, kioskID, "", now)
	if err != nil {
		t.Fatalf("failed to query committed paper sheets: %v", err)
	}
	expectedCommitted := 2 * sheetsPerOrder // 12 sheets
	if committed != expectedCommitted {
		t.Fatalf("database committed sheets mismatch: got %d, want %d", committed, expectedCommitted)
	}
}

// Test 4: Repeated requests and idempotency verification.
// Invariant: Repeatedly claiming the same order returns the exact same session without incrementing commitments.
func TestConcurrencyBaseline_RepeatedClaims_Idempotency(t *testing.T) {
	s, cleanup := openConcurrencyTestStore(t, "repeated_claims")
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	kioskID := fmt.Sprintf("kiosk-repeat-1-%d", time.Now().UnixNano()%1000000)
	orderID := fmt.Sprintf("ord_repeat_%d", time.Now().UnixNano())

	createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "9988", 4, contracts.JobStatusPendingPayment, now)

	// First claim
	sess1, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 50, 5*time.Minute, now)
	if err != nil {
		t.Fatalf("initial claim failed: %v", err)
	}

	committed1, _ := s.GetCommittedPaperSheets(ctx, kioskID, "", now)
	if committed1 != 4 {
		t.Fatalf("expected 4 committed sheets, got %d", committed1)
	}

	// Repeat claim 5 times sequentially
	for i := 0; i < 5; i++ {
		sessRepeat, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 50, 5*time.Minute, now)
		if err != nil {
			t.Fatalf("repeat claim %d failed: %v", i, err)
		}
		if sessRepeat.SessionID != sess1.SessionID {
			t.Fatalf("repeat claim %d returned different session ID: got %s, want %s", i, sessRepeat.SessionID, sess1.SessionID)
		}
	}

	// Committed sheets must still be exactly 4 (no double-counting)
	committed2, _ := s.GetCommittedPaperSheets(ctx, kioskID, "", now)
	if committed2 != 4 {
		t.Fatalf("idempotency violated: committed sheets inflated from 4 to %d", committed2)
	}

	// Exactly 1 row in kiosk_sessions
	var rowCount int
	_ = s.db.QueryRowContext(ctx, s.rebind(`SELECT COUNT(1) FROM kiosk_sessions WHERE order_id = ?;`), orderID).Scan(&rowCount)
	if rowCount != 1 {
		t.Fatalf("database row inflation: expected 1 row, got %d", rowCount)
	}
}

// Test 5: Expired sessions and automatic transition to EXPIRED on reclaim.
// Invariant: When an active session expires, its paper commitment drops to 0,
// and a subsequent claim marks the old session EXPIRED and creates a new session.
func TestConcurrencyBaseline_ExpiredSession_Reclaim(t *testing.T) {
	s, cleanup := openConcurrencyTestStore(t, "expired_session")
	defer cleanup()

	ctx := context.Background()
	t0 := time.Now().UTC()
	kioskID := fmt.Sprintf("kiosk-exp-1-%d", time.Now().UnixNano()%1000000)
	orderID := fmt.Sprintf("ord_exp_%d", time.Now().UnixNano())

	createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "7711", 5, contracts.JobStatusPendingPayment, t0)

	// Session with 2-minute duration
	sess1, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 20, 2*time.Minute, t0)
	if err != nil {
		t.Fatalf("initial claim failed: %v", err)
	}

	// At t0 + 1 min, session is still active
	t1 := t0.Add(1 * time.Minute)
	committedActive, _ := s.GetCommittedPaperSheets(ctx, kioskID, "", t1)
	if committedActive != 5 {
		t.Fatalf("active session committed sheets = %d, want 5", committedActive)
	}

	// At t0 + 3 min, session has expired
	t2 := t0.Add(3 * time.Minute)
	committedExpired, _ := s.GetCommittedPaperSheets(ctx, kioskID, "", t2)
	if committedExpired != 0 {
		t.Fatalf("expired session must commit 0 sheets, got %d", committedExpired)
	}

	// Reclaim order at t2
	sess2, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 20, 5*time.Minute, t2)
	if err != nil {
		t.Fatalf("reclaiming expired order failed: %v", err)
	}
	if sess2.SessionID == sess1.SessionID {
		t.Fatalf("reclaimed session must generate a new session ID, got same %s", sess2.SessionID)
	}

	// Verify database rows: exactly 1 EXPIRED row, exactly 1 PAYMENT_PENDING row
	var expiredCount, activeCount int
	_ = s.db.QueryRowContext(ctx, s.rebind(`SELECT COUNT(1) FROM kiosk_sessions WHERE order_id = ? AND status = 'EXPIRED';`), orderID).Scan(&expiredCount)
	_ = s.db.QueryRowContext(ctx, s.rebind(`SELECT COUNT(1) FROM kiosk_sessions WHERE order_id = ? AND status = 'PAYMENT_PENDING';`), orderID).Scan(&activeCount)

	if expiredCount != 1 {
		t.Errorf("expected 1 EXPIRED session row in database, got %d", expiredCount)
	}
	if activeCount != 1 {
		t.Errorf("expected 1 PAYMENT_PENDING session row in database, got %d", activeCount)
	}
}

// Test 6: Explicit session cancellation and paper commitment release.
// Invariant: Canceling a session transitions it to CANCELED and immediately releases its paper commitment.
func TestConcurrencyBaseline_Cancellation_Reclaim(t *testing.T) {
	s, cleanup := openConcurrencyTestStore(t, "cancel_session")
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	kioskID := fmt.Sprintf("kiosk-cancel-1-%d", time.Now().UnixNano()%1000000)
	orderID := fmt.Sprintf("ord_cancel_%d", time.Now().UnixNano())

	createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "8822", 7, contracts.JobStatusPendingPayment, now)

	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 30, 5*time.Minute, now)
	if err != nil {
		t.Fatalf("initial claim failed: %v", err)
	}

	committedBefore, _ := s.GetCommittedPaperSheets(ctx, kioskID, "", now)
	if committedBefore != 7 {
		t.Fatalf("expected 7 committed sheets, got %d", committedBefore)
	}

	// Cancel session
	if err := s.CancelKioskSession(ctx, sess.SessionID, kioskID, now); err != nil {
		t.Fatalf("CancelKioskSession failed: %v", err)
	}

	// Paper commitment must immediately drop to 0
	committedAfter, _ := s.GetCommittedPaperSheets(ctx, kioskID, "", now)
	if committedAfter != 0 {
		t.Fatalf("expected 0 committed sheets after cancellation, got %d", committedAfter)
	}

	// Verify database status is CANCELED
	var status string
	_ = s.db.QueryRowContext(ctx, s.rebind(`SELECT status FROM kiosk_sessions WHERE session_id = ?;`), sess.SessionID).Scan(&status)
	if status != "CANCELED" {
		t.Fatalf("expected database status CANCELED, got %s", status)
	}

	// Order can be re-claimed cleanly
	sessNew, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 30, 5*time.Minute, now)
	if err != nil {
		t.Fatalf("re-claim after cancel failed: %v", err)
	}
	if sessNew.SessionID == sess.SessionID {
		t.Fatalf("new session after cancellation must have distinct ID")
	}
}

// Test 7: Transaction rollback integrity on failure.
// Invariant: When CreateOrGetKioskSessionWithPaperCheck encounters an error (e.g. insufficient paper),
// the transaction rollback must leave zero orphan rows in kiosk_sessions.
func TestConcurrencyBaseline_RollbackIntegrity(t *testing.T) {
	s, cleanup := openConcurrencyTestStore(t, "rollback_int")
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	kioskID := fmt.Sprintf("kiosk-roll-1-%d", time.Now().UnixNano()%1000000)
	orderID := fmt.Sprintf("ord_roll_%d", time.Now().UnixNano())

	// Order requires 10 sheets, but kiosk only has 4 sheets
	createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "9933", 10, contracts.JobStatusPendingPayment, now)

	_, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 4, 5*time.Minute, now)
	if !errors.Is(err, ErrInsufficientPaper) {
		t.Fatalf("expected ErrInsufficientPaper, got %v", err)
	}

	// Direct database check: zero rows should exist in kiosk_sessions for this order
	var rowCount int
	err = s.db.QueryRowContext(ctx, s.rebind(`SELECT COUNT(1) FROM kiosk_sessions WHERE order_id = ?;`), orderID).Scan(&rowCount)
	if err != nil {
		t.Fatalf("failed to query database: %v", err)
	}
	if rowCount != 0 {
		t.Fatalf("rollback failure: expected 0 rows in kiosk_sessions after abort, found %d", rowCount)
	}
}

// Test 8: Schema Vulnerability & Mitigation Verification (PostgreSQL Concurrency Risk).
// In SQLite with MaxOpenConns(1), all transactions serialize on a single connection,
// which masks race conditions. Under PostgreSQL with MaxOpenConns(25) and READ COMMITTED isolation,
// two concurrent claims can simultaneously observe 0 active sessions and both execute INSERT.
//
// This test deterministically proves:
// 1. Under the current schema, duplicate active sessions (status='PAYMENT_PENDING') can coexist
//    for the same order_id because idx_kiosk_sessions_order_status is NOT unique.
// 2. Applying the recommended partial unique index:
//    CREATE UNIQUE INDEX idx_kiosk_sessions_active_order ON kiosk_sessions (order_id) WHERE status = 'PAYMENT_PENDING';
//    guarantees that the database engine rejects duplicate active sessions with a unique constraint violation,
//    while still allowing historical (EXPIRED, CANCELED, COMPLETED) rows.
func TestConcurrencyBaseline_PostgresPhantomRead_VulnerabilityDemonstration(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "phantom_demo.db")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to open test sqlite db: %v", err)
	}
	defer db.Close()

	// 1. Recreate current production table definition and current index
	schema := `
CREATE TABLE orders (order_id TEXT PRIMARY KEY, kiosk_id TEXT, status TEXT);
CREATE TABLE kiosk_sessions (session_id TEXT PRIMARY KEY, kiosk_id TEXT, order_id TEXT, status TEXT);
CREATE INDEX idx_kiosk_sessions_order_status ON kiosk_sessions (order_id, status);
INSERT INTO orders VALUES ('ord_phantom_1', 'kiosk-1', 'PENDING_PAYMENT');
`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema init failed: %v", err)
	}

	// 2. Demonstrate Vulnerability:
	// Two transactions under READ COMMITTED that both read 0 active rows will both execute INSERT.
	// Because idx_kiosk_sessions_order_status is non-unique, both inserts succeed:
	_, err1 := db.Exec(`INSERT INTO kiosk_sessions VALUES ('sess_tx1', 'kiosk-1', 'ord_phantom_1', 'PAYMENT_PENDING');`)
	if err1 != nil {
		t.Fatalf("insert 1 failed: %v", err1)
	}

	_, err2 := db.Exec(`INSERT INTO kiosk_sessions VALUES ('sess_tx2', 'kiosk-1', 'ord_phantom_1', 'PAYMENT_PENDING');`)
	if err2 != nil {
		t.Fatalf("insert 2 failed: %v", err2)
	}

	// Verify that the table now contains TWO active sessions for the exact same order!
	var duplicateCount int
	err = db.QueryRow(`SELECT COUNT(1) FROM kiosk_sessions WHERE order_id = 'ord_phantom_1' AND status = 'PAYMENT_PENDING';`).Scan(&duplicateCount)
	if err != nil {
		t.Fatalf("failed to query active sessions: %v", err)
	}
	if duplicateCount != 2 {
		t.Fatalf("expected 2 coexisting active sessions demonstrating vulnerability, found %d", duplicateCount)
	}
	t.Logf("Confirmed Vulnerability: Current schema allows %d active PAYMENT_PENDING sessions for the same order.", duplicateCount)

	// 3. Demonstrate Mitigation:
	// Clean up duplicate session
	_, _ = db.Exec(`DELETE FROM kiosk_sessions WHERE session_id = 'sess_tx2';`)

	// Create the recommended partial unique index
	_, err = db.Exec(`CREATE UNIQUE INDEX idx_kiosk_sessions_active_order ON kiosk_sessions (order_id) WHERE status = 'PAYMENT_PENDING';`)
	if err != nil {
		t.Fatalf("failed to create partial unique index: %v", err)
	}

	// Now attempt to insert the duplicate active session:
	_, err = db.Exec(`INSERT INTO kiosk_sessions VALUES ('sess_tx2', 'kiosk-1', 'ord_phantom_1', 'PAYMENT_PENDING');`)
	if err == nil {
		t.Fatalf("partial unique index failed: duplicate active session insert unexpectedly succeeded!")
	}
	t.Logf("Confirmed Mitigation: Database rejected duplicate active session with: %v", err)

	// Verify that inserting an EXPIRED session for the same order succeeds (partial index correctness)
	_, err = db.Exec(`INSERT INTO kiosk_sessions VALUES ('sess_tx3', 'kiosk-1', 'ord_phantom_1', 'EXPIRED');`)
	if err != nil {
		t.Fatalf("partial unique index incorrectly blocked EXPIRED session: %v", err)
	}

	// Verify that inserting a CANCELED session for the same order succeeds
	_, err = db.Exec(`INSERT INTO kiosk_sessions VALUES ('sess_tx4', 'kiosk-1', 'ord_phantom_1', 'CANCELED');`)
	if err != nil {
		t.Fatalf("partial unique index incorrectly blocked CANCELED session: %v", err)
	}
}
