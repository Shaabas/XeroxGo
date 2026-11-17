package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"xeroxgo/cloud-backend/internal/pricing"
)

// loadRequestTariff fetches one immutable rate snapshot for the entire request.
// Missing or unreadable configuration is an error, never a silent fallback.
func (s *appServer) loadRequestTariff(ctx context.Context) (pricing.Tariff, error) {
	return s.db.GetActiveSheetTariff(ctx)
}

// verifyAcceptedSheetQuote compares the customer's accepted cloud quotation
// with the canonical amount and tariff snapshot used for this upload.
// Order creation independently rechecks the revision inside its transaction.
func (s *appServer) verifyAcceptedSheetQuote(w http.ResponseWriter, r *http.Request, tariff pricing.Tariff, actual int64) bool {
	price, priceErr := strconv.ParseInt(strings.TrimSpace(r.FormValue("accepted_total_price_paise")), 10, 64)
	revision, revisionErr := strconv.ParseInt(strings.TrimSpace(r.FormValue("accepted_tariff_revision")), 10, 64)
	if priceErr != nil || revisionErr != nil || price <= 0 || revision <= 0 {
		http.Error(w, "Please confirm the current cloud quotation before checkout.", http.StatusConflict)
		return false
	}
	if price != actual || revision != tariff.Revision {
		http.Error(w, "Price changed. Review the updated quotation.", http.StatusConflict)
		return false
	}
	return true
}
