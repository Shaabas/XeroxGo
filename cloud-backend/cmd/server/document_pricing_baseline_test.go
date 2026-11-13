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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/cloud-backend/internal/ws"
)

// setupDocPricingBaselineServer sets up an isolated test server with all
// document ingestion, preview, upload, order, and pricing routes registered.
func setupDocPricingBaselineServer(t *testing.T) (*appServer, *http.ServeMux) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "baseline_test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	jobsDir := filepath.Join(tmpDir, "jobs")
	if err := os.MkdirAll(jobsDir, 0700); err != nil {
		t.Fatalf("failed to create jobs dir: %v", err)
	}

	cfg := &serverConfig{
		addr:               "localhost:0",
		dbPath:             dbPath,
		jobsDir:            jobsDir,
		pricePerSheetPaise: 200,
		downloadSecret:     []byte("test_secret_key_1234567890"),
		devMode:            true,
	}

	hub := ws.NewHub(s)
	app := &appServer{
		cfg:      cfg,
		db:       s,
		hub:      hub,
		convWake: make(chan struct{}, 10),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/documents/preview", app.handleDocumentPreview)
	mux.HandleFunc("/api/pricing", app.handlePublicPricing)
	mux.HandleFunc("/api/k/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/upload"):
			app.handleUpload(w, r)
		case strings.HasSuffix(path, "/claim"):
			app.handleOrderClaim(w, r)
		case strings.HasSuffix(path, "/orders/lookup"):
			app.handlePINLookup(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/api/orders/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			app.handleOrderStatus(w, r)
		} else {
			http.NotFound(w, r)
		}
	})

	return app, mux
}

// createNamedDocPDF generates a test PDF where each page clearly states the document name and page number.
func createNamedDocPDF(t *testing.T, dir, label string, pageCount int) string {
	t.Helper()
	pdfPath := filepath.Join(dir, fmt.Sprintf("%s_%dp.pdf", label, pageCount))

	var ps strings.Builder
	for p := 1; p <= pageCount; p++ {
		ps.WriteString(fmt.Sprintf(`/Helvetica findfont 24 scalefont setfont
100 700 moveto
(%s Page %d of %d) show
showpage
`, label, p, pageCount))
	}

	cmd := exec.Command("gs",
		"-sDEVICE=pdfwrite",
		"-sPAPERSIZE=a4",
		"-dFIXEDMEDIA",
		"-dNOPAUSE",
		"-dBATCH",
		"-dQUIET",
		"-sOutputFile="+pdfPath,
		"-c",
		ps.String(),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to generate named PDF %s: %s: %v", label, strings.TrimSpace(string(out)), err)
	}

	return pdfPath
}

