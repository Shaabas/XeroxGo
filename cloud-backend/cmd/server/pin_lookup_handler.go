package main

import (
	"crypto/hmac"
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"xeroxgo/pkg/contracts"
)

var pinRegex = regexp.MustCompile(`^\d{4}$`)

type pinLookupRequest struct {
	PIN string `json:"pin"`
}

type pinLookupResponse struct {
	OrderID          string `json:"order_id"`
	KioskID          string `json:"kiosk_id"`
	Status           string `json:"status"`
	TotalPricePaise  int64  `json:"total_price_paise"`
	TotalPages       int    `json:"total_pages"`
	TotalSheets      int    `json:"total_sheets"`
	Copies           int    `json:"copies"`
	Duplex           string `json:"duplex"`
	ColorMode        string `json:"color_mode"`
	PINExpiresAt     string `json:"pin_expires_at"`
	SecondsRemaining int64  `json:"seconds_remaining"`
}

// handlePINLookup handles authenticated, kiosk-scoped order lookup by 4-digit PIN.
// Endpoint: POST /api/k/{kiosk_id}/orders/lookup (and alias /api/k/{kiosk_id}/pin/lookup)
//
// Security & Hardening Invariants:
// 1. Strictly requires Authorization: Bearer <secret> when kioskSecret is configured (no query params).
// 2. Uniform error responses: unknown, expired, completed, or cross-kiosk lookups all return 404 "invalid or expired PIN",
//    preventing PIN enumeration or order existence oracles.
// 3. Durable rate limiting backed by database store to prevent brute-force attacks across server restarts.
// 4. Authoritative cloud expiration checks, strictly evaluated against server UTC time.
// 5. Strictly read-only: never marks orders paid, never dispatches print jobs, never exposes download URLs.
func (s *appServer) handlePINLookup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse kioskID from URL: /api/k/{kiosk_id}/orders/lookup
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/k/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" {
		http.Error(w, "invalid kiosk lookup path: expected /api/k/{kiosk_id}/orders/lookup", http.StatusBadRequest)
		return
	}
	kioskID := strings.TrimSpace(parts[0])

	// Kiosk Authorization: strictly require Authorization: Bearer <secret>
	if len(s.cfg.kioskSecret) > 0 {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "unauthorized kiosk: Authorization Bearer header required", http.StatusUnauthorized)
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		if !hmac.Equal([]byte(token), s.cfg.kioskSecret) {
			http.Error(w, "unauthorized kiosk: invalid credentials", http.StatusUnauthorized)
			return
		}
	}

	// Check rate limit (persisted in database across restarts)
	now := time.Now().UTC()
	if s.pinLimiter != nil {
		if allowed, retryAfter := s.pinLimiter.Check(r.Context(), kioskID, now); !allowed {
			w.Header().Set("Content-Type", "application/json")
			retrySec := int(math.Ceil(retryAfter.Seconds()))
			if retrySec < 1 {
				retrySec = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(retrySec))
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error":               "too many failed attempts, please try again later",
				"retry_after_seconds": retrySec,
			})
			return
		}
	}

	// Decode request body
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	var req pinLookupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if s.pinLimiter != nil {
			s.pinLimiter.RecordFailure(r.Context(), kioskID, now)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": "invalid request body",
		})
		return
	}

	pin := strings.TrimSpace(req.PIN)
	if !pinRegex.MatchString(pin) {
		if s.pinLimiter != nil {
			s.pinLimiter.RecordFailure(r.Context(), kioskID, now)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": "PIN must be exactly 4 numeric digits",
		})
		return
	}

	// Query database
	order, err := s.db.GetOrderByPIN(r.Context(), kioskID, pin)
	if err != nil {
		http.Error(w, "failed to query order by PIN", http.StatusInternalServerError)
		return
	}

	// Uniform failure handler: prevents probing/enumerating whether orders exist
	rejectAsInvalidOrExpired := func() {
		if s.pinLimiter != nil {
			s.pinLimiter.RecordFailure(r.Context(), kioskID, now)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound) // HTTP 404
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": "invalid or expired PIN",
		})
	}

	// Unknown PIN or kiosk mismatch
	if order == nil || order.Command.KioskID != kioskID {
		rejectAsInvalidOrExpired()
		return
	}

	// Server-enforced boundary expiry check: authoritative server UTC time
	if !order.PINExpiresAt.IsZero() && !now.Before(order.PINExpiresAt) {
		rejectAsInvalidOrExpired()
		return
	}

	// Only active orders awaiting redemption can be returned
	if order.Status != contracts.JobStatusPendingPayment && order.Status != contracts.JobStatusPaidWaiting {
		rejectAsInvalidOrExpired()
		return
	}

	// Success: clear rate limiter failures for this kiosk
	if s.pinLimiter != nil {
		s.pinLimiter.RecordSuccess(r.Context(), kioskID)
	}

	var secondsRemaining int64
	if !order.PINExpiresAt.IsZero() {
		rem := order.PINExpiresAt.Sub(now)
		if rem > 0 {
			secondsRemaining = int64(math.Ceil(rem.Seconds()))
		}
	}

	resp := pinLookupResponse{
		OrderID:          order.OrderID,
		KioskID:          order.Command.KioskID,
		Status:           string(order.Status),
		TotalPricePaise:  order.PricePaise,
		TotalPages:       order.Command.TotalPages,
		TotalSheets:      order.Command.TotalSheets,
		Copies:           order.Command.Copies,
		Duplex:           string(order.Command.Duplex),
		ColorMode:        order.ColorMode,
		PINExpiresAt:     order.PINExpiresAt.Format(time.RFC3339),
		SecondsRemaining: secondsRemaining,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
