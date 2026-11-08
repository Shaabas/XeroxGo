package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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

	// Verify the merged PDF can be verified by qpdf
	// The merged PDF is at filepath.Join(app.cfg.jobsDir, hex(sha256(orderID))+".pdf")
	h := sha256ChecksumString(orderID)
	mergedPDFPath := filepath.Join(app.cfg.jobsDir, h+".pdf")

	cmdCheck := exec.Command("qpdf", "--check", mergedPDFPath)
	outCheck, errCheck := cmdCheck.CombinedOutput()
	if errCheck != nil {
		t.Fatalf("qpdf --check failed on merged PDF: %s: %v", strings.TrimSpace(string(outCheck)), errCheck)
	}

	// Verify page count of merged PDF with pdfinfo or qpdf
	cmdCount := exec.Command("pdfinfo", mergedPDFPath)
	outCount, errCount := cmdCount.CombinedOutput()
	if errCount == nil {
		lines := strings.Split(string(outCount), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "Pages:") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					pCount, _ := strconv.Atoi(parts[1])
					// Doc 1 has 3 pages + 1 blank padding page for duplex = 4 pages
					// Doc 2 has 2 pages
					// Doc 3 has 1 page
					// Total logical pages in PDF = 4 + 2 + 1 = 7 pages
					if pCount != 7 {
						t.Errorf("merged PDF pages = %d, want 7 (with duplex blank back padding)", pCount)
					}
				}
			}
		}
	}
}

func sha256ChecksumString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
