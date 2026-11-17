package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"xeroxgo/cloud-backend/internal/pricing"
)

func TestPublicPricingRequiresConfiguredTariff(t *testing.T) {
	app, _ := setupDocPricingBaselineServer(t)
	read := func() *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		app.handlePublicPricing(w, httptest.NewRequest(http.MethodGet, "/api/pricing", nil))
		return w
	}
	if w := read(); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured GET returned %d; want 503", w.Code)
	}
	first, err := app.db.ActivateSheetTariff(context.Background(), 0, pricing.Tariff{
		BWSimplexPaise:300, BWDuplexSheetPaise:500,
		ColorSimplexPaise:800, ColorDuplexSheetPaise:1000,
		ColorEnabled:false,
	}, "test-admin")
	if err != nil { t.Fatal(err) }
	w := read()
	if w.Code != http.StatusOK { t.Fatalf("configured GET returned %d: %s", w.Code, w.Body.String()) }
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil { t.Fatal(err) }
	for name, want := range map[string]float64{
		"revision":float64(first.Revision),
		"bw_simplex_paise":300,
		"bw_duplex_sheet_paise":500,
		"color_simplex_paise":800,
		"color_duplex_sheet_paise":1000,
	} {
		if response[name] != want { t.Fatalf("%s = %v; want %v", name, response[name], want) }
	}
	if response["color_enabled"] != false { t.Fatalf("color flag = %v", response["color_enabled"]) }
	if _, exists := response["bw_price_per_sheet_paise"]; exists {
		t.Fatal("obsolete legacy pricing key is still exposed")
	}
}

func TestAcceptedSheetQuoteRequiresBothAmountAndRevision(t *testing.T) {
	app, _ := setupDocPricingBaselineServer(t)
	tariff := pricing.Tariff{Revision:2, BWSimplexPaise:300, BWDuplexSheetPaise:500,
		ColorSimplexPaise:800,ColorDuplexSheetPaise:1000}
	for _, tc := range []struct{ name string; amount string; revision string; want bool }{
		{"valid", "800", "2", true},
		{"missing amount", "", "2", false},
		{"missing revision", "800", "", false},
		{"stale revision", "800", "1", false},
		{"incorrect price", "900", "2", false},
		{"malformed", "eight", "2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := url.Values{}
			values.Set("accepted_total_price_paise", tc.amount)
			values.Set("accepted_tariff_revision", tc.revision)
			req := httptest.NewRequest(http.MethodPost, "/api/k/example/upload", strings.NewReader(values.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			ok := app.verifyAcceptedSheetQuote(w, req, tariff, 800)
			if ok != tc.want { t.Fatalf("accepted=%v, want %v", ok, tc.want) }
			if !ok && w.Code != http.StatusConflict {
				t.Fatalf("rejected request returned %s; want 409", strconv.Itoa(w.Code))
			}
		})
	}
}
