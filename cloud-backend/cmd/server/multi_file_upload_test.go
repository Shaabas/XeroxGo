package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestMultiFileUploadAndOrderCreation(t *testing.T) {
	app, mux := setupUploadTestServer(t)
	_ = app

	tmpDir := t.TempDir()
	// Create Doc 1: 3-page PDF (to test duplex with odd pages)
	doc1Path := createTestPDFWithPages(t, tmpDir, 3)
	// Create Doc 2: 2-page PDF
	doc2Path := createTestPDFWithPages(t, tmpDir, 2)
	// Create Doc 3: 1-page PDF
	doc3Path := createTestPDFWithPages(t, tmpDir, 1)

	doc1Bytes, err := os.ReadFile(doc1Path)
	if err != nil {
		t.Fatalf("failed to read doc1: %v", err)
	}
	doc2Bytes, err := os.ReadFile(doc2Path)
	if err != nil {
		t.Fatalf("failed to read doc2: %v", err)
	}
	doc3Bytes, err := os.ReadFile(doc3Path)
	if err != nil {
		t.Fatalf("failed to read doc3: %v", err)
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// Add files to "files" field
	p1, _ := writer.CreateFormFile("files", "Lecture Notes.pdf")
	_, _ = io.Copy(p1, bytes.NewReader(doc1Bytes))

	p2, _ := writer.CreateFormFile("files", "Assignment.pdf")
	_, _ = io.Copy(p2, bytes.NewReader(doc2Bytes))

	p3, _ := writer.CreateFormFile("files", "ID Photo.pdf")
	_, _ = io.Copy(p3, bytes.NewReader(doc3Bytes))

	// Settings metadata:
	// Doc 1: 3 pages, duplex, 1 copy -> 3 printed pages, 2 physical sheets
	// Doc 2: 2 pages, simplex, 1 copy -> 2 printed pages, 2 physical sheets
	// Doc 3: 1 page, simplex, 1 copy -> 1 printed page, 1 physical sheet
	metaJSON := `[
		{"copies": 1, "duplex": "DUPLEX_LONG_EDGE", "color_mode": "BW", "page_range": ""},
		{"copies": 1, "duplex": "SIMPLEX", "color_mode": "BW", "page_range": ""},
		{"copies": 1, "duplex": "SIMPLEX", "color_mode": "BW", "page_range": ""}
	]`
	_ = writer.WriteField("metadata", metaJSON)
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-multi-test/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()

	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse JSON response: %v", err)
	}

	orderID, _ := resp["order_id"].(string)
	displayCode, _ := resp["display_code"].(string)
	totalPages := int(resp["total_pages"].(float64))
	totalSheets := int(resp["total_sheets"].(float64))
	totalPricePaise := int64(resp["total_price_paise"].(float64))
	docsCount := int(resp["documents_count"].(float64))

	if orderID == "" {
		t.Error("expected non-empty order_id")
	}
	if len(displayCode) != 4 {
		t.Errorf("expected 4-digit display_code PIN, got %q", displayCode)
	}
	if docsCount != 3 {
		t.Errorf("documents_count = %d, want 3", docsCount)
	}

	// Doc 1: 3 printed sides (duplex rate ₹1.50 = 150 paise) = 450 paise
	// Doc 2: 2 printed sides (simplex rate ₹2.00 = 200 paise) = 400 paise
	// Doc 3: 1 printed side (simplex rate ₹2.00 = 200 paise) = 200 paise
	// Total printed pages/sides = 3 + 2 + 1 = 6
	// Total sheets = 2 (for doc1 3p duplex) + 2 (doc2 2p simplex) + 1 (doc3 1p simplex) = 5 physical sheets
	// Total price = 450 + 400 + 200 = 1050 paise
	if totalPages != 6 {
		t.Errorf("total_pages = %d, want 6", totalPages)
	}
	if totalSheets != 5 {
		t.Errorf("total_sheets = %d, want 5", totalSheets)
	}
	if totalPricePaise != 1050 {
		t.Errorf("total_price_paise = %d, want 1050", totalPricePaise)
	}

	// In multi-document printing architecture, no concatenated merged PDF is created.
	// Instead, each document is normalized independently into its own canonical PDF.
	h := sha256ChecksumString(orderID)
	mergedPDFPath := filepath.Join(app.cfg.jobsDir, h+".pdf")
	if _, err := os.Stat(mergedPDFPath); !os.IsNotExist(err) {
		t.Fatalf("expected no merged PDF at %s, but file exists", mergedPDFPath)
	}

	// Verify child documents from store:
	docs, err := app.db.GetOrderDocuments(context.Background(), orderID)
	if err != nil {
		t.Fatalf("GetOrderDocuments failed: %v", err)
	}
	if len(docs) != 3 {
		t.Fatalf("expected 3 child documents, got %d", len(docs))
	}

	// Doc 1: 3 content pages, duplex -> 4 pages on disk (with duplex blank back padding), 450 paise
	doc1 := docs[0]
	if doc1.DocumentIndex != 0 || doc1.TotalPages != 3 || doc1.TotalSheets != 2 || doc1.PricePaise != 450 {
		t.Errorf("doc1 mismatch: %+v", doc1)
	}
	d1PDFPath := filepath.Join(app.cfg.jobsDir, sha256ChecksumString(doc1.JobID)+".pdf")
	cmdCheck1 := exec.Command("qpdf", "--check", d1PDFPath)
	if out, err := cmdCheck1.CombinedOutput(); err != nil {
		t.Fatalf("qpdf --check failed on doc1: %s: %v", strings.TrimSpace(string(out)), err)
	}
	if getPDFPageCount(t, d1PDFPath) != 4 {
		t.Errorf("doc1 PDF pages = %d, want 4 (with duplex padding)", getPDFPageCount(t, d1PDFPath))
	}

	// Doc 2: 2 content pages, simplex -> 2 pages on disk, 400 paise
	doc2 := docs[1]
	if doc2.DocumentIndex != 1 || doc2.TotalPages != 2 || doc2.TotalSheets != 2 || doc2.PricePaise != 400 {
		t.Errorf("doc2 mismatch: %+v", doc2)
	}
	d2PDFPath := filepath.Join(app.cfg.jobsDir, sha256ChecksumString(doc2.JobID)+".pdf")
	cmdCheck2 := exec.Command("qpdf", "--check", d2PDFPath)
	if out, err := cmdCheck2.CombinedOutput(); err != nil {
		t.Fatalf("qpdf --check failed on doc2: %s: %v", strings.TrimSpace(string(out)), err)
	}
	if getPDFPageCount(t, d2PDFPath) != 2 {
		t.Errorf("doc2 PDF pages = %d, want 2", getPDFPageCount(t, d2PDFPath))
	}

	// Doc 3: 1 content page, simplex -> 1 page on disk, 200 paise
	doc3 := docs[2]
	if doc3.DocumentIndex != 2 || doc3.TotalPages != 1 || doc3.TotalSheets != 1 || doc3.PricePaise != 200 {
		t.Errorf("doc3 mismatch: %+v", doc3)
	}
	d3PDFPath := filepath.Join(app.cfg.jobsDir, sha256ChecksumString(doc3.JobID)+".pdf")
	cmdCheck3 := exec.Command("qpdf", "--check", d3PDFPath)
	if out, err := cmdCheck3.CombinedOutput(); err != nil {
		t.Fatalf("qpdf --check failed on doc3: %s: %v", strings.TrimSpace(string(out)), err)
	}
	if getPDFPageCount(t, d3PDFPath) != 1 {
		t.Errorf("doc3 PDF pages = %d, want 1", getPDFPageCount(t, d3PDFPath))
	}
}

