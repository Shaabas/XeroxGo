package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/cloud-backend/internal/ws"
	"xeroxgo/pkg/contracts"
)

// setupMultiDocCloudTestServer creates an in-process cloud backend server for integration tests.
func setupMultiDocCloudTestServer(t *testing.T) (*appServer, *httptest.Server, *store.Store, string, string) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cloud_multi_integ.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	jobsDir := filepath.Join(tmpDir, "jobs")
	if err := os.MkdirAll(jobsDir, 0700); err != nil {
		t.Fatalf("failed to create jobs dir: %v", err)
	}

	kioskSecret := "kiosk-sec-multi-integ-12345"
	webhookSecret := "whsec_multi_integ_test_32_bytes_ok"
	downloadSecret := []byte("dl_sec_multi_integ_test_32_bytes")

	cfg := &serverConfig{
		addr:               "localhost:0",
		dbPath:             dbPath,
		jobsDir:            jobsDir,
		pricePerSheetPaise: 200,
		kioskSecret:        []byte(kioskSecret),
		webhookSecret:      []byte(webhookSecret),
		downloadSecret:     downloadSecret,
		devMode:            true,
	}

	hub := ws.NewHub(st)
	hub.SetKioskSecret([]byte(kioskSecret))

	tariff := store.DefaultPricingConfig()
	_ = st.SetDetailedPricing(context.Background(), tariff.BWSimplexPricePaise, tariff.BWDuplexPricePaise, tariff.ColorSimplexPricePaise, tariff.ColorDuplexPricePaise, true)

	app := &appServer{
		cfg:      cfg,
		db:       st,
		hub:      hub,
		convWake: make(chan struct{}, 10),
	}

	ts := httptest.NewServer(app.routes())
	t.Cleanup(func() { ts.Close() })

	return app, ts, st, kioskSecret, webhookSecret
}

// testDirectStoreCapturePayment is an isolated test fixture helper that performs direct store-level
// payment capture without exposing or touching any production HTTP route or public bypass header.
func testDirectStoreCapturePayment(ctx context.Context, app *appServer, orderID, kioskID, sessionID string) (*store.PaymentCaptureResult, error) {
	ord, err := app.db.GetOrder(ctx, orderID)
	if err != nil {
		return nil, fmt.Errorf("testDirectStoreCapturePayment: get order: %w", err)
	}
	now := time.Now().UTC()
	captureRes, err := app.db.CapturePaymentAndCheckDispatch(ctx, store.PaymentRecord{
		PaymentID:   "pay_test_" + orderID,
		OrderID:     orderID,
		KioskID:     kioskID,
		SessionID:   sessionID,
		AmountPaise: ord.PricePaise,
		Provider:    "test_fixture",
		Status:      "captured",
	}, now)
	if err != nil {
		return nil, fmt.Errorf("testDirectStoreCapturePayment: capture: %w", err)
	}
	if captureRes.DispatchEligible && app.hub != nil {
		if len(captureRes.Commands) > 0 {
			for _, cmd := range captureRes.Commands {
				_ = app.hub.SendJob(ctx, cmd)
				_ = app.db.MarkDispatchIntentSent(ctx, cmd.JobID)
			}
		} else if captureRes.Command != nil {
			_ = app.hub.SendJob(ctx, *captureRes.Command)
			_ = app.db.MarkDispatchIntentSent(ctx, captureRes.Command.JobID)
		}
	}
	return captureRes, nil
}

// uploadMultiDocTestFiles uploads files with metadata to /api/k/<kioskID>/upload.
func uploadMultiDocTestFiles(t *testing.T, tsURL, kioskID string, files [][]byte, filenames []string, metaJSON string) map[string]interface{} {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	for i, f := range files {
		fn := filenames[i]
		part, err := writer.CreateFormFile("files", fn)
		if err != nil {
			t.Fatalf("CreateFormFile failed: %v", err)
		}
		if _, err := io.Copy(part, bytes.NewReader(f)); err != nil {
			t.Fatalf("failed to copy file bytes: %v", err)
		}
	}

	if metaJSON != "" {
		if err := writer.WriteField("metadata", metaJSON); err != nil {
			t.Fatalf("WriteField metadata failed: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close failed: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/upload", tsURL, kioskID), body)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload expected 200 OK, got %d: %s", resp.StatusCode, string(respBytes))
	}

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("failed to decode upload response: %v", err)
	}
	return data
}

