package main

import (
	"bytes"
	"context"
	"encoding/json"
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

func setupPreviewTestServer(t *testing.T) (*appServer, *http.ServeMux) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "preview_test.db")
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
	mux.HandleFunc("/api/k/", app.handleUpload)

	return app, mux
}

func submitPreviewRequest(t *testing.T, mux *http.ServeMux, fileBytes []byte, filename string, fields map[string]string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	if fileBytes != nil {
		part, err := writer.CreateFormFile("file", filename)
		if err != nil {
			t.Fatalf("CreateFormFile failed: %v", err)
		}
		if _, err := part.Write(fileBytes); err != nil {
			t.Fatalf("failed to write file bytes: %v", err)
		}
	}

	for k, v := range fields {
		if err := writer.WriteField(k, v); err != nil {
			t.Fatalf("WriteField(%q, %q) failed: %v", k, v, err)
		}
	}
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/documents/preview", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	for hk, hv := range headers {
		req.Header.Set(hk, hv)
	}

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

func verifyPDFStructureAndPages(t *testing.T, pdfBytes []byte, wantPages int) {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "preview_verify_*.pdf")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.Write(pdfBytes); err != nil {
		t.Fatalf("failed to write temp pdf: %v", err)
	}
	_ = tmpFile.Close()

	cmdCheck := exec.Command("qpdf", "--check", tmpFile.Name())
	outCheck, errCheck := cmdCheck.CombinedOutput()
	if errCheck != nil {
		t.Fatalf("qpdf --check failed on preview pdf: %s: %v", strings.TrimSpace(string(outCheck)), errCheck)
	}

	cmdCount := exec.Command("qpdf", "--show-npages", tmpFile.Name())
	outCount, errCount := cmdCount.CombinedOutput()
	if errCount != nil {
		t.Fatalf("qpdf --show-npages failed: %s: %v", strings.TrimSpace(string(outCount)), errCount)
	}
	gotPages, err := strconv.Atoi(strings.TrimSpace(string(outCount)))
	if err != nil {
		t.Fatalf("invalid page count %q: %v", strings.TrimSpace(string(outCount)), err)
	}
	if gotPages != wantPages {
		t.Fatalf("pdf page count = %d, want %d", gotPages, wantPages)
	}
}