func sha256ChecksumString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestSingleFileUploadWithMetadataAndDuplexFormats(t *testing.T) {
	_, mux := setupUploadTestServer(t)
	tmpDir := t.TempDir()
	docPath := createTestPDFWithPages(t, tmpDir, 2)
	docBytes, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("failed to read test pdf: %v", err)
	}

	tests := []struct {
		name         string
		metaJSON     string
		formDuplex   string
		formCopies   string
		wantDuplex   string
		wantStatus   int
		expectReject bool
	}{
		{
			name:       "single_file_metadata_string_duplex_long_edge",
			metaJSON:   `[{"copies": 1, "duplex": "DUPLEX_LONG_EDGE", "color_mode": "BW", "page_range": ""}]`,
			wantDuplex: "DUPLEX_LONG_EDGE",
			wantStatus: http.StatusOK,
		},
		{
			name:       "single_file_metadata_boolean_duplex_true",
			metaJSON:   `[{"copies": 1, "duplex": true, "color_mode": "BW", "page_range": ""}]`,
			wantDuplex: "DUPLEX_LONG_EDGE",
			wantStatus: http.StatusOK,
		},
		{
			name:       "single_file_metadata_boolean_duplex_false",
			metaJSON:   `[{"copies": 1, "duplex": false, "color_mode": "BW", "page_range": ""}]`,
			wantDuplex: "SIMPLEX",
			wantStatus: http.StatusOK,
		},
		{
			name:       "single_file_metadata_string_simplex",
			metaJSON:   `[{"copies": 1, "duplex": "SIMPLEX", "color_mode": "BW", "page_range": ""}]`,
			wantDuplex: "SIMPLEX",
			wantStatus: http.StatusOK,
		},
		{
			name:       "single_file_no_metadata_omitted_duplex_defaults_to_simplex",
			metaJSON:   "",
			formCopies: "1",
			wantDuplex: "SIMPLEX",
			wantStatus: http.StatusOK,
		},
		{
			name:         "single_file_invalid_duplex_rejected",
			metaJSON:     "",
			formDuplex:   "NOT_A_VALID_DUPLEX_MODE",
			wantStatus:   http.StatusBadRequest,
			expectReject: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := &bytes.Buffer{}
			writer := multipart.NewWriter(body)

			p, err := writer.CreateFormFile("files", "SingleDoc.pdf")
			if err != nil {
				t.Fatalf("CreateFormFile failed: %v", err)
			}
			if _, err := p.Write(docBytes); err != nil {
				t.Fatalf("failed to write doc bytes: %v", err)
			}

			if tc.metaJSON != "" {
				_ = writer.WriteField("metadata", tc.metaJSON)
			}
			if tc.formDuplex != "" {
				_ = writer.WriteField("duplex", tc.formDuplex)
			}
			if tc.formCopies != "" {
				_ = writer.WriteField("copies", tc.formCopies)
			}
			_ = writer.Close()

			req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-duplex-test/upload", body)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			w := httptest.NewRecorder()

			mux.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("expected HTTP status %d, got %d: %s", tc.wantStatus, w.Code, w.Body.String())
			}

			if !tc.expectReject {
				var resp map[string]interface{}
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("failed to parse JSON: %v", err)
				}
				gotDuplex, _ := resp["duplex"].(string)
				if gotDuplex != tc.wantDuplex {
					t.Errorf("expected duplex %q, got %q", tc.wantDuplex, gotDuplex)
				}
			}
		})
	}
}

