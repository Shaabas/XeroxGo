package main

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

const kioskCheckoutSessionDuration = 120 * time.Second

type sessionResponse struct {
	SessionID        string `json:"session_id"`
	OrderID          string `json:"order_id"`
	KioskID          string `json:"kiosk_id"`
	Status           string `json:"status"`
	TotalPricePaise  int64  `json:"total_price_paise"`
	ExpiresAt        string `json:"expires_at"`
	SecondsRemaining int64  `json:"seconds_remaining"`
}

func (s *appServer) verifyKioskAuth(w http.ResponseWriter, r *http.Request) bool {
	if len(s.cfg.kioskSecret) > 0 {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "unauthorized kiosk: Authorization Bearer header required", http.StatusUnauthorized)
			return false
		}
		token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		if !hmac.Equal([]byte(token), s.cfg.kioskSecret) {
			http.Error(w, "unauthorized kiosk: invalid credentials", http.StatusUnauthorized)
			return false
		}
	}
	return true
}

func parseKioskAndOrder(path, suffix string) (kioskID, orderID string, err error) {
	trimmed := strings.TrimPrefix(path, "/api/k/")
	trimmed = strings.TrimSuffix(trimmed, suffix)
	parts := strings.Split(trimmed, "/orders/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", fmt.Errorf("invalid order path: %s", path)
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
}

func parseKioskAndSession(path, suffix string) (kioskID, sessionID string, err error) {
	trimmed := strings.TrimPrefix(path, "/api/k/")
	if suffix != "" {
		if !strings.HasSuffix(trimmed, suffix) {
			return "", "", fmt.Errorf("invalid session path: %s", path)
		}
		trimmed = strings.TrimSuffix(trimmed, suffix)
	}
	parts := strings.Split(trimmed, "/sessions/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", fmt.Errorf("invalid session path: %s", path)
	}
	kioskID = strings.TrimSpace(parts[0])
	sessionID = strings.TrimSpace(parts[1])
	if strings.Contains(kioskID, "/") || strings.Contains(sessionID, "/") {
		return "", "", fmt.Errorf("invalid session path: %s", path)
	}
	return kioskID, sessionID, nil
}

type printerConditionError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *printerConditionError) Error() string {
	return e.Message
}

