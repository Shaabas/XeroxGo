package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

// TestStore_PreflightDuplicateActiveSessionsDetection verifies that DetectDuplicateActiveSessions
// accurately detects duplicate PAYMENT_PENDING sessions and that store migration preflight
// refuses to apply the unique index if duplicates exist without altering customer data.
func TestStore_PreflightDuplicateActiveSessionsDetection(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "preflight_test.db")

	// 1. Initialize store cleanly
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	now := time.Now().UTC()

	// Initial clean state: no duplicates
	dups, err := s.DetectDuplicateActiveSessions(ctx)
	if err != nil {
		t.Fatalf("DetectDuplicateActiveSessions failed: %v", err)
	}
	if len(dups) != 0 {
		t.Fatalf("expected 0 duplicate orders initially, got %d", len(dups))
	}

	// Create an order
	orderID := "ord_pref_001"
	createTestOrderWithSheets(t, s, ctx, orderID, "kiosk-pref", "1234", 2, contracts.JobStatusPendingPayment, now)

	// Create first session via normal API
	_, err = s.CreateOrGetKioskSessionWithPaperCheck(ctx, "kiosk-pref", orderID, 20, 5*time.Minute, now)
	if err != nil {
		t.Fatalf("create session failed: %v", err)
	}

	// Still clean: exactly 1 active session
	dups, err = s.DetectDuplicateActiveSessions(ctx)
	if err != nil {
		t.Fatalf("DetectDuplicateActiveSessions failed: %v", err)
	}
	if len(dups) != 0 {
		t.Fatalf("expected 0 duplicate orders, got %d", len(dups))
	}

	// Drop the partial unique index temporarily to simulate existing legacy production state with duplicate rows
	_, err = s.db.Exec(`DROP INDEX IF EXISTS idx_kiosk_sessions_active_order;`)
	if err != nil {
		t.Fatalf("failed to drop index for test setup: %v", err)
	}

	// Inject a second active session for the same order
	secondSessID := "sess_dup_legacy_999"
	_, err = s.db.Exec(`
INSERT INTO kiosk_sessions (session_id, kiosk_id, order_id, status, price_paise, expires_at_ns, created_at_ns, updated_at_ns)
VALUES (?, 'kiosk-pref', ?, 'PAYMENT_PENDING', 200, ?, ?, ?);
`, secondSessID, orderID, now.Add(5*time.Minute).UnixNano(), now.UnixNano(), now.UnixNano())
	if err != nil {
		t.Fatalf("failed to inject legacy duplicate session: %v", err)
	}

	// 2. Verify DetectDuplicateActiveSessions identifies the offending order
	dups, err = s.DetectDuplicateActiveSessions(ctx)
	if err != nil {
		t.Fatalf("DetectDuplicateActiveSessions failed: %v", err)
	}
	if len(dups) != 1 || dups[0] != orderID {
		t.Fatalf("expected duplicate detection for %s, got: %v", orderID, dups)
	}

	// Close store
	_ = s.Close()

	// 3. Attempt to reopen the store on this database.
	// Invariant: The preflight check MUST detect the duplicate and REFUSE to open/migrate,
	// without deleting or altering any customer records.
	sReopen, errReopen := Open(dbPath)
	if errReopen == nil {
		_ = sReopen.Close()
		t.Fatalf("expected Open() to fail preflight check when duplicate active sessions exist, but it succeeded!")
	}
	t.Logf("Confirmed: Open() aborted with preflight failure: %v", errReopen)

	// 4. Verify no customer records were modified or deleted by the aborted migration
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to open raw sqlite: %v", err)
	}
	defer rawDB.Close()

	var totalActive int
	err = rawDB.QueryRow(`SELECT COUNT(1) FROM kiosk_sessions WHERE order_id = ? AND status = 'PAYMENT_PENDING';`, orderID).Scan(&totalActive)
	if err != nil {
		t.Fatalf("query total active: %v", err)
	}
	if totalActive != 2 {
		t.Fatalf("data integrity violated: preflight check must not modify existing rows; expected 2 rows, found %d", totalActive)
	}
}