func TestMultiFileUploadWithOrientationAndNUp(t *testing.T) {
	app, mux := setupUploadTestServer(t)
	tmpDir := t.TempDir()

	doc1Path := createTestPDFWithPages(t, tmpDir, 4)
	doc2Path := createTestPDFWithPages(t, tmpDir, 6)

	doc1Bytes, _ := os.ReadFile(doc1Path)
	doc2Bytes, _ := os.ReadFile(doc2Path)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	p1, _ := writer.CreateFormFile("files", "Doc1_4p.pdf")
	_, _ = p1.Write(doc1Bytes)
	p2, _ := writer.CreateFormFile("files", "Doc2_6p.pdf")
	_, _ = p2.Write(doc2Bytes)

	// Doc 1: 4 pages, 2-up, landscape, simplex -> 2 imposed sides, 2 sheets, 400 paise
	// Doc 2: 6 pages, 4-up, auto, duplex -> 2 imposed sides, 1 sheet, 300 paise
	metaJSON := `[
		{"copies": 1, "duplex": "SIMPLEX", "color_mode": "BW", "page_range": "", "orientation": "LANDSCAPE", "pages_per_sheet": 2},
		{"copies": 1, "duplex": "DUPLEX_LONG_EDGE", "color_mode": "BW", "page_range": "", "orientation": "AUTO", "pages_per_sheet": 4}
	]`
	_ = writer.WriteField("metadata", metaJSON)
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-nup-test/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()

	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse JSON response: %v", err)
	}

	totalPages := int(resp["total_pages"].(float64))
	totalSheets := int(resp["total_sheets"].(float64))
	totalPricePaise := int64(resp["total_price_paise"].(float64))
	docsCount := int(resp["documents_count"].(float64))

	if docsCount != 2 {
		t.Errorf("documents_count = %d, want 2", docsCount)
	}
	if totalPages != 4 {
		t.Errorf("total_pages = %d, want 4", totalPages)
	}
	if totalSheets != 3 {
		t.Errorf("total_sheets = %d, want 3", totalSheets)
	}
	if totalPricePaise != 700 {
		t.Errorf("total_price_paise = %d, want 700", totalPricePaise)
	}

	orderID, _ := resp["order_id"].(string)
	h := sha256ChecksumString(orderID)
	mergedPDFPath := filepath.Join(app.cfg.jobsDir, h+".pdf")
	if _, err := os.Stat(mergedPDFPath); !os.IsNotExist(err) {
		t.Fatalf("expected no merged PDF at %s, but file exists", mergedPDFPath)
	}

	docs, err := app.db.GetOrderDocuments(context.Background(), orderID)
	if err != nil {
		t.Fatalf("GetOrderDocuments failed: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 child documents, got %d", len(docs))
	}

	// Doc 1: 4 pages, 2-up -> 2 imposed sides, 2 sheets, 400 paise
	doc1 := docs[0]
	if doc1.DocumentIndex != 0 || doc1.TotalPages != 2 || doc1.TotalSheets != 2 || doc1.PricePaise != 400 {
		t.Errorf("doc1 mismatch: %+v", doc1)
	}
	d1PDFPath := filepath.Join(app.cfg.jobsDir, sha256ChecksumString(doc1.JobID)+".pdf")
	if out, err := exec.Command("qpdf", "--check", d1PDFPath).CombinedOutput(); err != nil {
		t.Fatalf("qpdf --check failed on doc1: %s: %v", strings.TrimSpace(string(out)), err)
	}
	if getPDFPageCount(t, d1PDFPath) != 2 {
		t.Errorf("doc1 PDF pages = %d, want 2", getPDFPageCount(t, d1PDFPath))
	}

	// Doc 2: 6 pages, 4-up -> 2 imposed sides, 1 sheet, 300 paise
	doc2 := docs[1]
	if doc2.DocumentIndex != 1 || doc2.TotalPages != 2 || doc2.TotalSheets != 1 || doc2.PricePaise != 300 {
		t.Errorf("doc2 mismatch: %+v", doc2)
	}
	d2PDFPath := filepath.Join(app.cfg.jobsDir, sha256ChecksumString(doc2.JobID)+".pdf")
	if out, err := exec.Command("qpdf", "--check", d2PDFPath).CombinedOutput(); err != nil {
		t.Fatalf("qpdf --check failed on doc2: %s: %v", strings.TrimSpace(string(out)), err)
	}
	if getPDFPageCount(t, d2PDFPath) != 2 {
		t.Errorf("doc2 PDF pages = %d, want 2", getPDFPageCount(t, d2PDFPath))
	}
}

