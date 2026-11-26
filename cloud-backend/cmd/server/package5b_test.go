package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
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

func setupPackage5BTestServer(t *testing.T) (*appServer, *httptest.Server, string) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "p5b.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	jobsDir := filepath.Join(tmpDir, "jobs")
	if err := os.MkdirAll(jobsDir, 0700); err != nil {
		t.Fatalf("mkdir jobs failed: %v", err)
	}

	kioskSecret := "p5b-kiosk-secret-123456"
	cfg := &serverConfig{
		addr:           "localhost:0",
		dbPath:         dbPath,
		jobsDir:        jobsDir,
		kioskSecret:    []byte(kioskSecret),
		downloadSecret: []byte("p5b-download-secret-123456"),
		devMode:        false,
		duplexEnabled:  false,
	}

	hub := ws.NewHub(st)
	hub.SetKioskSecret([]byte(kioskSecret))
	_ = st.SetDetailedPricing(context.Background(), 300, 500, 800, 1000, false)
	app := &appServer{
		cfg:      cfg,
		db:       st,
		hub:      hub,
		convWake: make(chan struct{}, 10),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.ServeHTTP)
	mux.HandleFunc("/api/documents/preview", app.handleDocumentPreview)
	mux.HandleFunc("/api/pricing", app.handlePublicPricing)
	mux.HandleFunc("/api/k/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/upload"):
			app.handleUpload(w, r)
		case strings.HasSuffix(r.URL.Path, "/refill"):
			app.handleKioskRefill(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return app, ts, kioskSecret
}

func TestPackage5B_RefillEndpointValidationAndAckRequired(t *testing.T) {
	_, ts, secret := setupPackage5BTestServer(t)
	kioskID := "kiosk-5b-refill-api"

	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws?kiosk_id=" + kioskID + "&token=" + secret
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial failed: %v", err)
	}
	defer conn.CloseNow()

	// Send initial health with ConfiguredTrayCapacity = 150
	now := time.Now().UTC()
	_ = wsjson.Write(ctx, conn, map[string]interface{}{
		"type": "health",
		"health": contracts.PrinterHealth{
			Online:                 true,
			State:                  "IDLE",
			EvaluatedHealth:        contracts.EvaluatedHealthHealthy,
			PaperTrayStatus:        "OK",
			EstimatedSheets:        10,
			ConfiguredTrayCapacity: 150,
			InventoryCalibrated:    true,
			ExecutionSafe:          true,
			UpdatedAt:              now,
		},
	})
	time.Sleep(50 * time.Millisecond)

	postRefill := func(body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/refill", ts.URL, kioskID), strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST refill failed: %v", err)
		}
		return resp
	}

	// 1. Reject missing, fractional, negative, and >150 counts without 250 fallback
	for _, badBody := range []string{`{}`, `{"sheets": null}`, `{"sheets": 10.5}`, `{"sheets": -1}`, `{"sheets": 151}`, `{"sheets": 250}`} {
		resp := postRefill(badBody)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request for %s, got %d", badBody, resp.StatusCode)
		}
	}

	// 2. If Pi rejects refill_ack, endpoint must fail (409 Conflict)
	go func() {
		var msg struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Sheets    int    `json:"sheets"`
		}
		if err := wsjson.Read(ctx, conn, &msg); err == nil && msg.Type == "refill" {
			_ = wsjson.Write(ctx, conn, map[string]interface{}{
				"type":       "refill_ack",
				"request_id": msg.RequestID,
				"ok":         false,
				"error":      "active job in progress",
			})
		}
	}()
	respReject := postRefill(`{"sheets": 150}`)
	_ = respReject.Body.Close()
	if respReject.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when Pi rejects refill, got %d", respReject.StatusCode)
	}
}

func TestPackage5B_CloudCapabilityRejectionWithoutSilentConversion(t *testing.T) {
	app, ts, _ := setupPackage5BTestServer(t)
	app.cfg.devMode = true
	tmpDir := t.TempDir()
	pdfPath := createTestPDFWithPages(t, tmpDir, 2)
	pdfBytes, _ := os.ReadFile(pdfPath)

	postUpload := func(fields map[string]string) int {
		body := &bytes.Buffer{}
		w := multipart.NewWriter(body)
		part, _ := w.CreateFormFile("file", "doc.pdf")
		_, _ = part.Write(pdfBytes)
		for k, v := range fields {
			_ = w.WriteField(k, v)
		}
		_ = w.Close()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/k/kiosk-5b-cap/upload", body)
		req.Header.Set("Content-Type", w.FormDataContentType())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("upload failed: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if code := postUpload(map[string]string{"color_mode": "COLOR"}); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for COLOR under demo policy, got %d", code)
	}
	if code := postUpload(map[string]string{"duplex": "DUPLEX_LONG_EDGE"}); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for DUPLEX_LONG_EDGE under demo policy, got %d", code)
	}
	if code := postUpload(map[string]string{"paper_size": "LETTER"}); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for LETTER paper_size under demo policy, got %d", code)
	}
	if code := postUpload(map[string]string{"copies": "2", "duplex": "SIMPLEX", "color_mode": "BW", "pages_per_sheet": "2"}); code != http.StatusOK {
		t.Fatalf("expected 200 for BW/SIMPLEX/2-copies/2-up under demo policy, got %d", code)
	}

	// Verify /api/pricing advertises duplex_enabled=false and color_enabled=false
	resp, err := http.Get(ts.URL + "/api/pricing")
	if err != nil {
		t.Fatalf("GET /api/pricing failed: %v", err)
	}
	defer resp.Body.Close()
	var pricingResp map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&pricingResp)
	if pricingResp["color_enabled"] != false || pricingResp["duplex_enabled"] != false {
		t.Fatalf("expected color_enabled=false and duplex_enabled=false, got %v", pricingResp)
	}
}