func (s *appServer) checkPrinterCondition(kioskID string) (int, *printerConditionError) {
	if s.hub == nil {
		if s.cfg != nil && s.cfg.devMode {
			return 150, nil
		}
		return 0, &printerConditionError{
			StatusCode: http.StatusConflict,
			Code:       "PRINTER_OFFLINE",
			Message:    "communication hub not available",
		}
	}

	connected, health, hasHealth, ready, readyReason := s.hub.EvaluateKioskReadiness(kioskID)
	if !connected {
		if s.cfg != nil && s.cfg.devMode && !hasHealth {
			return 150, nil
		}
		return 0, &printerConditionError{
			StatusCode: http.StatusConflict,
			Code:       "PRINTER_OFFLINE",
			Message:    "kiosk printer is offline or disconnected",
		}
	}

	if !hasHealth {
		if s.cfg != nil && s.cfg.devMode {
			return 150, nil
		}
		return 0, &printerConditionError{
			StatusCode: http.StatusConflict,
			Code:       "TELEMETRY_MISSING",
			Message:    "printer health telemetry is not yet available",
		}
	}

	if !ready {
		switch health.EvaluatedHealth {
		case contracts.EvaluatedHealthFault:
			msg := "printer hardware error or offline"
			if health.PaperJam {
				msg = "printer has a paper jam"
			} else if health.CoverOpen {
				msg = "printer cover is open"
			} else if health.OutputBinFull {
				msg = "printer output bin is full"
			} else if health.ErrorMessage != "" {
				msg = fmt.Sprintf("printer error: %s", health.ErrorMessage)
			}
			return 0, &printerConditionError{
				StatusCode: http.StatusConflict,
				Code:       "PRINTER_ERROR",
				Message:    msg,
			}
		case contracts.EvaluatedHealthUnverified, "":
			code := "TELEMETRY_UNVERIFIED"
			if strings.Contains(strings.ToLower(readyReason), "expired") {
				code = "TELEMETRY_STALE"
			}
			msg := health.ErrorMessage
			if msg == "" {
				msg = readyReason
			}
			if msg == "" {
				msg = "printer telemetry readiness is unverified"
			}
			return 0, &printerConditionError{
				StatusCode: http.StatusConflict,
				Code:       code,
				Message:    msg,
			}
		}

		if !health.ExecutionSafe {
			msg := "printer local execution-safety barrier is active"
			if health.ExecutionSafetyReason != "" {
				msg = health.ExecutionSafetyReason
			}
			return 0, &printerConditionError{
				StatusCode: http.StatusConflict,
				Code:       "EXECUTION_UNSAFE",
				Message:    msg,
			}
		}

		return 0, &printerConditionError{
			StatusCode: http.StatusConflict,
			Code:       "PRINTER_ERROR",
			Message:    firstNonEmptySessionErr(health.ErrorMessage, readyReason, "printer hardware error or offline"),
		}
	}

	// Validate configured capacity and estimated stock consistently
	if health.ConfiguredTrayCapacity <= 0 {
		return 0, &printerConditionError{
			StatusCode: http.StatusConflict,
			Code:       "MALFORMED_TELEMETRY",
			Message:    "kiosk reported invalid or non-positive tray capacity",
		}
	}
	if health.EstimatedSheets < 0 || health.EstimatedSheets > health.ConfiguredTrayCapacity {
		return 0, &printerConditionError{
			StatusCode: http.StatusConflict,
			Code:       "MALFORMED_TELEMETRY",
			Message:    fmt.Sprintf("kiosk estimated sheets %d out of bounds [0, %d]", health.EstimatedSheets, health.ConfiguredTrayCapacity),
		}
	}

	// Existing inventory and calibration check
	tray := strings.ToUpper(strings.TrimSpace(health.PaperTrayStatus))
	if tray == "UNKNOWN" || !health.InventoryCalibrated {
		return 0, &printerConditionError{
			StatusCode: http.StatusConflict,
			Code:       "PAPER_UNKNOWN",
			Message:    "printer paper supply is unknown; tray calibration required",
		}
	}
	if tray == "EMPTY" || tray == "CHECK_PRINTER" || health.EstimatedSheets <= 0 {
		return 0, &printerConditionError{
			StatusCode: http.StatusConflict,
			Code:       "INSUFFICIENT_PAPER",
			Message:    "printer paper tray is empty or low; attendant refill required",
		}
	}

	// Conservative capacity enforcement: contradictory physical capacity must not permit
	// inventory or reservations above the verified physical limit.
	effectiveStock := contracts.EffectiveInventorySheets(
		health.EstimatedSheets,
		health.ConfiguredTrayCapacity,
		health.InputTrayCapacity,
		health.InputTrayCapacityKnown,
	)

	return effectiveStock, nil
}

func validateOrderAgainstPolicy(ord *store.Order, policy contracts.ProductCapabilityPolicy) *printerConditionError {
	if ord == nil {
		return &printerConditionError{
			StatusCode: http.StatusBadRequest,
			Code:       "MISSING_ORDER_DATA",
			Message:    "order data is missing or unavailable",
		}
	}
	if len(ord.Documents) > 0 {
		for _, doc := range ord.Documents {
			if err := contracts.ValidatePrintModeAgainstPolicy(doc.Duplex, doc.ColorMode, policy); err != nil {
				return &printerConditionError{
					StatusCode: http.StatusBadRequest,
					Code:       "CAPABILITY_UNSUPPORTED",
					Message:    fmt.Sprintf("document %d: %v", doc.DocumentIndex, err),
				}
			}
		}
	} else {
		if err := contracts.ValidatePrintModeAgainstPolicy(ord.Command.Duplex, ord.ColorMode, policy); err != nil {
			return &printerConditionError{
				StatusCode: http.StatusBadRequest,
				Code:       "CAPABILITY_UNSUPPORTED",
				Message:    err.Error(),
			}
		}
		if err := contracts.ValidatePrintModeAgainstPolicy(ord.Command.Duplex, ord.Command.ColorMode, policy); err != nil {
			return &printerConditionError{
				StatusCode: http.StatusBadRequest,
				Code:       "CAPABILITY_UNSUPPORTED",
				Message:    err.Error(),
			}
		}
	}
	return nil
}

