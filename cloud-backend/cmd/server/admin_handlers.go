package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"xeroxgo/cloud-backend/internal/pricing"
	"xeroxgo/cloud-backend/internal/store"
)

// handlePublicPricing exposes only the currently active canonical tariff.
// An unconfigured database returns 503 rather than legacy or default prices.
func (app *appServer) handlePublicPricing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tariff, err := app.db.GetActiveSheetTariff(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "pricing not configured")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tariff)
}

// handleAdminPricing exposes one four-rate pricing policy.
// ExpectedRevision provides optimistic concurrency protection on updates.
func (app *appServer) handleAdminPricing(w http.ResponseWriter, r *http.Request) {
	actor, ok := app.authenticateAdminRequest(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	switch r.Method {
	case http.MethodGet:
		tariff, err := app.db.GetActiveSheetTariff(r.Context())
		if errors.Is(err, store.ErrSheetTariffNotActive) {
			// Defaults here are a form template, NOT silently active tariffs.
			tariff = pricing.Tariff{
				BWSimplexPaise: 300, BWDuplexSheetPaise: 500,
				ColorSimplexPaise: 800, ColorDuplexSheetPaise: 1000,
				ColorEnabled: false,
			}
		} else if err != nil {
			writeJSONError(w, http.StatusServiceUnavailable, "pricing unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tariff)
	case http.MethodPut:
		var req struct {
			ExpectedRevision int64 `json:"expected_revision"`
			BWSimplexPaise int64 `json:"bw_simplex_paise"`
			BWDuplexSheetPaise int64 `json:"bw_duplex_sheet_paise"`
			ColorSimplexPaise int64 `json:"color_simplex_paise"`
			ColorDuplexSheetPaise int64 `json:"color_duplex_sheet_paise"`
			ColorEnabled bool `json:"color_enabled"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid pricing payload")
			return
		}
		if err := decoder.Decode(new(interface{})); !errors.Is(err, io.EOF) {
			writeJSONError(w, http.StatusBadRequest, "multiple JSON values")
			return
		}
		next, err := app.db.ActivateSheetTariff(r.Context(), req.ExpectedRevision, pricing.Tariff{
			BWSimplexPaise: req.BWSimplexPaise,
			BWDuplexSheetPaise: req.BWDuplexSheetPaise,
			ColorSimplexPaise: req.ColorSimplexPaise,
			ColorDuplexSheetPaise: req.ColorDuplexSheetPaise,
			ColorEnabled: req.ColorEnabled,
		}, actor)
		if errors.Is(err, store.ErrSheetTariffRevisionConflict) {
			writeJSONError(w, http.StatusConflict, "pricing was changed; reload before saving")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid or unavailable pricing configuration")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(next)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleAdminAnalytics handles GET /api/admin/analytics
func (app *appServer) handleAdminAnalytics(w http.ResponseWriter, r *http.Request) {
	if _, ok := app.authenticateAdminRequest(r); !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	timeRange := r.URL.Query().Get("range")
	if timeRange == "" {
		timeRange = "7d"
	}

	analytics, err := app.db.GetAnalytics(r.Context(), timeRange)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to fetch analytics: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(analytics)
}

// handleAdminOrders handles GET /api/admin/orders
func (app *appServer) handleAdminOrders(w http.ResponseWriter, r *http.Request) {
	if _, ok := app.authenticateAdminRequest(r); !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	status := r.URL.Query().Get("status")
	search := r.URL.Query().Get("search")
	kioskID := r.URL.Query().Get("kiosk_id")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	filter := store.OrderHistoryFilter{
		Status:  status,
		Search:  search,
		KioskID: kioskID,
		Limit:   limit,
		Offset:  offset,
	}

	history, err := app.db.GetOrderHistory(r.Context(), filter)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to fetch order history: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(history)
}

// handleAdminOrdersExport handles GET /api/admin/orders/export (CSV download)
func (app *appServer) handleAdminOrdersExport(w http.ResponseWriter, r *http.Request) {
	if _, ok := app.authenticateAdminRequest(r); !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	history, err := app.db.GetOrderHistory(r.Context(), store.OrderHistoryFilter{Limit: 2000})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to export orders: %v", err))
		return
	}

	filename := fmt.Sprintf("xeroxgo-orders-%s.csv", time.Now().UTC().Format("2006-01-02"))
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))

	writer := csv.NewWriter(w)
	defer writer.Flush()

	// Write CSV Header
	_ = writer.Write([]string{
		"Order ID",
		"Job ID",
		"Display Code",
		"Kiosk ID",
		"Date (UTC)",
		"Status",
		"Color Mode",
		"Pages",
		"Sheets",
		"Copies",
		"Duplex",
		"Amount (INR)",
		"Error Reason",
	})

	for _, item := range history.Orders {
		amountINR := fmt.Sprintf("%.2f", float64(item.PricePaise)/100.0)
		_ = writer.Write([]string{
			sanitizeCSVField(item.OrderID),
			sanitizeCSVField(item.JobID),
			sanitizeCSVField(item.DisplayCode),
			sanitizeCSVField(item.KioskID),
			item.CreatedAt.Format("2006-01-02 15:04:05"),
			sanitizeCSVField(string(item.Status)),
			sanitizeCSVField(item.ColorMode),
			strconv.Itoa(item.TotalPages),
			strconv.Itoa(item.TotalSheets),
			strconv.Itoa(item.Copies),
			sanitizeCSVField(string(item.Duplex)),
			amountINR,
			sanitizeCSVField(item.ErrorReason),
		})
	}
}

// sanitizeCSVField prevents Formula Injection (CSV injection) in spreadsheet applications
// by prefixing formula trigger characters (=, +, -, @, tab, CR) with a single quote.
func sanitizeCSVField(s string) string {
	if s == "" {
		return ""
	}
	first := s[0]
	if first == '=' || first == '+' || first == '-' || first == '@' || first == '\t' || first == '\r' {
		return "'" + s
	}
	return s
}
