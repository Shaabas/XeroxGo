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
	"path/filepath"
	"strconv"
	"testing"

	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/cloud-backend/internal/ws"
)

func setupAcceptanceTestServer(t *testing.T) (*appServer, *http.ServeMux, string) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "acceptance_test.db")
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
		addr:           "localhost:0",
		dbPath:         dbPath,
		jobsDir:        jobsDir,
		downloadSecret: []byte("test_secret_key_1234567890"),
		devMode:        true,
	}

	hub := ws.NewHub(s)
	app := &appServer{
		cfg:      cfg,
		db:       s,
		hub:      hub,
		convWake: make(chan struct{}, 10),
	}

	// Canonical rates: B&W simplex 300, duplex 500; Color simplex 800, duplex 1000
	_ = s.SetDetailedPricing(context.Background(), 300, 500, 800, 1000, true)

	if err := s.BootstrapAdmin(context.Background(), "admin", "adminpassword123"); err != nil {
		t.Fatalf("BootstrapAdmin failed: %v", err)
	}

	token, _, err := s.CreateAdminSession(context.Background(), "admin")
	if err != nil {
		t.Fatalf("CreateAdminSession failed: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/documents/preview", app.handleDocumentPreview)
	mux.HandleFunc("/api/pricing", app.handlePublicPricing)
	mux.HandleFunc("/api/admin/pricing", app.handleAdminPricing)
	mux.HandleFunc("/api/k/", app.handleUpload)

	return app, mux, token
}