func firstNonEmptySessionErr(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

func (s *appServer) dispatchAuthorizedCommands(ctx context.Context, tag string, cmds []contracts.PrintJobCommand) {
	if s == nil || s.hub == nil {
		return
	}
	policy := s.policy()
	for _, cmd := range cmds {
		if err := contracts.ValidatePrintModeAgainstPolicy(cmd.Duplex, cmd.ColorMode, policy); err != nil {
			log.Printf("%s Blocked dispatch of job %s to kiosk %s by product policy: %v", tag, cmd.JobID, cmd.KioskID, err)
			break
		}
		if err := s.hub.SendJob(ctx, cmd); err != nil {
			log.Printf("%s Warning: failed to send job %s to kiosk %s: %v", tag, cmd.JobID, cmd.KioskID, err)
			s.hub.MarkDispatchRetryNeeded(cmd.KioskID)
			break
		}
		if s.db != nil {
			if err := s.db.MarkDispatchIntentSent(ctx, cmd.JobID); err != nil {
				bgCtx, bgCancel := context.WithTimeout(context.Background(), 2*time.Second)
				retryErr := s.db.MarkDispatchIntentSent(bgCtx, cmd.JobID)
				bgCancel()
				if retryErr != nil {
					log.Printf("%s Warning: failed to mark job %s SENT: %v", tag, cmd.JobID, retryErr)
					s.hub.MarkDispatchRetryNeeded(cmd.KioskID)
					break
				}
			}
		}
	}
}

// handleOrderClaim creates or retrieves an active transaction session for a kiosk order.
// POST /api/k/{kiosk_id}/orders/{order_id}/claim (or /confirm)
func (s *appServer) handleOrderClaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.verifyKioskAuth(w, r) {
		return
	}

	suffix := "/claim"
	if strings.HasSuffix(r.URL.Path, "/confirm") {
		suffix = "/confirm"
	}
	kioskID, orderID, err := parseKioskAndOrder(r.URL.Path, suffix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	now := time.Now().UTC()

	// Validate printer condition and get current estimated physical stock
	stock, condErr := s.checkPrinterCondition(kioskID)
	if condErr != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(condErr.StatusCode)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": condErr.Message,
			"code":  condErr.Code,
		})
		return
	}

	ord, oErr := s.db.GetOrder(r.Context(), orderID)
	if oErr != nil || ord == nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}
	if polErr := validateOrderAgainstPolicy(ord, s.policy()); polErr != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(polErr.StatusCode)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": polErr.Message,
			"code":  polErr.Code,
		})
		return
	}

	sess, err := s.db.CreateOrGetKioskSessionWithPaperCheck(r.Context(), kioskID, orderID, stock, kioskCheckoutSessionDuration, now)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case errors.Is(err, store.ErrInsufficientPaper):
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "insufficient paper in kiosk tray",
				"code":  "INSUFFICIENT_PAPER",
			})
		case errors.Is(err, store.ErrOrderAlreadyClaimed):
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "order is already claimed by an active transaction session",
			})
		case errors.Is(err, store.ErrOrderExpired):
			w.WriteHeader(http.StatusGone)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "order has expired",
			})
		case errors.Is(err, store.ErrOrderNotFound):
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "order not found",
			})
		case errors.Is(err, store.ErrOrderNotPending):
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "order is not in pending payment state",
			})
		case errors.Is(err, store.ErrKioskMismatch):
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "order does not belong to this kiosk",
			})
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "internal error claiming order session",
			})
		}
		return
	}

	secRemaining := int64(sess.ExpiresAt.Sub(now).Seconds())
	if secRemaining < 0 {
		secRemaining = 0
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(sessionResponse{
		SessionID:        sess.SessionID,
		OrderID:          sess.OrderID,
		KioskID:          sess.KioskID,
		Status:           string(sess.Status),
		TotalPricePaise:  sess.PricePaise,
		ExpiresAt:        sess.ExpiresAt.Format(time.RFC3339),
		SecondsRemaining: secRemaining,
	})
}