// TestDocumentPreview_NUpModes tests 1-up, 2-up, 4-up, and 6-up imposition via POST /api/documents/preview.
func TestDocumentPreview_NUpModes(t *testing.T) {
	app, mux := setupPreviewTestServer(t)
	// Configure pricing in DB: BW Simplex 200, BW Duplex 150
	_ = app.db.SetDetailedPricing(context.Background(), 200, 150, 1000, 750, true)

	tmpDir := t.TempDir()
	doc6p := createTestPDFWithPages(t, tmpDir, 6)
	doc6Bytes, err := os.ReadFile(doc6p)
	if err != nil {
		t.Fatalf("failed to read 6-page test doc: %v", err)
	}

	tests := []struct {
		name              string
		nup               string
		duplex            string
		copies            string
		wantBilledSides   int
		wantTotalSheets   int
		wantNormalizedPgs int
		wantPaddedPgs     int
		wantPricePaise    int64
	}{
		{
			name:              "1-up Simplex 1 copy",
			nup:               "1",
			duplex:            "SIMPLEX",
			copies:            "1",
			wantBilledSides:   6,
			wantTotalSheets:   6,
			wantNormalizedPgs: 6,
			wantPaddedPgs:     6,
			wantPricePaise:    1200, // 6 * 200
		},
		{
			name:              "2-up Simplex 1 copy (6 / 2 = 3 sides)",
			nup:               "2",
			duplex:            "SIMPLEX",
			copies:            "1",
			wantBilledSides:   3,
			wantTotalSheets:   3,
			wantNormalizedPgs: 3,
			wantPaddedPgs:     3,
			wantPricePaise:    600, // 3 * 200
		},
		{
			name:              "2-up Duplex 1 copy (3 sides -> 2 sheets, 1 blank pad)",
			nup:               "2",
			duplex:            "DUPLEX_LONG_EDGE",
			copies:            "1",
			wantBilledSides:   3,   // Blank pad excluded from billing!
			wantTotalSheets:   2,   // ceil(3/2) = 2 physical sheets
			wantNormalizedPgs: 3,
			wantPaddedPgs:     4,   // 3 sides + 1 blank padding page
			wantPricePaise:    450, // 3 * 150 paise
		},
		{
			name:              "4-up Simplex 1 copy (ceil(6/4) = 2 sides)",
			nup:               "4",
			duplex:            "SIMPLEX",
			copies:            "1",
			wantBilledSides:   2,
			wantTotalSheets:   2,
			wantNormalizedPgs: 2,
			wantPaddedPgs:     2,
			wantPricePaise:    400, // 2 * 200
		},
		{
			name:              "4-up Duplex 1 copy (2 sides -> 1 sheet, no pad needed)",
			nup:               "4",
			duplex:            "DUPLEX_LONG_EDGE",
			copies:            "1",
			wantBilledSides:   2,
			wantTotalSheets:   1, // ceil(2/2) = 1 sheet
			wantNormalizedPgs: 2,
			wantPaddedPgs:     2,
			wantPricePaise:    300, // 2 * 150 paise
		},
		{
			name:              "6-up Simplex 1 copy (ceil(6/6) = 1 side)",
			nup:               "6",
			duplex:            "SIMPLEX",
			copies:            "1",
			wantBilledSides:   1,
			wantTotalSheets:   1,
			wantNormalizedPgs: 1,
			wantPaddedPgs:     1,
			wantPricePaise:    200, // 1 * 200
		},
		{
			name:              "6-up Duplex 1 copy (1 side -> 1 sheet, 1 blank pad)",
			nup:               "6",
			duplex:            "DUPLEX_LONG_EDGE",
			copies:            "1",
			wantBilledSides:   1,   // Blank pad excluded from billing
			wantTotalSheets:   1,   // ceil(1/2) = 1 sheet
			wantNormalizedPgs: 1,
			wantPaddedPgs:     2,   // 1 content side + 1 blank padding page
			wantPricePaise:    150, // 1 * 150 paise
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fields := map[string]string{
				"pages_per_sheet": tc.nup,
				"duplex":          tc.duplex,
				"copies":          tc.copies,
			}
			rr := submitPreviewRequest(t, mux, doc6Bytes, "test_doc6p.pdf", fields, nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("expected HTTP 200, got %d: %s", rr.Code, rr.Body.String())
			}

			// Validate headers
			if ct := rr.Header().Get("Content-Type"); ct != "application/pdf" {
				t.Errorf("Content-Type = %q, want application/pdf", ct)
			}
			if bs := rr.Header().Get("X-Billed-Sides"); bs != strconv.Itoa(tc.wantBilledSides) {
				t.Errorf("X-Billed-Sides = %q, want %d", bs, tc.wantBilledSides)
			}
			if ts := rr.Header().Get("X-Total-Sheets"); ts != strconv.Itoa(tc.wantTotalSheets) {
				t.Errorf("X-Total-Sheets = %q, want %d", ts, tc.wantTotalSheets)
			}
			if np := rr.Header().Get("X-Normalized-Pages"); np != strconv.Itoa(tc.wantNormalizedPgs) {
				t.Errorf("X-Normalized-Pages = %q, want %d", np, tc.wantNormalizedPgs)
			}
			if pp := rr.Header().Get("X-Padded-Pages"); pp != strconv.Itoa(tc.wantPaddedPgs) {
				t.Errorf("X-Padded-Pages = %q, want %d", pp, tc.wantPaddedPgs)
			}
			if tp := rr.Header().Get("X-Total-Price-Paise"); tp != strconv.FormatInt(tc.wantPricePaise, 10) {
				t.Errorf("X-Total-Price-Paise = %q, want %d", tp, tc.wantPricePaise)
			}
			if csum := rr.Header().Get("X-SHA256-Checksum"); len(csum) != 64 {
				t.Errorf("expected 64-char hex SHA256 checksum, got %q", csum)
			}

			// Validate actual returned PDF structure and page count
			verifyPDFStructureAndPages(t, rr.Body.Bytes(), tc.wantPaddedPgs)
		})
	}
}