func TestCanonicalPricing_PreviewUploadParity(t *testing.T) {
	_, mux, _ := setupAcceptanceTestServer(t)
	tmpDir := t.TempDir()

	doc3p := createTestPDFWithPages(t, tmpDir, 3)
	doc4p := createTestPDFWithPages(t, tmpDir, 4)

	doc3Bytes, _ := os.ReadFile(doc3p)
	doc4Bytes, _ := os.ReadFile(doc4p)

	cases := []struct {
		name      string
		fileBytes []byte
		filename  string
		duplex    string
		colorMode string
		copies    int
		nup       int
		pageRange string
	}{
		{
			name:      "3p BW Simplex 1 copy",
			fileBytes: doc3Bytes,
			filename:  "doc3.pdf",
			duplex:    "SIMPLEX",
			colorMode: "BW",
			copies:    1,
			nup:       1,
		},
		{
			name:      "3p BW Duplex 1 copy (odd page)",
			fileBytes: doc3Bytes,
			filename:  "doc3.pdf",
			duplex:    "DUPLEX_LONG_EDGE",
			colorMode: "BW",
			copies:    1,
			nup:       1,
		},
		{
			name:      "3p BW Duplex 2 copies",
			fileBytes: doc3Bytes,
			filename:  "doc3.pdf",
			duplex:    "DUPLEX_LONG_EDGE",
			colorMode: "BW",
			copies:    2,
			nup:       1,
		},
		{
			name:      "4p Color Duplex 1 copy",
			fileBytes: doc4Bytes,
			filename:  "doc4.pdf",
			duplex:    "DUPLEX_LONG_EDGE",
			colorMode: "COLOR",
			copies:    1,
			nup:       1,
		},
		{
			name:      "4p BW 2-up Duplex 1 copy",
			fileBytes: doc4Bytes,
			filename:  "doc4.pdf",
			duplex:    "DUPLEX_LONG_EDGE",
			colorMode: "BW",
			copies:    1,
			nup:       2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 1. Call Preview
			prevFields := map[string]string{
				"duplex":          tc.duplex,
				"color_mode":      tc.colorMode,
				"copies":          strconv.Itoa(tc.copies),
				"pages_per_sheet": strconv.Itoa(tc.nup),
				"format":          "json",
			}
			if tc.pageRange != "" {
				prevFields["page_range"] = tc.pageRange
			}
			prevRec := submitPreviewRequest(t, mux, tc.fileBytes, tc.filename, prevFields, nil)
			if prevRec.Code != http.StatusOK {
				t.Fatalf("preview failed with status %d: %s", prevRec.Code, prevRec.Body.String())
			}

			var prevResp map[string]interface{}
			if err := json.Unmarshal(prevRec.Body.Bytes(), &prevResp); err != nil {
				t.Fatalf("failed to decode preview response: %v", err)
			}
			prevPrice := int64(prevResp["total_price_paise"].(float64))
			prevSheets := int(prevResp["total_sheets"].(float64))
			prevNormalizedPages := int(prevResp["normalized_pages"].(float64))
			prevBilledSides := int(prevResp["billed_sides"].(float64))

			// Verify preview internal consistency: billed_sides = normalized_pages * copies
			if prevBilledSides != prevNormalizedPages*tc.copies {
				t.Errorf("Preview billed_sides (%d) != normalized_pages (%d) * copies (%d)",
					prevBilledSides, prevNormalizedPages, tc.copies)
			}

			// 2. Call Upload
			upBody := &bytes.Buffer{}
			upWriter := multipart.NewWriter(upBody)
			part, _ := upWriter.CreateFormFile("files", tc.filename)
			_, _ = part.Write(tc.fileBytes)
			metaJSON := fmt.Sprintf(`[{"copies": %d, "duplex": %q, "color_mode": %q, "page_range": %q, "pages_per_sheet": %d}]`,
				tc.copies, tc.duplex, tc.colorMode, tc.pageRange, tc.nup)
			_ = upWriter.WriteField("metadata", metaJSON)
			_ = upWriter.Close()

			upReq := httptest.NewRequest(http.MethodPost, "/api/k/kiosk_test/upload", upBody)
			upReq.Header.Set("Content-Type", upWriter.FormDataContentType())
			upRec := httptest.NewRecorder()
			mux.ServeHTTP(upRec, upReq)

			if upRec.Code != http.StatusOK {
				t.Fatalf("upload failed with status %d: %s", upRec.Code, upRec.Body.String())
			}

			var upResp map[string]interface{}
			if err := json.Unmarshal(upRec.Body.Bytes(), &upResp); err != nil {
				t.Fatalf("failed to decode upload response: %v", err)
			}
			upPrice := int64(upResp["total_price_paise"].(float64))
			upSheets := int(upResp["total_sheets"].(float64))
			upSides := int(upResp["total_pages"].(float64))

			// 3. Assert Exact Parity
			if prevPrice != upPrice {
				t.Errorf("Price mismatch: preview=%d paise, upload=%d paise", prevPrice, upPrice)
			}
			if prevSheets != upSheets {
				t.Errorf("Sheets mismatch: preview=%d, upload=%d", prevSheets, upSheets)
			}
			if prevNormalizedPages != upSides {
				t.Errorf("Normalized pages mismatch: preview=%d, upload=%d", prevNormalizedPages, upSides)
			}
		})
	}
}

