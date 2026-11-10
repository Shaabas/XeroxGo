package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

func createServerTestCanonicalPDF(t *testing.T, path string, numPages int) string {
	t.Helper()
	var gsCommands strings.Builder
	for i := 1; i <= numPages; i++ {
		gsCommands.WriteString(fmt.Sprintf("0 0 0 setrgbcolor /Helvetica findfont 36 scalefont setfont 100 500 moveto (Page %d) show showpage ", i))
	}

	args := []string{
		"-dNOPAUSE", "-dBATCH", "-sDEVICE=pdfwrite",
		"-sOutputFile=" + path,
		"-c", gsCommands.String(),
	}
	cmd := exec.Command("gs", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to create canonical test PDF: %s: %v", strings.TrimSpace(string(out)), err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read test PDF: %v", err)
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func setupRecoveryTestApp(t *testing.T) (*appServer, *http.ServeMux, string, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "server_recovery.db")
	jobsDir := filepath.Join(tmpDir, "jobs")
	if err := os.MkdirAll(jobsDir, 0700); err != nil {
		t.Fatalf("failed to create jobs dir: %v", err)
	}

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}

	cfg := &serverConfig{
		addr:               ":0",
		dbPath:             dbPath,
		jobsDir:            jobsDir,
		pricePerSheetPaise: 200,
		devMode:            true,
		downloadSecret:     []byte("test-dl-secret-key-1234"),
		webhookSecret:      []byte("test-wh-secret-key-1234"),
		kioskSecret:        []byte("kiosk-secret-xyz"),
	}

	app := &appServer{
		cfg:      cfg,
		db:       db,
		hub:      nil,
		convWake: make(chan struct{}, 1),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/k/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/upload"):
			app.handleUpload(w, r)
		case strings.HasSuffix(path, "/recover"):
			app.handleRecoverJob(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/api/recovery/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pdf") {
			app.handleDownloadRecoveryPDF(w, r)
			return
		}
		http.NotFound(w, r)
	})

	cleanup := func() {
		_ = db.Close()
	}

	return app, mux, jobsDir, cleanup
}

func TestCloudRecoveryEndpoint_ValidationAndAuth(t *testing.T) {
	app, mux, _, cleanup := setupRecoveryTestApp(t)
	defer cleanup()

	ctx := context.Background()

	// 1. Create order in DB
	order := store.Order{
		OrderID: "ord_auth_01",
		Command: contracts.PrintJobCommand{
			JobID:          "job_auth_01",
			KioskID:        "kiosk-1",
			DisplayCode:    "5555",
			DownloadURL:    "http://example.com/pdf",
			SHA256Checksum: "dummy",
			TotalPages:     4,
			TotalSheets:    2,
			Copies:         1,
			Duplex:         contracts.DuplexLongEdge,
			StartFromPage:  1,
		},
		Status:     contracts.JobStatusDispatched,
		PricePaise: 300,
		ColorMode:  "BW",
	}
	if err := app.db.CreateOrder(ctx, order); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	t.Run("unauthorized kiosk without Bearer token", func(t *testing.T) {
		body := `{"recovery_id":"r1","confirmed_sheets_printed":1,"output_confirmed":true}`
		req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-1/jobs/job_auth_01/recover", strings.NewReader(body))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected status 401, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("wrong kiosk ID returns 403 Forbidden", func(t *testing.T) {
		body := `{"recovery_id":"r1","confirmed_sheets_printed":1,"output_confirmed":true}`
		req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-other/jobs/job_auth_01/recover", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer kiosk-secret-xyz")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected status 403, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("non-existent job returns 404 Not Found", func(t *testing.T) {
		body := `{"recovery_id":"r1","confirmed_sheets_printed":1,"output_confirmed":true}`
		req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-1/jobs/job_missing/recover", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer kiosk-secret-xyz")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("expected status 404, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("output_confirmed=false returns 400 Bad Request (Requirement 10)", func(t *testing.T) {
		body := `{"recovery_id":"r1","confirmed_sheets_printed":1,"output_confirmed":false}`
		req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-1/jobs/job_auth_01/recover", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer kiosk-secret-xyz")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400, got %d: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "physical output is uncertain") {
			t.Fatalf("expected error about uncertain physical output, got: %s", w.Body.String())
		}
	})

	t.Run("invalid confirmed_sheets_printed bounds returns 400", func(t *testing.T) {
		body := `{"recovery_id":"r1","confirmed_sheets_printed":99,"output_confirmed":true}`
		req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-1/jobs/job_auth_01/recover", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer kiosk-secret-xyz")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400, got %d: %s", w.Code, w.Body.String())
		}
	})
}

