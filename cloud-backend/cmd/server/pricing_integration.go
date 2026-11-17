package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"xeroxgo/cloud-backend/internal/pricing"
	"xeroxgo/cloud-backend/internal/store"
)

// requestTariffs is loaded ONCE per upload or preview so that a mixed-file
// order never combines rates from different administrator revisions.
type requestTariffs struct {
	legacy store.PricingConfig
	hasLegacy bool
	sheet *pricing.Tariff
}

func (s *appServer) loadRequestTariffs(ctx context.Context) (requestTariffs, error) {
	active, err := s.db.GetActiveSheetTariff(ctx)
	if err == nil {
		return requestTariffs{
			legacy: store.PricingConfig{ColorEnabled: active.ColorEnabled},
			sheet: &active,
		}, nil
	}
	if !errors.Is(err, store.ErrSheetTariffNotActive) {
		return requestTariffs{}, err // Corrupt/missing configuration never becomes free/default pricing.
	}

	// The legacy billing flow stays available ONLY until the new policy is
	// explicitly activated. This is a transition, not a permanent dual policy.
	legacy, err := s.db.GetPricing(ctx)
	if err != nil {
		return requestTariffs{}, err
	}
	return requestTariffs{legacy: legacy, hasLegacy: true}, nil
}

func (p requestTariffs) colorEnabled() bool {
	if p.sheet != nil { return p.sheet.ColorEnabled }
	return p.legacy.ColorEnabled
}

func (p requestTariffs) sheetArgs() []pricing.Tariff {
	if p.sheet == nil { return nil }
	return []pricing.Tariff{*p.sheet}
}

// During sheet-policy rollout the old browser cannot confirm quotes. Reject
// checkout rather than silently inventing consent to an updated amount.
// A complete frontend implementation must supply BOTH accepted fields.
func (s *appServer) verifyAcceptedSheetQuote(w http.ResponseWriter, r *http.Request, p requestTariffs, actual int64) bool {
	if p.sheet == nil { return true }
	rawPrice := strings.TrimSpace(r.FormValue("accepted_total_price_paise"))
	rawRevision := strings.TrimSpace(r.FormValue("accepted_tariff_revision"))
	acceptedPrice, errPrice := strconv.ParseInt(rawPrice, 10, 64)
	acceptedRevision, errRevision := strconv.ParseInt(rawRevision, 10, 64)
	if errPrice != nil || errRevision != nil || acceptedPrice <= 0 || acceptedRevision <= 0 {
		http.Error(w, "Please review and confirm the updated cloud price before checkout.", http.StatusConflict)
		return false
	}
	if acceptedPrice != actual || acceptedRevision != p.sheet.Revision {
		http.Error(w, "Price changed. Please review the new quotation.", http.StatusConflict)
		return false
	}
	// Recheck before writing the order. An atomic tariff-revision check in
	// the order-creation transaction remains necessary for full linearizability.
	active, err := s.db.GetActiveSheetTariff(r.Context())
	if err != nil {
		http.Error(w, "Pricing temporarily unavailable.", http.StatusServiceUnavailable)
		return false
	}
	if active.Revision != acceptedRevision {
		http.Error(w, "Pricing changed. Please review the new quotation.", http.StatusConflict)
		return false
	}
	return true
}

// Unlike the historical parser, activated sheet pricing never silently
// substitutes B&W for an unsupported color request.
func validateColorRequest(r *http.Request, enabled bool, sheetEnabled bool, requestedMode string) bool {
	if !sheetEnabled { return true }
	if !strings.EqualFold(strings.TrimSpace(requestedMode), "COLOR") { return true }
	return enabled
}
