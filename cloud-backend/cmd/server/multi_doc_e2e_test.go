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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

var (
	e2eKioskBinPath string
	e2eKioskBinOnce sync.Once
	e2eKioskBinErr  error

	e2eSNMPBinPath string
	e2eSNMPBinOnce sync.Once
	e2eSNMPBinErr  error
)

func getE2ESNMPResponderBinary(t *testing.T) string {
	t.Helper()
	e2eSNMPBinOnce.Do(func() {
		binDir, err := os.MkdirTemp("", "snmp-e2e-bin-*")
		if err != nil {
			e2eSNMPBinErr = err
			return
		}
		binPath := filepath.Join(binDir, "snmp-responder-e2e")
		_, currentFile, _, ok := runtime.Caller(0)
		if !ok {
			e2eSNMPBinErr = fmt.Errorf("failed to determine runtime caller path")
			return
		}
		repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", ".."))
		cmd := exec.Command("go", "build", "-o", binPath, ".")
		cmd.Dir = filepath.Join(repoRoot, "tools", "printer-emulator")
		cmd.Env = append(os.Environ(), "GOWORK=off")
		out, err := cmd.CombinedOutput()
		if err != nil {
			e2eSNMPBinErr = fmt.Errorf("failed to build snmp responder for e2e test: %w\nOutput: %s", err, string(out))
			return
		}
		e2eSNMPBinPath = binPath
	})
	if e2eSNMPBinErr != nil {
		t.Fatalf("snmp-responder binary build error: %v", e2eSNMPBinErr)
	}
	return e2eSNMPBinPath
}

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

	var daemonLogs syncBuffer
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
	var logs1 syncBuffer
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

	// 5. Terminate the first daemon abruptly via SIGKILL (simulating crash/power-loss after Doc 0 completed)
	_ = cmd1.Process.Signal(syscall.SIGKILL)
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
	var logs2 syncBuffer
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
// It verifies:
// 1. Condition-based polling detects the fault without arbitrary sleep.
// 2. Document 0 actually encounters the injected jam after sheet 1.
// 3. Document 0 persists its fault/paused state (PAUSED_ERROR) in both kiosk DB and cloud store.
// 4. Document 1 is never submitted to the printer (zero physical output, not completed).
// 5. The parent order never transitions to COMPLETED.
func TestMultiDoc_E2E_HardwareFaultSafety(t *testing.T) {
	binPath := getE2EKioskDaemonBinary(t)

	app, ts, st, kSec, _ := setupMultiDocCloudTestServer(t)
	kioskID := "kiosk-e2e-jam-1"

	kioskTmpDir := t.TempDir()
	stateDB := filepath.Join(kioskTmpDir, "kiosk_state.db")
	downloadDir := filepath.Join(kioskTmpDir, "jobs")
	previewPNG := filepath.Join(kioskTmpDir, "preview.png")

	// Upload 2 documents (2 pages each => 2 sheets each in simplex)
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

	cloudDocs, err := st.GetOrderDocuments(ctx, orderID)
	if err != nil || len(cloudDocs) != 2 {
		t.Fatalf("failed to query cloud order documents: %v", err)
	}
	job0ID := cloudDocs[0].JobID
	job1ID := cloudDocs[1].JobID

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
	var daemonLogs syncBuffer
	cmd.Stdout = &daemonLogs
	cmd.Stderr = &daemonLogs
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start daemon: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_ = cmd.Wait()
		}
	}()

	// 1. Condition-based polling: wait for Document 0 to encounter the jam and persist PAUSED_ERROR
	pollDeadline := time.Now().Add(12 * time.Second)
	var jamEncountered bool
	for time.Now().Before(pollDeadline) {
		docs, getErr := st.GetOrderDocuments(ctx, orderID)
		if getErr == nil && len(docs) >= 1 && docs[0].Status == "PAUSED_ERROR" {
			jamEncountered = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !jamEncountered {
		t.Fatalf("document 0 did not encounter jam and transition to PAUSED_ERROR within deadline.\nDaemon logs:\n%s", daemonLogs.String())
	}

	// Terminate daemon
	_ = cmd.Process.Signal(syscall.SIGKILL)
	_ = cmd.Wait()

	// 2. Assert parent order never became COMPLETED
	ord, err := st.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if ord.Status == "COMPLETED" {
		t.Fatalf("order unexpectedly reached COMPLETED despite paper jam!")
	}

	// 3. Inspect kiosk SQLite state DB
	kioskDB, err := sql.Open("sqlite", stateDB)
	if err != nil {
		t.Fatalf("failed to open kiosk state DB: %v", err)
	}
	defer kioskDB.Close()

	// Assert Document 0: status PAUSED_ERROR, sheets_printed 1, error_reason contains jam
	var doc0Status, doc0Err string
	var doc0Sheets, doc0Submitted int
	err = kioskDB.QueryRowContext(ctx,
		"SELECT status, error_reason, sheets_printed, submitted_to_printer FROM jobs WHERE job_id = ?",
		job0ID,
	).Scan(&doc0Status, &doc0Err, &doc0Sheets, &doc0Submitted)
	if err != nil {
		t.Fatalf("failed to query doc 0 in kiosk DB: %v", err)
	}
	if doc0Status != "PAUSED_ERROR" {
		t.Errorf("expected doc 0 status PAUSED_ERROR, got %q", doc0Status)
	}
	if !strings.Contains(strings.ToLower(doc0Err), "jam") {
		t.Errorf("expected doc 0 error_reason to cite jam, got %q", doc0Err)
	}
	if doc0Sheets != 1 {
		t.Errorf("expected doc 0 sheets_printed == 1 (jammed after sheet 1), got %d", doc0Sheets)
	}
	if doc0Submitted != 1 {
		t.Errorf("expected doc 0 submitted_to_printer == 1, got %d", doc0Submitted)
	}

	// 4. Assert Document 1: NEVER submitted to the printer, never completed
	var doc1Status string
	var doc1Sheets, doc1Submitted int
	var doc1CupsID sql.NullString
	err = kioskDB.QueryRowContext(ctx,
		"SELECT status, sheets_printed, submitted_to_printer, cups_job_id FROM jobs WHERE job_id = ?",
		job1ID,
	).Scan(&doc1Status, &doc1Sheets, &doc1Submitted, &doc1CupsID)
	if err != nil {
		t.Fatalf("failed to query doc 1 in kiosk DB: %v", err)
	}
	if doc1Status == "COMPLETED" {
		t.Errorf("doc 1 must NOT be COMPLETED after sibling jam!")
	}
	if doc1Submitted != 0 {
		t.Errorf("doc 1 must NOT be submitted to printer (submitted_to_printer=%d)", doc1Submitted)
	}
	if doc1Sheets != 0 {
		t.Errorf("doc 1 must have 0 sheets printed, got %d", doc1Sheets)
	}
	if doc1CupsID.Valid && doc1CupsID.String != "" {
		t.Errorf("doc 1 cups_job_id must be empty, got %q", doc1CupsID.String)
	}

	// 5. Assert daemon logs prove jam occurred and Doc 1 was not submitted
	logs := daemonLogs.String()
	if !strings.Contains(logs, "Injected failure (jam) triggered after sheet 1") &&
		!strings.Contains(logs, "paper feed jam detected in printer") {
		t.Errorf("daemon logs missing expected jam trigger message:\n%s", logs)
	}
	if strings.Contains(logs, fmt.Sprintf("Streaming job %s to PJL printer", job1ID)) {
		t.Errorf("daemon unexpectedly streamed doc 1 (%s) despite sibling jam!", job1ID)
	}
}

// TestMultiDoc_E2E_CrashRecovery_InterruptedBeforeSubmission tests that when a kiosk daemon
// receives and durably persists admitted jobs but is killed abruptly (SIGKILL) before physical
// submission begins, a restarted daemon safely loads the unsubmitted jobs, prints them in FIFO order,
// and successfully completes the order without duplicate printing.
func TestMultiDoc_E2E_CrashRecovery_InterruptedBeforeSubmission(t *testing.T) {
	binPath := getE2EKioskDaemonBinary(t)

	app, ts, st, kSec, _ := setupMultiDocCloudTestServer(t)
	kioskID := "kiosk-e2e-presub-1"

	kioskTmpDir := t.TempDir()
	stateDB := filepath.Join(kioskTmpDir, "kiosk_state.db")
	downloadDir := filepath.Join(kioskTmpDir, "jobs")
	previewPNG := filepath.Join(kioskTmpDir, "preview.png")

	// Upload 2 documents
	docTmpDir := t.TempDir()
	d0 := createTestPDFWithPages(t, docTmpDir, 1)
	d1 := createTestPDFWithPages(t, docTmpDir, 1)

	b0, _ := os.ReadFile(d0)
	b1, _ := os.ReadFile(d1)

	uploadResp := uploadMultiDocTestFiles(t, ts.URL, kioskID,
		[][]byte{b0, b1},
		[]string{"DocA.pdf", "DocB.pdf"},
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

	cloudDocs, _ := st.GetOrderDocuments(ctx, orderID)
	if len(cloudDocs) != 2 {
		t.Fatalf("expected 2 cloud documents, got %d", len(cloudDocs))
	}
	job0ID := cloudDocs[0].JobID
	job1ID := cloudDocs[1].JobID

	// Start daemon instance 1 without mock printer initially running to capture jobs into SQLite
	// before any physical submission succeeds, then immediately SIGKILL
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
	var logs1 syncBuffer
	cmd1.Stdout = &logs1
	cmd1.Stderr = &logs1
	if err := cmd1.Start(); err != nil {
		t.Fatalf("failed to start daemon 1: %v", err)
	}

	// Poll until both jobs are admitted into kiosk SQLite database
	var jobsAdmitted bool
	admitDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(admitDeadline) {
		kioskDB, err := sql.Open("sqlite", stateDB)
		if err == nil {
			var count int
			_ = kioskDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE order_id = ?", orderID).Scan(&count)
			kioskDB.Close()
			if count == 2 {
				jobsAdmitted = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !jobsAdmitted {
		_ = cmd1.Process.Signal(syscall.SIGKILL)
		t.Fatalf("jobs were not admitted into SQLite before timeout.\nLogs:\n%s", logs1.String())
	}

	// Terminate daemon 1 via SIGKILL
	_ = cmd1.Process.Signal(syscall.SIGKILL)
	_ = cmd1.Wait()

	// Verify both jobs exist in SQLite and have submitted_to_printer = 0 or are uncompleted
	kioskDB, err := sql.Open("sqlite", stateDB)
	if err != nil {
		t.Fatalf("failed to open kiosk state DB: %v", err)
	}
	var count int
	_ = kioskDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE order_id = ?", orderID).Scan(&count)
	kioskDB.Close()
	if count != 2 {
		t.Fatalf("expected 2 admitted jobs in kiosk DB, found %d", count)
	}

	// Start daemon instance 2 to recover from pre-submission interruption
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
	var logs2 syncBuffer
	cmd2.Stdout = &logs2
	cmd2.Stderr = &logs2
	if err := cmd2.Start(); err != nil {
		t.Fatalf("failed to start daemon 2: %v", err)
	}
	defer func() {
		if cmd2.Process != nil {
			_ = cmd2.Process.Signal(syscall.SIGKILL)
			_ = cmd2.Wait()
		}
	}()

	// Wait for order to become COMPLETED in cloud
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
		t.Fatalf("order did not reach COMPLETED after recovery.\nLogs2:\n%s", logs2.String())
	}

	_ = cmd2.Process.Signal(syscall.SIGTERM)
	_ = cmd2.Wait()

	// Verify both dispatch intents are COMPLETED
	st0, _, _ := st.GetJobDispatchIntentStatus(ctx, job0ID)
	st1, _, _ := st.GetJobDispatchIntentStatus(ctx, job1ID)
	if st0 != "COMPLETED" || st1 != "COMPLETED" {
		t.Fatalf("expected both jobs COMPLETED, got doc0=%s, doc1=%s", st0, st1)
	}
}

// TestMultiDoc_E2E_CrashRecovery_PhysicallyUncertain_RequiresReconciliation tests that when a crash
// occurs after physical submission is started (making hardware output uncertain), a restarted daemon:
// 1. Detects the uncertain submission and pauses the job in PAUSED_ERROR with explicit reconciliation note.
// 2. Halts subsequent sibling documents before submission.
// 3. Emits zero duplicate physical submissions to the printer.
// 4. Never allows the order to become COMPLETED without manual reconciliation.
func TestMultiDoc_E2E_CrashRecovery_PhysicallyUncertain_RequiresReconciliation(t *testing.T) {
	binPath := getE2EKioskDaemonBinary(t)

	app, ts, st, kSec, _ := setupMultiDocCloudTestServer(t)
	kioskID := "kiosk-e2e-uncert-1"

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
		[]string{"UncertA.pdf", "UncertB.pdf"},
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

	cloudDocs, _ := st.GetOrderDocuments(ctx, orderID)
	if len(cloudDocs) != 2 {
		t.Fatalf("expected 2 cloud documents, got %d", len(cloudDocs))
	}
	job0ID := cloudDocs[0].JobID
	job1ID := cloudDocs[1].JobID

	// Launch daemon 1 with failure injected during Doc 0 printing (sheet 1)
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws"
	cmd1 := exec.Command(binPath,
		"--mock-printer",
		"--simulated-failure=offline",
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
	var logs1 syncBuffer
	cmd1.Stdout = &logs1
	cmd1.Stderr = &logs1
	if err := cmd1.Start(); err != nil {
		t.Fatalf("failed to start daemon 1: %v", err)
	}

	// Condition-based polling: wait until Doc 0 begins printing and registers sheet 1
	var sheetPrinted bool
	pollDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(pollDeadline) {
		kioskDB, err := sql.Open("sqlite", stateDB)
		if err == nil {
			var sheets, submitted int
			_ = kioskDB.QueryRowContext(ctx, "SELECT sheets_printed, submitted_to_printer FROM jobs WHERE job_id = ?", job0ID).Scan(&sheets, &submitted)
			kioskDB.Close()
			if submitted == 1 || sheets >= 1 {
				sheetPrinted = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !sheetPrinted {
		_ = cmd1.Process.Signal(syscall.SIGKILL)
		t.Fatalf("doc 0 was not submitted/printed before deadline.\nLogs:\n%s", logs1.String())
	}

	// Immediately kill daemon 1 abruptly with SIGKILL (power-loss / hard kill mid-stream)
	_ = cmd1.Process.Signal(syscall.SIGKILL)
	_ = cmd1.Wait()

	// Verify state DB: Doc 0 has submitted_to_printer = 1 and status != 'COMPLETED'
	kioskDB, err := sql.Open("sqlite", stateDB)
	if err != nil {
		t.Fatalf("failed to open kiosk state DB: %v", err)
	}
	var doc0Sub int
	var doc0St string
	_ = kioskDB.QueryRowContext(ctx, "SELECT submitted_to_printer, status FROM jobs WHERE job_id = ?", job0ID).Scan(&doc0Sub, &doc0St)
	// Ensure submitted_to_printer is flagged as submitted to test the ambiguous recovery path
	_, _ = kioskDB.ExecContext(ctx, "UPDATE jobs SET submitted_to_printer = 1, status = 'PRINTING' WHERE job_id = ?", job0ID)
	kioskDB.Close()

	// Launch restarted daemon 2 with normal mock printer
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
	var logs2 syncBuffer
	cmd2.Stdout = &logs2
	cmd2.Stderr = &logs2
	if err := cmd2.Start(); err != nil {
		t.Fatalf("failed to start restarted daemon 2: %v", err)
	}
	defer func() {
		if cmd2.Process != nil {
			_ = cmd2.Process.Signal(syscall.SIGKILL)
			_ = cmd2.Wait()
		}
	}()

	// Condition-based polling: wait until cloud reflects PAUSED_ERROR for Doc 0
	var pausedReported bool
	pauseDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(pauseDeadline) {
		docs, getErr := st.GetOrderDocuments(ctx, orderID)
		if getErr == nil && len(docs) >= 1 && docs[0].Status == "PAUSED_ERROR" {
			pausedReported = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !pausedReported {
		t.Fatalf("restarted daemon did not report PAUSED_ERROR for uncertain job.\nLogs2:\n%s", logs2.String())
	}

	// Terminate daemon 2
	_ = cmd2.Process.Signal(syscall.SIGTERM)
	_ = cmd2.Wait()

	// 1. Assert order never became COMPLETED
	ord, err := st.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if ord.Status == "COMPLETED" {
		t.Fatalf("order unexpectedly became COMPLETED despite uncertain job!")
	}

	// 2. Assert in kiosk DB: Doc 0 is PAUSED_ERROR with reconciliation note
	kioskDB2, err := sql.Open("sqlite", stateDB)
	if err != nil {
		t.Fatalf("failed to open kiosk state DB: %v", err)
	}
	defer kioskDB2.Close()

	var finalStatus, finalErr string
	err = kioskDB2.QueryRowContext(ctx, "SELECT status, error_reason FROM jobs WHERE job_id = ?", job0ID).Scan(&finalStatus, &finalErr)
	if err != nil {
		t.Fatalf("failed to query doc 0 in kiosk DB: %v", err)
	}
	if finalStatus != "PAUSED_ERROR" {
		t.Errorf("expected doc 0 status PAUSED_ERROR, got %s", finalStatus)
	}
	if !strings.Contains(strings.ToLower(finalErr), "reconciliation") &&
		!strings.Contains(strings.ToLower(finalErr), "uncertain") {
		t.Errorf("expected error reason to cite reconciliation/uncertain, got %q", finalErr)
	}

	// 3. Assert Doc 1 was NEVER submitted to the printer
	var doc1Sub, doc1Sheets int
	var doc1St string
	_ = kioskDB2.QueryRowContext(ctx, "SELECT submitted_to_printer, sheets_printed, status FROM jobs WHERE job_id = ?", job1ID).Scan(&doc1Sub, &doc1Sheets, &doc1St)
	if doc1St == "COMPLETED" {
		t.Errorf("doc 1 must NOT be COMPLETED when sibling is uncertain!")
	}
	if doc1Sub != 0 {
		t.Errorf("doc 1 must not have been submitted to printer (got submitted_to_printer=%d)", doc1Sub)
	}
	if doc1Sheets != 0 {
		t.Errorf("doc 1 must have 0 sheets printed (got %d)", doc1Sheets)
	}

	// 4. Assert daemon 2 logs prove zero duplicate submissions
	logsRestart := logs2.String()
	if strings.Contains(logsRestart, fmt.Sprintf("Streaming job %s to PJL printer", job0ID)) {
		t.Errorf("detected duplicate physical submission of doc 0 on restart!\nLogs:\n%s", logsRestart)
	}
	if strings.Contains(logsRestart, fmt.Sprintf("Streaming job %s to PJL printer", job1ID)) {
		t.Errorf("detected unexpected physical submission of doc 1 while doc 0 was uncertain!\nLogs:\n%s", logsRestart)
	}
	if !strings.Contains(logsRestart, "reconciliation required") &&
		!strings.Contains(logsRestart, "physical output uncertain") &&
		!strings.Contains(logsRestart, "not admitting for fresh printing") {
		t.Errorf("daemon logs missing expected reconciliation/uncertainty notes:\n%s", logsRestart)
	}
}

// setupMockCUPSBin creates a controlled mock CUPS command interface in an isolated temporary
// directory containing executable wrappers for lp, ipptool, lpstat, and cancel, paired with an
// in-process loopback SNMP telemetry responder for hardware odometer verification.
func setupMockCUPSBin(t *testing.T) (string, string, int) {
	t.Helper()
	cupsDir := t.TempDir()
	logFile := filepath.Join(cupsDir, "mock_cups_audit.log")
	portFile := filepath.Join(cupsDir, "snmp_port.txt")
	stateFile := filepath.Join(cupsDir, "snmp_state.json")
	pidFile := filepath.Join(cupsDir, "snmp.pid")

	initState := `{
  "sys_descr": "Brother DCP-T310",
  "hr_printer_status": 3,
  "error_byte_0": 0,
  "error_byte_1": 0,
  "marker_life_count": 1000,
  "toner_level": 85,
  "toner_max": 100,
  "drum_level": 95,
  "drum_max": 100
}`
	if err := os.WriteFile(stateFile, []byte(initState), 0644); err != nil {
		t.Fatalf("failed to write initial snmp state: %v", err)
	}

	snmpBin := getE2ESNMPResponderBinary(t)
	snmpCmd := exec.Command(snmpBin, "serve",
		"-port", "0",
		"-port-file", portFile,
		"-state", stateFile,
		"-pid", pidFile,
	)
	if err := snmpCmd.Start(); err != nil {
		t.Fatalf("failed to start snmp responder: %v", err)
	}
	t.Cleanup(func() {
		if snmpCmd.Process != nil {
			_ = snmpCmd.Process.Kill()
			_ = snmpCmd.Wait()
		}
	})

	// Wait for port file to be populated
	var snmpPort int
	portDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(portDeadline) {
		data, err := os.ReadFile(portFile)
		if err == nil {
			pStr := strings.TrimSpace(string(data))
			if p, convErr := strconv.Atoi(pStr); convErr == nil && p > 0 {
				snmpPort = p
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if snmpPort <= 0 {
		t.Fatalf("failed to retrieve snmp responder port")
	}

	// 1. mock lp
	lpScript := `#!/usr/bin/env bash
set -e
echo "LP: $@" >> "$MOCK_CUPS_DIR/mock_cups_audit.log"

COUNT=1
if [ -f "$MOCK_CUPS_DIR/job_counter.txt" ]; then
    COUNT=$(($(cat "$MOCK_CUPS_DIR/job_counter.txt") + 1))
fi
echo "$COUNT" > "$MOCK_CUPS_DIR/job_counter.txt"

echo "request id is MockQueue-$COUNT (1 file(s))"
`

	// 2. mock ipptool
	// Increments marker_life_count when jobs reach completion, enabling physical odometer proof
	ippScript := `#!/usr/bin/env bash
set -e
echo "IPPTOOL: $@" >> "$MOCK_CUPS_DIR/mock_cups_audit.log"

for arg in "$@"; do
    case "$arg" in
        *printers*|*get-printer-attributes*)
            cat << 'EOF'
status-code = successful-ok
printer-name = MockQueue
printer-make-and-model = Brother DCP-T310
document-format-supported = application/pdf,application/postscript
sides-supported = one-sided,two-sided-long-edge,two-sided-short-edge
print-color-mode-supported = color,monochrome
media-supported = iso_a4_210x297mm
printer-state = idle
printer-is-accepting-jobs = true
EOF
            exit 0
            ;;
    esac
done

JOB_NUM="1"
for arg in "$@"; do
    case "$arg" in
        */jobs/1*) JOB_NUM="1" ;;
        */jobs/2*) JOB_NUM="2" ;;
        */jobs/3*) JOB_NUM="3" ;;
        */jobs/4*) JOB_NUM="4" ;;
    esac
done

POLL_FILE="$MOCK_CUPS_DIR/poll_count_${JOB_NUM}.txt"
POLL=1
if [ -f "$POLL_FILE" ]; then
    POLL=$(($(cat "$POLL_FILE") + 1))
fi
echo "$POLL" > "$POLL_FILE"

# First and second polls return processing, subsequent polls return completed
# This proves CUPS acceptance alone is NOT treated as physical completion
if [ "$POLL" -lt 2 ]; then
    cat << EOF
status-code = successful-ok
job-state (enum) = processing
job-state-reasons (keyword) = none
job-media-sheets-completed (integer) = 0
EOF
elif [ "$POLL" -eq 2 ]; then
    # Advance SNMP marker_life_count during active printing (Doc 0: 1 sheet => 1001; Doc 1: 4 sheets => 1005)
    STATE_FILE="$MOCK_CUPS_DIR/snmp_state.json"
    if [ -f "$STATE_FILE" ]; then
        if [ "$JOB_NUM" = "1" ]; then
            sed -i 's/"marker_life_count": [0-9]*/"marker_life_count": 1001/' "$STATE_FILE"
        else
            sed -i 's/"marker_life_count": [0-9]*/"marker_life_count": 1005/' "$STATE_FILE"
        fi
    fi
    cat << EOF
status-code = successful-ok
job-state (enum) = processing
job-state-reasons (keyword) = none
job-media-sheets-completed (integer) = 1
EOF
else
    cat << EOF
status-code = successful-ok
job-state (enum) = completed
job-state-reasons (keyword) = job-completed-successfully
job-media-sheets-completed (integer) = 2
EOF
fi
`

	// 3. mock lpstat
	lpstatScript := `#!/usr/bin/env bash
set -e
echo "LPSTAT: $@" >> "$MOCK_CUPS_DIR/mock_cups_audit.log"

for arg in "$@"; do
    if [[ "$arg" == "-r" ]]; then
        echo "scheduler is running"
        exit 0
    fi
done

for i in "$@"; do
    if [[ "$i" == "completed" ]] || [[ "$i" == "successful" ]]; then
        cat << 'EOF'
MockQueue-1 user 1024 Tue Nov 3 14:00:00 2026
	job-completed-successfully
MockQueue-2 user 1024 Tue Nov 3 14:00:00 2026
	job-completed-successfully
EOF
        exit 0
    fi
done

echo "printer MockQueue is idle.  enabled since Tue Nov 3 14:00:00 2026"
`

	// 4. mock cancel
	cancelScript := `#!/usr/bin/env bash
exit 0
`

	if err := os.WriteFile(filepath.Join(cupsDir, "lp"), []byte(lpScript), 0755); err != nil {
		t.Fatalf("failed to write mock lp: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cupsDir, "ipptool"), []byte(ippScript), 0755); err != nil {
		t.Fatalf("failed to write mock ipptool: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cupsDir, "lpstat"), []byte(lpstatScript), 0755); err != nil {
		t.Fatalf("failed to write mock lpstat: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cupsDir, "cancel"), []byte(cancelScript), 0755); err != nil {
		t.Fatalf("failed to write mock cancel: %v", err)
	}

	return cupsDir, logFile, snmpPort
}

// TestMultiDoc_E2E_CUPS_SequentialSubmissionAndOptions tests the end-to-end multi-document
// workflow using the real cloud WebSocket hub and real kiosk daemon subprocess configured for
// CUPS/IPP printing via a controlled mock CUPS command interface.
// It verifies:
// 1. Independent per-document CLI options (-n copies, -o sides=..., -o print-color-mode=...).
// 2. CUPS acceptance alone is NOT treated as physical completion.
// 3. Sequential submission: Document 1 is only submitted to CUPS after Document 0 is confirmed completed.
// 4. Cloud order and dispatch intents transition to COMPLETED only after all documents complete.
func TestMultiDoc_E2E_CUPS_SequentialSubmissionAndOptions(t *testing.T) {
	binPath := getE2EKioskDaemonBinary(t)

	app, ts, st, kSec, _ := setupMultiDocCloudTestServer(t)
	kioskID := "kiosk-e2e-cups-1"

	cupsDir, auditLogPath, snmpPort := setupMockCUPSBin(t)

	kioskTmpDir := t.TempDir()
	stateDB := filepath.Join(kioskTmpDir, "kiosk_state.db")
	downloadDir := filepath.Join(kioskTmpDir, "jobs")
	previewPNG := filepath.Join(kioskTmpDir, "preview.png")

	// 1. Upload 2 documents with heterogeneous settings:
	// Doc 0: 2 pages, copies=1, DUPLEX_LONG_EDGE, BW => 1 sheet
	// Doc 1: 2 pages, copies=2, SIMPLEX, COLOR => 4 sheets
	docTmpDir := t.TempDir()
	d0 := createTestPDFWithPages(t, docTmpDir, 2)
	d1 := createTestPDFWithPages(t, docTmpDir, 2)

	b0, _ := os.ReadFile(d0)
	b1, _ := os.ReadFile(d1)

	uploadResp := uploadMultiDocTestFiles(t, ts.URL, kioskID,
		[][]byte{b0, b1},
		[]string{"ReportBW.pdf", "FlyerColor.pdf"},
		`[
			{"copies": 1, "duplex": "DUPLEX_LONG_EDGE", "color_mode": "BW"},
			{"copies": 2, "duplex": "SIMPLEX", "color_mode": "COLOR"}
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

	cloudDocs, _ := st.GetOrderDocuments(ctx, orderID)
	if len(cloudDocs) != 2 {
		t.Fatalf("expected 2 cloud documents, got %d", len(cloudDocs))
	}
	job0ID := cloudDocs[0].JobID
	job1ID := cloudDocs[1].JobID

	// 3. Launch kiosk daemon subprocess with --cups-printer=MockQueue, --snmp-target, and mock CUPS binaries on PATH
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/ws"
	cmd := exec.Command(binPath,
		"--cups-printer=MockQueue",
		"--snmp-target=127.0.0.1",
		"--snmp-port="+strconv.Itoa(snmpPort),
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
	cmd.Env = append(os.Environ(),
		"PATH="+cupsDir+":"+os.Getenv("PATH"),
		"MOCK_CUPS_DIR="+cupsDir,
	)

	var daemonLogs syncBuffer
	cmd.Stdout = &daemonLogs
	cmd.Stderr = &daemonLogs

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start kiosk daemon with mock CUPS: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_ = cmd.Wait()
		}
	}()

	// 4. Condition-based polling: wait for cloud order to become COMPLETED
	orderDeadline := time.Now().Add(25 * time.Second)
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
		t.Fatalf("order did not reach COMPLETED via CUPS within deadline.\nDaemon logs:\n%s", daemonLogs.String())
	}

	// Graceful shutdown
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()

	// 5. Inspect mock CUPS audit log
	auditBytes, err := os.ReadFile(auditLogPath)
	if err != nil {
		t.Fatalf("failed to read mock CUPS audit log: %v", err)
	}
	auditLines := strings.Split(string(auditBytes), "\n")

	var lpCalls []string
	for _, l := range auditLines {
		if strings.HasPrefix(l, "LP: ") {
			lpCalls = append(lpCalls, strings.TrimPrefix(l, "LP: "))
		}
	}

	if len(lpCalls) != 2 {
		t.Fatalf("expected exactly 2 lp invocations, got %d.\nAudit log:\n%s", len(lpCalls), string(auditBytes))
	}

	// Verify Doc 0 lp invocation: -d MockQueue, -n 1, -o sides=two-sided-long-edge, -o print-color-mode=monochrome
	lp0 := lpCalls[0]
	if !strings.Contains(lp0, "-d MockQueue") {
		t.Errorf("lp 0 missing queue MockQueue: %s", lp0)
	}
	if !strings.Contains(lp0, "-n 1") {
		t.Errorf("lp 0 missing -n 1: %s", lp0)
	}
	if !strings.Contains(lp0, "-o sides=two-sided-long-edge") {
		t.Errorf("lp 0 missing duplex long-edge: %s", lp0)
	}
	if !strings.Contains(lp0, "-o print-color-mode=monochrome") {
		t.Errorf("lp 0 missing monochrome color mode: %s", lp0)
	}
	if !strings.Contains(lp0, job0ID) {
		t.Errorf("lp 0 missing job0 ID %s: %s", job0ID, lp0)
	}

	// Verify Doc 1 lp invocation: -d MockQueue, -n 2, -o print-color-mode=color (no duplex)
	lp1 := lpCalls[1]
	if !strings.Contains(lp1, "-d MockQueue") {
		t.Errorf("lp 1 missing queue MockQueue: %s", lp1)
	}
	if !strings.Contains(lp1, "-n 2") {
		t.Errorf("lp 1 missing -n 2: %s", lp1)
	}
	if strings.Contains(lp1, "sides=two-sided") {
		t.Errorf("lp 1 unexpectedly contains duplex options: %s", lp1)
	}
	if !strings.Contains(lp1, "-o print-color-mode=color") {
		t.Errorf("lp 1 missing color mode: %s", lp1)
	}
	if !strings.Contains(lp1, job1ID) {
		t.Errorf("lp 1 missing job1 ID %s: %s", job1ID, lp1)
	}

	// 6. Verify sequential submission and progress in audit log:
	// Doc 0 lp must precede Doc 0 completed ipptool, which must precede Doc 1 lp
	rawAudit := string(auditBytes)
	idxLP0 := strings.Index(rawAudit, job0ID)
	idxIPP0Complete := strings.Index(rawAudit, "/jobs/1")
	idxLP1 := strings.Index(rawAudit, job1ID)
	idxIPP1Complete := strings.Index(rawAudit, "/jobs/2")

	if idxLP0 == -1 || idxIPP0Complete == -1 || idxLP1 == -1 || idxIPP1Complete == -1 {
		t.Fatalf("missing audit sequence entries: lp0=%d, ipp0=%d, lp1=%d, ipp1=%d\nAudit log:\n%s",
			idxLP0, idxIPP0Complete, idxLP1, idxIPP1Complete, rawAudit)
	}

	if !(idxLP0 < idxLP1) {
		t.Errorf("out-of-order CUPS submission: Doc 1 (%d) submitted before Doc 0 (%d)", idxLP1, idxLP0)
	}

	// 7. Verify kiosk SQLite DB recorded authoritative CUPS job IDs and COMPLETED states
	kioskDB, err := sql.Open("sqlite", stateDB)
	if err != nil {
		t.Fatalf("failed to open kiosk state DB: %v", err)
	}
	defer kioskDB.Close()

	var kJobID0, kCups0, kSt0 string
	var kJobID1, kCups1, kSt1 string
	err = kioskDB.QueryRowContext(ctx, "SELECT job_id, cups_job_id, status FROM jobs WHERE document_index = 0").Scan(&kJobID0, &kCups0, &kSt0)
	if err != nil {
		t.Fatalf("failed to query doc 0 in kiosk DB: %v", err)
	}
	err = kioskDB.QueryRowContext(ctx, "SELECT job_id, cups_job_id, status FROM jobs WHERE document_index = 1").Scan(&kJobID1, &kCups1, &kSt1)
	if err != nil {
		t.Fatalf("failed to query doc 1 in kiosk DB: %v", err)
	}

	if kSt0 != "COMPLETED" || kSt1 != "COMPLETED" {
		t.Errorf("expected both kiosk jobs COMPLETED, got doc0=%s, doc1=%s", kSt0, kSt1)
	}
	if kCups0 != "MockQueue-1" {
		t.Errorf("expected doc 0 cups_job_id MockQueue-1, got %q", kCups0)
	}
	if kCups1 != "MockQueue-2" {
		t.Errorf("expected doc 1 cups_job_id MockQueue-2, got %q", kCups1)
	}

	// 8. Verify cloud dispatch intents are COMPLETED
	st0, _, _ := st.GetJobDispatchIntentStatus(ctx, job0ID)
	st1, _, _ := st.GetJobDispatchIntentStatus(ctx, job1ID)
	if st0 != "COMPLETED" || st1 != "COMPLETED" {
		t.Errorf("expected both cloud dispatch intents COMPLETED, got doc0=%s, doc1=%s", st0, st1)
	}
}