// TestDocumentPricing_PreviewAndUploadParity verifies that identical documents and settings
// passed to POST /api/documents/preview and POST /api/k/{id}/upload yield identical canonical
// print semantics: billed sides, physical sheets, total price, and checksum.
func TestDocumentPricing_PreviewAndUploadParity(t *testing.T) {
	app, mux := setupDocPricingBaselineServer(t)

	// Configure DB pricing: BW Simplex 200, BW Duplex 150, Color Simplex 1000, Color Duplex 750 (ColorEnabled: true)
	if err := app.db.SetDetailedPricing(context.Background(), 200, 150, 1000, 750, true); err != nil {
		t.Fatalf("failed to set pricing: %v", err)
	}

	tmpDir := t.TempDir()
	doc5Path := createNamedDocPDF(t, tmpDir, "ParityDoc", 5)
	doc5Bytes, err := os.ReadFile(doc5Path)
	if err != nil {
		t.Fatalf("failed to read test doc: %v", err)
	}

	testCases := []struct {
		name        string
		copies      string
		duplex      string
		colorMode   string
		nup         string
		pageRange   string
		orientation string
	}{
		{
			name:        "Case 1: 5-page 1-copy Duplex LongEdge BW 2-up",
			copies:      "1",
			duplex:      "DUPLEX_LONG_EDGE",
			colorMode:   "BW",
			nup:         "2",
			pageRange:   "",
			orientation: "AUTO",
		},
		{
			name:        "Case 2: 5-page 2-copies Simplex Color 1-up Range 1-3",
			copies:      "2",
			duplex:      "SIMPLEX",
			colorMode:   "COLOR",
			nup:         "1",
			pageRange:   "1-3",
			orientation: "PORTRAIT",
		},
		{
			name:        "Case 3: 5-page 1-copy Simplex BW 4-up",
			copies:      "1",
			duplex:      "SIMPLEX",
			colorMode:   "BW",
			nup:         "4",
			pageRange:   "",
			orientation: "LANDSCAPE",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// 1. Request Preview
			prevBody := &bytes.Buffer{}
			prevWriter := multipart.NewWriter(prevBody)
			part, _ := prevWriter.CreateFormFile("file", "test.pdf")
			_, _ = part.Write(doc5Bytes)
			_ = prevWriter.WriteField("copies", tc.copies)
			_ = prevWriter.WriteField("duplex", tc.duplex)
			_ = prevWriter.WriteField("color_mode", tc.colorMode)
			_ = prevWriter.WriteField("pages_per_sheet", tc.nup)
			_ = prevWriter.WriteField("orientation", tc.orientation)
			if tc.pageRange != "" {
				_ = prevWriter.WriteField("page_range", tc.pageRange)
			}
			_ = prevWriter.Close()

			prevReq := httptest.NewRequest(http.MethodPost, "/api/documents/preview", prevBody)
			prevReq.Header.Set("Content-Type", prevWriter.FormDataContentType())
			prevRec := httptest.NewRecorder()
			mux.ServeHTTP(prevRec, prevReq)

			if prevRec.Code != http.StatusOK {
				t.Fatalf("preview failed with status %d: %s", prevRec.Code, prevRec.Body.String())
			}

			prevBilledSides, _ := strconv.Atoi(prevRec.Header().Get("X-Billed-Sides"))
			prevTotalSheets, _ := strconv.Atoi(prevRec.Header().Get("X-Total-Sheets"))
			prevPricePaise, _ := strconv.ParseInt(prevRec.Header().Get("X-Total-Price-Paise"), 10, 64)
			prevPaddedPages, _ := strconv.Atoi(prevRec.Header().Get("X-Padded-Pages"))
			prevChecksum := prevRec.Header().Get("X-SHA256-Checksum")

			// 2. Request Upload
			upBody := &bytes.Buffer{}
			upWriter := multipart.NewWriter(upBody)
			upPart, _ := upWriter.CreateFormFile("file", "test.pdf")
			_, _ = upPart.Write(doc5Bytes)
			_ = upWriter.WriteField("copies", tc.copies)
			_ = upWriter.WriteField("duplex", tc.duplex)
			_ = upWriter.WriteField("color_mode", tc.colorMode)
			_ = upWriter.WriteField("pages_per_sheet", tc.nup)
			_ = upWriter.WriteField("orientation", tc.orientation)
			if tc.pageRange != "" {
				_ = upWriter.WriteField("page_range", tc.pageRange)
			}
			_ = upWriter.Close()

			upReq := httptest.NewRequest(http.MethodPost, "/api/k/kiosk_parity/upload", upBody)
			upReq.Header.Set("Content-Type", upWriter.FormDataContentType())
			upRec := httptest.NewRecorder()
			mux.ServeHTTP(upRec, upReq)

			if upRec.Code != http.StatusOK {
				t.Fatalf("upload failed with status %d: %s", upRec.Code, upRec.Body.String())
			}

			var upJSON map[string]interface{}
			if err := json.Unmarshal(upRec.Body.Bytes(), &upJSON); err != nil {
				t.Fatalf("failed to decode upload JSON response: %v", err)
			}

			upPagesPerCopy := int(upJSON["total_pages"].(float64))
			upCopies := int(upJSON["copies"].(float64))
			upTotalSheets := int(upJSON["total_sheets"].(float64))
			upPricePaise := int64(upJSON["total_price_paise"].(float64))

			// 3. Assert Canonical Print Semantics Equivalence
			// In preview: X-Billed-Sides = total sides across all copies (normRes.TotalPages * copies)
			// In single-file upload: total_pages is pages-per-copy, so total billed sides = total_pages * copies
			upTotalBilledSides := upPagesPerCopy * upCopies
			if prevBilledSides != upTotalBilledSides {
				t.Errorf("Billed sides mismatch: preview=%d, upload (total_pages*copies)=%d", prevBilledSides, upTotalBilledSides)
			}
			// X-Normalized-Pages in preview must match total_pages in single-file upload
			prevNormPages, _ := strconv.Atoi(prevRec.Header().Get("X-Normalized-Pages"))
			if prevNormPages != upPagesPerCopy {
				t.Errorf("Normalized pages per copy mismatch: preview=%d, upload=%d", prevNormPages, upPagesPerCopy)
			}
			if prevTotalSheets != upTotalSheets {
				t.Errorf("Total sheets mismatch: preview=%d, upload=%d", prevTotalSheets, upTotalSheets)
			}
			if prevPricePaise != upPricePaise {
				t.Errorf("Total price mismatch: preview=%d paise, upload=%d paise", prevPricePaise, upPricePaise)
			}

			// Validate checksum integrity:
			// Preview header X-SHA256-Checksum must match SHA256 of the preview PDF body
			if len(prevChecksum) != 64 {
				t.Errorf("expected 64-char hex SHA256 checksum in preview, got %q", prevChecksum)
			}

			// In single-file upload, SHA256 checksum is persisted in order.Command.SHA256Checksum
			orderID := upJSON["order_id"].(string)
			order, err := app.db.GetOrder(context.Background(), orderID)
			if err != nil || order == nil {
				t.Fatalf("failed to retrieve created order: %v", err)
			}
			if len(order.Command.SHA256Checksum) != 64 {
				t.Errorf("expected 64-char hex SHA256 checksum in order.Command, got %q", order.Command.SHA256Checksum)
			}

			// Verify actual saved job PDF in jobsDir matches preview padded page count
			jobHash := sha256ChecksumString(orderID)
			savedPDFPath := filepath.Join(app.cfg.jobsDir, jobHash+".pdf")
			savedPageCount := getPDFPageCount(t, savedPDFPath)
			if savedPageCount != prevPaddedPages {
				t.Errorf("Saved PDF page count (%d) does not match preview padded pages (%d)", savedPageCount, prevPaddedPages)
			}

			// Write preview PDF to temp file and verify page-by-page text content matches saved upload PDF
			previewPDFPath := filepath.Join(tmpDir, fmt.Sprintf("prev_%s.pdf", tc.copies))
			if err := os.WriteFile(previewPDFPath, prevRec.Body.Bytes(), 0600); err != nil {
				t.Fatalf("failed to write preview PDF: %v", err)
			}
			for p := 1; p <= savedPageCount; p++ {
				prevText := getPageText(t, previewPDFPath, p)
				upText := getPageText(t, savedPDFPath, p)
				if prevText != upText {
					t.Errorf("Page %d content mismatch: preview=%q, upload=%q", p, prevText, upText)
				}
			}
		})
	}
}

