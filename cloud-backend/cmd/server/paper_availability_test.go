package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/cloud-backend/internal/ws"
	"xeroxgo/pkg/contracts"
)

type wireMsg struct {
	Type     string                      `json:"type"`
	Job      *contracts.PrintJobCommand  `json:"job,omitempty"`
	Progress *contracts.JobProgressEvent `json:"progress,omitempty"`
	Health   *contracts.PrinterHealth    `json:"health,omitempty"`
	Sheets   int                         `json:"sheets,omitempty"`
	Error    string                      `json:"error,omitempty"`
}

func connectKioskWithHealth(t *testing.T, srvURL, kioskID string, initialHealth contracts.PrinterHealth) (*websocket.Conn, context.CancelFunc) {
	t.Helper()
	u, err := url.Parse(srvURL)
	if err != nil {
		t.Fatalf("invalid srvURL: %v", err)
	}
	u.Scheme = "ws"
	u.Path = "/ws"
	q := u.Query()
	q.Set("kiosk_id", kioskID)
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithCancel(context.Background())
	conn, resp, err := websocket.Dial(ctx, u.String(), nil)
	if err != nil {
		cancel()
		t.Fatalf("kiosk websocket dial failed: %v", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	// Send initial health observation
	writeCtx, wCancel := context.WithTimeout(ctx, 2*time.Second)
	defer wCancel()
	if err := wsjson.Write(writeCtx, conn, wireMsg{Type: "health", Health: &initialHealth}); err != nil {
		cancel()
		t.Fatalf("failed to send initial health: %v", err)
	}

	// Give hub a moment to update health map
	time.Sleep(50 * time.Millisecond)

	return conn, cancel
}

func setupPaperTestServer(t *testing.T) (*appServer, *httptest.Server, *store.Store, string) {
	t.Helper()
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "paper_avail.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	secret := "paper-test-secret"
	hub := ws.NewHub(st)
	app := &appServer{
		cfg: &serverConfig{
			kioskSecret:   []byte(secret),
			webhookSecret: []byte("whsec_paper"),
			devMode:       false, // Production mode: enforces strict health and presence checks!
		},
		db:  st,
		hub: hub,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.ServeHTTP)
	mux.HandleFunc("/api/k/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/claim"), strings.HasSuffix(path, "/confirm"):
			app.handleOrderClaim(w, r)
		case strings.HasSuffix(path, "/initiate-payment"):
			app.handleInitiatePayment(w, r)
		case strings.HasSuffix(path, "/release"):
			app.handleOrderRelease(w, r)
		case strings.HasSuffix(path, "/refill"):
			app.handleKioskRefill(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	server := httptest.NewServer(mux)
	t.Cleanup(func() { server.Close() })

	return app, server, st, secret
}

func TestPaperAvailability_OrderClaim_Guards(t *testing.T) {
	_, server, st, secret := setupPaperTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	kioskID := "kiosk-paper-guards"

	// Create test order requiring 10 physical sheets
	seedTestOrder(t, st, "ord_guard_1", kioskID, "3001", 10, contracts.JobStatusPendingPayment, 2000, now.Add(15*time.Minute))

	// 1. Kiosk Offline: WebSocket not connected -> HTTP 409 Conflict (PRINTER_OFFLINE)
	claimURL := fmt.Sprintf("%s/api/k/%s/orders/ord_guard_1/claim", server.URL, kioskID)
	req, _ := http.NewRequest(http.MethodPost, claimURL, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("claim request failed: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when offline, got %d", res.StatusCode)
	}
	var errResp map[string]string
	_ = json.NewDecoder(res.Body).Decode(&errResp)
	if errResp["code"] != "PRINTER_OFFLINE" {
		t.Errorf("code = %s, want PRINTER_OFFLINE", errResp["code"])
	}

	// 2. Connect kiosk with Hardware Error (Paper Jam) -> HTTP 409 Conflict (PRINTER_ERROR)
	conn, cancel := connectKioskWithHealth(t, server.URL, kioskID, contracts.PrinterHealth{
		Online:          true,
		State:           "ERROR",
		PaperTrayStatus: "OK",
		EstimatedSheets: 100,
		PaperJam:        true,
		UpdatedAt:       time.Now().UTC(),
	})
	defer func() {
		cancel()
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	req2, _ := http.NewRequest(http.MethodPost, claimURL, nil)
	req2.Header.Set("Authorization", "Bearer "+secret)
	res2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("claim request 2 failed: %v", err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when jammed, got %d", res2.StatusCode)
	}
	_ = json.NewDecoder(res2.Body).Decode(&errResp)
	if errResp["code"] != "PRINTER_ERROR" {
		t.Errorf("code = %s, want PRINTER_ERROR", errResp["code"])
	}

	// 3. Connect/update with Tray UNKNOWN -> HTTP 409 Conflict (PAPER_UNKNOWN)
	_ = wsjson.Write(ctx, conn, wireMsg{Type: "health", Health: &contracts.PrinterHealth{
		Online:          true,
		State:           "IDLE",
		PaperTrayStatus: "UNKNOWN",
		EstimatedSheets: 100,
		UpdatedAt:       time.Now().UTC(),
	}})
	time.Sleep(50 * time.Millisecond)

	req3, _ := http.NewRequest(http.MethodPost, claimURL, nil)
	req3.Header.Set("Authorization", "Bearer "+secret)
	res3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("claim request 3 failed: %v", err)
	}
	defer res3.Body.Close()
	if res3.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when UNKNOWN, got %d", res3.StatusCode)
	}
	_ = json.NewDecoder(res3.Body).Decode(&errResp)
	if errResp["code"] != "PAPER_UNKNOWN" {
		t.Errorf("code = %s, want PAPER_UNKNOWN", errResp["code"])
	}

	// 4. Update with Tray EMPTY -> HTTP 409 Conflict (INSUFFICIENT_PAPER)
	_ = wsjson.Write(ctx, conn, wireMsg{Type: "health", Health: &contracts.PrinterHealth{
		Online:          true,
		State:           "IDLE",
		PaperTrayStatus: "EMPTY",
		EstimatedSheets: 0,
		UpdatedAt:       time.Now().UTC(),
	}})
	time.Sleep(50 * time.Millisecond)

	req4, _ := http.NewRequest(http.MethodPost, claimURL, nil)
	req4.Header.Set("Authorization", "Bearer "+secret)
	res4, err := http.DefaultClient.Do(req4)
	if err != nil {
		t.Fatalf("claim request 4 failed: %v", err)
	}
	defer res4.Body.Close()
	if res4.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when EMPTY, got %d", res4.StatusCode)
	}
	_ = json.NewDecoder(res4.Body).Decode(&errResp)
	if errResp["code"] != "INSUFFICIENT_PAPER" {
		t.Errorf("code = %s, want INSUFFICIENT_PAPER", errResp["code"])
	}

	// 5. Update with Insufficient Sheets: tray has 5 sheets, order needs 10 -> HTTP 409 Conflict (INSUFFICIENT_PAPER)
	_ = wsjson.Write(ctx, conn, wireMsg{Type: "health", Health: &contracts.PrinterHealth{
		Online:          true,
		State:           "IDLE",
		PaperTrayStatus: "LOW",
		EstimatedSheets: 5,
		UpdatedAt:       time.Now().UTC(),
	}})
	time.Sleep(50 * time.Millisecond)

	req5, _ := http.NewRequest(http.MethodPost, claimURL, nil)
	req5.Header.Set("Authorization", "Bearer "+secret)
	res5, err := http.DefaultClient.Do(req5)
	if err != nil {
		t.Fatalf("claim request 5 failed: %v", err)
	}
	defer res5.Body.Close()
	if res5.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when insufficient, got %d", res5.StatusCode)
	}
	_ = json.NewDecoder(res5.Body).Decode(&errResp)
	if errResp["code"] != "INSUFFICIENT_PAPER" {
		t.Errorf("code = %s, want INSUFFICIENT_PAPER", errResp["code"])
	}

	// 6. Update with Sufficient Sheets: tray has 50 sheets, order needs 10 -> HTTP 200 OK!
	_ = wsjson.Write(ctx, conn, wireMsg{Type: "health", Health: &contracts.PrinterHealth{
		Online:          true,
		State:           "IDLE",
		PaperTrayStatus: "OK",
		EstimatedSheets: 50,
		UpdatedAt:       time.Now().UTC(),
	}})
	time.Sleep(50 * time.Millisecond)

	req6, _ := http.NewRequest(http.MethodPost, claimURL, nil)
	req6.Header.Set("Authorization", "Bearer "+secret)
	res6, err := http.DefaultClient.Do(req6)
	if err != nil {
		t.Fatalf("claim request 6 failed: %v", err)
	}
	defer res6.Body.Close()
	if res6.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK when paper is sufficient, got %d", res6.StatusCode)
	}
	var sessResp sessionResponse
	_ = json.NewDecoder(res6.Body).Decode(&sessResp)
	if sessResp.SessionID == "" {
		t.Errorf("expected session ID, got empty")
	}
}

