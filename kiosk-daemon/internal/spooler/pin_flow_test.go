package spooler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/input"
	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/kiosk-daemon/internal/ui_ipc"
	"xeroxgo/pkg/contracts"
)

func waitCondition(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func TestPINFlow_EndToEndSimulation(t *testing.T) {
	tempDir := t.TempDir()
	stateDBPath := filepath.Join(tempDir, "state.db")
	store, err := state.Open(stateDBPath)
	if err != nil {
		t.Fatalf("failed to open state db: %v", err)
	}
	defer store.Close()

	// Mock cloud server handling WebSocket and PIN lookup
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. PIN Lookup endpoint
		if r.URL.Path == "/api/k/kiosk-e2e/orders/lookup" {
			auth := r.Header.Get("Authorization")
			if auth != "Bearer e2e-secret" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			var body struct {
				PIN string `json:"pin"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid body", http.StatusBadRequest)
				return
			}

			switch body.PIN {
			case "8899":
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"order_id":          "ord_e2e_valid",
					"kiosk_id":          "kiosk-e2e",
					"status":            "PENDING_PAYMENT",
					"total_price_paise": 2400,
					"total_pages":       8,
					"total_sheets":      4,
					"copies":            1,
					"duplex":            "long_edge",
					"color_mode":        "mono",
					"pin_expires_at":    time.Now().Add(12 * time.Minute).Format(time.RFC3339),
					"seconds_remaining": 720,
				})
			case "4444":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": "invalid or expired PIN",
				})
			default:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": "invalid or expired PIN",
				})
			}
			return
		}

		// 2. Mock ads endpoint
		if r.URL.Path == "/api/ads" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ads":[]}`))
			return
		}

		http.NotFound(w, r)
	}))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	simInput := input.NewSimulatedSource(64)
	defer simInput.Close()

	screenSink := func(img *image.RGBA) error {
		return nil
	}

	cfg := ClientConfig{
		CloudURL:                 wsURL,
		KioskID:                  "kiosk-e2e",
		KioskSecret:              "e2e-secret",
		StateStore:               store,
		PrinterAddr:              "127.0.0.1:9100",
		DownloadDir:              filepath.Join(tempDir, "downloads"),
		IdleQRContent:            "https://xeroxgo.demo/k/kiosk-e2e",
		AdsCachePath:             filepath.Join(tempDir, "ads.json"),
		CompletedDisplayDuration: 1 * time.Second,
		ScreenSink:               screenSink,
		InputSource:              simInput,
		InactivityTimeout:        10 * time.Second,
		SummaryTimeout:           10 * time.Second,
	}
	_ = os.MkdirAll(cfg.DownloadDir, 0700)

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	pinCtrl := client.PINController()
	if pinCtrl == nil {
		t.Fatalf("expected non-nil PINController on client")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	// Step 1: Input valid 4-digit PIN "8899"
	simInput.EmitDigits("8899")
	if !waitCondition(5*time.Second, func() bool { return pinCtrl.GetPINBuffer() == "8899" }) {
		t.Fatalf("expected buffer 8899, got %s", pinCtrl.GetPINBuffer())
	}

	// Submit PIN
	simInput.EmitSubmit()
	if !waitCondition(5*time.Second, func() bool { return pinCtrl.GetState() == "ORDER_SUMMARY" }) {
		t.Fatalf("expected state ORDER_SUMMARY, got %s", pinCtrl.GetState())
	}

	history := client.RenderHistory()
	if len(history) == 0 {
		t.Fatalf("expected rendered states in client history")
	}
	lastState := history[len(history)-1]

	if lastState.Mode != ui_ipc.ModeOrderSummary {
		t.Errorf("expected mode ORDER_SUMMARY, got %s", lastState.Mode)
	}
	if lastState.OrderSummary == nil {
		t.Fatalf("expected non-nil OrderSummary")
	}
	if lastState.OrderSummary.OrderID != "ord_e2e_valid" {
		t.Errorf("expected orderId ord_e2e_valid, got %s", lastState.OrderSummary.OrderID)
	}
	if lastState.OrderSummary.TotalPricePaise != 2400 {
		t.Errorf("expected price 2400 paise, got %d", lastState.OrderSummary.TotalPricePaise)
	}
	if lastState.OrderSummary.TotalPages != 8 {
		t.Errorf("expected total pages 8, got %d", lastState.OrderSummary.TotalPages)
	}
	if lastState.OrderSummary.TotalSheets != 4 {
		t.Errorf("expected total sheets 4, got %d", lastState.OrderSummary.TotalSheets)
	}

	// Step 2: Cancel from order summary returns to idle
	simInput.EmitCancel()
	if !waitCondition(5*time.Second, func() bool { return pinCtrl.GetState() == "IDLE" }) {
		t.Errorf("expected state IDLE after cancel, got %s", pinCtrl.GetState())
	}

	// Step 3: Input invalid PIN "4444"
	simInput.EmitDigits("4444")
	if !waitCondition(5*time.Second, func() bool { return pinCtrl.GetPINBuffer() == "4444" }) {
		t.Fatalf("expected buffer 4444, got %s", pinCtrl.GetPINBuffer())
	}
	simInput.EmitSubmit()
	if !waitCondition(5*time.Second, func() bool { return pinCtrl.GetPINError() != "" }) {
		t.Fatalf("expected PINError set after invalid PIN")
	}

	if pinCtrl.GetState() != "PIN_ENTRY" {
		t.Errorf("expected state PIN_ENTRY after invalid PIN, got %s", pinCtrl.GetState())
	}

	if pinCtrl.GetPINError() != "Invalid or expired PIN. Please try again." {
		t.Errorf("expected error 'Invalid or expired PIN. Please try again.', got %q", pinCtrl.GetPINError())
	}

	// Step 4: Cancel returns to IDLE
	simInput.EmitCancel()
	if !waitCondition(5*time.Second, func() bool { return pinCtrl.GetState() == "IDLE" }) {
		t.Errorf("expected state IDLE after cancel, got %s", pinCtrl.GetState())
	}
}

