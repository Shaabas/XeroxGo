package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"xeroxgo/pkg/contracts"
)

// A mobile customer only receives a PIN when paper is available and atomically
// reserved. The next upload must not reuse that same physical sheet.
func TestMobileUploadReservesLastSheetBeforeIssuingPIN(t *testing.T) {
	app, mux := setupUploadTestServer(t)
	app.cfg.devMode = false
	srv := httptest.NewServer(mux)
	defer srv.Close()

	pdf := createTestPDFWithPages(t, t.TempDir(), 1)
	fileBytes, err := os.ReadFile(pdf)
	if err != nil { t.Fatal(err) }

	postOrder := func() (int, map[string]interface{}) {
		t.Helper()
		body := &bytes.Buffer{}
		mp := multipart.NewWriter(body)
		part, err := mp.CreateFormFile("file", filepath.Base(pdf))
		if err != nil { t.Fatal(err) }
		if _, err := part.Write(fileBytes); err != nil { t.Fatal(err) }
		if err := mp.WriteField("copies", "1"); err != nil { t.Fatal(err) }
		if err := mp.WriteField("duplex", "SIMPLEX"); err != nil { t.Fatal(err) }
		if err := mp.Close(); err != nil { t.Fatal(err) }

		req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/k/kiosk-mobile-stock/upload", body)
		if err != nil { t.Fatal(err) }
		req.Header.Set("Content-Type", mp.FormDataContentType())
		resp, err := http.DefaultClient.Do(req)
		if err != nil { t.Fatal(err) }
		defer resp.Body.Close()
		var data map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			t.Fatalf("upload returned non-JSON status %d: %v", resp.StatusCode, err)
		}
		return resp.StatusCode, data
	}

	// Without recent printer telemetry, the mobile site must not issue a PIN.
	if status, data := postOrder(); status != http.StatusConflict || data["display_code"] != nil {
		t.Fatalf("offline upload: status=%d response=%v", status, data)
	}

	conn, cancel := connectKioskWithHealth(t, srv.URL, "kiosk-mobile-stock", contracts.PrinterHealth{
		Online: true,
		State: "IDLE",
		PaperTrayStatus: "OK",
		EstimatedSheets: 1,
		UpdatedAt: time.Now().UTC(),
	})
	defer func() { cancel(); _ = conn.CloseNow() }()

	status, first := postOrder()
	if status != http.StatusOK {
		t.Fatalf("first order: status=%d response=%v", status, first)
	}
	pin, ok := first["display_code"].(string)
	if !ok || len(pin) != 4 || first["total_sheets"] != float64(1) {
		t.Fatalf("PIN must be issued only after reserving 1 sheet: %v", first)
	}

	status, second := postOrder()
	if status != http.StatusConflict || second["code"] != "INSUFFICIENT_PAPER" {
		t.Fatalf("second order must be denied: status=%d response=%v", status, second)
	}
	if second["display_code"] != nil {
		t.Fatalf("out-of-stock customer received a PIN: %v", second)
	}
}