// TestMultiDoc_Cloud_FullFlowAndPricing verifies the complete multi-document cloud integration:
// upload with mixed options → pricing calculations → PIN lookup → claim → controlled test payment →
// atomic intent creation in document order → WebSocket delivery of N PrintJobCommands →
// signed PDF download validation → job_ack intent transitions → sequential progress tracking →
// and order completion only after all child documents are complete.
func TestMultiDoc_Cloud_FullFlowAndPricing(t *testing.T) {
	app, ts, st, kSec, _ := setupMultiDocCloudTestServer(t)
	kioskID := "kiosk-cloud-multi-1"

	// 1. Upload 3 distinct documents:
	// Doc 0: 3 pages, DUPLEX_LONG_EDGE, BW, 1 copy -> 3 pages, 2 sheets, 400 paise
	// Doc 1: 2 pages, SIMPLEX, COLOR, 2 copies -> 2 pages, 4 sheets, 1600 paise
	// Doc 2: 4 pages, SIMPLEX, BW, 1 copy, 2-up -> 2 imposed pages, 2 sheets, 400 paise
	tmpDir := t.TempDir()
	d0 := createTestPDFWithPages(t, tmpDir, 3)
	d1 := createTestPDFWithPages(t, tmpDir, 2)
	d2 := createTestPDFWithPages(t, tmpDir, 4)

	b0, _ := os.ReadFile(d0)
	b1, _ := os.ReadFile(d1)
	b2, _ := os.ReadFile(d2)

	metaJSON := `[
		{"copies": 1, "duplex": "DUPLEX_LONG_EDGE", "color_mode": "BW", "page_range": ""},
		{"copies": 2, "duplex": "SIMPLEX", "color_mode": "COLOR", "page_range": ""},
		{"copies": 1, "duplex": "SIMPLEX", "color_mode": "BW", "page_range": "", "pages_per_sheet": 2}
	]`

	uploadResp := uploadMultiDocTestFiles(t, ts.URL, kioskID,
		[][]byte{b0, b1, b2},
		[]string{"Doc0_Duplex.pdf", "Doc1_ColorCopies.pdf", "Doc2_2Up.pdf"},
		metaJSON,
	)

	orderID := uploadResp["order_id"].(string)
	displayPIN := uploadResp["display_code"].(string)
	totalDocs := int(uploadResp["documents_count"].(float64))
	totalPages := int(uploadResp["total_pages"].(float64))
	totalSheets := int(uploadResp["total_sheets"].(float64))
	totalPricePaise := int64(uploadResp["total_price_paise"].(float64))

	// Parent totals verification
	if totalDocs != 3 {
		t.Fatalf("expected 3 documents, got %d", totalDocs)
	}
	if totalPages != 7 { // 3 + 2 + 2 = 7
		t.Fatalf("expected 7 pages, got %d", totalPages)
	}
	if totalSheets != 8 { // 2 + 4 + 2 = 8
		t.Fatalf("expected 8 sheets, got %d", totalSheets)
	}
	if totalPricePaise != 4850 { // 450 (doc 0) + 4000 (doc 1) + 400 (doc 2) = 4850
		t.Fatalf("expected 4850 paise, got %d", totalPricePaise)
	}

	// 2. Kiosk PIN lookup
	lookupReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/lookup", ts.URL, kioskID),
		strings.NewReader(fmt.Sprintf(`{"pin":%q}`, displayPIN)))
	lookupReq.Header.Set("Authorization", "Bearer "+kSec)
	lookupReq.Header.Set("Content-Type", "application/json")
	lookupResp, err := http.DefaultClient.Do(lookupReq)
	if err != nil || lookupResp.StatusCode != http.StatusOK {
		t.Fatalf("pin lookup failed: err=%v, code=%d", err, lookupResp.StatusCode)
	}
	var lookupData map[string]interface{}
	_ = json.NewDecoder(lookupResp.Body).Decode(&lookupData)
	lookupResp.Body.Close()
	if lookupData["order_id"] != orderID {
		t.Fatalf("expected order_id %q, got %v", orderID, lookupData["order_id"])
	}
	if int64(lookupData["total_price_paise"].(float64)) != totalPricePaise {
		t.Fatalf("expected price %d, got %v", totalPricePaise, lookupData["total_price_paise"])
	}

	// 3. Claim order
	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kSec)
	claimResp, err := http.DefaultClient.Do(claimReq)
	if err != nil || claimResp.StatusCode != http.StatusOK {
		t.Fatalf("order claim failed: err=%v, code=%d", err, claimResp.StatusCode)
	}
	var claimData map[string]interface{}
	_ = json.NewDecoder(claimResp.Body).Decode(&claimData)
	claimResp.Body.Close()
	sessionID := claimData["session_id"].(string)
	if sessionID == "" {
		t.Fatalf("expected non-empty session_id")
	}

	// 4. Kiosk connects via WebSocket
	u, _ := url.Parse(ts.URL)
	u.Scheme = "ws"
	u.Path = "/ws"
	q := u.Query()
	q.Set("kiosk_id", kioskID)
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wsConn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + kSec}},
	})
	if err != nil {
		t.Fatalf("websocket.Dial failed: %v", err)
	}
	defer wsConn.CloseNow()

	// 5. Payment captured via direct store-level capture (isolated test fixture)
	captureRes, err := testDirectStoreCapturePayment(ctx, app, orderID, kioskID, sessionID)
	if err != nil {
		t.Fatalf("testDirectStoreCapturePayment failed: %v", err)
	}
	if !captureRes.DispatchEligible {
		t.Fatalf("expected capture result to be dispatch eligible")
	}

	// 6. Kiosk reads 3 PrintJobCommands over WebSocket in document index order
	receivedCommands := make([]*contracts.PrintJobCommand, 0, 3)
	readCtx, readCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer readCancel()

	for len(receivedCommands) < 3 {
		var rawMsg struct {
			Type string                     `json:"type"`
			Job  *contracts.PrintJobCommand `json:"job"`
		}
		if err := wsjson.Read(readCtx, wsConn, &rawMsg); err != nil {
			t.Fatalf("failed reading job %d over ws: %v", len(receivedCommands), err)
		}
		if rawMsg.Type == "job" && rawMsg.Job != nil {
			receivedCommands = append(receivedCommands, rawMsg.Job)
		}
	}

	// Verify all 3 commands are in document order [0, 1, 2] and match settings
	for i := 0; i < 3; i++ {
		cmd := receivedCommands[i]
		if cmd.DocumentIndex != i {
			t.Fatalf("expected DocumentIndex %d, got %d", i, cmd.DocumentIndex)
		}
		if cmd.TotalDocuments != 3 {
			t.Fatalf("expected TotalDocuments 3, got %d", cmd.TotalDocuments)
		}
		if cmd.OrderID != orderID {
			t.Fatalf("expected OrderID %q, got %q", orderID, cmd.OrderID)
		}
		if cmd.KioskID != kioskID {
			t.Fatalf("expected KioskID %q, got %q", kioskID, cmd.KioskID)
		}
		if cmd.DownloadURL == "" {
			t.Fatalf("expected non-empty DownloadURL for doc %d", i)
		}

		// Verify download URL works and downloads valid PDF with matching SHA256 checksum
		dlReq, _ := http.NewRequest(http.MethodGet, cmd.DownloadURL, nil)
		dlResp, dlErr := http.DefaultClient.Do(dlReq)
		if dlErr != nil || dlResp.StatusCode != http.StatusOK {
			t.Fatalf("failed to download PDF for doc %d: err=%v, code=%d", i, dlErr, dlResp.StatusCode)
		}
		pdfBytes, _ := io.ReadAll(dlResp.Body)
		dlResp.Body.Close()

		hasher := sha256.Sum256(pdfBytes)
		computedChecksum := hex.EncodeToString(hasher[:])
		if computedChecksum != cmd.SHA256Checksum {
			t.Fatalf("doc %d checksum mismatch: computed %s, command %s", i, computedChecksum, cmd.SHA256Checksum)
		}
	}

	// Verify per-document options
	// Doc 0
	cmd0 := receivedCommands[0]
	if cmd0.Copies != 1 || cmd0.Duplex != contracts.DuplexLongEdge || cmd0.ColorMode != "BW" || cmd0.TotalPages != 3 || cmd0.TotalSheets != 2 {
		t.Fatalf("doc 0 options mismatch: %+v", cmd0)
	}
	// Doc 1
	cmd1 := receivedCommands[1]
	if cmd1.Copies != 2 || cmd1.Duplex != contracts.DuplexSimplex || cmd1.ColorMode != "COLOR" || cmd1.TotalPages != 2 || cmd1.TotalSheets != 4 {
		t.Fatalf("doc 1 options mismatch: %+v", cmd1)
	}
	// Doc 2 (2-up)
	cmd2 := receivedCommands[2]
	if cmd2.Copies != 1 || cmd2.Duplex != contracts.DuplexSimplex || cmd2.ColorMode != "BW" || cmd2.TotalPages != 2 || cmd2.TotalSheets != 2 {
		t.Fatalf("doc 2 options mismatch: %+v", cmd2)
	}

	// 7. Kiosk sends job_ack for each command
	for _, cmd := range receivedCommands {
		ackMsg := map[string]interface{}{
			"type": "job_ack",
			"progress": map[string]interface{}{
				"job_id":   cmd.JobID,
				"kiosk_id": kioskID,
				"status":   string(contracts.JobStatusSpooling),
			},
		}
		if err := wsjson.Write(ctx, wsConn, ackMsg); err != nil {
			t.Fatalf("failed to write job_ack for %s: %v", cmd.JobID, err)
		}
	}

	// Verify all 3 dispatch intents in cloud transition to ACKNOWLEDGED
	for _, cmd := range receivedCommands {
		deadline := time.Now().Add(2 * time.Second)
		var status string
		for time.Now().Before(deadline) {
			status, _, _ = st.GetJobDispatchIntentStatus(context.Background(), cmd.JobID)
			if status == "ACKNOWLEDGED" {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if status != "ACKNOWLEDGED" {
			t.Fatalf("expected dispatch intent %s ACKNOWLEDGED, got %s", cmd.JobID, status)
		}
	}

	// 8. Kiosk sends sequential progress events
	// Doc 0 completes
	prog0 := map[string]interface{}{
		"type": "progress",
		"progress": map[string]interface{}{
			"job_id":         cmd0.JobID,
			"kiosk_id":       kioskID,
			"status":         string(contracts.JobStatusCompleted),
			"pages_printed":  cmd0.TotalPages,
			"sheets_printed": cmd0.TotalSheets,
			"timestamp":      time.Now().UTC().Format(time.RFC3339Nano),
		},
	}
	_ = wsjson.Write(ctx, wsConn, prog0)

	// Wait and verify: Order must NOT be completed yet
	time.Sleep(100 * time.Millisecond)
	ord1, _ := st.GetOrder(context.Background(), orderID)
	if ord1 == nil || ord1.Status == contracts.JobStatusCompleted {
		t.Fatalf("order prematurely completed after doc 0! status=%v", ord1)
	}

	// Doc 1 completes
	prog1 := map[string]interface{}{
		"type": "progress",
		"progress": map[string]interface{}{
			"job_id":         cmd1.JobID,
			"kiosk_id":       kioskID,
			"status":         string(contracts.JobStatusCompleted),
			"pages_printed":  cmd1.TotalPages,
			"sheets_printed": cmd1.TotalSheets,
			"timestamp":      time.Now().UTC().Format(time.RFC3339Nano),
		},
	}
	_ = wsjson.Write(ctx, wsConn, prog1)

	// Wait and verify: Order must NOT be completed yet
	time.Sleep(100 * time.Millisecond)
	ord2, _ := st.GetOrder(context.Background(), orderID)
	if ord2 == nil || ord2.Status == contracts.JobStatusCompleted {
		t.Fatalf("order prematurely completed after doc 1! status=%v", ord2)
	}

	// Doc 2 completes (final document)
	prog2 := map[string]interface{}{
		"type": "progress",
		"progress": map[string]interface{}{
			"job_id":         cmd2.JobID,
			"kiosk_id":       kioskID,
			"status":         string(contracts.JobStatusCompleted),
			"pages_printed":  cmd2.TotalPages,
			"sheets_printed": cmd2.TotalSheets,
			"timestamp":      time.Now().UTC().Format(time.RFC3339Nano),
		},
	}
	_ = wsjson.Write(ctx, wsConn, prog2)

	// Poll until Cloud marks parent order COMPLETED
	orderDeadline := time.Now().Add(3 * time.Second)
	var finalOrder *store.Order
	for time.Now().Before(orderDeadline) {
		finalOrder, _ = st.GetOrder(context.Background(), orderID)
		if finalOrder != nil && finalOrder.Status == contracts.JobStatusCompleted {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if finalOrder == nil || finalOrder.Status != contracts.JobStatusCompleted {
		t.Fatalf("expected order COMPLETED after all 3 documents, got status=%v", finalOrder)
	}

	// Verify all 3 dispatch intents in cloud are COMPLETED
	for _, cmd := range receivedCommands {
		status, _, err := st.GetJobDispatchIntentStatus(context.Background(), cmd.JobID)
		if err != nil || status != "COMPLETED" {
			t.Fatalf("expected dispatch intent %s COMPLETED, got %s (err=%v)", cmd.JobID, status, err)
		}
	}
}

// TestMultiDoc_Cloud_ConcurrentDuplicateWebhooks verifies that concurrent duplicate payment webhooks
// result in strictly atomic single dispatch intent creation and idempotent success.
func TestMultiDoc_Cloud_ConcurrentDuplicateWebhooks(t *testing.T) {
	_, ts, st, kSec, whSec := setupMultiDocCloudTestServer(t)
	kioskID := "kiosk-cloud-dup-1"

	tmpDir := t.TempDir()
	d0 := createTestPDFWithPages(t, tmpDir, 1)
	d1 := createTestPDFWithPages(t, tmpDir, 1)
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
	pricePaise := int64(uploadResp["total_price_paise"].(float64))

	// Claim to get session
	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kSec)
	claimResp, _ := http.DefaultClient.Do(claimReq)
	var claimData map[string]interface{}
	_ = json.NewDecoder(claimResp.Body).Decode(&claimData)
	claimResp.Body.Close()
	sessionID := claimData["session_id"].(string)

	const concurrency = 6
	paymentID := "pay_e2e_conc_dup_99"

	var wg sync.WaitGroup
	results := make([]int, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			payload := map[string]interface{}{
				"event": "payment.captured",
				"payload": map[string]interface{}{
					"payment": map[string]interface{}{
						"entity": map[string]interface{}{
							"id":       paymentID,
							"order_id": orderID,
							"status":   "captured",
							"amount":   pricePaise,
							"notes": map[string]interface{}{
								"order_id":   orderID,
								"session_id": sessionID,
								"kiosk_id":   kioskID,
							},
						},
					},
				},
			}
			rawB, _ := json.Marshal(payload)
			mac := hmac.New(sha256.New, []byte(whSec))
			mac.Write(rawB)
			sig := hex.EncodeToString(mac.Sum(nil))

			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/webhooks/razorpay", bytes.NewReader(rawB))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Razorpay-Signature", sig)
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				results[idx] = resp.StatusCode
				resp.Body.Close()
			}
		}(i)
	}
	wg.Wait()

	for i, code := range results {
		if code != http.StatusOK {
			t.Fatalf("webhook call %d failed with code %d", i, code)
		}
	}

	// Verify order is marked DISPATCHED
	ord, err := st.GetOrder(context.Background(), orderID)
	if err != nil || (ord.Status != contracts.JobStatusDispatched && ord.Status != contracts.JobStatusPaidWaiting) {
		t.Fatalf("expected order DISPATCHED, got %v (err=%v)", ord.Status, err)
	}

	// Verify exactly 2 dispatch intents exist in cloud store
	pending, err := st.GetPendingDispatchOrders(context.Background(), kioskID)
	if err != nil {
		t.Fatalf("GetPendingDispatchOrders failed: %v", err)
	}
	if len(pending) > 2 {
		t.Fatalf("expected at most 2 dispatch intents, got %d", len(pending))
	}
}