func TestCanonicalPricing_AdminPersistenceAndAuthorization(t *testing.T) {
	_, mux, adminToken := setupAcceptanceTestServer(t)

	// 1. GET without auth -> 401
	unauthGet := httptest.NewRequest(http.MethodGet, "/api/admin/pricing", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, unauthGet)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauth GET, got %d", rec.Code)
	}

	// 2. PUT without auth -> 401
	unauthPut := httptest.NewRequest(http.MethodPut, "/api/admin/pricing", bytes.NewReader([]byte(`{}`)))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, unauthPut)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauth PUT, got %d", rec.Code)
	}

	// 3. PUT with invalid (zero/negative) rates -> 400
	invalidPayload := []byte(`{
		"bw_simplex_price_paise": 0,
		"bw_duplex_price_paise": 500,
		"color_simplex_price_paise": 800,
		"color_duplex_price_paise": 1000,
		"color_enabled": true
	}`)
	badReq := httptest.NewRequest(http.MethodPut, "/api/admin/pricing", bytes.NewReader(invalidPayload))
	badReq.Header.Set("Authorization", "Bearer "+adminToken)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, badReq)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for zero rate, got %d", rec.Code)
	}

	// 4. PUT valid rates -> 200
	validPayload := []byte(`{
		"bw_simplex_price_paise": 400,
		"bw_duplex_price_paise": 650,
		"color_simplex_price_paise": 900,
		"color_duplex_price_paise": 1200,
		"color_enabled": true
	}`)
	goodReq := httptest.NewRequest(http.MethodPut, "/api/admin/pricing", bytes.NewReader(validPayload))
	goodReq.Header.Set("Authorization", "Bearer "+adminToken)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, goodReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid PUT, got %d: %s", rec.Code, rec.Body.String())
	}

	// 5. GET with auth -> verifies persistence
	authGet := httptest.NewRequest(http.MethodGet, "/api/admin/pricing", nil)
	authGet.Header.Set("Authorization", "Bearer "+adminToken)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, authGet)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for auth GET, got %d", rec.Code)
	}

	var p store.PricingConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("failed to decode admin pricing response: %v", err)
	}
	if p.BWSimplexPricePaise != 400 || p.BWDuplexPricePaise != 650 ||
		p.ColorSimplexPricePaise != 900 || p.ColorDuplexPricePaise != 1200 || !p.ColorEnabled {
		t.Fatalf("unexpected admin pricing values: %+v", p)
	}

	// 6. Public GET /api/pricing returns the updated rates
	pubGet := httptest.NewRequest(http.MethodGet, "/api/pricing", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, pubGet)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for public GET, got %d", rec.Code)
	}
	var pubP store.PricingConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &pubP); err != nil {
		t.Fatalf("failed to decode public pricing response: %v", err)
	}
	if pubP.BWSimplexPricePaise != 400 || pubP.BWDuplexPricePaise != 650 ||
		pubP.ColorSimplexPricePaise != 900 || pubP.ColorDuplexPricePaise != 1200 {
		t.Fatalf("unexpected public pricing values: %+v", pubP)
	}
}

func TestCanonicalPricing_MissingTariffFailClosed(t *testing.T) {
	app, mux, _ := setupAcceptanceTestServer(t)
	tmpDir := t.TempDir()
	doc1p := createTestPDFWithPages(t, tmpDir, 1)
	doc1Bytes, _ := os.ReadFile(doc1p)

	// Wipe pricing from database to simulate unconfigured tariff
	_, err := app.db.DB().Exec("DELETE FROM pricing WHERE id = 1;")
	if err != nil {
		t.Fatalf("failed to delete pricing: %v", err)
	}

	// 1. Public pricing endpoint must fail closed with 500
	pubReq := httptest.NewRequest(http.MethodGet, "/api/pricing", nil)
	pubRec := httptest.NewRecorder()
	mux.ServeHTTP(pubRec, pubReq)
	if pubRec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for unconfigured public pricing, got %d", pubRec.Code)
	}

	// 2. Preview must fail closed with 500 (never guessing defaults)
	prevRec := submitPreviewRequest(t, mux, doc1Bytes, "doc1.pdf", map[string]string{
		"duplex": "SIMPLEX",
		"format": "json",
	}, nil)
	if prevRec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for preview when pricing missing, got %d: %s", prevRec.Code, prevRec.Body.String())
	}

	// 3. Upload must fail closed with 500 (never issuing valid quotes with guessed defaults)
	upBody := &bytes.Buffer{}
	upWriter := multipart.NewWriter(upBody)
	part, _ := upWriter.CreateFormFile("files", "doc1.pdf")
	_, _ = part.Write(doc1Bytes)
	_ = upWriter.WriteField("metadata", `[{"copies": 1, "duplex": "SIMPLEX", "color_mode": "BW"}]`)
	_ = upWriter.Close()

	upReq := httptest.NewRequest(http.MethodPost, "/api/k/kiosk_test/upload", upBody)
	upReq.Header.Set("Content-Type", upWriter.FormDataContentType())
	upRec := httptest.NewRecorder()
	mux.ServeHTTP(upRec, upReq)
	if upRec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for upload when pricing missing, got %d: %s", upRec.Code, upRec.Body.String())
	}
}