// TestDocumentPreview_PricingAndDuplexBlankPadding verifies backend-authoritative pricing
// and that duplex blank padding is excluded from billed sides across multiple copies.
func TestDocumentPreview_PricingAndDuplexBlankPadding(t *testing.T) {
	app, mux := setupPreviewTestServer(t)
	// Explicit 4-tier pricing: BW Simplex 200, BW Duplex 150
	_ = app.db.SetDetailedPricing(context.Background(), 200, 150, 1000, 750, true)

	tmpDir := t.TempDir()
	doc3p := createTestPDFWithPages(t, tmpDir, 3)
	doc3Bytes, _ := os.ReadFile(doc3p)

	t.Run("3-page duplex 1 copy via PDF streaming", func(t *testing.T) {
		fields := map[string]string{
			"copies": "1",
			"duplex": "DUPLEX_LONG_EDGE",
		}
		rr := submitPreviewRequest(t, mux, doc3Bytes, "doc3.pdf", fields, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected HTTP 200, got %d: %s", rr.Code, rr.Body.String())
		}

		// 3 content pages in duplex:
		// Billed sides = 3
		// Total sheets = ceil(3/2) = 2
		// Padded pages = 4 (reverse side of 2nd sheet has blank page)
		// Total price = 3 * 150 = 450 paise (not 600 paise)
		if bs := rr.Header().Get("X-Billed-Sides"); bs != "3" {
			t.Errorf("X-Billed-Sides = %q, want 3", bs)
		}
		if ts := rr.Header().Get("X-Total-Sheets"); ts != "2" {
			t.Errorf("X-Total-Sheets = %q, want 2", ts)
		}
		if pp := rr.Header().Get("X-Padded-Pages"); pp != "4" {
			t.Errorf("X-Padded-Pages = %q, want 4", pp)
		}
		if tp := rr.Header().Get("X-Total-Price-Paise"); tp != "450" {
			t.Errorf("X-Total-Price-Paise = %q, want 450", tp)
		}

		verifyPDFStructureAndPages(t, rr.Body.Bytes(), 4)
	})

	t.Run("3-page duplex 2 copies via JSON format", func(t *testing.T) {
		fields := map[string]string{
			"copies": "2",
			"duplex": "DUPLEX_LONG_EDGE",
			"format": "json",
		}
		rr := submitPreviewRequest(t, mux, doc3Bytes, "doc3.pdf", fields, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected HTTP 200, got %d: %s", rr.Code, rr.Body.String())
		}

		if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}

		var resp map[string]interface{}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to parse JSON response: %v", err)
		}

		// 3 content pages * 2 copies = 6 billed sides
		// Total sheets = ceil(3/2) * 2 = 4 sheets
		// Price = 6 * 150 = 900 paise
		if bs := int(resp["billed_sides"].(float64)); bs != 6 {
			t.Errorf("billed_sides = %d, want 6", bs)
		}
		if ts := int(resp["total_sheets"].(float64)); ts != 4 {
			t.Errorf("total_sheets = %d, want 4", ts)
		}
		if tp := int64(resp["total_price_paise"].(float64)); tp != 900 {
			t.Errorf("total_price_paise = %d, want 900", tp)
		}
		if pp := int(resp["padded_pages"].(float64)); pp != 4 {
			t.Errorf("padded_pages = %d, want 4", pp)
		}
	})
}

// TestDocumentPreview_PageRangeAndOrientation tests page range selection combined with orientation and N-up.
func TestDocumentPreview_PageRangeAndOrientation(t *testing.T) {
	_, mux := setupPreviewTestServer(t)

	tmpDir := t.TempDir()
	doc5p := createTestPDFWithPages(t, tmpDir, 5)
	doc5Bytes, _ := os.ReadFile(doc5p)

	fields := map[string]string{
		"page_range":      "1,3,5",
		"pages_per_sheet": "2",
		"orientation":     "LANDSCAPE",
		"duplex":          "SIMPLEX",
		"copies":          "1",
	}
	rr := submitPreviewRequest(t, mux, doc5Bytes, "doc5.pdf", fields, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Range "1,3,5" -> 3 pages.
	// 2-up -> ceil(3/2) = 2 imposed sides.
	// Simplex -> 2 physical sheets.
	if bs := rr.Header().Get("X-Billed-Sides"); bs != "2" {
		t.Errorf("X-Billed-Sides = %q, want 2", bs)
	}
	if ts := rr.Header().Get("X-Total-Sheets"); ts != "2" {
		t.Errorf("X-Total-Sheets = %q, want 2", ts)
	}
	verifyPDFStructureAndPages(t, rr.Body.Bytes(), 2)
}

// TestDocumentPreview_NonPDFConversion tests converting a text document to PDF for preview.
func TestDocumentPreview_NonPDFConversion(t *testing.T) {
	_, mux := setupPreviewTestServer(t)

	txtContent := []byte("Hello XeroxGo WYSIWYG Preview!\nThis is a plain text file converted for print preview.")
	fields := map[string]string{
		"pages_per_sheet": "1",
		"duplex":          "SIMPLEX",
		"copies":          "1",
	}
	rr := submitPreviewRequest(t, mux, txtContent, "notes.txt", fields, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", rr.Code, rr.Body.String())
	}

	if ct := rr.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("Content-Type = %q, want application/pdf", ct)
	}
	if bs := rr.Header().Get("X-Billed-Sides"); bs != "1" {
		t.Errorf("X-Billed-Sides = %q, want 1", bs)
	}

	verifyPDFStructureAndPages(t, rr.Body.Bytes(), 1)
}

