package main

// These test-only capture fixtures inject synthetic events into the canonical
// store/dispatch transaction. Neither endpoint exists in the production binary.
import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"xeroxgo/cloud-backend/internal/payments"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

func (s *appServer) handleFixtureSimulatePayment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !s.cfg.devMode {
		http.Error(w, "payment simulation only available in development mode", http.StatusForbidden)
		return
	}
	if s.cfg.demoMode && r.URL.Query().Get("force") != "1" && r.URL.Query().Get("bypass") != "1" {
		http.Error(w, "direct order simulation is disabled in demo mode; use secure /pay/{token} checkout (or append ?force=1 to bypass)", http.StatusForbidden)
		return
	}

	prefix := "/api/orders/"
	suffix := "/test/simulate-payment"
	if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, suffix) {
		http.NotFound(w, r)
		return
	}
	orderID := strings.TrimPrefix(r.URL.Path, prefix)
	orderID = strings.TrimSuffix(orderID, suffix)
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		http.Error(w, "order id required", http.StatusBadRequest)
		return
	}

	order, err := s.db.GetOrder(r.Context(), orderID)
	if err != nil || order == nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}

	if order.Status == contracts.JobStatusCompleted ||
		order.Status == contracts.JobStatusPrinting ||
		order.Status == contracts.JobStatusSpooling ||
		order.Status == contracts.JobStatusDispatched {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"order_id": orderID,
			"status":   string(order.Status),
		})
		return
	}

	if order.Status == contracts.JobStatusPendingPayment && !order.PINExpiresAt.IsZero() && !time.Now().UTC().Before(order.PINExpiresAt) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"order_id": orderID,
			"status":   "EXPIRED",
			"error":    "order has expired",
		})
		return
	}

	syntheticPayload := map[string]interface{}{
		"event": "payment.captured",
		"payload": map[string]interface{}{
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_sim_" + orderID,
					"order_id": orderID,
					"notes": map[string]interface{}{
						"order_id": orderID,
					},
					"status": "captured",
					"amount": order.PricePaise,
				},
			},
		},
	}
	rawPayload, err := json.Marshal(syntheticPayload)
	if err != nil {
		http.Error(w, "failed to marshal payload", http.StatusInternalServerError)
		return
	}

	mac := hmac.New(sha256.New, s.cfg.webhookSecret)
	mac.Write(rawPayload)
	sig := hex.EncodeToString(mac.Sum(nil))

	if !payments.VerifyWebhookSignature(rawPayload, sig, string(s.cfg.webhookSecret)) {
		http.Error(w, "internal signature verification failed", http.StatusInternalServerError)
		return
	}

	requireSession := r.URL.Query().Get("require_session") == "1" || r.URL.Query().Get("strict_session") == "1"
	now := time.Now().UTC()

	var simSessionID string
	// In development simulation: if strict session is not required and no active session exists,
	// auto-create a session so automated integration tests run end-to-end.
	if !requireSession {
		activeSess, _ := s.db.GetActiveKioskSession(r.Context(), order.Command.KioskID, orderID, now)
		if activeSess == nil {
			activeSess, _ = s.db.CreateOrGetKioskSession(r.Context(), order.Command.KioskID, orderID, 5*time.Minute, now)
		}
		if activeSess != nil {
			simSessionID = activeSess.SessionID
		}
	} else {
		if activeSess, _ := s.db.GetActiveKioskSession(r.Context(), order.Command.KioskID, orderID, now); activeSess != nil {
			simSessionID = activeSess.SessionID
		}
	}
	if sessParam := strings.TrimSpace(r.URL.Query().Get("session_id")); sessParam != "" {
		simSessionID = sessParam
	}

	captureRes, err := s.db.CapturePaymentAndCheckDispatch(r.Context(), store.PaymentRecord{
		PaymentID:   "pay_sim_" + orderID,
		OrderID:     orderID,
		KioskID:     order.Command.KioskID,
		SessionID:   simSessionID,
		AmountPaise: order.PricePaise,
		Provider:    "simulate",
		Status:      "captured",
	}, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if captureRes.DispatchEligible {
		commands := captureRes.Commands
		if len(commands) == 0 && captureRes.Command != nil {
			commands = []contracts.PrintJobCommand{*captureRes.Command}
		}
		if err := s.deliverAuthorizedJobs(r.Context(), orderID, commands); err != nil {
			log.Printf("[SIMULATION] Order %s paid but delivery unresolved: %v", orderID, err)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"order_id": orderID,
				"status":   "DISPATCH_PENDING",
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"order_id": orderID,
			"status":   string(contracts.JobStatusDispatched),
		})
		return
	}

	// No session or not dispatch eligible
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"order_id": orderID,
		"status":   string(captureRes.OrderStatus),
	})
}