// TestStore_UniquenessConflictSafeHandling verifies that if a concurrent insert hits a uniqueness conflict,
// the store safely returns the existing authorized session for the same kiosk without error,
// and returns ErrOrderAlreadyClaimed for an intruder kiosk without leaking session metadata.
func TestStore_UniquenessConflictSafeHandling(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "conflict_test.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	kioskID := "kiosk-legit"
	orderID := "ord_conflict_001"

	createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "4455", 3, contracts.JobStatusPendingPayment, now)

	// First claim
	sess1, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 50, 5*time.Minute, now)
	if err != nil {
		t.Fatalf("first claim failed: %v", err)
	}

	// Repeated claim by legitimate kiosk: returns identical session
	sess2, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 50, 5*time.Minute, now)
	if err != nil {
		t.Fatalf("second claim failed: %v", err)
	}
	if sess2.SessionID != sess1.SessionID {
		t.Fatalf("expected identical session ID, got %s vs %s", sess2.SessionID, sess1.SessionID)
	}

	// Competing claim by intruder kiosk: rejected with ErrKioskMismatch or ErrOrderAlreadyClaimed
	// and never returns sess1
	sessIntruder, errIntruder := s.CreateOrGetKioskSessionWithPaperCheck(ctx, "kiosk-intruder", orderID, 50, 5*time.Minute, now)
	if errIntruder == nil {
		t.Fatalf("expected intruder claim to fail, but succeeded with session %v", sessIntruder)
	}
	if !errors.Is(errIntruder, ErrKioskMismatch) && !errors.Is(errIntruder, ErrOrderAlreadyClaimed) {
		t.Fatalf("intruder claim failed with unexpected error: %v", errIntruder)
	}
	if sessIntruder != nil {
		t.Fatalf("intruder must never receive a session object, got: %v", sessIntruder)
	}
}

