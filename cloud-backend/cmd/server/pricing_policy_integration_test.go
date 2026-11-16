package main

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"xeroxgo/cloud-backend/internal/pricing"
)

// The PDF has three content sides plus one blank duplex-padding side.
// Bibin's policy bills one full sheet and one single occupied side.
func TestBibinPricing_PreviewUploadAndAcceptedPrice(t *testing.T) {
	app, mux := setupDocPricingBaselineServer(t)
	app.cfg.pricingPolicy = pricing.PricingPerSheet
	if err := app.db.SetDetailedPricing(context.Background(), 300, 500, 800, 1000, true); err != nil {
		t.Fatal(err)
	}
	pdfPath := createNamedDocPDF(t, t.TempDir(), "BibinPricing", 3)
	data, err := os.ReadFile(pdfPath)
	if err != nil {
		t.Fatal(err)
	}

	request := func(path, accepted string) *httptest.ResponseRecorder {
		t.Helper()
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)
		part, err := writer.CreateFormFile("file", "three-pages.pdf")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatal(err)
		}
		for key, value := range map[string]string{
			"copies": "1", "duplex": "DUPLEX_LONG_EDGE",
			"color_mode": "BW", "format": "json",
		} {
			if err := writer.WriteField(key, value); err != nil {
				t.Fatal(err)
			}
		}
		if accepted != "" {
			if err := writer.WriteField("accepted_total_price_paise", accepted); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, path, body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	preview := request("/api/documents/preview", "")
	if preview.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", preview.Code, preview.Body.String())
	}
	var quote struct {
		Total int64 `json:"total_price_paise"`
		Sides int   `json:"billed_sides"`
		Padded int  `json:"padded_pages"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &quote); err != nil {
		t.Fatal(err)
	}
	if quote.Total != 800 || quote.Sides != 3 || quote.Padded != 4 {
		t.Fatalf("wrong duplex quote: %+v", quote)
	}

	stale := request("/api/k/kiosk_bibin/upload", "500")
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale quote should reject before order creation: %d %s", stale.Code, stale.Body.String())
	}

	accepted := request("/api/k/kiosk_bibin/upload", "800")
	if accepted.Code != http.StatusOK {
		t.Fatalf("accepted quote: %d %s", accepted.Code, accepted.Body.String())
	}
	var order struct {
		OrderID string `json:"order_id"`
		Total int64 `json:"total_price_paise"`
	}
	if err := json.Unmarshal(accepted.Body.Bytes(), &order); err != nil {
		t.Fatal(err)
	}
	if order.Total != quote.Total {
		t.Fatalf("stored quote %d differs from preview %d", order.Total, quote.Total)
	}
	stored, err := app.db.GetOrder(context.Background(), order.OrderID)
	if err != nil || stored == nil || stored.PricePaise != quote.Total {
		t.Fatalf("order storage mismatch: order=%+v err=%v", stored, err)
	}
}