func (s *appServer) handleFixtureCapture(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	const maxWebhookSize = 1024 * 1024 // 1 MiB
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookSize)
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read test capture event body", http.StatusBadRequest)
		return
	}

	sig := r.Header.Get("X-Fixture-Signature")
	prov := s.getPaymentProvider()
	if sig == "" || !prov.VerifyWebhookSignature(rawBody, sig) {
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	var parsed struct {
		Event   string `json:"event"`
		Payload struct {
			Payment struct {
				Entity struct {
					ID       string `json:"id"`
					OrderID  string `json:"order_id"`
					Amount   int64  `json:"amount"`
					Currency string `json:"currency"`
					Status   string `json:"status"`
					Notes    struct {
						OrderID   string `json:"order_id"`
						SessionID string `json:"session_id"`
						KioskID   string `json:"kiosk_id"`
					} `json:"notes"`
				} `json:"entity"`
			} `json:"payment"`
		} `json:"payload"`
	}

	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		http.Error(w, "malformed test capture event json", http.StatusBadRequest)
		return
	}

	if parsed.Event != "payment.captured" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ignored"}`))
		return
	}

	entity := parsed.Payload.Payment.Entity
	if entity.ID == "" {
		http.Error(w, "payment id missing in payment entity", http.StatusBadRequest)
		return
	}
	if entity.Status != "captured" {
		http.Error(w, "payment status is not captured", http.StatusBadRequest)
		return
	}
	if entity.Currency != "" && strings.ToUpper(strings.TrimSpace(entity.Currency)) != "INR" {
		http.Error(w, "invalid currency: only INR is supported", http.StatusBadRequest)
		return
	}

	// Stored attempt association (Requirement 7 & 8: never trust notes alone as proof of transaction ownership)
	var attempt *store.PaymentAttempt
	providerOrderID := strings.TrimSpace(entity.OrderID)
	notesOrderID := strings.TrimSpace(entity.Notes.OrderID)
	notesSessionID := strings.TrimSpace(entity.Notes.SessionID)

	if providerOrderID != "" {
		att, err := s.db.GetPaymentAttemptByProviderOrder(r.Context(), "", providerOrderID)
		if err == nil && att != nil {
			attempt = att
		}
	}
	if attempt == nil {
		lookupOrderID := notesOrderID
		if lookupOrderID == "" {
			lookupOrderID = providerOrderID
		}
		if lookupOrderID != "" {
			att, err := s.db.GetPaymentAttemptByOrderAndSession(r.Context(), lookupOrderID, notesSessionID)
			if err == nil && att != nil {
				attempt = att
			}
		}
	}

	var (
		orderID      string
		sessionID    string
		kioskID      string
		amount       int64
		providerName string
	)

	if attempt != nil {
		// Strictly enforce attempt binding
		if entity.Amount <= 0 {
			http.Error(w, "payment amount must be greater than zero", http.StatusBadRequest)
			return
		}
		if entity.Amount != attempt.AmountPaise {
			// Payment was captured by the provider for a known attempt.
			// Never discard actual money: persist as refund-only, never print.
			log.Printf("[PAYMENT] Captured payment %s has unexpected amount for attempt %s; refund-only accounting required", entity.ID, attempt.AttemptID)
		}
		if entity.Currency != "" && attempt.Currency != "" && !strings.EqualFold(entity.Currency, attempt.Currency) {
			http.Error(w, "payment currency mismatch with stored attempt", http.StatusBadRequest)
			return
		}
		orderID = attempt.OrderID
		kioskID = attempt.KioskID
		sessionID = attempt.SessionID
		amount = entity.Amount
		providerName = attempt.Provider
	} else {
		// Fallback for orders without an explicit attempt:
		// Requirement 8: Never trust payment notes alone as proof of transaction ownership!
		// We do NOT trust notes.SessionID to authorize physical dispatch.
		lookupOrderID := notesOrderID
		if lookupOrderID == "" {
			lookupOrderID = providerOrderID
		}
		if lookupOrderID == "" {
			http.Error(w, "unassociated payment: order id missing", http.StatusBadRequest)
			return
		}
		order, err := s.db.GetOrder(r.Context(), lookupOrderID)
		if err != nil || order == nil {
			http.Error(w, "unassociated payment attempt: no matching stored attempt or order found", http.StatusBadRequest)
			return
		}
		if entity.Amount <= 0 {
			http.Error(w, "payment amount must be greater than zero", http.StatusBadRequest)
			return
		}
		if entity.Amount != order.PricePaise {
			http.Error(w, fmt.Sprintf("payment amount mismatch: expected %d paise, got %d paise", order.PricePaise, entity.Amount), http.StatusBadRequest)
			return
		}
		orderID = order.OrderID
		kioskID = order.Command.KioskID
		now := time.Now().UTC()
		if notesSessionID != "" {
			sess, sErr := s.db.GetKioskSession(r.Context(), notesSessionID, now)
			if sErr == nil && sess != nil && sess.OrderID == orderID && sess.Status == store.KioskSessionPaymentPending && sess.ExpiresAt.After(now) {
				sessionID = sess.SessionID
			}
		}
		amount = order.PricePaise
		providerName = "mock"
	}
	if providerName == "" {
		providerName = "mock"
	}

	eventID := entity.ID
	now := time.Now().UTC()
	captureRes, err := s.db.CapturePaymentAndCheckDispatch(r.Context(), store.PaymentRecord{
		PaymentID:   eventID,
		OrderID:     orderID,
		KioskID:     kioskID,
		SessionID:   sessionID,
		AmountPaise: amount,
		Provider:    providerName,
		Status:      entity.Status,
	}, now)
	if err != nil {
		if errors.Is(err, store.ErrOrderNotFound) {
			http.Error(w, "order not found", http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if attempt != nil && !captureRes.ExcessCapture {
		// A refund-only capture (wrong amount or additional payment) must
		// never mark the original checkout attempt as the funding payment.
		_ = s.db.MarkPaymentAttemptPaid(r.Context(), attempt.AttemptID, eventID)
	}

	if captureRes.ExcessCapture {
		// A second distinct captured payment never authorizes additional printing.
		// Its independent refund obligation committed with the payment record.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"excess_payment_refund_required"}`))
		return
	}
	if captureRes.AlreadyProcessed {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"already_processed"}`))
		return
	}

	// A verified payment creates durable per-document intents in the same
	// transaction as capture. WebSocket transport errors are NOT evidence
	// of printing failure or a safe physical stop. Preserve the paid order,
	// reservation, and intents; reconnect retries the same immutable JobIDs.
	if captureRes.DispatchEligible {
		log.Printf("[PAYMENT] Order %s authorized for print (active session %s)", orderID, captureRes.ActiveSessionID)
		commands := captureRes.Commands
		if len(commands) == 0 && captureRes.Command != nil {
			commands = []contracts.PrintJobCommand{*captureRes.Command}
		}
		if err := s.deliverAuthorizedJobs(r.Context(), orderID, commands); err != nil {
			// Captured money and per-document dispatch intents are durable.
			// A network write error cannot prove the printer was stopped, and
			// cannot justify blocking the kiosk or refunding the payment.
			log.Printf("[PAYMENT] Order %s delivery unresolved; retry on authenticated Pi reconnect: %v", orderID, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"dispatch_pending"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"dispatched"}`))
		return
	}

	// Late payment or absent active kiosk session: money captured in PAID_WAITING without unattended printing
	log.Printf("[PAYMENT] Payment captured for order %s, but no active kiosk session authorizes printing. Preserving in PAID_WAITING.", orderID)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"paid_unprinted"}`))
}