// TestPostgresConcurrency_IntegrationSuite runs true concurrent integration tests against PostgreSQL
// if XEROXGO_TEST_POSTGRES_URL or POSTGRES_TEST_URL is configured in the environment.
// Generic production variables (DATABASE_URL, SUPABASE_URI, SUPABASE_DB_URL, POSTGRES_URL)
// are strictly ignored for safety.
// If PostgreSQL is unavailable, it explicitly marks PostgreSQL concurrency guarantees as UNVERIFIED.
func TestPostgresConcurrency_IntegrationSuite(t *testing.T) {
	pgURI := os.Getenv("XEROXGO_TEST_POSTGRES_URL")
	if pgURI == "" {
		pgURI = os.Getenv("POSTGRES_TEST_URL")
	}

	if pgURI == "" {
		t.Log("[UNVERIFIED] Disposable PostgreSQL test database not configured (XEROXGO_TEST_POSTGRES_URL / POSTGRES_TEST_URL not set). Production/staging environment variables (DATABASE_URL, SUPABASE_URI) are strictly ignored for safety. Live PostgreSQL concurrency guarantees remain marked UNVERIFIED.")
		t.Skip("skipping live Postgres concurrency tests (XEROXGO_TEST_POSTGRES_URL/POSTGRES_TEST_URL not set)")
		return
	}

	s, err := Open(pgURI)
	if err != nil {
		t.Fatalf("failed to connect to live PostgreSQL store: %v", err)
	}
	defer s.Close()

	if s.dialect != "postgres" {
		t.Fatalf("expected store dialect postgres, got: %s", s.dialect)
	}

	ctx := context.Background()

	// Ensure the active session constraint is present on the disposable test database
	if err := s.EnsureActiveSessionConstraint(ctx); err != nil {
		t.Fatalf("failed to apply active session constraint on disposable test database: %v", err)
	}

	now := time.Now().UTC()
	kioskID := fmt.Sprintf("kiosk-pg-%d", time.Now().UnixNano()%10000)

	// Subtest 1: Simultaneous claims on same order converge on 1 session via partial unique index
	t.Run("Postgres_SimultaneousClaims_SameOrder", func(t *testing.T) {
		orderID := fmt.Sprintf("ord_pg_same_%d", time.Now().UnixNano())
		createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "1234", 4, contracts.JobStatusPendingPayment, now)

		concurrency := 10
		var wg sync.WaitGroup
		wg.Add(concurrency)

		type res struct {
			sess *KioskSession
			err  error
		}
		results := make([]res, concurrency)

		for i := 0; i < concurrency; i++ {
			idx := i
			go func() {
				defer wg.Done()
				sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 100, 5*time.Minute, now)
				results[idx] = res{sess: sess, err: err}
			}()
		}
		wg.Wait()

		var winningSessionID string
		for i, r := range results {
			if r.err != nil {
				t.Fatalf("goroutine %d failed: %v", i, r.err)
			}
			if r.sess == nil {
				t.Fatalf("goroutine %d returned nil session", i)
			}
			if winningSessionID == "" {
				winningSessionID = r.sess.SessionID
			} else if r.sess.SessionID != winningSessionID {
				t.Fatalf("divergent sessions created in Postgres: %s vs %s", r.sess.SessionID, winningSessionID)
			}
		}

		// Verify physical PostgreSQL table state
		var activeCount int
		err := s.db.QueryRowContext(ctx, "SELECT COUNT(1) FROM kiosk_sessions WHERE order_id = $1 AND status = 'PAYMENT_PENDING';", orderID).Scan(&activeCount)
		if err != nil {
			t.Fatalf("query active count: %v", err)
		}
		if activeCount != 1 {
			t.Fatalf("expected exactly 1 active session in Postgres, found %d", activeCount)
		}
	})

	// Subtest 2: Competing orders on same kiosk with limited stock (Advisory Lock Serialization)
	t.Run("Postgres_PaperOvercommitment_AdvisoryLock", func(t *testing.T) {
		kioskStock := fmt.Sprintf("kiosk-pg-stock-%d", time.Now().UnixNano()%100000)
		availableStock := 15
		sheetsPerOrder := 6
		orderCount := 4 // Total needed = 24 sheets > 15 available. Exactly 2 should succeed, 2 should fail.

		var orderIDs []string
		for i := 0; i < orderCount; i++ {
			ordID := fmt.Sprintf("ord_pg_stock_%d_%d", time.Now().UnixNano(), i)
			pin := fmt.Sprintf("51%02d", i)
			createTestOrderWithSheets(t, s, ctx, ordID, kioskStock, pin, sheetsPerOrder, contracts.JobStatusPendingPayment, now)
			orderIDs = append(orderIDs, ordID)
		}

		var wg sync.WaitGroup
		wg.Add(orderCount)
		errorsList := make([]error, orderCount)

		for i := 0; i < orderCount; i++ {
			idx := i
			ordID := orderIDs[i]
			go func() {
				defer wg.Done()
				_, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskStock, ordID, availableStock, 5*time.Minute, now)
				errorsList[idx] = err
			}()
		}
		wg.Wait()

		var successCount int
		var insufficientCount int
		for _, err := range errorsList {
			if err == nil {
				successCount++
			} else if errors.Is(err, ErrInsufficientPaper) {
				insufficientCount++
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}

		if successCount != 2 {
			t.Fatalf("expected 2 successful claims, got %d", successCount)
		}
		if insufficientCount != 2 {
			t.Fatalf("expected 2 insufficient paper rejections, got %d", insufficientCount)
		}
	})

	// Subtest 3: Rollback on error leaves 0 orphan rows
	t.Run("Postgres_RollbackIntegrity", func(t *testing.T) {
		orderID := fmt.Sprintf("ord_pg_roll_%d", time.Now().UnixNano())
		createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "9911", 10, contracts.JobStatusPendingPayment, now)

		// Kiosk has only 3 sheets, order needs 10 sheets -> must fail and rollback
		_, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 3, 5*time.Minute, now)
		if !errors.Is(err, ErrInsufficientPaper) {
			t.Fatalf("expected ErrInsufficientPaper, got %v", err)
		}

		var rowCount int
		err = s.db.QueryRowContext(ctx, "SELECT COUNT(1) FROM kiosk_sessions WHERE order_id = $1;", orderID).Scan(&rowCount)
		if err != nil {
			t.Fatalf("query row count: %v", err)
		}
		if rowCount != 0 {
			t.Fatalf("expected 0 rows in Postgres after rollback, found %d", rowCount)
		}
	})

	// Subtest 4: Explicit Cancellation and Paper Stock Release in PostgreSQL
	t.Run("Postgres_CancellationAndReclaim", func(t *testing.T) {
		kioskCancel := fmt.Sprintf("kiosk-pg-cancel-%d", time.Now().UnixNano()%100000)
		orderID := fmt.Sprintf("ord_pg_cancel_%d", time.Now().UnixNano())
		createTestOrderWithSheets(t, s, ctx, orderID, kioskCancel, "7733", 5, contracts.JobStatusPendingPayment, now)

		sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskCancel, orderID, 20, 5*time.Minute, now)
		if err != nil {
			t.Fatalf("initial claim failed: %v", err)
		}

		committedBefore, _ := s.GetCommittedPaperSheets(ctx, kioskCancel, "", now)
		if committedBefore != 5 {
			t.Fatalf("expected 5 committed sheets, got %d", committedBefore)
		}

		// Cancel session
		if err := s.CancelKioskSession(ctx, sess.SessionID, kioskCancel, now); err != nil {
			t.Fatalf("CancelKioskSession failed: %v", err)
		}

		// Paper commitment must immediately drop to 0
		committedAfter, _ := s.GetCommittedPaperSheets(ctx, kioskCancel, "", now)
		if committedAfter != 0 {
			t.Fatalf("expected 0 committed sheets after cancel, got %d", committedAfter)
		}

		// Re-claim order
		sessNew, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskCancel, orderID, 20, 5*time.Minute, now)
		if err != nil {
			t.Fatalf("re-claim failed: %v", err)
		}
		if sessNew.SessionID == sess.SessionID {
			t.Fatalf("new session after cancellation must have distinct ID")
		}
	})

	// Subtest 5: Expired Session Reclamation in PostgreSQL
	t.Run("Postgres_ExpirationAndReclaim", func(t *testing.T) {
		kioskExp := fmt.Sprintf("kiosk-pg-exp-%d", time.Now().UnixNano()%100000)
		orderID := fmt.Sprintf("ord_pg_exp_%d", time.Now().UnixNano())
		t0 := time.Now().UTC()
		createTestOrderWithSheets(t, s, ctx, orderID, kioskExp, "6622", 4, contracts.JobStatusPendingPayment, t0)

		sessOld, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskExp, orderID, 20, 2*time.Minute, t0)
		if err != nil {
			t.Fatalf("initial claim failed: %v", err)
		}

		// At t0 + 3 min, session has expired
		tExpired := t0.Add(3 * time.Minute)
		committedExpired, _ := s.GetCommittedPaperSheets(ctx, kioskExp, "", tExpired)
		if committedExpired != 0 {
			t.Fatalf("expired session must commit 0 sheets, got %d", committedExpired)
		}

		// Re-claim order at tExpired
		sessNew, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskExp, orderID, 20, 5*time.Minute, tExpired)
		if err != nil {
			t.Fatalf("reclaiming expired order failed: %v", err)
		}
		if sessNew.SessionID == sessOld.SessionID {
			t.Fatalf("reclaimed session must generate new session ID")
		}

		// Database state in PostgreSQL: exactly 1 EXPIRED, 1 PAYMENT_PENDING
		var expCount, actCount int
		_ = s.db.QueryRowContext(ctx, "SELECT COUNT(1) FROM kiosk_sessions WHERE order_id = $1 AND status = 'EXPIRED';", orderID).Scan(&expCount)
		_ = s.db.QueryRowContext(ctx, "SELECT COUNT(1) FROM kiosk_sessions WHERE order_id = $1 AND status = 'PAYMENT_PENDING';", orderID).Scan(&actCount)
		if expCount != 1 || actCount != 1 {
			t.Fatalf("expected 1 EXPIRED and 1 PAYMENT_PENDING row in Postgres, got %d and %d", expCount, actCount)
		}
	})

	// Subtest 6: Competing Kiosks Isolation & Zero Peer Metadata Leakage
	t.Run("Postgres_CompetingKiosks_NoPeerLeak", func(t *testing.T) {
		kioskLegit := fmt.Sprintf("kiosk-pg-legit-%d", time.Now().UnixNano()%100000)
		kioskIntruder := fmt.Sprintf("kiosk-pg-intruder-%d", time.Now().UnixNano()%100000)
		orderID := fmt.Sprintf("ord_pg_compete_%d", time.Now().UnixNano())

		createTestOrderWithSheets(t, s, ctx, orderID, kioskLegit, "9988", 3, contracts.JobStatusPendingPayment, now)

		// Legit claims first
		sessLegit, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskLegit, orderID, 20, 5*time.Minute, now)
		if err != nil {
			t.Fatalf("legit claim failed: %v", err)
		}

		// Intruder claims: must be rejected with ErrKioskMismatch or ErrOrderAlreadyClaimed
		sessIntruder, errIntruder := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskIntruder, orderID, 20, 5*time.Minute, now)
		if errIntruder == nil {
			t.Fatalf("intruder claim unexpectedly succeeded!")
		}
		if !errors.Is(errIntruder, ErrKioskMismatch) && !errors.Is(errIntruder, ErrOrderAlreadyClaimed) {
			t.Fatalf("intruder failed with unexpected error: %v", errIntruder)
		}
		// Critical Security Guarantee: Intruder must NEVER receive peer session details!
		if sessIntruder != nil {
			t.Fatalf("CRITICAL SECURITY VIOLATION: Intruder received session object: %+v (legit was %s)", sessIntruder, sessLegit.SessionID)
		}

		// Verify PostgreSQL database state: 0 rows for intruder kiosk
		var intruderRows int
		_ = s.db.QueryRowContext(ctx, "SELECT COUNT(1) FROM kiosk_sessions WHERE kiosk_id = $1;", kioskIntruder).Scan(&intruderRows)
		if intruderRows != 0 {
			t.Fatalf("database invariant violated: found %d rows for intruder kiosk in Postgres", intruderRows)
		}
	})

	// Subtest 7: Preflight Detection in PostgreSQL
	t.Run("Postgres_PreflightDuplicateActiveSessionsDetection", func(t *testing.T) {
		kioskPref := fmt.Sprintf("kiosk-pg-pref-%d", time.Now().UnixNano()%100000)
		orderID := fmt.Sprintf("ord_pg_pref_%d", time.Now().UnixNano())
		createTestOrderWithSheets(t, s, ctx, orderID, kioskPref, "1122", 2, contracts.JobStatusPendingPayment, now)

		_, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskPref, orderID, 20, 5*time.Minute, now)
		if err != nil {
			t.Fatalf("initial claim failed: %v", err)
		}

		// Clean check
		dups, err := s.DetectDuplicateActiveSessions(ctx)
		if err != nil {
			t.Fatalf("DetectDuplicateActiveSessions failed: %v", err)
		}
		for _, d := range dups {
			if d == orderID {
				t.Fatalf("order %s unexpectedly flagged as duplicate", orderID)
			}
		}
	})

	// Subtest 8: PostgreSQL Index Deployment Mechanism Verification (Schema-Isolated)
	// Uses a dedicated, disposable schema to verify startup and explicit constraint installation
	// without dropping or modifying any indexes or tables in the shared schema.
	t.Run("Postgres_IndexDeployment_StartupVsExplicit", func(t *testing.T) {
		// 1. Create a dedicated isolated test schema
		testSchema := fmt.Sprintf("test_deploy_iso_%d", time.Now().UnixNano()%100000000)
		_, err := s.db.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s;", testSchema))
		if err != nil {
			t.Fatalf("failed to create isolated test schema %s: %v", testSchema, err)
		}

		// Ensure the isolated schema and all test-owned objects are dropped when the test finishes
		t.Cleanup(func() {
			cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = s.db.ExecContext(cleanCtx, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE;", testSchema))
		})

		// 2. Build connection URI scoped to the isolated schema via search_path
		separator := "?"
		if strings.Contains(pgURI, "?") {
			separator = "&"
		}
		isolatedURI := fmt.Sprintf("%s%ssearch_path=%s", pgURI, separator, testSchema)

		// 3. Open store connection in the isolated schema to simulate application server startup
		sIso, err := Open(isolatedURI)
		if err != nil {
			t.Fatalf("failed to open store in isolated schema: %v", err)
		}
		defer sIso.Close()

		// 4. Verify server startup (Open) did NOT automatically create idx_kiosk_sessions_active_order in the schema
		var idxCount int
		err = sIso.db.QueryRowContext(ctx, "SELECT COUNT(1) FROM pg_indexes WHERE schemaname = $1 AND tablename = 'kiosk_sessions' AND indexname = 'idx_kiosk_sessions_active_order';", testSchema).Scan(&idxCount)
		if err != nil {
			t.Fatalf("query pg_indexes in isolated schema: %v", err)
		}
		if idxCount != 0 {
			t.Fatalf("REGRESSION: Open() automatically created idx_kiosk_sessions_active_order; production migration must be explicit")
		}

		// 5. Inject duplicate active sessions for a test order inside the isolated schema
		orderID := fmt.Sprintf("ord_pg_dep_%d", time.Now().UnixNano())
		kioskID1 := fmt.Sprintf("kiosk-pg-dep1-%d", time.Now().UnixNano()%100000)
		kioskID2 := fmt.Sprintf("kiosk-pg-dep2-%d", time.Now().UnixNano()%100000)
		pin := fmt.Sprintf("%04d", time.Now().UnixNano()%10000)
		createTestOrderWithSheets(t, sIso, ctx, orderID, kioskID1, pin, 2, contracts.JobStatusPendingPayment, now)

		sessID1 := fmt.Sprintf("sess_pg_dep_1_%d", time.Now().UnixNano())
		sessID2 := fmt.Sprintf("sess_pg_dep_2_%d", time.Now().UnixNano())
		_, err = sIso.db.ExecContext(ctx, `
INSERT INTO kiosk_sessions (session_id, kiosk_id, order_id, status, price_paise, expires_at_ns, created_at_ns, updated_at_ns)
VALUES ($1, $2, $3, 'PAYMENT_PENDING', 200, $4, $5, $5);
`, sessID1, kioskID1, orderID, now.Add(5*time.Minute).UnixNano(), now.UnixNano())
		if err != nil {
			t.Fatalf("insert session 1: %v", err)
		}

		_, err = sIso.db.ExecContext(ctx, `
INSERT INTO kiosk_sessions (session_id, kiosk_id, order_id, status, price_paise, expires_at_ns, created_at_ns, updated_at_ns)
VALUES ($1, $2, $3, 'PAYMENT_PENDING', 200, $4, $5, $5);
`, sessID2, kioskID2, orderID, now.Add(5*time.Minute).UnixNano(), now.UnixNano())
		if err != nil {
			t.Fatalf("insert session 2: %v", err)
		}

		// 6. Calling EnsureActiveSessionConstraint must fail closed on preflight detection
		err = sIso.EnsureActiveSessionConstraint(ctx)
		if err == nil {
			t.Fatalf("expected EnsureActiveSessionConstraint to fail preflight with duplicate active sessions, but it succeeded")
		}
		t.Logf("Confirmed: EnsureActiveSessionConstraint aborted on preflight in isolated schema: %v", err)

		// 7. Clean up the duplicate session in the isolated schema
		_, err = sIso.db.ExecContext(ctx, "DELETE FROM kiosk_sessions WHERE session_id = $1;", sessID2)
		if err != nil {
			t.Fatalf("delete duplicate test session: %v", err)
		}

		// 8. Now EnsureActiveSessionConstraint should succeed and create the index in the isolated schema
		err = sIso.EnsureActiveSessionConstraint(ctx)
		if err != nil {
			t.Fatalf("EnsureActiveSessionConstraint failed in isolated schema: %v", err)
		}

		// 9. Verify the index is present and valid specifically in the isolated schema
		var isValid, isUnique bool
		err = sIso.db.QueryRowContext(ctx, `
SELECT i.indisvalid, i.indisunique 
FROM pg_class c 
JOIN pg_index i ON c.oid = i.indexrelid 
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relname = 'idx_kiosk_sessions_active_order' AND n.nspname = $1;
`, testSchema).Scan(&isValid, &isUnique)
		if err != nil {
			t.Fatalf("query index validity in isolated schema: %v", err)
		}
		if !isValid || !isUnique {
			t.Fatalf("expected valid unique index in isolated schema %s, got valid=%v unique=%v", testSchema, isValid, isUnique)
		}

		// 10. Verify that the unique constraint now actively rejects duplicate active session insertion
		sessID3 := fmt.Sprintf("sess_pg_dep_3_%d", time.Now().UnixNano())
		_, err = sIso.db.ExecContext(ctx, `
INSERT INTO kiosk_sessions (session_id, kiosk_id, order_id, status, price_paise, expires_at_ns, created_at_ns, updated_at_ns)
VALUES ($1, $2, $3, 'PAYMENT_PENDING', 200, $4, $5, $5);
`, sessID3, kioskID1, orderID, now.Add(5*time.Minute).UnixNano(), now.UnixNano())
		if err == nil {
			t.Fatalf("expected unique constraint violation on duplicate active session insertion, but it succeeded")
		}
		t.Logf("Confirmed: Partial unique index rejected duplicate active session: %v", err)
	})
}