func TestCloudRecoveryEndpoint_FullLifecycleAndIdempotency(t *testing.T) {
	app, mux, jobsDir, cleanup := setupRecoveryTestApp(t)
	defer cleanup()

	ctx := context.Background()

	// Create canonical 6-page duplex document (3 sheets total)
	orderID := "ord_rec_life_01"
	jobID := "job_rec_life_01"
	kioskID := "kiosk-demo"

	h := sha256.Sum256([]byte(orderID))
	pdfPath := filepath.Join(jobsDir, hex.EncodeToString(h[:])+".pdf")
	origChecksum := createServerTestCanonicalPDF(t, pdfPath, 6)

	order := store.Order{
		OrderID: orderID,
		Command: contracts.PrintJobCommand{
			JobID:          jobID,
			KioskID:        kioskID,
			DisplayCode:    "7777",
			DownloadURL:    "http://example.com/pdf",
			SHA256Checksum: origChecksum,
			TotalPages:     6,
			TotalSheets:    3,
			Copies:         1,
			Duplex:         contracts.DuplexLongEdge,
			StartFromPage:  1,
		},
		Status:     contracts.JobStatusPausedError,
		PricePaise: 450,
		ColorMode:  "BW",
	}
	if err := app.db.CreateOrder(ctx, order); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	// 1. Recover after 1 sheet printed (2 sheets = 4 pages remaining)
	recReq1 := map[string]interface{}{
		"recovery_id":              "rec_req_001",
		"confirmed_sheets_printed": 1,
		"output_confirmed":         true,
		"reason":                   "cleared jam at tray",
	}
	recReqBytes, _ := json.Marshal(recReq1)

	req1 := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/k/%s/jobs/%s/recover", kioskID, jobID), bytes.NewReader(recReqBytes))
	req1.Header.Set("Authorization", "Bearer kiosk-secret-xyz")
	w1 := httptest.NewRecorder()
	mux.ServeHTTP(w1, req1)

	if w1.Code != http.StatusOK {
		t.Fatalf("recovery request 1 failed with %d: %s", w1.Code, w1.Body.String())
	}

	var resp1 struct {
		RecoveryID      string                    `json:"recovery_id"`
		SequenceID      int                       `json:"sequence_id"`
		JobID           string                    `json:"job_id"`
		RemainingPages  int                       `json:"remaining_pages"`
		RemainingSheets int                       `json:"remaining_sheets"`
		Command         contracts.PrintJobCommand `json:"command"`
	}
	if err := json.Unmarshal(w1.Body.Bytes(), &resp1); err != nil {
		t.Fatalf("unmarshal response 1 failed: %v", err)
	}

	if resp1.RecoveryID != "rec_req_001" {
		t.Fatalf("expected recovery_id rec_req_001, got %s", resp1.RecoveryID)
	}
	if resp1.SequenceID != 1 {
		t.Fatalf("expected sequence_id 1, got %d", resp1.SequenceID)
	}
	if resp1.RemainingSheets != 2 || resp1.RemainingPages != 4 {
		t.Fatalf("expected 2 sheets, 4 pages; got %d sheets, %d pages", resp1.RemainingSheets, resp1.RemainingPages)
	}
	if resp1.Command.StartFromPage != 1 {
		t.Fatalf("expected StartFromPage=1 for continuation command, got %d", resp1.Command.StartFromPage)
	}
	if resp1.Command.Copies != 1 {
		t.Fatalf("expected Copies=1 for continuation command, got %d", resp1.Command.Copies)
	}

	// 2. Idempotency test: Resend the exact same recovery request
	req1Repeat := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/k/%s/jobs/%s/recover", kioskID, jobID), bytes.NewReader(recReqBytes))
	req1Repeat.Header.Set("Authorization", "Bearer kiosk-secret-xyz")
	w1Repeat := httptest.NewRecorder()
	mux.ServeHTTP(w1Repeat, req1Repeat)

	if w1Repeat.Code != http.StatusOK {
		t.Fatalf("idempotent recovery request failed with %d: %s", w1Repeat.Code, w1Repeat.Body.String())
	}

	var resp1Repeat struct {
		SequenceID int                       `json:"sequence_id"`
		Command    contracts.PrintJobCommand `json:"command"`
	}
	_ = json.Unmarshal(w1Repeat.Body.Bytes(), &resp1Repeat)
	if resp1Repeat.SequenceID != 1 {
		t.Fatalf("expected same sequence_id 1, got %d", resp1Repeat.SequenceID)
	}
	if resp1Repeat.Command.SHA256Checksum != resp1.Command.SHA256Checksum {
		t.Fatalf("checksum mismatch on idempotent request: got %s, want %s", resp1Repeat.Command.SHA256Checksum, resp1.Command.SHA256Checksum)
	}

	// 3. Test continuation PDF download endpoint
	dlReq := httptest.NewRequest(http.MethodGet, resp1.Command.DownloadURL, nil)
	wDL := httptest.NewRecorder()
	mux.ServeHTTP(wDL, dlReq)

	if wDL.Code != http.StatusOK {
		t.Fatalf("continuation download failed with %d: %s", wDL.Code, wDL.Body.String())
	}
	if wDL.Header().Get("Content-Type") != "application/pdf" {
		t.Fatalf("expected Content-Type application/pdf, got %s", wDL.Header().Get("Content-Type"))
	}

	downloadedBytes := wDL.Body.Bytes()
	dlHash := sha256.Sum256(downloadedBytes)
	if hex.EncodeToString(dlHash[:]) != strings.ToLower(resp1.Command.SHA256Checksum) {
		t.Fatalf("downloaded continuation PDF checksum mismatch: got %x, want %s", dlHash, resp1.Command.SHA256Checksum)
	}

	// Verify page count of downloaded continuation PDF
	tmpContPath := filepath.Join(t.TempDir(), "downloaded_cont.pdf")
	_ = os.WriteFile(tmpContPath, downloadedBytes, 0600)
	cmdCheck := exec.Command("qpdf", "--show-npages", tmpContPath)
	outCheck, err := cmdCheck.CombinedOutput()
	if err != nil || strings.TrimSpace(string(outCheck)) != "4" {
		t.Fatalf("expected 4 pages in downloaded continuation PDF, got %s (err: %v)", string(outCheck), err)
	}

	// 4. Test recovery when all sheets are confirmed printed (marks job COMPLETED)
	recReqDone := map[string]interface{}{
		"recovery_id":              "rec_req_done",
		"confirmed_sheets_printed": 3, // all 3 sheets confirmed
		"output_confirmed":         true,
	}
	doneBytes, _ := json.Marshal(recReqDone)
	reqDone := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/k/%s/jobs/%s/recover", kioskID, jobID), bytes.NewReader(doneBytes))
	reqDone.Header.Set("Authorization", "Bearer kiosk-secret-xyz")
	wDone := httptest.NewRecorder()
	mux.ServeHTTP(wDone, reqDone)

	if wDone.Code != http.StatusOK {
		t.Fatalf("complete recovery failed with %d: %s", wDone.Code, wDone.Body.String())
	}
	if !strings.Contains(wDone.Body.String(), "COMPLETED") {
		t.Fatalf("expected COMPLETED status, got: %s", wDone.Body.String())
	}

	// Verify DB status is now COMPLETED
	updatedOrder, _ := app.db.GetOrderByJobID(ctx, jobID)
	if updatedOrder.Status != contracts.JobStatusCompleted {
		t.Fatalf("expected order status COMPLETED in DB, got %s", updatedOrder.Status)
	}

	// 5. Subsequent recovery on completed job is rejected with 409 Conflict
	reqAfterDone := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/k/%s/jobs/%s/recover", kioskID, jobID), bytes.NewReader(recReqBytes))
	reqAfterDone.Header.Set("Authorization", "Bearer kiosk-secret-xyz")
	wAfterDone := httptest.NewRecorder()
	mux.ServeHTTP(wAfterDone, reqAfterDone)
	if wAfterDone.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for completed job, got %d", wAfterDone.Code)
	}
}