// TestDocumentPreview_ValidationErrors tests rejection of invalid inputs, security, and sizes.
func TestDocumentPreview_ValidationErrors(t *testing.T) {
	_, mux := setupPreviewTestServer(t)

	tmpDir := t.TempDir()
	doc2p := createTestPDFWithPages(t, tmpDir, 2)
	doc2Bytes, _ := os.ReadFile(doc2p)

	// Case 1: GET method
	reqGet := httptest.NewRequest(http.MethodGet, "/api/documents/preview", nil)
	rrGet := httptest.NewRecorder()
	mux.ServeHTTP(rrGet, reqGet)
	if rrGet.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET method status = %d, want 405", rrGet.Code)
	}

	// Case 2: Missing file field
	rrNoFile := submitPreviewRequest(t, mux, nil, "", map[string]string{"copies": "1"}, nil)
	if rrNoFile.Code != http.StatusBadRequest {
		t.Errorf("missing file status = %d, want 400", rrNoFile.Code)
	}

	// Case 3: Unsupported format (.exe)
	rrBadExt := submitPreviewRequest(t, mux, []byte("binary data"), "virus.exe", map[string]string{"copies": "1"}, nil)
	if rrBadExt.Code != http.StatusBadRequest {
		t.Errorf("unsupported format status = %d, want 400", rrBadExt.Code)
	}

	// Case 4: Invalid copies (0 or 1000)
	rrZeroCopies := submitPreviewRequest(t, mux, doc2Bytes, "doc.pdf", map[string]string{"copies": "0"}, nil)
	if rrZeroCopies.Code != http.StatusBadRequest {
		t.Errorf("zero copies status = %d, want 400", rrZeroCopies.Code)
	}
	rrHighCopies := submitPreviewRequest(t, mux, doc2Bytes, "doc.pdf", map[string]string{"copies": "1000"}, nil)
	if rrHighCopies.Code != http.StatusBadRequest {
		t.Errorf("1000 copies status = %d, want 400", rrHighCopies.Code)
	}

	// Case 5: Invalid duplex mode
	rrBadDuplex := submitPreviewRequest(t, mux, doc2Bytes, "doc.pdf", map[string]string{"duplex": "SIDEWAYS"}, nil)
	if rrBadDuplex.Code != http.StatusBadRequest {
		t.Errorf("invalid duplex status = %d, want 400", rrBadDuplex.Code)
	}

	// Case 6: Invalid orientation
	rrBadOrient := submitPreviewRequest(t, mux, doc2Bytes, "doc.pdf", map[string]string{"orientation": "UPSIDEDOWN"}, nil)
	if rrBadOrient.Code != http.StatusBadRequest {
		t.Errorf("invalid orientation status = %d, want 400", rrBadOrient.Code)
	}

	// Case 7: Invalid pages_per_sheet (3-up or 8-up not supported)
	rrBadNUp := submitPreviewRequest(t, mux, doc2Bytes, "doc.pdf", map[string]string{"pages_per_sheet": "3"}, nil)
	if rrBadNUp.Code != http.StatusBadRequest {
		t.Errorf("3-up status = %d, want 400", rrBadNUp.Code)
	}

	// Case 8: Invalid page range out of bounds
	rrBadRange := submitPreviewRequest(t, mux, doc2Bytes, "doc.pdf", map[string]string{"page_range": "10-20"}, nil)
	if rrBadRange.Code != http.StatusBadRequest {
		t.Errorf("out of bounds page range status = %d, want 400", rrBadRange.Code)
	}
}