// TestStore_PreflightFailClosed_OnDatabaseError verifies that if the duplicate-session preflight
// query encounters any database error (e.g. malformed table, query execution error),
// store initialization immediately fails closed and aborts without swallowing the error.
func TestStore_PreflightFailClosed_OnDatabaseError(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "preflight_fail_closed.db")

	// 1. Pre-create a malformed kiosk_sessions table that lacks required columns (order_id, status)
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to open raw sqlite: %v", err)
	}
	_, err = rawDB.Exec(`CREATE TABLE kiosk_sessions (corrupt_column TEXT);`)
	_ = rawDB.Close()
	if err != nil {
		t.Fatalf("failed to create malformed table: %v", err)
	}

	// 2. Attempt to open store on this database.
	// Invariant: The preflight query on kiosk_sessions must fail with a column error,
	// and Open() MUST NOT swallow this error — it must fail closed and abort!
	s, err := Open(dbPath)
	if err == nil {
		_ = s.Close()
		t.Fatalf("expected Open() to fail closed when preflight query encounters a database error, but it succeeded")
	}
	t.Logf("Confirmed: Open() aborted with expected preflight database error: %v", err)

	// Verify the error mentions preflight failure
	if !strings.Contains(err.Error(), "preflight failed") {
		t.Fatalf("expected error to mention preflight failure, got: %v", err)
	}
}