// handleSessionCancel cancels an active transaction session.
// POST /api/k/{kiosk_id}/sessions/{session_id}/cancel
// POST /api/k/{kiosk_id}/orders/{order_id}/cancel-session
func (s *appServer) handleSessionCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.verifyKioskAuth(w, r) {
		return
	}

	now := time.Now().UTC()
	var kioskID, sessionID string
	var err error

	if strings.Contains(r.URL.Path, "/sessions/") {
		kioskID, sessionID, err = parseKioskAndSession(r.URL.Path, "/cancel")
	} else if strings.HasSuffix(r.URL.Path, "/cancel-session") {
		var orderID string
		kioskID, orderID, err = parseKioskAndOrder(r.URL.Path, "/cancel-session")
		if err == nil {
			activeSess, _ := s.db.GetActiveKioskSession(r.Context(), kioskID, orderID, now)
			if activeSess != nil {
				sessionID = activeSess.SessionID
			}
		}
	}
	if err != nil || sessionID == "" {
		http.Error(w, "invalid cancel path or session not found", http.StatusBadRequest)
		return
	}

	if err := s.db.CancelKioskSession(r.Context(), sessionID, kioskID, now); err != nil {
		if errors.Is(err, store.ErrSessionNotFound) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to cancel session", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"session_id": sessionID,
		"status":     "CANCELED",
	})
}