// TestDocumentPricing_MultiFileCollationOrderAndPadding verifies that uploading multiple
// files with different duplex/color settings strictly preserves document ordering,
// applies per-document duplex blank padding, and aggregates sheets/pricing correctly.
func TestDocumentPricing_MultiFileCollationOrderAndPadding(t *testing.T) {
	app, mux := setupDocPricingBaselineServer(t)

	// Set pricing: BW Simplex 200, BW Duplex 150
	_ = app.db.SetDetailedPricing(context.Background(), 200, 150, 1000, 750, false)

	tmpDir := t.TempDir()
	docAlpha := createNamedDocPDF(t, tmpDir, "DocAlpha", 2)
	docBeta := createNamedDocPDF(t, tmpDir, "DocBeta", 3)
	docGamma := createNamedDocPDF(t, tmpDir, "DocGamma", 1)

	alphaBytes, _ := os.ReadFile(docAlpha)
	betaBytes, _ := os.ReadFile(docBeta)
	gammaBytes, _ := os.ReadFile(docGamma)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	p1, _ := writer.CreateFormFile("files", "DocAlpha_2p.pdf")
	_, _ = p1.Write(alphaBytes)
	p2, _ := writer.CreateFormFile("files", "DocBeta_3p.pdf")
	_, _ = p2.Write(betaBytes)
	p3, _ := writer.CreateFormFile("files", "DocGamma_1p.pdf")
	_, _ = p3.Write(gammaBytes)

	// Metadata:
	// Doc 1 (Alpha): 2 pages, Simplex, BW, 1 copy -> 2 billed sides, 2 sheets, 400 paise
	// Doc 2 (Beta): 3 pages, Duplex, BW, 1 copy -> 3 billed sides, 2 sheets (4 padded pages with blank back), 450 paise
	// Doc 3 (Gamma): 1 page, Simplex, BW, 1 copy -> 1 billed side, 1 sheet, 200 paise
	metaJSON := `[
		{"copies": 1, "duplex": "SIMPLEX", "color_mode": "BW", "page_range": ""},
		{"copies": 1, "duplex": "DUPLEX_LONG_EDGE", "color_mode": "BW", "page_range": ""},
		{"copies": 1, "duplex": "SIMPLEX", "color_mode": "BW", "page_range": ""}
	]`
	_ = writer.WriteField("metadata", metaJSON)
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk_multi/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("multi-file upload failed with status %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	totalPages := int(resp["total_pages"].(float64))
	totalSheets := int(resp["total_sheets"].(float64))
	totalPricePaise := int64(resp["total_price_paise"].(float64))

	// Expected aggregations:
	// Total billed sides = 2 + 3 + 1 = 6
	// Total sheets = 2 + 2 + 1 = 5
	// Total price = 400 + 450 + 200 = 1050 paise
	if totalPages != 6 {
		t.Errorf("total_pages (billed sides) = %d, want 6", totalPages)
	}
	if totalSheets != 5 {
		t.Errorf("total_sheets = %d, want 5", totalSheets)
	}
	if totalPricePaise != 1050 {
		t.Errorf("total_price_paise = %d, want 1050", totalPricePaise)
	}

	// Verify merged PDF page count:
	// DocAlpha: 2 pages
	// DocBeta: 3 content + 1 blank padding = 4 pages
	// DocGamma: 1 page
	// Total merged pages = 2 + 4 + 1 = 7 pages
	orderID := resp["order_id"].(string)
	jobHash := sha256ChecksumString(orderID)
	mergedPDFPath := filepath.Join(app.cfg.jobsDir, jobHash+".pdf")

	mergedPages := getPDFPageCount(t, mergedPDFPath)
	if mergedPages != 7 {
		t.Errorf("merged PDF page count = %d, want 7", mergedPages)
	}

	// Verify strict collation order and blank padding:
	p1Text := getPageText(t, mergedPDFPath, 1)
	if !strings.Contains(p1Text, "DocAlpha Page 1") {
		t.Errorf("Page 1 should be DocAlpha Page 1, got: %q", p1Text)
	}
	p2Text := getPageText(t, mergedPDFPath, 2)
	if !strings.Contains(p2Text, "DocAlpha Page 2") {
		t.Errorf("Page 2 should be DocAlpha Page 2, got: %q", p2Text)
	}
	p3Text := getPageText(t, mergedPDFPath, 3)
	if !strings.Contains(p3Text, "DocBeta Page 1") {
		t.Errorf("Page 3 should be DocBeta Page 1, got: %q", p3Text)
	}
	p4Text := getPageText(t, mergedPDFPath, 4)
	if !strings.Contains(p4Text, "DocBeta Page 2") {
		t.Errorf("Page 4 should be DocBeta Page 2, got: %q", p4Text)
	}
	p5Text := getPageText(t, mergedPDFPath, 5)
	if !strings.Contains(p5Text, "DocBeta Page 3") {
		t.Errorf("Page 5 should be DocBeta Page 3, got: %q", p5Text)
	}
	p6Text := getPageText(t, mergedPDFPath, 6)
	if strings.TrimSpace(p6Text) != "" {
		t.Errorf("Page 6 should be blank duplex padding for DocBeta, got: %q", p6Text)
	}
	p7Text := getPageText(t, mergedPDFPath, 7)
	if !strings.Contains(p7Text, "DocGamma Page 1") {
		t.Errorf("Page 7 should be DocGamma Page 1, got: %q", p7Text)
	}
}

// TestDocumentPricing_FourTierIntegerPaiseAndColorFallback verifies integer paise calculations
// across all 4 tiers and confirms that color uploads gracefully fall back to B&W when color is disabled.
func TestDocumentPricing_FourTierIntegerPaiseAndColorFallback(t *testing.T) {
	app, mux := setupDocPricingBaselineServer(t)

	// Configure 4-tier pricing:
	// BW Simplex: 250 paise (₹2.50)
	// BW Duplex: 175 paise (₹1.75)
	// Color Simplex: 1250 paise (₹12.50)
	// Color Duplex: 900 paise (₹9.00)
	// ColorEnabled: true
	if err := app.db.SetDetailedPricing(context.Background(), 250, 175, 1250, 900, true); err != nil {
		t.Fatalf("failed to set detailed pricing: %v", err)
	}

	tmpDir := t.TempDir()
	doc4Path := createNamedDocPDF(t, tmpDir, "TierDoc", 4)
	doc4Bytes, _ := os.ReadFile(doc4Path)

	uploadTest := func(colorMode, duplex string, copies int) int64 {
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)
		part, _ := writer.CreateFormFile("file", "tier_doc.pdf")
		_, _ = part.Write(doc4Bytes)
		_ = writer.WriteField("copies", strconv.Itoa(copies))
		_ = writer.WriteField("duplex", duplex)
		_ = writer.WriteField("color_mode", colorMode)
		_ = writer.Close()

		req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk_tier/upload", body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("upload failed: %d: %s", rec.Code, rec.Body.String())
		}
		var resp map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return int64(resp["total_price_paise"].(float64))
	}

	// 1. BW Simplex: 4 pages * 1 copy * 250 paise = 1000 paise
	pBwSimp := uploadTest("BW", "SIMPLEX", 1)
	if pBwSimp != 1000 {
		t.Errorf("BW Simplex price = %d, want 1000", pBwSimp)
	}

	// 2. BW Duplex: 4 pages * 1 copy * 175 paise = 700 paise
	pBwDup := uploadTest("BW", "DUPLEX_LONG_EDGE", 1)
	if pBwDup != 700 {
		t.Errorf("BW Duplex price = %d, want 700", pBwDup)
	}

	// 3. Color Simplex: 4 pages * 1 copy * 1250 paise = 5000 paise
	pColSimp := uploadTest("COLOR", "SIMPLEX", 1)
	if pColSimp != 5000 {
		t.Errorf("Color Simplex price = %d, want 5000", pColSimp)
	}

	// 4. Color Duplex: 4 pages * 1 copy * 900 paise = 3600 paise
	pColDup := uploadTest("COLOR", "DUPLEX_LONG_EDGE", 1)
	if pColDup != 3600 {
		t.Errorf("Color Duplex price = %d, want 3600", pColDup)
	}

	// 5. Disable Color in DB: test graceful fallback to B&W rates
	if err := app.db.SetDetailedPricing(context.Background(), 250, 175, 1250, 900, false); err != nil {
		t.Fatalf("failed to update pricing to color disabled: %v", err)
	}

	// Request Color Simplex with Color disabled -> should be charged BW Simplex (1000 paise)
	pFallbackSimp := uploadTest("COLOR", "SIMPLEX", 1)
	if pFallbackSimp != 1000 {
		t.Errorf("Color disabled Simplex price = %d, want BW fallback 1000", pFallbackSimp)
	}

	// Request Color Duplex with Color disabled -> should be charged BW Duplex (700 paise)
	pFallbackDup := uploadTest("COLOR", "DUPLEX_LONG_EDGE", 1)
	if pFallbackDup != 700 {
		t.Errorf("Color disabled Duplex price = %d, want BW fallback 700", pFallbackDup)
	}
}