func TestPaperAvailability_PaymentInitiation_Revalidation(t *testing.T) {
	_, server, st, secret := setupPaperTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	kioskID := "kiosk-payment-reval"

	// Create test order (needs 10 sheets)
	seedTestOrder(t, st, "ord_pay_reval", kioskID, "3002", 10, contracts.JobStatusPendingPayment, 2000, now.Add(15*time.Minute))

	// Connect kiosk with 20 sheets
	conn, cancel := connectKioskWithHealth(t, server.URL, kioskID, contracts.PrinterHealth{
		Online:          true,
		State:           "IDLE",
		PaperTrayStatus: "OK",
		EstimatedSheets: 20,
		UpdatedAt:       time.Now().UTC(),
	})
	defer func() {
		cancel()
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	// 1. Claim succeeds
	claimURL := fmt.Sprintf("%s/api/k/%s/orders/ord_pay_reval/claim", server.URL, kioskID)
	reqClaim, _ := http.NewRequest(http.MethodPost, claimURL, nil)
	reqClaim.Header.Set("Authorization", "Bearer "+secret)
	resClaim, err := http.DefaultClient.Do(reqClaim)
	if err != nil || resClaim.StatusCode != http.StatusOK {
		t.Fatalf("initial claim failed: status=%d err=%v", resClaim.StatusCode, err)
	}
	var sess sessionResponse
	_ = json.NewDecoder(resClaim.Body).Decode(&sess)
	_ = resClaim.Body.Close()

	// 2. Now printer paper status drops to EMPTY before payment is initiated
	_ = wsjson.Write(ctx, conn, wireMsg{Type: "health", Health: &contracts.PrinterHealth{
		Online:          true,
		State:           "IDLE",
		PaperTrayStatus: "EMPTY",
		EstimatedSheets: 0,
		UpdatedAt:       time.Now().UTC(),
	}})
	time.Sleep(50 * time.Millisecond)

	// 3. Initiate payment -> Revalidation catches the shortage and blocks payment!
	payURL := fmt.Sprintf("%s/api/k/%s/orders/ord_pay_reval/initiate-payment", server.URL, kioskID)
	reqPay, _ := http.NewRequest(http.MethodPost, payURL, nil)
	reqPay.Header.Set("Authorization", "Bearer "+secret)
	resPay, err := http.DefaultClient.Do(reqPay)
	if err != nil {
		t.Fatalf("initiate payment request failed: %v", err)
	}
	defer resPay.Body.Close()
	if resPay.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when paper dropped to empty, got %d", resPay.StatusCode)
	}
	var errResp map[string]string
	_ = json.NewDecoder(resPay.Body).Decode(&errResp)
	if errResp["code"] != "INSUFFICIENT_PAPER" {
		t.Errorf("code = %s, want INSUFFICIENT_PAPER", errResp["code"])
	}
}