// handleGetSession queries session details.
// GET /api/k/{kiosk_id}/sessions/{session_id}
func (s *appServer) handleGetSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.verifyKioskAuth(w, r) {
		return
	}

	kioskID, sessionID, err := parseKioskAndSession(r.URL.Path, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	now := time.Now().UTC()
	sess, err := s.db.GetKioskSession(r.Context(), sessionID, now)
	if err != nil {
		if errors.Is(err, store.ErrSessionNotFound) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to get session", http.StatusInternalServerError)
		return
	}

	if sess.KioskID != kioskID {
		http.Error(w, "session does not belong to this kiosk", http.StatusForbidden)
		return
	}

	secRemaining := int64(sess.ExpiresAt.Sub(now).Seconds())
	if secRemaining < 0 {
		secRemaining = 0
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(sessionResponse{
		SessionID:        sess.SessionID,
		OrderID:          sess.OrderID,
		KioskID:          sess.KioskID,
		Status:           string(sess.Status),
		TotalPricePaise:  sess.PricePaise,
		ExpiresAt:        sess.ExpiresAt.Format(time.RFC3339),
		SecondsRemaining: secRemaining,
	})
}

// handleInitiatePayment verifies the exact requested active transaction session before initiating payment.
// POST /api/k/{kiosk_id}/sessions/{session_id}/initiate-payment
func (s *appServer) handleInitiatePayment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.verifyKioskAuth(w, r) {
		return
	}

	if !strings.Contains(r.URL.Path, "/sessions/") {
		http.NotFound(w, r)
		return
	}

	kioskID, sessionID, err := parseKioskAndSession(r.URL.Path, "/initiate-payment")
	if err != nil {
		http.Error(w, "invalid initiate payment path", http.StatusBadRequest)
		return
	}

	now := time.Now().UTC()
	sess, err := s.db.GetKioskSession(r.Context(), sessionID, now)
	if err != nil {
		if errors.Is(err, store.ErrSessionNotFound) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "valid active transaction session required to initiate payment",
			})
			return
		}
		http.Error(w, "failed to check active session", http.StatusInternalServerError)
		return
	}
	if sess == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "valid active transaction session required to initiate payment",
		})
		return
	}
	if sess.KioskID != kioskID {
		http.Error(w, "session does not belong to this kiosk", http.StatusForbidden)
		return
	}
	if sess.Status != store.KioskSessionPaymentPending || !sess.ExpiresAt.After(now) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "valid active transaction session required to initiate payment",
		})
		return
	}

	orderID := sess.OrderID

	// Load order to verify document count and printable readiness
	ord, oErr := s.db.GetOrder(r.Context(), orderID)
	if oErr != nil || ord == nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}
	if polErr := validateOrderAgainstPolicy(ord, s.policy()); polErr != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(polErr.StatusCode)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": polErr.Message,
			"code":  polErr.Code,
		})
		return
	}

	// Revalidate printer condition and fresh stock before initiating payment
	stock, condErr := s.checkPrinterCondition(kioskID)
	if condErr != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(condErr.StatusCode)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": condErr.Message,
			"code":  condErr.Code,
		})
		return
	}

	// Verify paper supply against other commitments
	committedSheets, cErr := s.db.GetCommittedPaperSheets(r.Context(), kioskID, orderID, now)
	if cErr != nil {
		log.Printf("[INITIATE-PAYMENT] Failed to query paper commitments: %v", cErr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "failed to verify paper commitments",
			"code":  "DATABASE_ERROR",
		})
		return
	}

	reqSheets := ord.TotalSheets
	if reqSheets <= 0 {
		reqSheets = ord.Command.TotalSheets
	}
	if reqSheets <= 0 {
		reqSheets = 1
	}
	if (stock - committedSheets) < reqSheets {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "insufficient paper in kiosk tray to fulfill order",
			"code":  "INSUFFICIENT_PAPER",
		})
		return
	}

	// Idempotency: check if an active, unexpired payment attempt already exists for this session
	activeAttempt, err := s.db.GetActivePaymentAttempt(r.Context(), sess.SessionID, now)
	if err != nil {
		log.Printf("[PAYMENT] Failed to query active payment attempt for session %s: %v", sess.SessionID, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "failed to check active payment attempt",
			"code":  "DATABASE_ERROR",
		})
		return
	}
	if activeAttempt != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"attempt_id":        activeAttempt.AttemptID,
			"session_id":        activeAttempt.SessionID,
			"order_id":          activeAttempt.OrderID,
			"kiosk_id":          activeAttempt.KioskID,
			"provider":          activeAttempt.Provider,
			"provider_order_id": activeAttempt.ProviderOrderID,
			"amount_paise":      activeAttempt.AmountPaise,
			"currency":          activeAttempt.Currency,
			"status":            string(sess.Status),
			"attempt_status":    activeAttempt.Status,
			"qr_payload":        activeAttempt.QRPayload,
			"qr_image_url":      activeAttempt.QRImageURL,
			"demo_token":        activeAttempt.DemoToken,
			"expires_at":        activeAttempt.ExpiresAt.Format(time.RFC3339),
		})
		return
	}

	// The payment attempt and QR must expire no later than the checkout session.
	attemptExpiresAt := sess.ExpiresAt
	if maxExp := now.Add(kioskCheckoutSessionDuration); attemptExpiresAt.After(maxExp) {
		attemptExpiresAt = maxExp
	}

	// One checkout path: generate an unpredictable demo token and encode
	// its XeroxGo-hosted confirmation URL in the kiosk QR. No UPI intent
	// and no external payment provider are involved.
	tokenHex, err := randomHex(24)
	if err != nil {
		log.Printf("[PAYMENT] Failed to generate secure random demo token: %v", err)
		http.Error(w, "internal server error generating demo token", http.StatusInternalServerError)
		return
	}
	demoToken := "dmo_" + tokenHex
	checkoutURL := fmt.Sprintf("%s/pay/%s", s.getBaseURL(r), demoToken)

	attemptID := fmt.Sprintf("att_%s_%d", sess.OrderID, now.UnixNano())
	newAttempt := store.PaymentAttempt{
		AttemptID:         attemptID,
		OrderID:           sess.OrderID,
		KioskID:           sess.KioskID,
		SessionID:         sess.SessionID,
		Provider:          "mock",
		ProviderOrderID:   demoToken,
		ProviderPaymentID: "",
		AmountPaise:       sess.PricePaise,
		Currency:          "INR",
		Status:            "CREATED",
		QRPayload:         checkoutURL,
		QRImageURL:        "",
		DemoToken:         demoToken,
		ExpiresAt:         attemptExpiresAt,
	}

	if err := s.db.CreatePaymentAttempt(r.Context(), newAttempt); err != nil {
		log.Printf("[PAYMENT] CreatePaymentAttempt db error for order %s: %v", sess.OrderID, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "failed to persist payment attempt",
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"attempt_id":        newAttempt.AttemptID,
		"session_id":        newAttempt.SessionID,
		"order_id":          newAttempt.OrderID,
		"kiosk_id":          newAttempt.KioskID,
		"provider":          newAttempt.Provider,
		"provider_order_id": newAttempt.ProviderOrderID,
		"amount_paise":      newAttempt.AmountPaise,
		"currency":          newAttempt.Currency,
		"status":            string(sess.Status),
		"attempt_status":    newAttempt.Status,
		"qr_payload":        newAttempt.QRPayload,
		"qr_image_url":      newAttempt.QRImageURL,
		"demo_token":        newAttempt.DemoToken,
		"expires_at":        newAttempt.ExpiresAt.Format(time.RFC3339),
	})
}