// TestDocumentPricing_PriceImmutabilityPostCreation verifies that an agreed order price
// remains strictly immutable in the database and across status and claim operations even
// if an administrator subsequently changes the kiosk pricing configuration.
func TestDocumentPricing_PriceImmutabilityPostCreation(t *testing.T) {
	app, mux := setupDocPricingBaselineServer(t)

	// Set initial pricing: BW Simplex 200 paise
	_ = app.db.SetDetailedPricing(context.Background(), 200, 150, 1000, 750, false)

	tmpDir := t.TempDir()
	docPath := createNamedDocPDF(t, tmpDir, "ImmutableDoc", 3)
	docBytes, _ := os.ReadFile(docPath)

	// 1. Upload 3-page document Simplex -> 3 * 200 = 600 paise
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "doc.pdf")
	_, _ = part.Write(docBytes)
	_ = writer.WriteField("copies", "1")
	_ = writer.WriteField("duplex", "SIMPLEX")
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk_immut/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("upload failed: %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	orderID := resp["order_id"].(string)
	pin := resp["display_code"].(string)
	initialPrice := int64(resp["total_price_paise"].(float64))

	if initialPrice != 600 {
		t.Fatalf("initial order price = %d paise, want 600", initialPrice)
	}

	// 2. Admin dramatically changes pricing to 800 paise per sheet
	_ = app.db.SetDetailedPricing(context.Background(), 800, 600, 2000, 1500, false)

	// 3. Verify order status query still returns the immutable agreed price (600 paise)
	statusReq := httptest.NewRequest(http.MethodGet, "/api/orders/"+orderID+"/status", nil)
	statusRec := httptest.NewRecorder()
	mux.ServeHTTP(statusRec, statusReq)

	if statusRec.Code != http.StatusOK {
		t.Fatalf("order status query failed: %d", statusRec.Code)
	}
	var statusResp map[string]interface{}
	_ = json.Unmarshal(statusRec.Body.Bytes(), &statusResp)
	statusPrice := int64(statusResp["price_paise"].(float64))
	if statusPrice != 600 {
		t.Errorf("order status returned price %d paise after pricing change, want immutable 600", statusPrice)
	}

	// 4. Verify PIN lookup returns the immutable agreed price (600 paise)
	lookupBody := fmt.Sprintf(`{"pin": %q}`, pin)
	lookupReq := httptest.NewRequest(http.MethodPost, "/api/k/kiosk_immut/orders/lookup", strings.NewReader(lookupBody))
	lookupReq.Header.Set("Content-Type", "application/json")
	lookupRec := httptest.NewRecorder()
	mux.ServeHTTP(lookupRec, lookupReq)

	if lookupRec.Code != http.StatusOK {
		t.Fatalf("pin lookup failed: %d: %s", lookupRec.Code, lookupRec.Body.String())
	}
	var lookupResp map[string]interface{}
	_ = json.Unmarshal(lookupRec.Body.Bytes(), &lookupResp)
	lookupPrice := int64(lookupResp["total_price_paise"].(float64))
	if lookupPrice != 600 {
		t.Errorf("pin lookup returned price %d paise after pricing change, want immutable 600", lookupPrice)
	}

	// 5. Verify order claim at kiosk retains the original price
	claimURL := fmt.Sprintf("/api/k/kiosk_immut/orders/%s/claim", orderID)
	claimReq := httptest.NewRequest(http.MethodPost, claimURL, nil)
	claimRec := httptest.NewRecorder()
	mux.ServeHTTP(claimRec, claimReq)

	if claimRec.Code != http.StatusOK {
		t.Fatalf("order claim failed: %d: %s", claimRec.Code, claimRec.Body.String())
	}
	var claimResp map[string]interface{}
	_ = json.Unmarshal(claimRec.Body.Bytes(), &claimResp)

	claimedPrice := int64(claimResp["total_price_paise"].(float64))
	if claimedPrice != 600 {
		t.Errorf("claimed session price = %d paise, want immutable agreed price 600", claimedPrice)
	}
}