func TestMultiFileUpload_PerJobSecureDownload(t *testing.T) {
	app, mux := setupUploadTestServer(t)
	mux.HandleFunc("/api/orders/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pdf") {
			app.handleDownloadPDF(w, r)
			return
		}
		http.NotFound(w, r)
	})

	tmpDir := t.TempDir()
	doc1Path := createTestPDFWithPages(t, tmpDir, 2)
	doc2Path := createTestPDFWithPages(t, tmpDir, 1)

	doc1Bytes, _ := os.ReadFile(doc1Path)
	doc2Bytes, _ := os.ReadFile(doc2Path)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	p1, _ := writer.CreateFormFile("files", "Doc1.pdf")
	_, _ = p1.Write(doc1Bytes)
	p2, _ := writer.CreateFormFile("files", "Doc2.pdf")
	_, _ = p2.Write(doc2Bytes)
	metaJSON := `[
		{"copies": 1, "duplex": "SIMPLEX", "color_mode": "BW", "page_range": ""},
		{"copies": 1, "duplex": "SIMPLEX", "color_mode": "BW", "page_range": ""}
	]`
	_ = writer.WriteField("metadata", metaJSON)
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-dl-test/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload failed: %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	orderID := resp["order_id"].(string)

	docs, err := app.db.GetOrderDocuments(context.Background(), orderID)
	if err != nil || len(docs) != 2 {
		t.Fatalf("expected 2 docs, err: %v", err)
	}

	for i, doc := range docs {
		// 1. Successful download using authoritative DownloadURL
		dlReq := httptest.NewRequest(http.MethodGet, doc.DownloadURL, nil)
		dlRec := httptest.NewRecorder()
		mux.ServeHTTP(dlRec, dlReq)

		if dlRec.Code != http.StatusOK {
			t.Fatalf("doc[%d] download failed with %d: %s", i, dlRec.Code, dlRec.Body.String())
		}
		if ct := dlRec.Header().Get("Content-Type"); ct != "application/pdf" {
			t.Errorf("doc[%d] Content-Type = %q, want application/pdf", i, ct)
		}

		downloadedBytes := dlRec.Body.Bytes()
		h := sha256.Sum256(downloadedBytes)
		calcChecksum := hex.EncodeToString(h[:])
		if calcChecksum != doc.SHA256Checksum {
			t.Errorf("doc[%d] checksum mismatch: got %s, want %s", i, calcChecksum, doc.SHA256Checksum)
		}

		// 2. Tampered signature rejected (401)
		tamperedURL := doc.DownloadURL + "tampered"
		tReq := httptest.NewRequest(http.MethodGet, tamperedURL, nil)
		tRec := httptest.NewRecorder()
		mux.ServeHTTP(tRec, tReq)
		if tRec.Code != http.StatusUnauthorized {
			t.Errorf("doc[%d] tampered sig expected 401, got %d", i, tRec.Code)
		}

		// 3. Expired timestamp rejected (401)
		expiredURL := generateSignedJobDownloadURL("http://example.com", orderID, doc.JobID, app.cfg.downloadSecret, -1*time.Hour)
		expReq := httptest.NewRequest(http.MethodGet, expiredURL, nil)
		expRec := httptest.NewRecorder()
		mux.ServeHTTP(expRec, expReq)
		if expRec.Code != http.StatusUnauthorized {
			t.Errorf("doc[%d] expired sig expected 401, got %d", i, expRec.Code)
		}

		// 4. Cross-order isolation: wrong orderID with valid signature for wrong order rejected (403)
		crossOrderURL := generateSignedJobDownloadURL("http://example.com", "ord_evil_other", doc.JobID, app.cfg.downloadSecret, 24*time.Hour)
		crReq := httptest.NewRequest(http.MethodGet, crossOrderURL, nil)
		crRec := httptest.NewRecorder()
		mux.ServeHTTP(crRec, crReq)
		if crRec.Code != http.StatusForbidden {
			t.Errorf("doc[%d] cross-order download expected 403, got %d", i, crRec.Code)
		}
	}

	// 5. Calling order-level /api/orders/{orderID}/pdf on multi-doc order is rejected (400)
	orderLevelURL := generateSignedDownloadURL("http://example.com", orderID, app.cfg.downloadSecret, 24*time.Hour)
	olReq := httptest.NewRequest(http.MethodGet, orderLevelURL, nil)
	olRec := httptest.NewRecorder()
	mux.ServeHTTP(olRec, olReq)
	if olRec.Code != http.StatusBadRequest {
		t.Errorf("order-level download on multi-doc order expected 400, got %d", olRec.Code)
	}
}