// handleOrderRelease authorizes physical print dispatch for an already paid order.
// POST /api/k/{kiosk_id}/orders/{order_id}/release
func (s *appServer) handleOrderRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.verifyKioskAuth(w, r) {
		return
	}

	kioskID, orderID, err := parseKioskAndOrder(r.URL.Path, "/release")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Physical presence check: verify actual kiosk presence before releasing paid order
	if s.hub != nil && !s.hub.IsConnected(kioskID) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "kiosk is offline; physical presence required to release print job",
			"code":  "PRINTER_OFFLINE",
		})
		return
	}

	// Validate printer condition and stock before releasing paid order
	_, condErr := s.checkPrinterCondition(kioskID)
	if condErr != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(condErr.StatusCode)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": condErr.Message,
			"code":  condErr.Code,
		})
		return
	}

	ord, oErr := s.db.GetOrder(r.Context(), orderID)
	if oErr != nil || ord == nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}
	if polErr := validateOrderAgainstPolicy(ord, s.policy()); polErr != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(polErr.StatusCode)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": polErr.Message,
			"code":  polErr.Code,
		})
		return
	}

	var reqBody struct {
		SessionID string `json:"session_id"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
	}

	reqBody.SessionID = strings.TrimSpace(reqBody.SessionID)
	if reqBody.SessionID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "session_id is required for paid-order release",
		})
		return
	}

	now := time.Now().UTC()
	order, err := s.db.ReleasePaidOrder(r.Context(), kioskID, orderID, reqBody.SessionID, now)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case errors.Is(err, store.ErrSessionNotFound):
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "valid active transaction session required to release paid order",
			})
		case errors.Is(err, store.ErrOrderNotFound):
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "order not found",
			})
		case errors.Is(err, store.ErrKioskMismatch):
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "order does not belong to this kiosk",
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": err.Error(),
			})
		}
		return
	}

	// Dispatch job to kiosk physical printer
	if s.hub != nil {
		if len(order.Documents) > 0 {
			cmds := make([]contracts.PrintJobCommand, 0, len(order.Documents))
			for _, doc := range order.Documents {
				cmds = append(cmds, doc.Command(order.Command.KioskID, order.Command.DisplayCode, len(order.Documents)))
			}
			s.dispatchAuthorizedCommands(r.Context(), "[RELEASE]", cmds)
		} else {
			s.dispatchAuthorizedCommands(r.Context(), "[RELEASE]", []contracts.PrintJobCommand{order.Command})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"order_id": orderID,
		"kiosk_id": kioskID,
		"status":   string(contracts.JobStatusDispatched),
	})
}

// handleRecoveryPIN generates a fresh 4-digit PIN for a paid-but-unprinted order whose original PIN expired.
// Requires verified recovery credential (claim token) delivered at order creation.
// POST /api/orders/{order_id}/recovery-pin (or POST /api/k/{kiosk_id}/orders/{order_id}/recovery-pin)
func (s *appServer) handleRecoveryPIN(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var orderID string
	var kioskID string
	if strings.HasPrefix(r.URL.Path, "/api/k/") {
		if !s.verifyKioskAuth(w, r) {
			return
		}
		kID, oID, err := parseKioskAndOrder(r.URL.Path, "/recovery-pin")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		kioskID = kID
		orderID = oID
	} else {
		trimmed := strings.TrimPrefix(r.URL.Path, "/api/orders/")
		orderID = strings.TrimSuffix(trimmed, "/recovery-pin")
		orderID = strings.TrimSpace(orderID)
	}

	if orderID == "" {
		http.Error(w, "order id required", http.StatusBadRequest)
		return
	}

	// Extract recovery credential from secure headers or request body (never from URL query parameters)
	recoveryToken := strings.TrimSpace(r.Header.Get("X-Recovery-Token"))
	if recoveryToken == "" {
		recoveryToken = strings.TrimSpace(r.Header.Get("X-Claim-Token"))
	}
	if recoveryToken == "" && !strings.HasPrefix(r.URL.Path, "/api/k/") {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			recoveryToken = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
	}

	if r.Body != nil {
		var body struct {
			RecoveryToken string `json:"recovery_token"`
			ClaimToken    string `json:"claim_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if recoveryToken == "" {
			recoveryToken = strings.TrimSpace(body.RecoveryToken)
		}
		if recoveryToken == "" {
			recoveryToken = strings.TrimSpace(body.ClaimToken)
		}
	}

	if recoveryToken == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "unauthorized: recovery credential (X-Recovery-Token header) required",
		})
		return
	}

	order, err := s.db.GetOrder(r.Context(), orderID)
	if err != nil || order == nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}

	if kioskID != "" && order.Command.KioskID != kioskID {
		http.Error(w, "order does not belong to this kiosk", http.StatusForbidden)
		return
	}

	if order.Status != contracts.JobStatusPaidWaiting {
		http.Error(w, fmt.Sprintf("cannot generate recovery PIN: order is in status %s (expected %s)", order.Status, contracts.JobStatusPaidWaiting), http.StatusBadRequest)
		return
	}

	// Generate fresh 4-digit PIN with collision retry
	const maxRetries = 25
	var newPIN string
	// Recovery is only for an already-paid PAID_WAITING order: durable paid entitlement.
	newExpiry := store.PaidPINExpiry()
	renewed := false

	for attempt := 0; attempt < maxRetries; attempt++ {
		code, err := randomDisplayCode()
		if err != nil {
			http.Error(w, "failed to generate pin", http.StatusInternalServerError)
			return
		}
		err = s.db.RenewOrderPIN(r.Context(), orderID, code, recoveryToken, newExpiry)
		if err == nil {
			newPIN = code
			renewed = true
			break
		}
		if errors.Is(err, store.ErrUnauthorized) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "unauthorized: invalid recovery credential",
			})
			return
		}
		if errors.Is(err, store.ErrPINCollision) {
			continue
		}
		http.Error(w, fmt.Sprintf("failed to renew pin: %v", err), http.StatusInternalServerError)
		return
	}

	if !renewed {
		http.Error(w, "failed to reserve pin after maximum retries", http.StatusConflict)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"order_id":       orderID,
		"kiosk_id":       order.Command.KioskID,
		"display_code":   newPIN,
		"pin_expires_at": newExpiry.Format(time.RFC3339),
		"status":         string(contracts.JobStatusPaidWaiting),
	})
}

