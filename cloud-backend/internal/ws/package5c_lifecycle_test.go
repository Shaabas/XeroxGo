package ws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket/wsjson"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

func seedTestUnpaidOrderAndSession(t *testing.T, db *store.Store, kioskID, ordID string) *store.KioskSession {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := db.CreateOrder(ctx, store.Order{
		OrderID:      ordID,
		Status:       contracts.JobStatusPendingPayment,
		PricePaise:   300,
		TotalPages:   1,
		TotalSheets:  1,
		ColorMode:    "BW",
		PINExpiresAt: now.Add(10 * time.Minute),
		Documents: []store.OrderDocument{{
			JobID: "job-" + ordID, OrderID: ordID, DocumentIndex: 0,
			OriginalFilename: "doc.pdf", Duplex: contracts.DuplexSimplex, ColorMode: "BW",
			TotalPages: 1, TotalSheets: 1, Copies: 1, PricePaise: 300,
		}},
		Command: contracts.PrintJobCommand{
			JobID: "job-" + ordID, OrderID: ordID, KioskID: kioskID, DisplayCode: "9090",
			TotalPages: 1, TotalSheets: 1, Copies: 1, Duplex: contracts.DuplexSimplex, ColorMode: "BW", StartFromPage: 1,
		},
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateOrder(%s) failed: %v", ordID, err)
	}
	sess, err := db.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, ordID, 50, 2*time.Minute, now)
	if err != nil {
		t.Fatalf("CreateOrGetKioskSessionWithPaperCheck(%s) failed: %v", ordID, err)
	}
	return sess
}

func TestPackage5C_InvalidationGenerationMonotonicAndObsoleteWorkerBlocked(t *testing.T) {
	db := newTestStore(t)
	hub := NewHub(db)
	defer hub.Close()
	kioskID := "kiosk-5c-gen"
	ctx := context.Background()

	// 1. Verify generation identities are never reused across completed invalidations.
	hub.invalidationMu.Lock()
	gen1 := hub.bumpInvalidationLocked(kioskID)
	hub.invalidationMu.Unlock()
	hub.runUnpaidInvalidation(kioskID, gen1, 0)

	hub.invalidationMu.Lock()
	gen2 := hub.bumpInvalidationLocked(kioskID)
	hub.invalidationMu.Unlock()
	hub.runUnpaidInvalidation(kioskID, gen2, 0)

	if gen1 != 1 || gen2 != 2 {
		t.Fatalf("expected strictly monotonic generations 1, 2 without reuse, got gen1=%d gen2=%d", gen1, gen2)
	}

	// 2. An obsolete worker waiting on LockKioskReservation cannot cancel a checkout
	// created after a newer invalidation completes.
	unlock := hub.LockKioskReservation(kioskID)

	hub.invalidationMu.Lock()
	obsGen := hub.bumpInvalidationLocked(kioskID)
	hub.invalidationMu.Unlock()

	obsDone := make(chan struct{})
	go func() {
		defer close(obsDone)
		hub.runUnpaidInvalidation(kioskID, obsGen, 0)
	}()

	// Wait briefly so obs worker is blocked on LockKioskReservation, then advance to a newer
	// invalidation and complete it before releasing the lock.
	time.Sleep(30 * time.Millisecond)
	hub.invalidationMu.Lock()
	newerGen := hub.bumpInvalidationLocked(kioskID)
	if newerGen <= obsGen {
		hub.invalidationMu.Unlock()
		unlock()
		t.Fatalf("expected newerGen (%d) > obsGen (%d)", newerGen, obsGen)
	}
	// Simulate newer invalidation finishing and clearing pendingGen, followed by a new session creation.
	hub.invalidations[kioskID].pendingGen = 0
	hub.invalidationMu.Unlock()

	sessNew := seedTestUnpaidOrderAndSession(t, db, kioskID, "ord-5c-obs-safe")
	unlock()

	<-obsDone
	gotSess, err := db.GetKioskSession(ctx, sessNew.SessionID, time.Now().UTC())
	if err != nil || gotSess == nil || gotSess.Status != store.KioskSessionPaymentPending {
		t.Fatalf("obsolete worker must NOT cancel session created after newer invalidation, got %+v err=%v", gotSess, err)
	}
}

