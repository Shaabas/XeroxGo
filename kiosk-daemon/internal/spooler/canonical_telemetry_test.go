package spooler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/pkg/contracts"
)

func TestSpoolerCanonical_ImmediateStartup_NonOverlapping_AndAgeNotRefreshedOnRead(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := state.Open(filepath.Join(tmpDir, "canon_timing.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer st.Close()

	var calls atomic.Int32
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32

	poller := func(ctx context.Context, sheets int) contracts.PrinterHealth {
		cur := inFlight.Add(1)
		for {
			prev := maxInFlight.Load()
			if cur <= prev || maxInFlight.CompareAndSwap(prev, cur) {
				break
			}
		}
		defer inFlight.Add(-1)

		calls.Add(1)
		time.Sleep(60 * time.Millisecond)
		return testHealthyMockHealth(ctx, sheets)
	}

	client, err := NewClient(ClientConfig{
		CloudURL:           "ws://127.0.0.1:1/ws",
		KioskID:            "kiosk-canon-timing",
		StateStore:         st,
		PrinterAddr:        "127.0.0.1:9100",
		DownloadDir:        tmpDir,
		IdleQRContent:      "https://kiosk.test/timing",
		ScreenSink:         func(*image.RGBA) error { return nil },
		HealthPollInterval: 20 * time.Millisecond,
		MockHealth:         poller,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	// Must acquire immediately on startup.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() >= 3 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if calls.Load() < 1 {
		t.Fatal("expected immediate startup telemetry acquisition")
	}
	if maxInFlight.Load() > 1 {
		t.Fatalf("expected non-overlapping telemetry cycles, got max concurrent=%d", maxInFlight.Load())
	}

	// Cancel Run so the loop stops, then verify cached CurrentHealthSnapshot does not refresh UpdatedAt
	// and expires deterministically past 6s even without loop execution.
	cancel()
	time.Sleep(30 * time.Millisecond)

	s1 := client.CurrentHealthSnapshot(context.Background())
	s2 := client.CurrentHealthSnapshot(context.Background())
	if !s1.UpdatedAt.Equal(s2.UpdatedAt) {
		t.Fatalf("reading cached health must not refresh UpdatedAt: %v vs %v", s1.UpdatedAt, s2.UpdatedAt)
	}

	client.snapshotMu.Lock()
	client.latestSnap.AcquiredAt = time.Now().UTC().Add(-7 * time.Second)
	client.latestSnap.Health.UpdatedAt = client.latestSnap.AcquiredAt
	expiredAt := client.latestSnap.Health.UpdatedAt
	client.snapshotMu.Unlock()

	s3 := client.CurrentHealthSnapshot(context.Background())
	if s3.EvaluatedHealth != contracts.EvaluatedHealthUnverified || s3.Online {
		t.Fatalf("expected stale >6s cached snapshot to expire to UNVERIFIED offline, got %s online=%v", s3.EvaluatedHealth, s3.Online)
	}
	if !s3.UpdatedAt.Equal(expiredAt) {
		t.Fatalf("stale expiration must preserve original UpdatedAt (%v), got %v", expiredAt, s3.UpdatedAt)
	}
}

func TestSpoolerCanonical_RecoveryStreak3DistinctCycles_ResetsOnIncompleteOrFault(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := state.Open(filepath.Join(tmpDir, "canon_streak.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer st.Close()

	var phase atomic.Int32 // 0=fault, 1=healthy, 2=unverified
	poller := func(ctx context.Context, sheets int) contracts.PrinterHealth {
		switch phase.Load() {
		case 0:
			return contracts.PrinterHealth{
				Online:          true,
				State:           "ERROR",
				EvaluatedHealth: contracts.EvaluatedHealthFault,
				PaperJam:        true,
				PaperTrayStatus: "OK",
				EstimatedSheets: sheets,
				ErrorMessage:    "paper feed jam detected",
				UpdatedAt:       time.Now().UTC(),
			}
		case 2:
			return contracts.PrinterHealth{
				Online:          false,
				State:           "UNKNOWN",
				EvaluatedHealth: contracts.EvaluatedHealthUnverified,
				PaperTrayStatus: "UNKNOWN",
				EstimatedSheets: sheets,
				ErrorMessage:    "SNMP timeout",
				UpdatedAt:       time.Now().UTC(),
			}
		default:
			return testHealthyMockHealth(ctx, sheets)
		}
	}

	client, err := NewClient(ClientConfig{
		CloudURL:                     "ws://127.0.0.1:1/ws",
		KioskID:                      "kiosk-streak",
		StateStore:                   st,
		PrinterAddr:                  "127.0.0.1:9100",
		DownloadDir:                  tmpDir,
		IdleQRContent:                "https://kiosk.test/streak",
		ScreenSink:                   func(*image.RGBA) error { return nil },
		MockHealth:                   poller,
		RequiredRecoveryHealthyCount: 3,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Cycle 1: Fault activates hardware fault barrier.
	_ = client.CheckPrinterHealth(context.Background())
	if !client.isHardwareFaultActive() {
		t.Fatal("expected fault to be active")
	}

	// Healthy cycle 1 + multiple cached reads: streak must remain 1 (never advanced by cached reads).
	phase.Store(1)
	_ = client.CheckPrinterHealth(context.Background())
	_ = client.CurrentHealthSnapshot(context.Background())
	_ = client.CurrentHealthSnapshot(context.Background())
	_ = client.CurrentHealthSnapshot(context.Background())
	if !client.isHardwareFaultActive() {
		t.Fatal("cached reads must not advance recovery streak past 1")
	}

	// Healthy cycle 2: still only 2 distinct healthy cycles.
	_ = client.CheckPrinterHealth(context.Background())
	if !client.isHardwareFaultActive() {
		t.Fatal("expected fault to stay active at streak=2 of 3")
	}

	// Incomplete / unverified evidence resets streak to 0!
	phase.Store(2)
	_ = client.CheckPrinterHealth(context.Background())
	if !client.isHardwareFaultActive() {
		t.Fatal("unverified cycle must keep fault active")
	}

	// Now requires 3 fresh distinct healthy cycles (cycles 1 and 2 must NOT finish recovery; cycle 3 clears).
	phase.Store(1)
	_ = client.CheckPrinterHealth(context.Background())
	if !client.isHardwareFaultActive() {
		t.Fatal("expected streak reset: cycle 1/3 after unverified must not clear fault")
	}
	_ = client.CheckPrinterHealth(context.Background())
	if !client.isHardwareFaultActive() {
		t.Fatal("expected streak reset: cycle 2/3 after unverified must not clear fault")
	}
	_ = client.CheckPrinterHealth(context.Background())
	if client.isHardwareFaultActive() {
		t.Fatal("expected fault to clear cleanly on 3rd distinct consecutive healthy cycle")
	}
}

func TestSpoolerCanonical_MidPrintExpiry_ConservativeUncertaintyPause_WithoutFabricatedFault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	st, err := state.Open(filepath.Join(tmpDir, "canon_midprint.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer st.Close()
	_ = st.ResetPaperTray(ctx, 100)

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	sum := sha256.Sum256(pdfContent)
	pdfSum := hex.EncodeToString(sum[:])

	pdfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(pdfContent)
	}))
	defer pdfSrv.Close()

	var phase atomic.Int32 // 0=healthy, 1=expired/unverified
	mockHealth := func(ctx context.Context, sheets int) contracts.PrinterHealth {
		if phase.Load() == 1 {
			return contracts.PrinterHealth{
				Online:          false,
				State:           "UNKNOWN",
				EvaluatedHealth: contracts.EvaluatedHealthUnverified,
				PaperTrayStatus: "UNKNOWN",
				EstimatedSheets: sheets,
				ErrorMessage:    "required SNMP telemetry expired",
				UpdatedAt:       time.Now().UTC(),
			}
		}
		return testHealthyMockHealth(ctx, sheets)
	}

	var holdCalled atomic.Bool
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			for i, a := range args {
				if a == "-H" && i+1 < len(args) && args[i+1] == "hold" {
					holdCalled.Store(true)
					return []byte(""), nil
				}
			}
			return []byte("request id is HPTank-888 (1 file(s))"), nil
		}
		if name == "ipptool" {
			if holdCalled.Load() {
				return []byte("job-state (enum) = held\njob-state-reasons (keyword) = job-hold-until-specified\n"), nil
			}
			phase.Store(1) // Expire telemetry while actively printing
			time.Sleep(60 * time.Millisecond)
			return []byte("job-state (enum) = processing\n"), nil
		}
		if name == "lpstat" {
			return []byte("HPTank-888 user 1024 Tue Nov 3 14:00:00 2026"), nil
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:           "ws://127.0.0.1:1/ws",
		KioskID:            "kiosk-mid-exp",
		StateStore:         st,
		CUPSQueue:          "HPTank",
		CUPSRunner:         runner,
		DownloadDir:        tmpDir,
		IdleQRContent:      "https://kiosk.test/exp",
		HealthPollInterval: 15 * time.Millisecond,
		ScreenSink:         func(*image.RGBA) error { return nil },
		MockHealth:         mockHealth,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go func() { _ = client.Run(runCtx) }()

	// Wait for initial healthy acquisition before submitting job
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if h := client.CurrentHealthSnapshot(ctx); h.EvaluatedHealth == contracts.EvaluatedHealthHealthy {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cmd := contracts.PrintJobCommand{
		JobID:          "job-exp-mid-01",
		KioskID:        "kiosk-mid-exp",
		DownloadURL:    pdfSrv.URL + "/doc.pdf",
		SHA256Checksum: pdfSum,
		TotalPages:     2,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	client.processJob(ctx, cmd)

	saved, err := st.GetJob(ctx, cmd.JobID)
	if err != nil || saved == nil {
		t.Fatalf("GetJob failed: %v", err)
	}
	if saved.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected PAUSED_ERROR on mid-print expiry, got %s", saved.Status)
	}
	if !holdCalled.Load() {
		t.Fatal("expected active CUPS job to be held on mid-print readiness loss")
	}
	if !strings.Contains(saved.ErrorReason, "unverified") || !strings.Contains(saved.ErrorReason, "buffered sheets may continue printing") {
		t.Fatalf("expected conservative uncertainty & hold-buffer warning without fabricated completion, got %q", saved.ErrorReason)
	}
	for _, fabricated := range []string{"paper jam", "cover open", "output bin"} {
		if strings.Contains(strings.ToLower(saved.ErrorReason), fabricated) {
			t.Fatalf("must not fabricate specific hardware fault %q on expiry: %q", fabricated, saved.ErrorReason)
		}
	}
}

func TestSpoolerCanonical_MissingProductionConfig_NeverFabricatesReady(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := state.Open(filepath.Join(tmpDir, "canon_noconf.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer st.Close()

	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://127.0.0.1:1/ws",
		KioskID:       "kiosk-no-conf",
		StateStore:    st,
		PrinterAddr:   "127.0.0.1:9100",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://kiosk.test/noconf",
		ScreenSink:    func(*image.RGBA) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	h, _, _ := client.acquireTelemetryCycle(context.Background())
	if h.Online || h.EvaluatedHealth != contracts.EvaluatedHealthUnverified || h.State != "UNKNOWN" {
		t.Fatalf("expected missing config to produce UNVERIFIED offline state, got %+v", h)
	}
}

func TestSpoolerCanonical_RefillPreservesPhysicalEmptyFault_AndHeartbeatPassive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	st, err := state.Open(filepath.Join(tmpDir, "canon_refill.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer st.Close()

	var pollerCalls atomic.Int32
	poller := func(ctx context.Context, sheets int) contracts.PrinterHealth {
		pollerCalls.Add(1)
		return contracts.PrinterHealth{
			Online:          true,
			State:           "ERROR",
			EvaluatedHealth: contracts.EvaluatedHealthFault,
			PaperTrayStatus: "EMPTY",
			EstimatedSheets: sheets,
			ExecutionSafe:   true,
			BlockingReasons: []contracts.HealthReason{{Code: "MEDIA_EMPTY", Message: "paper tray is empty", Source: "SNMP"}},
			ErrorMessage:    "paper tray is empty",
			UpdatedAt:       time.Now().UTC(),
		}
	}

	var gotRefilledSnapshot atomic.Bool
	var heartbeats atomic.Int32
	var wsMu sync.Mutex
	var refillOK bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()

		// Send refill command
		_ = wsjson.Write(r.Context(), c, wireMessage{Type: "refill", Sheets: 200})

		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), c, &msg); err != nil {
				return
			}
			if msg.Type == "heartbeat" {
				heartbeats.Add(1)
			}
			if msg.Type == "health" && msg.Health != nil && msg.Health.EstimatedSheets == 200 {
				wsMu.Lock()
				// Physical EMPTY fault MUST NOT be overwritten with PaperTrayStatus="OK"
				refillOK = msg.Health.PaperTrayStatus == "EMPTY" &&
					msg.Health.EvaluatedHealth == contracts.EvaluatedHealthFault &&
					msg.Health.InventoryCalibrated
				gotRefilledSnapshot.Store(true)
				wsMu.Unlock()
			}
		}
	}))
	defer srv.Close()

	client, err := NewClient(ClientConfig{
		CloudURL:           "ws" + strings.TrimPrefix(srv.URL, "http"),
		KioskID:            "kiosk-refill-safe",
		StateStore:         st,
		PrinterAddr:        "127.0.0.1:9100",
		DownloadDir:        tmpDir,
		IdleQRContent:      "https://kiosk.test/refill",
		ScreenSink:         func(*image.RGBA) error { return nil },
		HealthPollInterval: 5 * time.Second, // long telemetry interval vs fast 20ms heartbeats
		HeartbeatInterval:  20 * time.Millisecond,
		MockHealth:         poller,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go func() { _ = client.Run(runCtx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if gotRefilledSnapshot.Load() && heartbeats.Load() >= 3 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	wsMu.Lock()
	ok := refillOK
	wsMu.Unlock()
	if !gotRefilledSnapshot.Load() || !ok {
		t.Fatal("refill must publish updated inventory while preserving physical EMPTY/FAULT status")
	}
	if heartbeats.Load() < 3 {
		t.Fatalf("expected >= 3 keepalive heartbeats, got %d", heartbeats.Load())
	}
	// With 5s telemetry interval, heartbeats (20ms) must NEVER have triggered extra poller calls beyond startup (1).
	if pollerCalls.Load() != 1 {
		t.Fatalf("heartbeats must not trigger hardware acquisition; expected 1 startup call, got %d", pollerCalls.Load())
	}
}

func TestSpoolerCanonical_DurableExecutionBarrier_BlocksDrainAndSubmissionAfterHardwareRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	st, err := state.Open(filepath.Join(tmpDir, "canon_barrier.db"))
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer st.Close()
	_ = st.ResetPaperTray(ctx, 100)

	// Unresolved paused job with submitted_to_printer=1 from prior session
	pausedCmd := contracts.PrintJobCommand{
		JobID:          "job-unresolved-paused",
		KioskID:        "kiosk-barrier",
		DownloadURL:    "http://127.0.0.1:1/old.pdf",
		SHA256Checksum: strings.Repeat("a", 64),
		TotalPages:     2,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	_ = st.SaveJob(ctx, pausedCmd, contracts.JobStatusPausedError)
	_ = st.MarkJobSubmitting(ctx, pausedCmd.JobID)

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	sum := sha256.Sum256(pdfContent)
	pdfSum := hex.EncodeToString(sum[:])
	pdfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(pdfContent)
	}))
	defer pdfSrv.Close()

	var lpInvoked atomic.Bool
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "lp" {
			lpInvoked.Store(true)
			return []byte("request id is HPTank-999 (1 file(s))"), nil
		}
		return []byte(""), nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://127.0.0.1:1/ws",
		KioskID:       "kiosk-barrier",
		StateStore:    st,
		CUPSQueue:     "HPTank",
		CUPSRunner:    runner,
		DownloadDir:   tmpDir,
		IdleQRContent: "https://kiosk.test/barrier",
		ScreenSink:    func(*image.RGBA) error { return nil },
		MockHealth:    testHealthyMockHealth,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Acquire 3 healthy cycles cleanly: hardware is HEALTHY, yet durable barrier remains blocking!
	for i := 0; i < 3; i++ {
		h, _, _ := client.acquireTelemetryCycle(ctx)
		if h.EvaluatedHealth != contracts.EvaluatedHealthHealthy || h.ExecutionSafe {
			t.Fatalf("expected healthy hardware with ExecutionSafe=false due to durable barrier, got %+v", h)
		}
	}

	// Defer a new job and attempt drainDeferredJobsIfSafe when kiosk has no active customer or print worker:
	// MUST NOT admit work while execution-safety barrier is unresolved.
	newCmd := contracts.PrintJobCommand{
		JobID:          "job-new-deferred",
		KioskID:        "kiosk-barrier",
		DownloadURL:    pdfSrv.URL + "/new.pdf",
		SHA256Checksum: pdfSum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	client.queueMu.Lock()
	client.deferredJobs = append(client.deferredJobs, newCmd)
	client.queueMu.Unlock()

	client.drainDeferredJobsIfSafe(ctx)
	if len(client.jobQueue) != 0 {
		t.Fatalf("drainDeferredJobsIfSafe must not admit jobs while execution-safety barrier is active, queued=%d", len(client.jobQueue))
	}

	// Direct submission of newCmd must also be rejected before calling lp!
	client.processJob(ctx, newCmd)
	if lpInvoked.Load() {
		t.Fatal("lp must NEVER be called while unresolved paused job barrier exists after hardware recovery")
	}
}