// handleKioskRefill sends an operator paper refill calibration command to the connected kiosk.
// POST /api/k/{kiosk_id}/refill
func (s *appServer) handleKioskRefill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.verifyKioskAuth(w, r) {
		return
	}

	trimmed := strings.TrimPrefix(r.URL.Path, "/api/k/")
	trimmed = strings.TrimSuffix(trimmed, "/refill")
	kioskID := strings.TrimSpace(trimmed)
	if kioskID == "" {
		http.Error(w, "invalid refill path: missing kiosk id", http.StatusBadRequest)
		return
	}

	if r.Body == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "request body with integer sheets count is required",
			"code":  "INVALID_REFILL_COUNT",
		})
		return
	}

	var raw struct {
		Sheets json.RawMessage `json:"sheets"`
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&raw); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": fmt.Sprintf("malformed refill JSON payload: %v", err),
			"code":  "INVALID_REFILL_COUNT",
		})
		return
	}

	sheetsStr := strings.TrimSpace(string(raw.Sheets))
	if sheetsStr == "" || sheetsStr == "null" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "sheets count is required",
			"code":  "INVALID_REFILL_COUNT",
		})
		return
	}
	sheets, err := strconv.Atoi(sheetsStr)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": fmt.Sprintf("sheets must be an integer, got %s", sheetsStr),
			"code":  "INVALID_REFILL_COUNT",
		})
		return
	}

	if s.hub == nil {
		http.Error(w, "websocket hub not configured", http.StatusInternalServerError)
		return
	}

	_, health, hasHealth, _, _ := s.hub.EvaluateKioskReadiness(kioskID)
	if !hasHealth || health.ConfiguredTrayCapacity <= 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "kiosk configured tray capacity is unavailable; cannot validate refill",
			"code":  "CAPACITY_UNAVAILABLE",
		})
		return
	}
	maxCapacity := contracts.EffectiveTrayCapacity(
		health.ConfiguredTrayCapacity,
		health.InputTrayCapacity,
		health.InputTrayCapacityKnown,
	)
	if sheets < 0 || sheets > maxCapacity {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": fmt.Sprintf("sheets must be between 0 and %d inclusive, got %d", maxCapacity, sheets),
			"code":  "INVALID_REFILL_COUNT",
		})
		return
	}

	refillCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.hub.SendRefill(refillCtx, kioskID, sheets); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": fmt.Sprintf("refill calibration not confirmed by kiosk: %v", err),
			"code":  "REFILL_FAILED",
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":         true,
		"calibrated": true,
		"kiosk_id":   kioskID,
		"sheets":     sheets,
	})
}



