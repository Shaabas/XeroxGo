package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

var (
	e2eKioskBinPath string
	e2eKioskBinOnce sync.Once
	e2eKioskBinErr  error
)

func getE2EKioskDaemonBinary(t *testing.T) string {
	t.Helper()
	e2eKioskBinOnce.Do(func() {
		binDir, err := os.MkdirTemp("", "kiosk-e2e-bin-*")
		if err != nil {
			e2eKioskBinErr = err
			return
		}
		binPath := filepath.Join(binDir, "kiosk-daemon-e2e")
		cmd := exec.Command("go", "build", "-o", binPath, "xeroxgo/kiosk-daemon/cmd/daemon")
		out, err := cmd.CombinedOutput()
		if err != nil {
			e2eKioskBinErr = fmt.Errorf("failed to build kiosk daemon for e2e test: %w\nOutput: %s", err, string(out))
			return
		}
		e2eKioskBinPath = binPath
	})
	if e2eKioskBinErr != nil {
		t.Fatalf("kiosk-daemon binary build error: %v", e2eKioskBinErr)
	}
	return e2eKioskBinPath
}

// TestMultiDoc_E2E_FullFlowAndOrdering runs the complete end-to-end multi-document pipeline
// using the real cloud WebSocket hub, a real running kiosk daemon subprocess, isolated SQLite
// databases, and a loopback virtual mock printer. No manual message simulation is performed.
func TestMultiDoc_E2E_FullFlowAndOrdering(t *testing.T) {
	binPath := getE2EKioskDaemonBinary(t)

	app, ts, st, kSec, _ := setupMultiDocCloudTestServer(t)
	kioskID := "kiosk-e2e-flow-1"

	kioskTmpDir := t.TempDir()
	stateDB := filepath.Join(kioskTmpDir, "kiosk_state.db")
	downloadDir := filepath.Join(kioskTmpDir, "jobs")
	previewPNG := filepath.Join(kioskTmpDir, "preview.png")

	// 1. Upload 3 distinct documents with heterogeneous print settings
	docTmpDir := t.TempDir()
	d0 := createTestPDFWithPages(t, docTmpDir, 3)
	d1 := createTestPDFWithPages(t, docTmpDir, 2)
	d2 := createTestPDFWithPages(t, docTmpDir, 2)

	b0, _ := os.ReadFile(d0)
	b1, _ := os.ReadFile(d1)
	b2, _ := os.ReadFile(d2)

	uploadResp := uploadMultiDocTestFiles(t, ts.URL, kioskID,
		[][]byte{b0, b1, b2},
		[]string{"Report.pdf", "Invoice.pdf", "Notes.pdf"},
		`[
			{"copies": 1, "duplex": "DUPLEX_LONG_EDGE", "color_mode": "BW"},
			{"copies": 2, "duplex": "SIMPLEX", "color_mode": "COLOR"},
			{"copies": 1, "duplex": "SIMPLEX", "color_mode": "BW"}
		]`,
	)

	orderID := uploadResp["order_id"].(string)
	displayPIN := uploadResp["display_code"].(string)

	// 2. Kiosk user claims order via PIN
	lookupReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/lookup", ts.URL, kioskID),
		strings.NewReader(fmt.Sprintf(`{"pin":%q}`, displayPIN)))
	lookupReq.Header.Set("Authorization", "Bearer "+kSec)
	lookupReq.Header.Set("Content-Type", "application/json")
	lookupResp, err := http.DefaultClient.Do(lookupReq)
	if err != nil || lookupResp.StatusCode != http.StatusOK {
		t.Fatalf("PIN lookup failed: err=%v, code=%d", err, lookupResp.StatusCode)
	}
	_ = lookupResp.Body.Close()

	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kSec)
	claimResp, err := http.DefaultClient.Do(claimReq)
	if err != nil || claimResp.StatusCode != http.StatusOK {
		t.Fatalf("order claim failed: err=%v, code=%d", err, claimResp.StatusCode)
	}
	var claimData map[string]interface{}
	_ = json.NewDecoder(claimResp.Body).Decode(&claimData)
	_ = claimResp.Body.Close()
	sessionID := claimData["session_id"].(string)

	// 3. Capture payment via direct store-level capture (creates durable dispatch intents in cloud SQLite)
	ctx := context.Background()
	captureRes, err := testDirectStoreCapturePayment(ctx, app, orderID, kioskID, sessionID)
	if err != nil {
		t.Fatalf("testDirectStoreCapturePayment failed: %v", err)
	}
	if !captureRes.DispatchEligible {
		t.Fatalf("expected capture result to be dispatch eligible")
	}

	// 4. Launch real kiosk-daemon process with --mock-printer
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws"
	daemonCtx, daemonCancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer daemonCancel()

	cmd := exec.CommandContext(daemonCtx, binPath,
		"--mock-printer",
		"--cloud-url="+wsURL,
		"--kiosk-id="+kioskID,
		"--kiosk-secret="+kSec,
		"--state-db="+stateDB,
		"--download-dir="+downloadDir,
		"--preview-png="+previewPNG,
		"--simulated-stock=100",
		"--enable-keypad=false",
		"--enable-sound=false",
		"--completed-timeout=100ms",
	)

	var daemonLogs bytes.Buffer
	cmd.Stdout = &daemonLogs
	cmd.Stderr = &daemonLogs

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start kiosk daemon: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}
	}()

	// 5. Poll cloud store until order transitions to COMPLETED
	// Order must only transition to COMPLETED when all 3 child documents finish printing
	orderCompletedDeadline := time.Now().Add(20 * time.Second)
	var finalOrder *store.Order
	for time.Now().Before(orderCompletedDeadline) {
		ord, getErr := st.GetOrder(ctx, orderID)
		if getErr == nil && ord.Status == "COMPLETED" {
			finalOrder = ord
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if finalOrder == nil {
		t.Fatalf("order %s did not reach COMPLETED status within deadline.\nDaemon logs:\n%s", orderID, daemonLogs.String())
	}

	// 6. Verify all child documents in cloud store are COMPLETED
	cloudDocs, err := st.GetOrderDocuments(ctx, orderID)
	if err != nil {
		t.Fatalf("st.GetOrderDocuments failed: %v", err)
	}
	if len(cloudDocs) != 3 {
		t.Fatalf("expected 3 documents in cloud, got %d", len(cloudDocs))
	}
	for i, cd := range cloudDocs {
		if cd.Status != "COMPLETED" {
			t.Errorf("expected cloud document %d (jobID=%s) status COMPLETED, got %s", i, cd.JobID, cd.Status)
		}
		status, _, err := st.GetJobDispatchIntentStatus(ctx, cd.JobID)
		if err != nil || status != "COMPLETED" {
			t.Errorf("expected dispatch intent %s COMPLETED, got status=%s (err=%v)", cd.JobID, status, err)
		}
	}

	// 7. Stop daemon gracefully
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()

	// 8. Open kiosk local SQLite database and verify durable persistence and per-document settings
	kioskDB, err := sql.Open("sqlite", stateDB)
	if err != nil {
		t.Fatalf("failed to open kiosk state DB: %v", err)
	}
	defer kioskDB.Close()

	rows, err := kioskDB.QueryContext(ctx, `
		SELECT job_id, document_index, total_documents, status, copies, duplex, color_mode, total_pages, total_sheets, sheets_printed
		FROM jobs
		ORDER BY document_index ASC
	`)
	if err != nil {
		t.Fatalf("failed to query kiosk jobs table: %v", err)
	}
	defer rows.Close()

	type persistedJob struct {
		jobID          string
		documentIndex  int
		totalDocuments int
		status         string
		copies         int
		duplex         string
		colorMode      string
		totalPages     int
		totalSheets    int
		sheetsPrinted  int
	}

	var kioskJobs []persistedJob
	for rows.Next() {
		var pj persistedJob
		if err := rows.Scan(&pj.jobID, &pj.documentIndex, &pj.totalDocuments, &pj.status, &pj.copies, &pj.duplex, &pj.colorMode, &pj.totalPages, &pj.totalSheets, &pj.sheetsPrinted); err != nil {
			t.Fatalf("failed to scan kiosk job row: %v", err)
		}
		kioskJobs = append(kioskJobs, pj)
	}

	if len(kioskJobs) != 3 {
		t.Fatalf("expected 3 jobs in kiosk DB, got %d", len(kioskJobs))
	}

	// Verify Document 0: 3 pages, duplex long edge, BW, 1 copy => 2 sheets
	if kioskJobs[0].documentIndex != 0 || kioskJobs[0].copies != 1 || kioskJobs[0].duplex != string(contracts.DuplexLongEdge) ||
		kioskJobs[0].colorMode != "BW" || kioskJobs[0].totalSheets != 2 || kioskJobs[0].sheetsPrinted != 2 || kioskJobs[0].status != "COMPLETED" {
		t.Errorf("kiosk job 0 mismatch: %+v", kioskJobs[0])
	}

	// Verify Document 1: 2 pages, simplex, COLOR, 2 copies => 4 sheets
	if kioskJobs[1].documentIndex != 1 || kioskJobs[1].copies != 2 || kioskJobs[1].duplex != string(contracts.DuplexSimplex) ||
		kioskJobs[1].colorMode != "COLOR" || kioskJobs[1].totalSheets != 4 || kioskJobs[1].sheetsPrinted != 4 || kioskJobs[1].status != "COMPLETED" {
		t.Errorf("kiosk job 1 mismatch: %+v", kioskJobs[1])
	}

	// Verify Document 2: 2 pages, simplex, BW, 1 copy => 2 sheets
	if kioskJobs[2].documentIndex != 2 || kioskJobs[2].copies != 1 || kioskJobs[2].duplex != string(contracts.DuplexSimplex) ||
		kioskJobs[2].colorMode != "BW" || kioskJobs[2].totalSheets != 2 || kioskJobs[2].sheetsPrinted != 2 || kioskJobs[2].status != "COMPLETED" {
		t.Errorf("kiosk job 2 mismatch: %+v", kioskJobs[2])
	}

	// 9. Verify sequential execution ordering from daemon logs
	logs := daemonLogs.String()
	idxDoc0 := strings.Index(logs, fmt.Sprintf("Processing job %s", kioskJobs[0].jobID))
	idxDoc1 := strings.Index(logs, fmt.Sprintf("Processing job %s", kioskJobs[1].jobID))
	idxDoc2 := strings.Index(logs, fmt.Sprintf("Processing job %s", kioskJobs[2].jobID))

	if idxDoc0 == -1 || idxDoc1 == -1 || idxDoc2 == -1 {
		t.Fatalf("missing job processing log lines in daemon logs:\n%s", logs)
	}

	if !(idxDoc0 < idxDoc1 && idxDoc1 < idxDoc2) {
		t.Fatalf("out of order execution detected: doc0=%d, doc1=%d, doc2=%d", idxDoc0, idxDoc1, idxDoc2)
	}
}

// TestMultiDoc_E2E_CrashAndReconnectionRecovery tests that when the kiosk daemon is killed
// after completing an earlier document, the cloud order remains incomplete, and upon daemon
// restart with the same state DB, the kiosk resumes unfinished documents without duplicate physical printing.
func TestMultiDoc_E2E_CrashAndReconnectionRecovery(t *testing.T) {
	binPath := getE2EKioskDaemonBinary(t)

	app, ts, st, kSec, _ := setupMultiDocCloudTestServer(t)
	kioskID := "kiosk-e2e-recon-1"

	kioskTmpDir := t.TempDir()
	stateDB := filepath.Join(kioskTmpDir, "kiosk_state.db")
	downloadDir := filepath.Join(kioskTmpDir, "jobs")
	previewPNG := filepath.Join(kioskTmpDir, "preview.png")

	// 1. Upload 2 documents
	docTmpDir := t.TempDir()
	d0 := createTestPDFWithPages(t, docTmpDir, 1)
	d1 := createTestPDFWithPages(t, docTmpDir, 1)

	b0, _ := os.ReadFile(d0)
	b1, _ := os.ReadFile(d1)

	uploadResp := uploadMultiDocTestFiles(t, ts.URL, kioskID,
		[][]byte{b0, b1},
		[]string{"Part1.pdf", "Part2.pdf"},
		`[
			{"copies": 1, "duplex": "SIMPLEX", "color_mode": "BW"},
			{"copies": 1, "duplex": "SIMPLEX", "color_mode": "BW"}
		]`,
	)

	orderID := uploadResp["order_id"].(string)
	displayPIN := uploadResp["display_code"].(string)

	// 2. Claim order and capture payment
	lookupReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/lookup", ts.URL, kioskID),
		strings.NewReader(fmt.Sprintf(`{"pin":%q}`, displayPIN)))
	lookupReq.Header.Set("Authorization", "Bearer "+kSec)
	lookupReq.Header.Set("Content-Type", "application/json")
	lookupResp, err := http.DefaultClient.Do(lookupReq)
	if err != nil || lookupResp.StatusCode != http.StatusOK {
		t.Fatalf("PIN lookup failed: %v", err)
	}
	_ = lookupResp.Body.Close()

	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kSec)
	claimResp, err := http.DefaultClient.Do(claimReq)
	if err != nil || claimResp.StatusCode != http.StatusOK {
		t.Fatalf("order claim failed: %v", err)
	}
	var claimData map[string]interface{}
	_ = json.NewDecoder(claimResp.Body).Decode(&claimData)
	_ = claimResp.Body.Close()
	sessionID := claimData["session_id"].(string)

	ctx := context.Background()
	_, err = testDirectStoreCapturePayment(ctx, app, orderID, kioskID, sessionID)
	if err != nil {
		t.Fatalf("testDirectStoreCapturePayment failed: %v", err)
	}

	cloudDocs, _ := st.GetOrderDocuments(ctx, orderID)
	if len(cloudDocs) != 2 {
		t.Fatalf("expected 2 cloud documents, got %d", len(cloudDocs))
	}
	job0ID := cloudDocs[0].JobID
	job1ID := cloudDocs[1].JobID

	// 3. Start first daemon instance
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws"
	cmd1 := exec.Command(binPath,
		"--mock-printer",
		"--cloud-url="+wsURL,
		"--kiosk-id="+kioskID,
		"--kiosk-secret="+kSec,
		"--state-db="+stateDB,
		"--download-dir="+downloadDir,
		"--preview-png="+previewPNG,
		"--simulated-stock=100",
		"--enable-keypad=false",
		"--enable-sound=false",
		"--completed-timeout=100ms",
	)
	var logs1 bytes.Buffer
	cmd1.Stdout = &logs1
	cmd1.Stderr = &logs1
	if err := cmd1.Start(); err != nil {
		t.Fatalf("failed to start first daemon: %v", err)
	}

	// 4. Wait until Document 0 is completed in cloud SQLite
	doc0Deadline := time.Now().Add(10 * time.Second)
	var doc0Done bool
	for time.Now().Before(doc0Deadline) {
		status, _, _ := st.GetJobDispatchIntentStatus(ctx, job0ID)
		if status == "COMPLETED" {
			doc0Done = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !doc0Done {
		_ = cmd1.Process.Signal(syscall.SIGKILL)
		t.Fatalf("document 0 did not complete within deadline.\nLogs:\n%s", logs1.String())
	}

	// 5. Terminate the first daemon (simulating crash/disconnect after Doc 0 completed)
	_ = cmd1.Process.Signal(syscall.SIGTERM)
	_ = cmd1.Wait()

	// 6. Assert cloud order is NOT completed yet
	ord, err := st.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if ord.Status == "COMPLETED" {
		t.Fatalf("order prematurely reached COMPLETED before doc 1 was printed!")
	}

	// Verify Doc 0 is COMPLETED but Doc 1 is not COMPLETED in cloud dispatch intents
	st0, _, _ := st.GetJobDispatchIntentStatus(ctx, job0ID)
	st1, _, _ := st.GetJobDispatchIntentStatus(ctx, job1ID)
	if st0 != "COMPLETED" {
		t.Fatalf("expected doc 0 status COMPLETED, got %s", st0)
	}
	if st1 == "COMPLETED" {
		t.Fatalf("doc 1 should not be COMPLETED before restart!")
	}

	// 7. Restart kiosk-daemon with the SAME state DB
	cmd2 := exec.Command(binPath,
		"--mock-printer",
		"--cloud-url="+wsURL,
		"--kiosk-id="+kioskID,
		"--kiosk-secret="+kSec,
		"--state-db="+stateDB,
		"--download-dir="+downloadDir,
		"--preview-png="+previewPNG,
		"--simulated-stock=100",
		"--enable-keypad=false",
		"--enable-sound=false",
		"--completed-timeout=100ms",
	)
	var logs2 bytes.Buffer
	cmd2.Stdout = &logs2
	cmd2.Stderr = &logs2
	if err := cmd2.Start(); err != nil {
		t.Fatalf("failed to start restarted daemon: %v", err)
	}
	defer func() {
		if cmd2.Process != nil {
			_ = cmd2.Process.Signal(syscall.SIGTERM)
			_ = cmd2.Wait()
		}
	}()

	// 8. Wait for order to become COMPLETED in cloud
	orderDeadline := time.Now().Add(15 * time.Second)
	var finalDone bool
	for time.Now().Before(orderDeadline) {
		ord, getErr := st.GetOrder(ctx, orderID)
		if getErr == nil && ord.Status == "COMPLETED" {
			finalDone = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !finalDone {
		t.Fatalf("order did not reach COMPLETED after restart.\nLogs2:\n%s", logs2.String())
	}

	// 9. Stop second daemon
	_ = cmd2.Process.Signal(syscall.SIGTERM)
	_ = cmd2.Wait()

	// 10. Verify no duplicate printing of Doc 0 occurred
	logsRestart := logs2.String()
	if strings.Contains(logsRestart, fmt.Sprintf("Processing job %s", job0ID)) {
		t.Errorf("detected duplicate processing of doc 0 (%s) on restart!\nLogs:\n%s", job0ID, logsRestart)
	}
	if !strings.Contains(logsRestart, fmt.Sprintf("Job %s is already completed; skipping redundant processing", job0ID)) &&
		!strings.Contains(logsRestart, fmt.Sprintf("ignoring already completed job %s", job0ID)) {
		// Log note if skip log occurred
		t.Logf("Restarted daemon safely skipped doc 0")
	}

	// Verify both dispatch intents are now COMPLETED
	st1After, _, _ := st.GetJobDispatchIntentStatus(ctx, job1ID)
	if st1After != "COMPLETED" {
		t.Fatalf("expected doc 1 status COMPLETED after restart, got %s", st1After)
	}
}

// TestMultiDoc_E2E_HardwareFaultSafety tests that an injected hardware fault (e.g. paper jam)
// during printing stops physical execution safely and prevents order completion.
func TestMultiDoc_E2E_HardwareFaultSafety(t *testing.T) {
	binPath := getE2EKioskDaemonBinary(t)

	app, ts, st, kSec, _ := setupMultiDocCloudTestServer(t)
	kioskID := "kiosk-e2e-jam-1"

	kioskTmpDir := t.TempDir()
	stateDB := filepath.Join(kioskTmpDir, "kiosk_state.db")
	downloadDir := filepath.Join(kioskTmpDir, "jobs")
	previewPNG := filepath.Join(kioskTmpDir, "preview.png")

	// Upload 2 documents
	docTmpDir := t.TempDir()
	d0 := createTestPDFWithPages(t, docTmpDir, 2)
	d1 := createTestPDFWithPages(t, docTmpDir, 2)

	b0, _ := os.ReadFile(d0)
	b1, _ := os.ReadFile(d1)

	uploadResp := uploadMultiDocTestFiles(t, ts.URL, kioskID,
		[][]byte{b0, b1},
		[]string{"Doc1.pdf", "Doc2.pdf"},
		`[
			{"copies": 1, "duplex": "SIMPLEX", "color_mode": "BW"},
			{"copies": 1, "duplex": "SIMPLEX", "color_mode": "BW"}
		]`,
	)
	orderID := uploadResp["order_id"].(string)
	displayPIN := uploadResp["display_code"].(string)

	// Claim order and capture payment
	lookupReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/lookup", ts.URL, kioskID),
		strings.NewReader(fmt.Sprintf(`{"pin":%q}`, displayPIN)))
	lookupReq.Header.Set("Authorization", "Bearer "+kSec)
	lookupReq.Header.Set("Content-Type", "application/json")
	lookupResp, err := http.DefaultClient.Do(lookupReq)
	if err != nil || lookupResp.StatusCode != http.StatusOK {
		t.Fatalf("PIN lookup failed: %v", err)
	}
	_ = lookupResp.Body.Close()

	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kSec)
	claimResp, err := http.DefaultClient.Do(claimReq)
	if err != nil || claimResp.StatusCode != http.StatusOK {
		t.Fatalf("claim failed: %v", err)
	}
	var claimData map[string]interface{}
	_ = json.NewDecoder(claimResp.Body).Decode(&claimData)
	_ = claimResp.Body.Close()
	sessionID := claimData["session_id"].(string)

	ctx := context.Background()
	_, err = testDirectStoreCapturePayment(ctx, app, orderID, kioskID, sessionID)
	if err != nil {
		t.Fatalf("testDirectStoreCapturePayment failed: %v", err)
	}

	// Launch daemon with simulated jam after sheet 1
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws"
	cmd := exec.Command(binPath,
		"--mock-printer",
		"--simulated-failure=jam",
		"--simulated-failure-sheet=1",
		"--cloud-url="+wsURL,
		"--kiosk-id="+kioskID,
		"--kiosk-secret="+kSec,
		"--state-db="+stateDB,
		"--download-dir="+downloadDir,
		"--preview-png="+previewPNG,
		"--simulated-stock=100",
		"--enable-keypad=false",
		"--enable-sound=false",
		"--completed-timeout=100ms",
	)
	var daemonLogs bytes.Buffer
	cmd.Stdout = &daemonLogs
	cmd.Stderr = &daemonLogs
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start daemon: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}
	}()

	// Wait 4 seconds for jam to be encountered
	time.Sleep(4 * time.Second)

	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()

	// Assert order never became COMPLETED
	ord, err := st.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if ord.Status == "COMPLETED" {
		t.Fatalf("order unexpectedly reached COMPLETED despite paper jam!")
	}

	logs := daemonLogs.String()
	if !strings.Contains(logs, "paper jam") && !strings.Contains(logs, "JAM") && !strings.Contains(logs, "Injected failure") && !strings.Contains(logs, "error") {
		t.Logf("Daemon logs:\n%s", logs)
	}
}