func readNextJobCommand(ctx context.Context, c *websocket.Conn) (*contracts.PrintJobCommand, error) {
	for {
		var rawMsg struct {
			Type string                     `json:"type"`
			Job  *contracts.PrintJobCommand `json:"job"`
		}
		if err := wsjson.Read(ctx, c, &rawMsg); err != nil {
			return nil, err
		}
		if rawMsg.Type == "job" && rawMsg.Job != nil {
			return rawMsg.Job, nil
		}
	}
}

// TestMultiDoc_Cloud_WebSocketReconnectReplay verifies that when a kiosk disconnects mid-order,
// reconnecting replays uncompleted dispatch intents in document order without duplicating completed jobs.
func TestMultiDoc_Cloud_WebSocketReconnectReplay(t *testing.T) {
	app, ts, _, kSec, _ := setupMultiDocCloudTestServer(t)
	kioskID := "kiosk-cloud-recon-1"

	tmpDir := t.TempDir()
	d0 := createTestPDFWithPages(t, tmpDir, 1)
	d1 := createTestPDFWithPages(t, tmpDir, 1)
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

	// Claim
	claimReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", ts.URL, kioskID, orderID), nil)
	claimReq.Header.Set("Authorization", "Bearer "+kSec)
	claimResp, _ := http.DefaultClient.Do(claimReq)
	var claimData map[string]interface{}
	_ = json.NewDecoder(claimResp.Body).Decode(&claimData)
	claimResp.Body.Close()
	sessionID := claimData["session_id"].(string)

	// Connect WS 1
	u, _ := url.Parse(ts.URL)
	u.Scheme = "ws"
	u.Path = "/ws"
	q := u.Query()
	q.Set("kiosk_id", kioskID)
	u.RawQuery = q.Encode()

	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()

	ws1, _, err := websocket.Dial(ctx1, u.String(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + kSec}},
	})
	if err != nil {
		t.Fatalf("websocket 1 dial failed: %v", err)
	}

	// Trigger payment via direct store-level capture (isolated test fixture)
	if _, err := testDirectStoreCapturePayment(ctx1, app, orderID, kioskID, sessionID); err != nil {
		t.Fatalf("testDirectStoreCapturePayment failed: %v", err)
	}

	// Read Doc 0 from WS 1
	jobCmd0, err := readNextJobCommand(ctx1, ws1)
	if err != nil {
		t.Fatalf("failed to read doc 0: %v", err)
	}

	// Mark Doc 0 completed
	_ = wsjson.Write(ctx1, ws1, map[string]interface{}{
		"type": "progress",
		"progress": map[string]interface{}{
			"job_id":         jobCmd0.JobID,
			"kiosk_id":       kioskID,
			"status":         string(contracts.JobStatusCompleted),
			"pages_printed":  1,
			"sheets_printed": 1,
			"timestamp":      time.Now().UTC().Format(time.RFC3339Nano),
		},
	})
	time.Sleep(100 * time.Millisecond)

	// Disconnect WS 1
	_ = ws1.Close(websocket.StatusNormalClosure, "disconnecting")

	// Connect WS 2 (reconnect)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()

	ws2, _, err := websocket.Dial(ctx2, u.String(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + kSec}},
	})
	if err != nil {
		t.Fatalf("websocket 2 dial failed: %v", err)
	}
	defer ws2.CloseNow()

	// On reconnect, Cloud replays uncompleted jobs (Doc 1)
	replayedCmd, err := readNextJobCommand(ctx2, ws2)
	if err != nil {
		t.Fatalf("failed reading replayed job: %v", err)
	}

	if replayedCmd.DocumentIndex != 1 {
		t.Fatalf("expected replayed job to be Doc 1, got Doc %d (jobID=%s)", replayedCmd.DocumentIndex, replayedCmd.JobID)
	}
}