func TestPaperAvailability_Refill_Endpoint(t *testing.T) {
	_, server, _, secret := setupPaperTestServer(t)
	kioskID := "kiosk-refill-test"

	conn, cancel := connectKioskWithHealth(t, server.URL, kioskID, contracts.PrinterHealth{
		Online:          true,
		State:           "IDLE",
		PaperTrayStatus: "EMPTY",
		EstimatedSheets: 0,
		UpdatedAt:       time.Now().UTC(),
	})
	defer func() {
		cancel()
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	// Background reader for kiosk WebSocket to receive the refill command
	receivedRefill := make(chan wireMsg, 1)
	go func() {
		for {
			var msg wireMsg
			if err := wsjson.Read(context.Background(), conn, &msg); err != nil {
				return
			}
			if msg.Type == "refill" {
				receivedRefill <- msg
				return
			}
		}
	}()

	// Call POST /api/k/{kiosk_id}/refill with {"sheets": 250}
	refillURL := fmt.Sprintf("%s/api/k/%s/refill", server.URL, kioskID)
	reqBody := bytes.NewReader([]byte(`{"sheets": 250}`))
	req, _ := http.NewRequest(http.MethodPost, refillURL, reqBody)
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("refill request failed: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from refill endpoint, got %d", res.StatusCode)
	}

	var resData map[string]interface{}
	_ = json.NewDecoder(res.Body).Decode(&resData)
	if resData["ok"] != true || resData["sheets"] != float64(250) {
		t.Errorf("unexpected refill response: %v", resData)
	}

	select {
	case msg := <-receivedRefill:
		if msg.Type != "refill" || msg.Sheets != 250 {
			t.Errorf("expected wire message {Type: refill, Sheets: 250}, got %+v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for kiosk to receive refill wire message")
	}
}

func seedTestOrder(t *testing.T, st *store.Store, orderID, kioskID, pin string, sheets int, status contracts.JobStatus, pricePaise int64, pinExpiresAt time.Time) {
	t.Helper()
	cmd := contracts.PrintJobCommand{
		JobID:          "job_" + orderID,
		KioskID:        kioskID,
		DownloadURL:    "https://storage.test/" + orderID + ".pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages:     sheets,
		TotalSheets:    sheets,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
		DisplayCode:    pin,
	}
	ord := store.Order{
		OrderID:      orderID,
		Command:      cmd,
		Status:       status,
		PricePaise:   pricePaise,
		PINExpiresAt: pinExpiresAt,
	}
	if err := st.CreateOrder(context.Background(), ord); err != nil {
		t.Fatalf("failed to seed test order: %v", err)
	}
}