func TestMultiFileUpload_AtomicRollbackOnFailure(t *testing.T) {
	app, mux := setupUploadTestServer(t)
	tmpDir := t.TempDir()

	doc1Path := createTestPDFWithPages(t, tmpDir, 2)
	doc1Bytes, _ := os.ReadFile(doc1Path)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	p1, _ := writer.CreateFormFile("files", "DocValid1.pdf")
	_, _ = p1.Write(doc1Bytes)

	// Doc 2 is corrupt (invalid PDF bytes)
	p2, _ := writer.CreateFormFile("files", "DocCorrupt.pdf")
	_, _ = p2.Write([]byte("not-a-valid-pdf-header-content"))

	p3, _ := writer.CreateFormFile("files", "DocValid2.pdf")
	_, _ = p3.Write(doc1Bytes)

	_ = writer.Close()

	initialFiles, _ := os.ReadDir(app.cfg.jobsDir)

	req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-atomic-test/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for corrupted upload, got %d: %s", w.Code, w.Body.String())
	}

	// Verify no orphaned files were left in jobsDir
	afterFiles, _ := os.ReadDir(app.cfg.jobsDir)
	if len(afterFiles) != len(initialFiles) {
		var names []string
		for _, f := range afterFiles {
			names = append(names, f.Name())
		}
		t.Fatalf("orphaned files left after rollback: %v", names)
	}
}