func TestPINFlow_KioskIsolationAndUnauthorized(t *testing.T) {
	tempDir := t.TempDir()
	store, _ := state.Open(filepath.Join(tempDir, "state.db"))
	defer store.Close()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer kiosk-secret-A" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/api/k/kiosk-A/orders/lookup" {
			// Cross-kiosk lookup forbidden
			http.Error(w, "forbidden kiosk", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"order_id": "ord_isolated",
			"kiosk_id": "kiosk-A",
			"status": "PENDING_PAYMENT",
			"total_price_paise": 500,
			"total_pages": 1,
			"total_sheets": 1,
			"copies": 1,
			"duplex": "simplex",
			"color_mode": "mono"
		}`))
	}))
	defer ts.Close()

	simInput := input.NewSimulatedSource(16)
	defer simInput.Close()

	// Configure client for kiosk-B with secret-B talking to same server
	cfgB := ClientConfig{
		CloudURL:          "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws",
		KioskID:           "kiosk-B",
		KioskSecret:       "kiosk-secret-B", // wrong secret for server
		StateStore:        store,
		PrinterAddr:       "127.0.0.1:9100",
		DownloadDir:       filepath.Join(tempDir, "dl"),
		IdleQRContent:     "https://kiosk-b",
		ScreenSink:        func(img *image.RGBA) error { return nil },
		InputSource:       simInput,
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
	}
	_ = os.MkdirAll(cfgB.DownloadDir, 0700)

	clientB, err := NewClient(cfgB)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctrlB := clientB.PINController()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = clientB.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)

	simInput.EmitDigits("1234")
	if !waitCondition(5*time.Second, func() bool { return ctrlB.GetPINBuffer() == "1234" }) {
		t.Fatalf("expected buffer 1234, got %s", ctrlB.GetPINBuffer())
	}
	simInput.EmitSubmit()

	if !waitCondition(5*time.Second, func() bool { return ctrlB.GetPINError() != "" }) {
		t.Fatalf("expected PINError on auth failure")
	}

	// Must fail with authentication error and stay on PIN_ENTRY
	if ctrlB.GetState() != "PIN_ENTRY" {
		t.Errorf("expected PIN_ENTRY on auth failure, got %s", ctrlB.GetState())
	}
	if ctrlB.GetPINError() != "Kiosk authentication error. Please see attendant." {
		t.Errorf("expected auth error message, got %q", ctrlB.GetPINError())
	}
}

func TestPINFlow_ReadOnlyInvariant_NoPrintingOrPaidMutation(t *testing.T) {
	tempDir := t.TempDir()
	store, _ := state.Open(filepath.Join(tempDir, "state.db"))
	defer store.Close()

	cupsCalled := false
	var cupsMu sync.Mutex

	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(int)) error {
		cupsMu.Lock()
		cupsCalled = true
		cupsMu.Unlock()
		return nil
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"order_id":          "ord_readonly",
			"kiosk_id":          "kiosk-ro",
			"status":            "PENDING_PAYMENT",
			"total_price_paise": 1000,
			"total_pages":       2,
			"total_sheets":      1,
			"copies":            1,
			"duplex":            "simplex",
			"color_mode":        "mono",
		})
	}))
	defer ts.Close()

	simInput := input.NewSimulatedSource(16)
	defer simInput.Close()

	cfg := ClientConfig{
		CloudURL:          "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws",
		KioskID:           "kiosk-ro",
		StateStore:        store,
		PrinterAddr:       "127.0.0.1:9100",
		CUPSQueue:         "test-queue",
		MockCUPSPrint:     mockCUPS,
		DownloadDir:       filepath.Join(tempDir, "dl_ro"),
		IdleQRContent:     "https://kiosk-ro",
		ScreenSink:        func(img *image.RGBA) error { return nil },
		InputSource:       simInput,
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
	}
	_ = os.MkdirAll(cfg.DownloadDir, 0700)

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)

	simInput.EmitDigits("1231")
	if !waitCondition(5*time.Second, func() bool { return client.PINController().GetPINBuffer() == "1231" }) {
		t.Fatalf("expected buffer 1231, got %s", client.PINController().GetPINBuffer())
	}
	simInput.EmitSubmit()

	if !waitCondition(5*time.Second, func() bool { return client.PINController().GetState() == "ORDER_SUMMARY" }) {
		t.Fatalf("expected state ORDER_SUMMARY, got %s", client.PINController().GetState())
	}

	// User submits on order summary
	simInput.EmitSubmit()
	time.Sleep(50 * time.Millisecond)

	// CUPS print function must NEVER have been called!
	cupsMu.Lock()
	wasCalled := cupsCalled
	cupsMu.Unlock()

	if wasCalled {
		t.Errorf("INVARIANT VIOLATION: CUPS print was invoked by PIN lookup or confirmation!")
	}

	// State store should contain 0 active jobs
	job, err := store.GetJob(context.Background(), "ord_readonly")
	if err == nil && job != nil {
		t.Errorf("INVARIANT VIOLATION: order was saved to kiosk state store by PIN lookup!")
	}
}

func TestPINFlow_OrderConfirmAndUnrelatedJobProtection(t *testing.T) {
	tempDir := t.TempDir()
	st, _ := state.Open(filepath.Join(tempDir, "state.db"))
	defer st.Close()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/k/kiosk-prot/orders/lookup":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"order_id":          "ord_prot_01",
				"kiosk_id":          "kiosk-prot",
				"status":            "PENDING_PAYMENT",
				"total_price_paise": 3000,
				"total_pages":       6,
				"total_sheets":      3,
				"copies":            1,
				"duplex":            "simplex",
				"color_mode":        "mono",
				"seconds_remaining": 600,
			})
		case r.URL.Path == "/api/k/kiosk-prot/orders/ord_prot_01/claim":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"session_id":        "sess_prot_123",
				"order_id":          "ord_prot_01",
				"kiosk_id":          "kiosk-prot",
				"status":            "PAYMENT_PENDING",
				"total_price_paise": 3000,
				"seconds_remaining": 300,
			})
		case strings.HasPrefix(r.URL.Path, "/api/k/kiosk-prot/sessions/") && strings.HasSuffix(r.URL.Path, "/cancel"):
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	simInput := input.NewSimulatedSource(16)
	defer simInput.Close()

	cfg := ClientConfig{
		CloudURL:          "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws",
		KioskID:           "kiosk-prot",
		KioskSecret:       "prot-secret",
		StateStore:        st,
		PrinterAddr:       "127.0.0.1:9100",
		DownloadDir:       filepath.Join(tempDir, "dl_prot"),
		IdleQRContent:     "https://kiosk-prot",
		ScreenSink:        func(img *image.RGBA) error { return nil },
		InputSource:       simInput,
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
		PaymentTimeout:    10 * time.Second,
	}
	_ = os.MkdirAll(cfg.DownloadDir, 0700)

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)

	// 1. Enter PIN & lookup -> ORDER_SUMMARY
	simInput.EmitDigits("9876")
	if !waitCondition(5*time.Second, func() bool { return client.PINController().GetPINBuffer() == "9876" }) {
		t.Fatalf("expected buffer 9876, got %s", client.PINController().GetPINBuffer())
	}
	simInput.EmitSubmit()
	if !waitCondition(5*time.Second, func() bool { return client.PINController().GetState() == "ORDER_SUMMARY" }) {
		t.Fatalf("expected state ORDER_SUMMARY, got %s", client.PINController().GetState())
	}

	// 2. Confirm order -> PAYMENT_PENDING
	simInput.EmitSubmit()
	if !waitCondition(5*time.Second, func() bool { return client.PINController().GetState() == "PAYMENT_PENDING" }) {
		t.Fatalf("expected state PAYMENT_PENDING, got %s", client.PINController().GetState())
	}

	// Verify Payment Pending screen state
	hist := client.RenderHistory()
	if len(hist) == 0 || hist[len(hist)-1].Mode != ui_ipc.ModePaymentPending {
		t.Fatalf("expected last rendered mode PAYMENT_PENDING, got %v", hist[len(hist)-1].Mode)
	}
	if hist[len(hist)-1].SessionID != "sess_prot_123" {
		t.Errorf("expected session ID sess_prot_123, got %s", hist[len(hist)-1].SessionID)
	}

	// 3. Inject an unrelated incoming print job into the job queue
	unrelatedCmd := contracts.PrintJobCommand{
		JobID:          "job_unrelated_888",
		KioskID:        "kiosk-prot",
		DownloadURL:    "https://storage.test/dummy.pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	select {
	case client.jobQueue <- unrelatedCmd:
	default:
		t.Fatalf("failed to queue unrelated job")
	}

	// Sleep briefly to let worker process
	time.Sleep(100 * time.Millisecond)

	// Invariant: Controller MUST still be in PAYMENT_PENDING! Unrelated job must NOT clear active transaction!
	if client.PINController().GetState() != "PAYMENT_PENDING" {
		t.Errorf("INVARIANT VIOLATION: unrelated job cleared active transaction state to %s", client.PINController().GetState())
	}
	lastMode := client.currentMode
	if lastMode != ui_ipc.ModePaymentPending {
		t.Errorf("INVARIANT VIOLATION: unrelated job changed screen mode to %s", lastMode)
	}

	// 4. User cancels active transaction -> returns to IDLE
	simInput.EmitCancel()
	if !waitCondition(5*time.Second, func() bool { return client.PINController().GetState() == "IDLE" }) {
		t.Errorf("expected state IDLE after cancel, got %s", client.PINController().GetState())
	}
}

func TestPINFlow_ConfirmInitiatesPaymentAndRendersDynamicQR(t *testing.T) {
	tempDir := t.TempDir()
	st, _ := state.Open(filepath.Join(tempDir, "state.db"))
	defer st.Close()

	var initiateCalled bool
	var initiateMu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/k/kiosk-qr/orders/lookup":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"order_id":          "ord_qr_01",
				"kiosk_id":          "kiosk-qr",
				"status":            "PENDING_PAYMENT",
				"total_price_paise": 1500,
				"total_pages":       2,
				"total_sheets":      1,
				"copies":            1,
				"duplex":            "simplex",
				"color_mode":        "mono",
				"seconds_remaining": 600,
			})
		case r.URL.Path == "/api/k/kiosk-qr/orders/ord_qr_01/claim":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"session_id":        "sess_qr_789",
				"order_id":          "ord_qr_01",
				"kiosk_id":          "kiosk-qr",
				"status":            "PAYMENT_PENDING",
				"total_price_paise": 1500,
				"seconds_remaining": 300,
			})
		case r.URL.Path == "/api/k/kiosk-qr/sessions/sess_qr_789/initiate-payment":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			initiateMu.Lock()
			initiateCalled = true
			initiateMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"attempt_id":   "att_qr_999",
				"session_id":   "sess_qr_789",
				"order_id":     "ord_qr_01",
				"kiosk_id":     "kiosk-qr",
				"provider":     "mock",
				"amount_paise": 1500,
				"status":       "INITIATED",
				"qr_payload":   "https://xeroxgo.demo/pay/tok_dynamic_qr",
				"demo_token":   "tok_dynamic_qr",
			})
		case strings.HasPrefix(r.URL.Path, "/api/k/kiosk-qr/sessions/") && strings.HasSuffix(r.URL.Path, "/cancel"):
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	simInput := input.NewSimulatedSource(16)
	defer simInput.Close()

	cfg := ClientConfig{
		CloudURL:          "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws",
		KioskID:           "kiosk-qr",
		KioskSecret:       "qr-secret",
		StateStore:        st,
		PrinterAddr:       "127.0.0.1:9100",
		DownloadDir:       filepath.Join(tempDir, "dl_qr"),
		IdleQRContent:     "https://kiosk-qr",
		ScreenSink:        func(img *image.RGBA) error { return nil },
		InputSource:       simInput,
		InactivityTimeout: 10 * time.Second,
		SummaryTimeout:    10 * time.Second,
		PaymentTimeout:    10 * time.Second,
	}
	_ = os.MkdirAll(cfg.DownloadDir, 0700)

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)

	// 1. Enter PIN & lookup -> ORDER_SUMMARY
	simInput.EmitDigits("1122")
	if !waitCondition(5*time.Second, func() bool { return client.PINController().GetPINBuffer() == "1122" }) {
		t.Fatalf("expected buffer 1122, got %s", client.PINController().GetPINBuffer())
	}
	simInput.EmitSubmit()
	if !waitCondition(5*time.Second, func() bool { return client.PINController().GetState() == "ORDER_SUMMARY" }) {
		t.Fatalf("expected state ORDER_SUMMARY, got %s", client.PINController().GetState())
	}

	// 2. Confirm order -> PAYMENT_PENDING with dynamic QR
	simInput.EmitSubmit()
	if !waitCondition(5*time.Second, func() bool { return client.PINController().GetState() == "PAYMENT_PENDING" }) {
		t.Fatalf("expected state PAYMENT_PENDING, got %s", client.PINController().GetState())
	}

	// 3. Verify that InitiatePayment was called via HTTPPINClient
	if !waitCondition(5*time.Second, func() bool {
		initiateMu.Lock()
		defer initiateMu.Unlock()
		return initiateCalled
	}) {
		t.Fatalf("InitiatePayment was not called on /sessions/sess_qr_789/initiate-payment")
	}

	// 4. Verify that dynamic QR content is delivered to the rendered screen state
	if !waitCondition(5*time.Second, func() bool {
		hist := client.RenderHistory()
		if len(hist) == 0 {
			return false
		}
		last := hist[len(hist)-1]
		return last.Mode == ui_ipc.ModePaymentPending && last.QRContent == "https://xeroxgo.demo/pay/tok_dynamic_qr"
	}) {
		hist := client.RenderHistory()
		last := hist[len(hist)-1]
		t.Fatalf("expected rendered state with dynamic QR payload, got mode=%s qr=%s error=%s", last.Mode, last.QRContent, client.PINController().GetPINError())
	}

	if client.PINController().GetPINError() != "" {
		t.Errorf("expected empty PIN error, got %q", client.PINController().GetPINError())
	}
}

func TestSpooler_ExactURLPathCorrelation(t *testing.T) {
	// 1. extractOrderIDFromDownloadURL exact matching tests
	tests := []struct {
		name     string
		url      string
		expected string
	}{
		{"valid standard order URL", "http://localhost:8080/api/orders/ord_123/pdf", "ord_123"},
		{"valid https order URL", "https://cloud.xeroxgo.demo/api/orders/ord_abc-xyz_99/pdf", "ord_abc-xyz_99"},
		{"prefix collision rejected", "http://localhost:8080/api/orders/ord_123_evil/pdf", "ord_123_evil"},
		{"query collision rejected", "http://localhost:8080/api/orders/ord_123?ext=pdf", ""},
		{"extra path segments rejected", "http://localhost:8080/api/orders/ord_123/pdf/extra", ""},
		{"missing pdf segment rejected", "http://localhost:8080/api/orders/ord_123", ""},
		{"query parameter containing path rejected", "http://localhost:8080/api/other?q=/api/orders/ord_123/pdf", ""},
		{"recovery URL returns empty string", "http://localhost:8080/api/recovery/rec_456/pdf", ""},
		{"malformed URL returns empty string", "http://[::1:invalid/api/orders/ord_123/pdf", ""},
		{"empty string returns empty string", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractOrderIDFromDownloadURL(tc.url)
			if got != tc.expected {
				t.Errorf("extractOrderIDFromDownloadURL(%q) = %q, want %q", tc.url, got, tc.expected)
			}
		})
	}

	// 2. isJobForActiveOrder tests
	activeOrder := "ord_active_target"

	// Match by JobID
	if !isJobForActiveOrder(contracts.PrintJobCommand{JobID: activeOrder}, activeOrder) {
		t.Errorf("expected isJobForActiveOrder to match when JobID == activeOrderID")
	}

	// Match by exact download URL path segment
	if !isJobForActiveOrder(contracts.PrintJobCommand{
		JobID:       "job_unique_uuid_1",
		DownloadURL: "http://127.0.0.1:8080/api/orders/ord_active_target/pdf",
	}, activeOrder) {
		t.Errorf("expected isJobForActiveOrder to match by exact parsed path segment")
	}

	// Reject substring / prefix false positives
	if isJobForActiveOrder(contracts.PrintJobCommand{
		JobID:       "job_unique_uuid_2",
		DownloadURL: "http://127.0.0.1:8080/api/orders/ord_active_target_evil/pdf",
	}, activeOrder) {
		t.Errorf("expected isJobForActiveOrder to reject prefix false match")
	}

	// Reject recovery URLs pretending to be order IDs
	if isJobForActiveOrder(contracts.PrintJobCommand{
		JobID:       "job_unique_uuid_3",
		DownloadURL: "http://127.0.0.1:8080/api/recovery/ord_active_target/pdf",
	}, activeOrder) {
		t.Errorf("expected isJobForActiveOrder to reject recovery URL false match")
	}
}

func TestSpooler_TrustedOriginValidation(t *testing.T) {
	client := &Client{
		cfg: ClientConfig{
			CloudURL:       "http://127.0.0.1:8080/ws",
			TrustedOrigins: []string{"cdn.xeroxgo.demo", "storage.internal:8443"},
		},
	}

	// Trusted origins
	validURLs := []string{
		"http://127.0.0.1:8080/api/orders/ord1/pdf",
		"http://localhost:8080/api/orders/ord1/pdf",
		"http://127.0.0.1:9090/api/orders/ord1/pdf",
		"https://cdn.xeroxgo.demo/api/orders/ord1/pdf",
		"https://storage.internal:8443/api/orders/ord1/pdf",
		"https://storage.example.com/api/orders/ord1/pdf",
		"https://test.example.invalid/api/orders/ord1/pdf",
	}

	for _, u := range validURLs {
		if err := client.validateDownloadURL(u); err != nil {
			t.Errorf("expected %q to be trusted, got error: %v", u, err)
		}
	}

	// Untrusted / Malformed origins
	invalidURLs := []struct {
		url     string
		wantErr string
	}{
		{"", "download URL is empty"},
		{"http://evil-attacker.com/api/orders/ord1/pdf", "not trusted"},
		{"ftp://127.0.0.1:8080/api/orders/ord1/pdf", "unsupported download URL scheme"},
		{"http://user:pass@127.0.0.1:8080/api/orders/ord1/pdf", "cannot contain user credentials"},
		{"://bad-url", "invalid download URL"},
	}

	for _, tc := range invalidURLs {
		err := client.validateDownloadURL(tc.url)
		if err == nil {
			t.Errorf("expected error for URL %q, got nil", tc.url)
		} else if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("for URL %q expected error containing %q, got %q", tc.url, tc.wantErr, err.Error())
		}
	}
}

func TestSpooler_UnrelatedJobBeforeActive_NoDeadlock(t *testing.T) {
	tempDir := t.TempDir()
	st, err := state.Open(filepath.Join(tempDir, "state.db"))
	if err != nil {
		t.Fatalf("failed to open state db: %v", err)
	}
	defer st.Close()

	// 1. Create a dummy test PDF
	pdfBytes := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF\n")
	h := sha256.Sum256(pdfBytes)
	checksum := hex.EncodeToString(h[:])

	// 2. Setup mock HTTP server for PIN lookup and PDF downloads
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/k/kiosk-deadlock-test/orders/lookup":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"order_id":          "ord_active_deadlock",
				"kiosk_id":          "kiosk-deadlock-test",
				"status":            "PENDING_PAYMENT",
				"total_price_paise": 2000,
				"total_pages":       1,
				"total_sheets":      1,
				"copies":            1,
				"duplex":            "simplex",
				"color_mode":        "mono",
				"seconds_remaining": 600,
			})
		case r.URL.Path == "/api/k/kiosk-deadlock-test/orders/ord_active_deadlock/claim":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"session_id":        "sess_deadlock_123",
				"order_id":          "ord_active_deadlock",
				"kiosk_id":          "kiosk-deadlock-test",
				"status":            "PAYMENT_PENDING",
				"total_price_paise": 2000,
				"seconds_remaining": 300,
			})
		case strings.HasSuffix(r.URL.Path, "/pdf"):
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write(pdfBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	// 3. Setup mock PJL printer
	printerLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("printer listener failed: %v", err)
	}
	defer printerLn.Close()

	go func() {
		for {
			conn, err := printerLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(io.Discard, c)
				_, _ = c.Write([]byte("@PJL USTATUS PAGE\r\n1\r\n@PJL USTATUS JOB\r\nEND\r\n"))
			}(conn)
		}
	}()

	simInput := input.NewSimulatedSource(16)
	defer simInput.Close()

	completedJobs := make(chan string, 10)

	cfg := ClientConfig{
		CloudURL:                 "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws",
		KioskID:                  "kiosk-deadlock-test",
		KioskSecret:              "secret-deadlock",
		StateStore:               st,
		PrinterAddr:              printerLn.Addr().String(),
		DownloadDir:              filepath.Join(tempDir, "downloads"),
		IdleQRContent:            "https://kiosk-deadlock",
		ScreenSink:               func(img *image.RGBA) error { return nil },
		InputSource:              simInput,
		InactivityTimeout:        10 * time.Second,
		SummaryTimeout:           10 * time.Second,
		PaymentTimeout:           10 * time.Second,
		CompletedDisplayDuration: 100 * time.Millisecond,
		OnJobCompleted: func(jobID string) {
			completedJobs <- jobID
		},
	}
	_ = os.MkdirAll(cfg.DownloadDir, 0700)

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)

	// 4. Customer enters PIN "5555" -> claims order -> PAYMENT_PENDING
	simInput.EmitDigits("5555")
	if !waitCondition(5*time.Second, func() bool { return client.PINController().GetPINBuffer() == "5555" }) {
		t.Fatalf("expected buffer 5555, got %s", client.PINController().GetPINBuffer())
	}
	simInput.EmitSubmit()
	if !waitCondition(5*time.Second, func() bool { return client.PINController().GetState() == "ORDER_SUMMARY" }) {
		t.Fatalf("expected state ORDER_SUMMARY, got %s", client.PINController().GetState())
	}
	simInput.EmitSubmit()
	if !waitCondition(5*time.Second, func() bool { return client.PINController().GetState() == "PAYMENT_PENDING" }) {
		t.Fatalf("expected state PAYMENT_PENDING, got %s", client.PINController().GetState())
	}

	// 5. Unrelated job arrives FIRST while customer is looking at QR payment screen
	unrelatedCmd := contracts.PrintJobCommand{
		JobID:          "job_unrelated_first",
		KioskID:        "kiosk-deadlock-test",
		DownloadURL:    ts.URL + "/api/orders/ord_unrelated_other/pdf",
		SHA256Checksum: checksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	client.handleIncomingJob(unrelatedCmd)

	// Wait briefly: customer transaction must NOT be interrupted
	time.Sleep(100 * time.Millisecond)
	if client.PINController().GetState() != "PAYMENT_PENDING" {
		t.Fatalf("unrelated job interrupted active customer transaction! State is %s", client.PINController().GetState())
	}
	if !client.PINController().HasActiveTransaction() {
		t.Fatalf("expected HasActiveTransaction to remain true")
	}

	// 6. Active customer's job arrives SECOND
	activeCmd := contracts.PrintJobCommand{
		JobID:          "job_active_customer",
		KioskID:        "kiosk-deadlock-test",
		DownloadURL:    ts.URL + "/api/orders/ord_active_deadlock/pdf",
		SHA256Checksum: checksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	client.handleIncomingJob(activeCmd)

	// 7. Verify active job completes immediately without deadlock!
	select {
	case jobID := <-completedJobs:
		if jobID != "job_active_customer" {
			t.Fatalf("expected active customer job to complete first, got %s", jobID)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("DEADLOCK DETECTED: active job failed to complete within 5 seconds behind unrelated job")
	}

	// 8. Verify that after active job finishes, the deferred unrelated job is processed and completes
	select {
	case jobID := <-completedJobs:
		if jobID != "job_unrelated_first" {
			t.Fatalf("expected deferred unrelated job to complete, got %s", jobID)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("TIMED OUT waiting for deferred unrelated job to be processed")
	}
}