func TestPackage5C_DatabaseFailurePreservesBarrierUntilRetrySucceeds(t *testing.T) {
	db := newTestStore(t)
	hub := NewHub(db)
	defer hub.Close()
	kioskID := "kiosk-5c-dbretry"
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(hub.ServeHTTP))
	defer srv.Close()

	conn, _ := connectTestKiosk(t, hub, srv.URL, kioskID)
	defer conn.CloseNow()
	sendTestReadyHealth(ctx, t, hub, conn, kioskID)

	sess := seedTestUnpaidOrderAndSession(t, db, kioskID, "ord-5c-dbretry")

	// Force CancelActiveUnpaidSessionsForKiosk to fail via a temporary SQLite trigger.
	if _, err := db.DB().Exec(`CREATE TRIGGER fail_cancel_5c BEFORE UPDATE ON kiosk_sessions WHEN NEW.status = 'CANCELED' BEGIN SELECT RAISE(ABORT, 'forced cancel error'); END;`); err != nil {
		t.Fatalf("failed creating trigger: %v", err)
	}

	hub.triggerUnpaidInvalidation(kioskID)
	if !hub.hasPendingInvalidation(kioskID) {
		t.Fatalf("expected pending invalidation barrier preserved after DB failure")
	}

	// Even if healthy telemetry arrives, admission MUST remain blocked while invalidation is pending.
	now := time.Now().UTC()
	h := contracts.PrinterHealth{
		Online:              true,
		State:               "IDLE",
		EvaluatedHealth:     contracts.EvaluatedHealthHealthy,
		PaperTrayStatus:     "OK",
		EstimatedSheets:     250,
		InventoryCalibrated: true,
		ExecutionSafe:       true,
		TonerLevelPercent:   90,
		TonerLevelKnown:     true,
		DrumLifePercent:     95,
		DrumLifeKnown:       true,
		UpdatedAt:           now,
		EvidenceFreshness:   contracts.BuildUniformEvidenceFreshness(now, DefaultTelemetryFreshnessWindow),
	}
	_ = wsjson.Write(ctx, conn, wireMessage{Type: "health", Health: &h})
	time.Sleep(30 * time.Millisecond)
	_, _, _, ready, reason := hub.EvaluateKioskReadiness(kioskID)
	if ready {
		t.Fatalf("expected kiosk unavailable while DB cancellation is pending, got ready=true (reason=%s)", reason)
	}

	// Remove the trigger so the bounded retry succeeds.
	if _, err := db.DB().Exec(`DROP TRIGGER fail_cancel_5c;`); err != nil {
		t.Fatalf("failed dropping trigger: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !hub.hasPendingInvalidation(kioskID) {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if hub.hasPendingInvalidation(kioskID) {
		t.Fatalf("expected pending invalidation cleared after retry succeeded")
	}
	gotSess, _ := db.GetKioskSession(ctx, sess.SessionID, time.Now().UTC())
	if gotSess == nil || gotSess.Status != store.KioskSessionCanceled {
		t.Fatalf("expected unpaid session canceled once retry succeeded, got %+v", gotSess)
	}
	_, _, _, readyAfter, reasonAfter := hub.EvaluateKioskReadiness(kioskID)
	if !readyAfter {
		t.Fatalf("expected kiosk ready after retry succeeded, got ready=false (%s)", reasonAfter)
	}
}

func TestPackage5C_ProlongedDatabaseOutageRecoversAfterRetryBudgetExhausted(t *testing.T) {
	db := newTestStore(t)
	hub := NewHub(db)
	defer hub.Close()
	kioskID := "kiosk-5c-prolonged-db"
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(hub.ServeHTTP))
	defer srv.Close()

	conn, _ := connectTestKiosk(t, hub, srv.URL, kioskID)
	defer conn.CloseNow()
	sendTestReadyHealth(ctx, t, hub, conn, kioskID)

	sess := seedTestUnpaidOrderAndSession(t, db, kioskID, "ord-5c-prolonged-db")

	// Force CancelActiveUnpaidSessionsForKiosk to fail, then exhaust the retry budget immediately
	// at the final attempt boundary so no background retry timer remains active.
	if _, err := db.DB().Exec(`CREATE TRIGGER fail_cancel_prolonged BEFORE UPDATE ON kiosk_sessions WHEN NEW.status = 'CANCELED' BEGIN SELECT RAISE(ABORT, 'forced prolonged outage'); END;`); err != nil {
		t.Fatalf("failed creating trigger: %v", err)
	}

	hub.invalidationMu.Lock()
	gen := hub.bumpInvalidationLocked(kioskID)
	hub.invalidationMu.Unlock()
	hub.runUnpaidInvalidation(kioskID, gen, maxInvalidationRetries-1)

	if !hub.hasPendingInvalidation(kioskID) {
		t.Fatalf("expected pending invalidation preserved after retry budget exhaustion")
	}
	hub.invalidationMu.Lock()
	st := hub.invalidations[kioskID]
	hasTimer := st != nil && st.retryTimer != nil
	if st != nil {
		// Allow immediate lifecycle-driven retry once DB recovers.
		st.lastAttemptAt = time.Now().Add(-time.Second)
	}
	hub.invalidationMu.Unlock()
	if hasTimer {
		t.Fatalf("expected no active retry timer after retry budget exhausted")
	}

	// Database recovers later: drop trigger and send healthy telemetry lifecycle event.
	if _, err := db.DB().Exec(`DROP TRIGGER fail_cancel_prolonged;`); err != nil {
		t.Fatalf("failed dropping trigger: %v", err)
	}

	sendTestReadyHealth(ctx, t, hub, conn, kioskID)

	if hub.hasPendingInvalidation(kioskID) {
		t.Fatalf("expected pending invalidation cleared after DB recovery + telemetry lifecycle event")
	}
	gotSess, _ := db.GetKioskSession(ctx, sess.SessionID, time.Now().UTC())
	if gotSess == nil || gotSess.Status != store.KioskSessionCanceled {
		t.Fatalf("expected unpaid session canceled after DB recovery, got %+v", gotSess)
	}
	_, _, _, ready, reason := hub.EvaluateKioskReadiness(kioskID)
	if !ready {
		t.Fatalf("expected kiosk ready after prolonged DB outage recovery, got ready=false (%s)", reason)
	}
}