func TestInitPricing_PostgresNeverSeedsEvenInDevMode(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "postgres_sim.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	// Even with devMode = true and isNewSQLite = true, PostgreSQL connection must NEVER seed default tariffs
	err = initPricing(ctx, s, true /* isPostgres */, true /* devMode */, true /* isNewSQLite */)
	if err == nil {
		t.Fatalf("expected error from initPricing on Postgres, got nil")
	}

	var count int
	if err := s.DB().QueryRow("SELECT COUNT(1) FROM pricing;").Scan(&count); err != nil {
		t.Fatalf("failed to query pricing count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 pricing rows in Postgres database, got %d", count)
	}
}

func TestInitPricing_FailsClosedOnInvalidExistingRows(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "invalid_rows.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	// Insert an invalid row (rates <= 0) by disabling check constraints temporarily
	_, err = s.DB().Exec(`
PRAGMA ignore_check_constraints = ON;
INSERT INTO pricing (id, price_per_sheet_paise, bw_price_per_sheet_paise, color_price_per_sheet_paise,
                     bw_simplex_price_paise, bw_duplex_price_paise,
                     color_simplex_price_paise, color_duplex_price_paise,
                     color_enabled, updated_at_ns)
VALUES (1, 1, 1, 1, 0, 0, 0, 0, 0, 1000);
PRAGMA ignore_check_constraints = OFF;
`)
	if err != nil {
		t.Fatalf("failed to insert invalid pricing row: %v", err)
	}

	// initPricing must fail closed on invalid existing rows and never overwrite with defaults
	err = initPricing(ctx, s, false /* isPostgres */, true /* devMode */, false /* isNewSQLite */)
	if err == nil {
		t.Fatalf("expected initPricing to fail closed on invalid existing rows, got nil")
	}

	var bwSimplex int64
	if err := s.DB().QueryRow("SELECT bw_simplex_price_paise FROM pricing WHERE id = 1;").Scan(&bwSimplex); err != nil {
		t.Fatalf("failed to query bw_simplex: %v", err)
	}
	if bwSimplex != 0 {
		t.Fatalf("invalid row was overwritten with %d, must remain untouched", bwSimplex)
	}
}

func TestInitPricing_FailsClosedOnDatabaseReadError(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "db_error.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}

	// Canceled context simulating connectivity / timeout error
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	err = initPricing(canceledCtx, s, false /* isPostgres */, true /* devMode */, true /* isNewSQLite */)
	if err == nil {
		t.Fatalf("expected initPricing to fail closed on canceled context, got nil")
	}

	_ = s.Close()
	// Calling with closed database must also fail closed
	err = initPricing(context.Background(), s, false, true, true)
	if err == nil {
		t.Fatalf("expected initPricing to fail closed on closed database, got nil")
	}
}

func TestInitPricing_PreservesExistingTariffsAndColorEnabled(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "preserve_rates.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	// Set custom existing tariffs and color_enabled = true
	if err := s.SetDetailedPricing(ctx, 450, 650, 950, 1250, true); err != nil {
		t.Fatalf("SetDetailedPricing failed: %v", err)
	}

	// Run initPricing with devMode = true (isNewSQLite = false because DB already existed)
	if err := initPricing(ctx, s, false /* isPostgres */, true /* devMode */, false /* isNewSQLite */); err != nil {
		t.Fatalf("initPricing returned unexpected error: %v", err)
	}

	// Verify rates and color_enabled were NOT overwritten
	p, err := s.GetPricing(ctx)
	if err != nil {
		t.Fatalf("GetPricing failed: %v", err)
	}
	if p.BWSimplexPricePaise != 450 || p.BWDuplexPricePaise != 650 ||
		p.ColorSimplexPricePaise != 950 || p.ColorDuplexPricePaise != 1250 ||
		!p.ColorEnabled {
		t.Fatalf("existing tariffs or color_enabled were modified by initPricing: %+v", p)
	}
}