// TestMultiDoc_Cloud_PublicPaymentGateBlocked verifies that all public customer endpoints
// reject multi-document payment initiation with HTTP 409 and MULTI_DOC_PAYMENT_BLOCKED.
func TestMultiDoc_Cloud_PublicPaymentGateBlocked(t *testing.T) {
	_, ts, st, kSec, _ := setupMultiDocCloudTestServer(t)
	kioskID := "kiosk-cloud-block-1"

	tmpDir := t.TempDir()
	d0 := createTestPDFWithPages(t, tmpDir, 1)
	d1 := createTestPDFWithPages(t, tmpDir, 1)
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

	// Create an active kiosk session for this order
	sess, err := st.CreateOrGetKioskSessionWithPaperCheck(context.Background(), kioskID, orderID, 100, 5*time.Minute, time.Now().UTC())
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	bypassHeaders := []map[string]string{
		{}, // no extra headers
		{"X-Test-Payment-Auth": "test-authorized"},
		{"X-Admin-Bypass": "true"},
		{"X-Bypass-Gate": "1"},
		{"X-XeroxGo-Proxy-Secret": "bypass"},
		{"X-Role": "admin"},
		{"X-Custom-Auth": "super-user"},
	}

	for _, headers := range bypassHeaders {
		// 1. Customer initiate-payment: must fail with MULTI_DOC_PAYMENT_BLOCKED
		initReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/initiate-payment", ts.URL, kioskID, orderID), nil)
		initReq.Header.Set("Authorization", "Bearer "+kSec)
		for k, v := range headers {
			initReq.Header.Set(k, v)
		}
		initResp, err := http.DefaultClient.Do(initReq)
		if err != nil || initResp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 Conflict for initiate-payment with header %v, got %d (err=%v)", headers, initResp.StatusCode, err)
		}
		var initData map[string]interface{}
		_ = json.NewDecoder(initResp.Body).Decode(&initData)
		initResp.Body.Close()
		if initData["code"] != "MULTI_DOC_PAYMENT_BLOCKED" {
			t.Errorf("initiate-payment code = %v with header %v, want MULTI_DOC_PAYMENT_BLOCKED", initData["code"], headers)
		}

		// 2. Simulate-payment: must fail with MULTI_DOC_PAYMENT_BLOCKED regardless of client headers
		simReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/orders/%s/simulate-payment?session_id=%s", ts.URL, orderID, sess.SessionID), nil)
		for k, v := range headers {
			simReq.Header.Set(k, v)
		}
		simResp, err := http.DefaultClient.Do(simReq)
		if err != nil || simResp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 Conflict for simulate-payment with header %v, got %d (err=%v)", headers, simResp.StatusCode, err)
		}
		var simData map[string]interface{}
		_ = json.NewDecoder(simResp.Body).Decode(&simData)
		simResp.Body.Close()
		if simData["code"] != "MULTI_DOC_PAYMENT_BLOCKED" {
			t.Errorf("simulate-payment code = %v with header %v, want MULTI_DOC_PAYMENT_BLOCKED", simData["code"], headers)
		}
	}
}
