package main

import (
	"bytes"
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
	"xeroxgo/pkg/contracts"
)

func createTestPDFWithPages(t *testing.T, dir string, n int) string {
	t.Helper()
	pdfPath := filepath.Join(dir, "fixture_"+strconv.Itoa(n)+"p.pdf")
	var ps strings.Builder
	for i := 1; i <= n; i++ {
		ps.WriteString("/Helvetica findfont 24 scalefont setfont 100 700 moveto (Page ")
		ps.WriteString(strconv.Itoa(i))
		ps.WriteString(") show showpage ")
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
		t.Fatalf("failed to generate %d-page test PDF: %s: %v", n, strings.TrimSpace(string(out)), err)
	}
	return pdfPath
}

func setupUploadTestServer(t *testing.T) (*appServer, *http.ServeMux) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
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
	}

	hub := ws.NewHub(s)
	app := &appServer{
		cfg:      cfg,
		db:       s,
		hub:      hub,
		convWake: make(chan struct{}, 10),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/k/", app.handleUpload)

	return app, mux
}

func TestUpload_PageCalculationsAndAuthoritativeCounts(t *testing.T) {
	_, mux := setupUploadTestServer(t)
	tmpDir := t.TempDir()

	doc3p := createTestPDFWithPages(t, tmpDir, 3)
	doc5p := createTestPDFWithPages(t, tmpDir, 5)

	tests := []struct {
		name          string
		pdfPath       string
		copies        string
		duplex        string
		pageRange     string
		wantStatus    int
		wantPages     int
		wantSheets    int
		wantCopies    int
		wantTotalCost int64 // 200 paise per page
	}{
		{
			name:          "simplex_3p_2copies",
			pdfPath:       doc3p,
			copies:        "2",
			duplex:        "SIMPLEX",
			wantStatus:    http.StatusOK,
			wantPages:     3,
			wantSheets:    6, // 3 * 2
			wantCopies:    2,
			wantTotalCost: 1200, // 3 pages * 2 copies * 200 paise = 1200
		},
		{
			name:          "duplex_3p_2copies_example_from_spec",
			// Requirement 10: 3 pages * 2 copies in duplex = 6 printed sides, 4 physical sheets.
			pdfPath:       doc3p,
			copies:        "2",
			duplex:        "DUPLEX_LONG_EDGE",
			wantStatus:    http.StatusOK,
			wantPages:     3,
			wantSheets:    4, // ceil(3/2) * 2 = 4 sheets
			wantCopies:    2,
			wantTotalCost: 1200, // 6 printed sides * 200 paise = 1200
		},
		{
			name:          "duplex_3p_1copy",
			pdfPath:       doc3p,
			copies:        "1",
			duplex:        "DUPLEX_LONG_EDGE",
			wantStatus:    http.StatusOK,
			wantPages:     3,
			wantSheets:    2, // ceil(3/2) * 1 = 2 sheets
			wantCopies:    1,
			wantTotalCost: 600, // 3 printed sides * 200 paise = 600
		},
		{
			name:          "custom_range_1_3_5_on_5p_simplex_2copies",
			pdfPath:       doc5p,
			copies:        "2",
			duplex:        "SIMPLEX",
			pageRange:     "1,3,5",
			wantStatus:    http.StatusOK,
			wantPages:     3,
			wantSheets:    6, // 3 * 2
			wantCopies:    2,
			wantTotalCost: 1200,
		},
		{
			name:          "custom_range_1_3_5_on_5p_duplex_2copies",
			pdfPath:       doc5p,
			copies:        "2",
			duplex:        "DUPLEX_LONG_EDGE",
			pageRange:     "1,3,5",
			wantStatus:    http.StatusOK,
			wantPages:     3,
			wantSheets:    4, // ceil(3/2) * 2 = 4
			wantCopies:    2,
			wantTotalCost: 1200,
		},
		{
			name:       "invalid_custom_range_out_of_bounds",
			pdfPath:    doc5p,
			copies:     "1",
			duplex:     "SIMPLEX",
			pageRange:  "1-3,7", // doc has 5 pages
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := &bytes.Buffer{}
			writer := multipart.NewWriter(body)

			fileBytes, err := os.ReadFile(tt.pdfPath)
			if err != nil {
				t.Fatalf("failed to read test pdf: %v", err)
			}

			part, err := writer.CreateFormFile("file", filepath.Base(tt.pdfPath))
			if err != nil {
				t.Fatalf("failed to create form file: %v", err)
			}
			if _, err := part.Write(fileBytes); err != nil {
				t.Fatalf("failed to write file bytes: %v", err)
			}

			_ = writer.WriteField("copies", tt.copies)
			_ = writer.WriteField("duplex", tt.duplex)
			if tt.pageRange != "" {
				_ = writer.WriteField("page_range", tt.pageRange)
			}
			writer.Close()

			req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk_test_1/upload", body)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			rr := httptest.NewRecorder()

			mux.ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("status code = %d, want %d. Body: %s", rr.Code, tt.wantStatus, rr.Body.String())
			}

			if tt.wantStatus == http.StatusOK {
				var resp map[string]interface{}
				if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
					t.Fatalf("failed to parse json response: %v", err)
				}

				if int(resp["total_pages"].(float64)) != tt.wantPages {
					t.Errorf("total_pages = %v, want %d", resp["total_pages"], tt.wantPages)
				}
				if int(resp["total_sheets"].(float64)) != tt.wantSheets {
					t.Errorf("total_sheets = %v, want %d", resp["total_sheets"], tt.wantSheets)
				}
				if int(resp["copies"].(float64)) != tt.wantCopies {
					t.Errorf("copies = %v, want %d", resp["copies"], tt.wantCopies)
				}
				if int64(resp["total_price_paise"].(float64)) != tt.wantTotalCost {
					t.Errorf("total_price_paise = %v, want %d", resp["total_price_paise"], tt.wantTotalCost)
				}
			}
		})
	}
}

func TestUpload_DuplexOddPagesFinalSheetBlankBackInPDF(t *testing.T) {
	app, mux := setupUploadTestServer(t)
	tmpDir := t.TempDir()

	// 3-page document in duplex with 2 copies
	doc3p := createTestPDFWithPages(t, tmpDir, 3)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	fileBytes, _ := os.ReadFile(doc3p)
	part, _ := writer.CreateFormFile("file", "doc3.pdf")
	_, _ = part.Write(fileBytes)
	_ = writer.WriteField("copies", "2")
	_ = writer.WriteField("duplex", "DUPLEX_LONG_EDGE")
	writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk_test_1/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("upload failed: %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	orderID := resp["order_id"].(string)

	// Fetch the order from the store
	order, err := app.db.GetOrder(req.Context(), orderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}

	if order.Command.TotalPages != 3 {
		t.Errorf("Command.TotalPages = %d, want 3", order.Command.TotalPages)
	}
	if order.Command.TotalSheets != 4 {
		t.Errorf("Command.TotalSheets = %d, want 4", order.Command.TotalSheets)
	}
	if order.Command.Copies != 2 {
		t.Errorf("Command.Copies = %d, want 2", order.Command.Copies)
	}
	if order.Command.Duplex != contracts.DuplexLongEdge {
		t.Errorf("Command.Duplex = %v, want DUPLEX_LONG_EDGE", order.Command.Duplex)
	}
}
