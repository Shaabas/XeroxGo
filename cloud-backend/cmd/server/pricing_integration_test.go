package main

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"xeroxgo/cloud-backend/internal/pricing"
)

// TestSheetPricingPreviewAndUpload verifies that the server computes the
// same price from canonical PDF normalization for preview and persisted order.
// Unlike unit tests, this uses actual multipart handlers and generated PDFs.
func TestSheetPricingPreviewAndUpload(t *testing.T) {
	app, mux := setupDocPricingBaselineServer(t)
	rates := pricing.Tariff{
		BWSimplexPaise: 300, BWDuplexSheetPaise: 500,
		ColorSimplexPaise: 800, ColorDuplexSheetPaise: 1000,
		ColorEnabled: false,
	}
	active, err := app.db.ActivateSheetTariff(context.Background(), 0, rates, "integration-test")
	if err != nil { t.Fatal(err) }
	pdfPath := createNamedDocPDF(t, t.TempDir(), "SheetQuote", 3)
	pdfData, err := os.ReadFile(pdfPath)
	if err != nil { t.Fatal(err) }

	call := func(url string, acceptedAmount string, revision string, color string) *httptest.ResponseRecorder {
		t.Helper()
		body := new(bytes.Buffer)
		writer := multipart.NewWriter(body)
		part, err := writer.CreateFormFile("file", "document.pdf")
		if err != nil { t.Fatal(err) }
		if _, err := part.Write(pdfData); err != nil { t.Fatal(err) }
		for k,v := range map[string]string{
			"format":"json", "copies":"1", "duplex":"DUPLEX_LONG_EDGE",
			"color_mode":color, "pages_per_sheet":"1",
		} {
			if err := writer.WriteField(k,v); err != nil { t.Fatal(err) }
		}
		if acceptedAmount != "" {
			_ = writer.WriteField("accepted_total_price_paise", acceptedAmount)
		}
		if revision != "" {
			_ = writer.WriteField("accepted_tariff_revision", revision)
		}
		if err := writer.Close(); err != nil { t.Fatal(err) }
		req := httptest.NewRequest(http.MethodPost, url, body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	preview := call("/api/documents/preview", "", "", "BW")
	if preview.Code != http.StatusOK { t.Fatalf("preview %d: %s", preview.Code, preview.Body.String()) }
	var quoted struct {
		Total int64 `json:"total_price_paise"`
		Revision int64 `json:"tariff_revision"`
		BilledSides int64 `json:"billed_sides"`
		PaddedPages int `json:"padded_pages"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &quoted); err != nil { t.Fatal(err) }
	if quoted.Total != 800 || quoted.Revision != active.Revision || quoted.BilledSides != 3 || quoted.PaddedPages != 4 {
		t.Fatalf("wrong preview quote: %+v", quoted)
	}

	for _, trial := range []struct{name,amount,revision string; status int}{
		{"missing confirmation","","",http.StatusConflict},
		{"stale amount","500","1",http.StatusConflict},
		{"stale revision","800","999",http.StatusConflict},
		{"matching amount","800","1",http.StatusOK},
	} {
		t.Run(trial.name,func(t *testing.T){
			result := call("/api/k/kiosk_sheet/upload",trial.amount,trial.revision,"BW")
			if result.Code != trial.status { t.Fatalf("status %d want %d: %s",result.Code,trial.status,result.Body.String()) }
			if trial.status != http.StatusOK { return }
			var created struct { OrderID string `json:"order_id"`; Total int64 `json:"total_price_paise"`; Revision int64 `json:"tariff_revision"` }
			if err := json.Unmarshal(result.Body.Bytes(), &created); err != nil { t.Fatal(err) }
			if created.Total != quoted.Total || created.Revision != quoted.Revision {
				t.Fatalf("created amount/revision = %+v, preview %+v",created,quoted)
			}
			stored, err := app.db.GetOrder(context.Background(),created.OrderID)
			if err != nil || stored == nil { t.Fatalf("get order: %v",err) }
			if stored.PricePaise != quoted.Total { t.Fatalf("stored order %d != quote %d",stored.PricePaise,quoted.Total) }
		})
	}
	disabled := call("/api/documents/preview","","","COLOR")
	if disabled.Code != http.StatusBadRequest { t.Fatalf("disabled color status %d", disabled.Code) }
	// A subsequent admin change invalidates an earlier tariff revision.
	next := rates
	next.BWSimplexPaise = 350
	if _,err := app.db.ActivateSheetTariff(context.Background(),active.Revision,next,"admin"); err != nil { t.Fatal(err) }
	stale := call("/api/k/kiosk_sheet/upload",strconv.FormatInt(quoted.Total,10),strconv.FormatInt(active.Revision,10),"BW")
	if stale.Code != http.StatusConflict { t.Fatalf("stale tariff status %d: %s",stale.Code,stale.Body.String()) }
}