func TestInitPricing_SeedsOnlyForNewlyCreatedDevSQLite(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "fresh_dev.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	defer s.Close()

	ctx := context.Background()

	// 1. In production mode (devMode = false), missing pricing must fail closed even if new SQLite
	if err := initPricing(ctx, s, false /* isPostgres */, false /* devMode */, true /* isNewSQLite */); err == nil {
		t.Fatalf("expected error in production mode for unconfigured pricing, got nil")
	}

	// 2. In dev mode with genuinely new database (isNewSQLite = true), seeding succeeds
	if err := initPricing(ctx, s, false /* isPostgres */, true /* devMode */, true /* isNewSQLite */); err != nil {
		t.Fatalf("expected successful seeding in dev mode, got: %v", err)
	}

	p, err := s.GetPricing(ctx)
	if err != nil {
		t.Fatalf("GetPricing failed: %v", err)
	}
	if p.BWSimplexPricePaise != 300 || p.BWDuplexPricePaise != 500 ||
		p.ColorSimplexPricePaise != 800 || p.ColorDuplexPricePaise != 1000 ||
		p.ColorEnabled != false {
		t.Fatalf("unexpected seeded default rates: %+v", p)
	}

	// 3. Existing SQLite database with orders but unconfigured pricing must fail closed
	tmpDir2 := t.TempDir()
	dbPath2 := filepath.Join(tmpDir2, "existing_with_orders.db")
	s2, err := store.Open(dbPath2)
	if err != nil {
		t.Fatalf("failed to open test store 2: %v", err)
	}
	defer s2.Close()

	// Insert a dummy order to simulate an existing database with history
	_, err = s2.DB().Exec(`
INSERT INTO orders (order_id, job_id, kiosk_id, display_code, status, price_paise, created_at_ns, updated_at_ns)
VALUES ('ord-test-1', 'job-test-1', 'kiosk-1', '1234', 'COMPLETED', 500, 1000, 1000);
`)
	if err != nil {
		t.Fatalf("failed to insert test order: %v", err)
	}

	// Attempt initPricing on existing database missing pricing
	if err := initPricing(ctx, s2, false, true, false /* isNewSQLite */); err == nil {
		t.Fatalf("expected initPricing to fail closed on existing SQLite database with orders, got nil")
	}
}

func TestInitPricing_FailsClosedOnExistingUnconfiguredSQLiteWithoutOrders(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "existing_empty.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	defer s.Close()

	ctx := context.Background()

	// Existing database (isNewSQLite = false) with zero orders must fail closed
	err = initPricing(ctx, s, false /* isPostgres */, true /* devMode */, false /* isNewSQLite */)
	if err == nil {
		t.Fatalf("expected initPricing to fail closed on existing unconfigured SQLite without orders, got nil")
	}

	var count int
	if err := s.DB().QueryRow("SELECT COUNT(1) FROM pricing;").Scan(&count); err != nil {
		t.Fatalf("failed to check pricing count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 rows in pricing, got %d", count)
	}
}

func TestInitPricing_FailsClosedOnOrdersQueryError(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "orders_err.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	defer s.Close()

	ctx := context.Background()

	// Simulate an error on SELECT COUNT(1) FROM orders by dropping the orders table
	if _, err := s.DB().Exec("DROP TABLE orders;"); err != nil {
		t.Fatalf("failed to drop orders table: %v", err)
	}

	// initPricing must immediately fail closed upon error scanning orders count
	err = initPricing(ctx, s, false /* isPostgres */, true /* devMode */, true /* isNewSQLite */)
	if err == nil {
		t.Fatalf("expected initPricing to fail closed when orders query fails, got nil")
	}

	var count int
	if err := s.DB().QueryRow("SELECT COUNT(1) FROM pricing;").Scan(&count); err != nil {
		t.Fatalf("failed to check pricing count: %v", err)
	}
	if count != 0 {
		t.Fatalf("pricing must not be seeded on orders query error, got %d rows", count)
	}
}