func TestMultiFileUpload_PaymentInitiationSupported(t *testing.T) {
	app, _ := setupUploadTestServer(t)
	app.cfg.devMode = true
	app.hub = nil
	app.cfg.webhookSecret = []byte("whsec_test_secret_32_bytes_long_ok")
	mux := http.NewServeMux()
	app.cfg.kioskSecret = []byte("mock-kiosk-secret")
	mux.HandleFunc("/api/k/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/initiate-payment") {
			app.handleInitiatePayment(w, r)
			return
		}
		app.handleUpload(w, r)
	})
	mux.HandleFunc("/api/orders/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/test/simulate-payment") {
			app.handleFixtureSimulatePayment(w, r)
			return
		}
		http.NotFound(w, r)
	})

	tmpDir := t.TempDir()
	doc1Path := createTestPDFWithPages(t, tmpDir, 1)
	doc2Path := createTestPDFWithPages(t, tmpDir, 1)
	d1Bytes, _ := os.ReadFile(doc1Path)
	d2Bytes, _ := os.ReadFile(doc2Path)

	// 1. Upload multi-doc order
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	p1, _ := writer.CreateFormFile("files", "Doc1.pdf")
	_, _ = p1.Write(d1Bytes)
	p2, _ := writer.CreateFormFile("files", "Doc2.pdf")
	_, _ = p2.Write(d2Bytes)
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-pay-guard/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload failed: %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	orderID := resp["order_id"].(string)

	now := time.Now().UTC()
	// Create active kiosk session for this order
	_, err := app.db.CreateOrGetKioskSessionWithPaperCheck(context.Background(), "kiosk-pay-guard", orderID, 100, 5*time.Minute, now)
	if err != nil {
		t.Fatalf("failed to create kiosk session: %v", err)
	}

	// 2. Attempt handleInitiatePayment on multi-doc order
	payReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/k/kiosk-pay-guard/orders/%s/initiate-payment", orderID), nil)
	payReq.Header.Set("Authorization", "Bearer mock-kiosk-secret")
	payRec := httptest.NewRecorder()
	mux.ServeHTTP(payRec, payReq)

	if payRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for multi-doc payment initiation, got %d: %s", payRec.Code, payRec.Body.String())
	}
	var payResp map[string]interface{}
	_ = json.Unmarshal(payRec.Body.Bytes(), &payResp)
	if payResp["attempt_id"] == nil || payResp["attempt_id"] == "" {
		t.Errorf("expected non-empty attempt_id in payment response, got %v", payResp)
	}

	// 3. Attempt handleFixtureSimulatePayment on multi-doc order in devMode
	app.cfg.devMode = true
	simReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/orders/%s/test/simulate-payment?force=1", orderID), nil)
	simRec := httptest.NewRecorder()
	mux.ServeHTTP(simRec, simReq)

	if simRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for multi-doc simulate-payment, got %d: %s", simRec.Code, simRec.Body.String())
	}
	var simResp map[string]interface{}
	_ = json.Unmarshal(simRec.Body.Bytes(), &simResp)
	if simResp["status"] != "DISPATCH_PENDING" {
		t.Errorf("simulated paid multi-doc order without a hub must retain pending delivery, got %v", simResp["status"])
	}
	ord, err := app.db.GetOrder(context.Background(), orderID)
	if err != nil || ord.Status != contracts.JobStatusDispatched {
		t.Fatalf("multi-document payment authorization was lost: %+v %v",ord,err)
	}
	pending, err := app.db.GetPendingDispatchOrders(context.Background(),"kiosk-pay-guard")
	if err != nil || len(pending) != len(ord.Documents) {
		t.Fatalf("every paid child must have a reconnectable dispatch intent: %+v %v",pending,err)
	}
}