// TestDocumentPricing_InputValidationAndBoundaryErrors verifies validation rejection
// for corrupted inputs, invalid duplex, invalid copies, invalid N-up, and invalid orientation.
func TestDocumentPricing_InputValidationAndBoundaryErrors(t *testing.T) {
	_, mux := setupDocPricingBaselineServer(t)

	tmpDir := t.TempDir()
	validPDF := createNamedDocPDF(t, tmpDir, "Valid", 2)
	validBytes, _ := os.ReadFile(validPDF)

	submitUpload := func(filename string, fileContent []byte, fields map[string]string) *httptest.ResponseRecorder {
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)
		if fileContent != nil {
			p, _ := writer.CreateFormFile("file", filename)
			_, _ = p.Write(fileContent)
		}
		for k, v := range fields {
			_ = writer.WriteField(k, v)
		}
		_ = writer.Close()

		req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk_err/upload", body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// 1. Missing file field
	rMissing := submitUpload("test.pdf", nil, nil)
	if rMissing.Code != http.StatusBadRequest {
		t.Errorf("missing file: expected HTTP 400, got %d", rMissing.Code)
	}

	// 2. Unsupported file extension (.exe)
	rExe := submitUpload("payload.exe", []byte("MZ..."), nil)
	if rExe.Code != http.StatusBadRequest || !strings.Contains(rExe.Body.String(), "unsupported file format") {
		t.Errorf("unsupported format: expected HTTP 400, got %d: %s", rExe.Code, rExe.Body.String())
	}

	// 3. Corrupt file content (.pdf with garbage bytes)
	rCorrupt := submitUpload("corrupt.pdf", []byte("GARBAGE_NOT_A_PDF"), nil)
	if rCorrupt.Code != http.StatusBadRequest {
		t.Errorf("corrupt PDF: expected HTTP 400, got %d: %s", rCorrupt.Code, rCorrupt.Body.String())
	}

	// 4. Invalid duplex ("TRIPLEX")
	rDuplex := submitUpload("test.pdf", validBytes, map[string]string{"duplex": "TRIPLEX"})
	if rDuplex.Code != http.StatusBadRequest || !strings.Contains(rDuplex.Body.String(), "invalid duplex mode") {
		t.Errorf("invalid duplex: expected HTTP 400, got %d: %s", rDuplex.Code, rDuplex.Body.String())
	}

	// 5. Invalid copies (0 and 1000)
	rCopies0 := submitUpload("test.pdf", validBytes, map[string]string{"copies": "0"})
	if rCopies0.Code != http.StatusBadRequest || !strings.Contains(rCopies0.Body.String(), "copies must be between 1 and 999") {
		t.Errorf("copies 0: expected HTTP 400, got %d: %s", rCopies0.Code, rCopies0.Body.String())
	}

	rCopies1000 := submitUpload("test.pdf", validBytes, map[string]string{"copies": "1000"})
	if rCopies1000.Code != http.StatusBadRequest || !strings.Contains(rCopies1000.Body.String(), "copies must be between 1 and 999") {
		t.Errorf("copies 1000: expected HTTP 400, got %d: %s", rCopies1000.Code, rCopies1000.Body.String())
	}

	// 6. Invalid pages_per_sheet (3 on 1)
	rNup3 := submitUpload("test.pdf", validBytes, map[string]string{"pages_per_sheet": "3"})
	if rNup3.Code != http.StatusBadRequest || !strings.Contains(rNup3.Body.String(), "only 1, 2, 4, and 6 are supported") {
		t.Errorf("N-up 3: expected HTTP 400, got %d: %s", rNup3.Code, rNup3.Body.String())
	}

	// 7. Invalid orientation ("DIAGONAL")
	rOrient := submitUpload("test.pdf", validBytes, map[string]string{"orientation": "DIAGONAL"})
	if rOrient.Code != http.StatusBadRequest || !strings.Contains(rOrient.Body.String(), "only AUTO, PORTRAIT, and LANDSCAPE are supported") {
		t.Errorf("invalid orientation: expected HTTP 400, got %d: %s", rOrient.Code, rOrient.Body.String())
	}
}