func TestMultiFileUpload_CopiesPreservedExactlyOnce(t *testing.T) {
	app, mux := setupUploadTestServer(t)
	tmpDir := t.TempDir()

	docPath := createTestPDFWithPages(t, tmpDir, 2)
	docBytes, _ := os.ReadFile(docPath)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	p, _ := writer.CreateFormFile("files", "CopiesDoc.pdf")
	_, _ = p.Write(docBytes)
	metaJSON := `[{"copies": 3, "duplex": "SIMPLEX", "color_mode": "BW", "page_range": ""}]`
	_ = writer.WriteField("metadata", metaJSON)
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-copies-test/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("upload failed: %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	orderID := resp["order_id"].(string)

	docs, err := app.db.GetOrderDocuments(context.Background(), orderID)
	if err != nil || len(docs) != 1 {
		t.Fatalf("expected 1 doc, err: %v", err)
	}

	doc := docs[0]
	if doc.Copies != 3 {
		t.Errorf("Copies = %d, want 3", doc.Copies)
	}
	if doc.TotalPages != 2 {
		t.Errorf("TotalPages = %d, want 2", doc.TotalPages)
	}
	if doc.TotalSheets != 6 { // 2 sheets per copy * 3 copies = 6 sheets
		t.Errorf("TotalSheets = %d, want 6", doc.TotalSheets)
	}
	if doc.PricePaise != 1200 { // 2 pages * 3 copies * 200 paise = 1200 paise
		t.Errorf("PricePaise = %d, want 1200", doc.PricePaise)
	}

	// Verify that the PDF on disk is EXACTLY 1 copy (2 pages), NOT duplicated 3 times (6 pages)!
	docPathOnDisk := filepath.Join(app.cfg.jobsDir, sha256ChecksumString(doc.JobID)+".pdf")
	if pages := getPDFPageCount(t, docPathOnDisk); pages != 2 {
		t.Errorf("PDF on disk has %d pages, want 2 (hardware replication exactly once)", pages)
	}
}

func TestMultiFileUpload_MetadataCountMismatch_Rejected(t *testing.T) {
	_, mux := setupUploadTestServer(t)

	tmpDir := t.TempDir()
	doc1Path := createTestPDFWithPages(t, tmpDir, 1)
	doc2Path := createTestPDFWithPages(t, tmpDir, 1)

	doc1Bytes, _ := os.ReadFile(doc1Path)
	doc2Bytes, _ := os.ReadFile(doc2Path)

	// Case 1: 2 files, 1 metadata item
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	p1, _ := writer.CreateFormFile("files", "doc1.pdf")
	_, _ = p1.Write(doc1Bytes)
	p2, _ := writer.CreateFormFile("files", "doc2.pdf")
	_, _ = p2.Write(doc2Bytes)
	_ = writer.WriteField("metadata", `[{"copies": 1, "duplex": "SIMPLEX", "color_mode": "BW"}]`)
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-meta-test/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for metadata count mismatch (1 vs 2), got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "metadata count (1) must match uploaded files count (2)") {
		t.Errorf("unexpected error message: %s", w.Body.String())
	}

	// Case 2: 2 files, 3 metadata items
	body2 := &bytes.Buffer{}
	writer2 := multipart.NewWriter(body2)
	p1_2, _ := writer2.CreateFormFile("files", "doc1.pdf")
	_, _ = p1_2.Write(doc1Bytes)
	p2_2, _ := writer2.CreateFormFile("files", "doc2.pdf")
	_, _ = p2_2.Write(doc2Bytes)
	_ = writer2.WriteField("metadata", `[
		{"copies": 1, "duplex": "SIMPLEX", "color_mode": "BW"},
		{"copies": 1, "duplex": "SIMPLEX", "color_mode": "BW"},
		{"copies": 1, "duplex": "SIMPLEX", "color_mode": "BW"}
	]`)
	_ = writer2.Close()

	req2 := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-meta-test/upload", body2)
	req2.Header.Set("Content-Type", writer2.FormDataContentType())
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)

	if w2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for metadata count mismatch (3 vs 2), got %d: %s", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), "metadata count (3) must match uploaded files count (2)") {
		t.Errorf("unexpected error message: %s", w2.Body.String())
	}
}

func TestSingleFileUpload_CleanupOnFailure(t *testing.T) {
	app, mux := setupUploadTestServer(t)

	// Upload invalid corrupt PDF disguised as a single file
	corruptBytes := []byte("Not a real PDF file header corrupted garbage")

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	p, _ := writer.CreateFormFile("file", "corrupt.pdf")
	_, _ = p.Write(corruptBytes)
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk-single-fail/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Fatalf("expected upload failure for corrupt file, got 200 OK")
	}

	// Verify no PDF files left in jobsDir
	entries, err := os.ReadDir(app.cfg.jobsDir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".pdf") {
			t.Errorf("found orphaned PDF file in jobsDir after failed single upload: %s", entry.Name())
		}
	}
}
