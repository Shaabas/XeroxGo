package main

import (
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"strings"
	"time"

	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

func isJSONRequest(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	contentType := r.Header.Get("Content-Type")
	return strings.Contains(accept, "application/json") || strings.Contains(contentType, "application/json")
}

// handleDemoPayRoutes handles GET and POST requests under /pay/{token}.
func (s *appServer) handleDemoPayRoutes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Accept")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	if s.cfg == nil || !s.cfg.demoMode {
		http.NotFound(w, r)
		return
	}

	token := strings.TrimPrefix(r.URL.Path, "/pay/")
	token = strings.TrimSpace(token)
	if token == "" || strings.Contains(token, "/") {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleDemoCheckoutPage(w, r, token)
	case http.MethodPost:
		s.handleDemoPaymentSubmit(w, r, token)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *appServer) handleDemoCheckoutPage(w http.ResponseWriter, r *http.Request, token string) {
	att, err := s.db.GetPaymentAttemptByToken(r.Context(), token)
	if err != nil || att == nil {
		if isJSONRequest(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "payment link not found or expired"})
			return
		}
		s.renderDemoErrorPage(w, http.StatusNotFound, "Payment Link Not Found", "The payment link you scanned is invalid or has expired. Please check the kiosk screen for an updated QR code.")
		return
	}

	now := time.Now().UTC()
	order, _ := s.db.GetOrder(r.Context(), att.OrderID)
	sess, _ := s.db.GetKioskSession(r.Context(), att.SessionID, now)

	isExpired := now.After(att.ExpiresAt) || att.Status == "EXPIRED"
	isAlreadyPaid := att.Status == "PAID" || (order != nil && (order.Status == contracts.JobStatusPaidWaiting || order.Status == contracts.JobStatusDispatched || order.Status == contracts.JobStatusCompleted))
	isCanceled := sess != nil && (sess.Status == store.KioskSessionCanceled || sess.Status == store.KioskSessionExpired)

	if isAlreadyPaid {
		if isJSONRequest(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":       "already_processed",
				"order_id":     att.OrderID,
				"kiosk_id":     att.KioskID,
				"amount_paise": att.AmountPaise,
			})
			return
		}
		s.renderDemoSuccessPage(w, att, order, "already_processed")
		return
	}

	if isExpired || isCanceled {
		if isJSONRequest(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusGone)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "expired",
				"error":  "Payment session has expired or was canceled",
			})
			return
		}
		s.renderDemoErrorPage(w, http.StatusGone, "Payment Session Expired", "This kiosk payment session has timed out or was canceled. Please start a new session at the kiosk to print your documents.")
		return
	}

	amountRupees := float64(att.AmountPaise) / 100.0
	var totalPages, totalSheets, copies int
	duplexStr := "Single-sided (Simplex)"
	colorModeStr := "Black & White (Mono)"

	if order != nil {
		totalPages = order.TotalPages
		if totalPages <= 0 {
			totalPages = order.Command.TotalPages
		}
		totalSheets = order.TotalSheets
		if totalSheets <= 0 {
			totalSheets = order.Command.TotalSheets
		}
		copies = order.Command.Copies
		if copies <= 0 {
			copies = 1
		}
		if order.Command.Duplex == contracts.DuplexLongEdge || order.Command.Duplex == contracts.DuplexShortEdge {
			duplexStr = "Double-sided (Duplex)"
		}
		if strings.EqualFold(order.ColorMode, "COLOR") {
			colorModeStr = "Full Color"
		}
	}

	if isJSONRequest(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		jsonResp := map[string]interface{}{
			"status":        "ACTIVE",
			"order_id":      att.OrderID,
			"session_id":    att.SessionID,
			"kiosk_id":      att.KioskID,
			"amount_paise":  att.AmountPaise,
			"amount_rupees": fmt.Sprintf("%.2f", amountRupees),
			"total_pages":   totalPages,
			"total_sheets":  totalSheets,
			"copies":        copies,
			"duplex":        duplexStr,
			"color_mode":    colorModeStr,
			"expires_at":    att.ExpiresAt.Format(time.RFC3339),
		}
		if order != nil && len(order.Documents) > 1 {
			jsonResp["documents_count"] = len(order.Documents)
		}
		_ = json.NewEncoder(w).Encode(jsonResp)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	htmlContent := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0, maximum-scale=1.0, user-scalable=no">
  <title>XeroxGo Demo Payment</title>
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      background: #F8FAFC;
      color: #0F172A;
      min-height: 100vh;
      display: flex;
      flex-direction: column;
      align-items: center;
      padding: 24px 16px 40px;
      -webkit-font-smoothing: antialiased;
    }
    .container {
      width: 100%%;
      max-width: 440px;
      margin: 0 auto;
    }
    .brand-header {
      text-align: center;
      margin-bottom: 20px;
    }
    .brand-title {
      font-size: 26px;
      font-weight: 900;
      letter-spacing: -0.5px;
      color: #2563EB;
      display: inline-flex;
      align-items: center;
      gap: 6px;
    }
    .brand-title span { color: #64748B; }
    .demo-badge {
      display: inline-block;
      margin-top: 6px;
      background: #EFF6FF;
      color: #2563EB;
      border: 1px solid #BFDBFE;
      padding: 3px 10px;
      border-radius: 9999px;
      font-size: 11px;
      font-weight: 700;
      text-transform: uppercase;
      letter-spacing: 0.05em;
    }
    .card {
      background: #FFFFFF;
      border: 1px solid #E2E8F0;
      border-radius: 20px;
      padding: 24px;
      box-shadow: 0 4px 20px -2px rgba(15, 23, 42, 0.06);
      margin-bottom: 16px;
    }
    .price-hero {
      background: #EFF6FF;
      border: 1px solid #BFDBFE;
      border-radius: 16px;
      padding: 20px 16px;
      text-align: center;
      margin-bottom: 20px;
    }
    .price-hero-label {
      font-size: 12px;
      font-weight: 700;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: #2563EB;
      margin-bottom: 4px;
    }
    .price-hero-amount {
      font-size: 38px;
      font-weight: 900;
      color: #1E3A8A;
      letter-spacing: -1px;
      line-height: 1.1;
    }
    .price-hero-sub {
      font-size: 12px;
      color: #64748B;
      margin-top: 4px;
    }
    .specs-grid {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 10px;
      margin-bottom: 20px;
    }
    .spec-cell {
      background: #F8FAFC;
      border: 1px solid #F1F5F9;
      border-radius: 12px;
      padding: 10px 12px;
    }
    .spec-label {
      font-size: 11px;
      font-weight: 600;
      color: #64748B;
      text-transform: uppercase;
      letter-spacing: 0.04em;
      margin-bottom: 2px;
    }
    .spec-val {
      font-size: 14px;
      font-weight: 700;
      color: #0F172A;
    }
    .order-meta {
      border-top: 1px solid #F1F5F9;
      padding-top: 14px;
      margin-bottom: 20px;
      font-size: 12px;
      color: #64748B;
      display: flex;
      flex-direction: column;
      gap: 4px;
    }
    .order-meta strong { color: #1E293B; }
    .demo-notice {
      background: #FFFBEB;
      border: 1px solid #FDE68A;
      border-radius: 12px;
      padding: 12px 14px;
      font-size: 12px;
      line-height: 1.45;
      color: #92400E;
      margin-bottom: 20px;
    }
    .demo-notice strong { color: #78350F; }
    .btn-pay {
      display: block;
      width: 100%%;
      padding: 16px 20px;
      background: #2563EB;
      color: #FFFFFF;
      border: none;
      border-radius: 14px;
      font-size: 16px;
      font-weight: 700;
      cursor: pointer;
      box-shadow: 0 4px 12px rgba(37, 99, 235, 0.25);
      transition: background 0.15s ease, transform 0.05s ease;
      text-align: center;
    }
    .btn-pay:hover { background: #1D4ED8; }
    .btn-pay:active { transform: scale(0.99); }
    .btn-pay:disabled {
      opacity: 0.7;
      cursor: not-allowed;
      transform: none;
    }
    .status-alert {
      margin-top: 14px;
      padding: 14px 16px;
      border-radius: 12px;
      font-size: 13px;
      line-height: 1.4;
      display: none;
    }
    .status-alert.success {
      display: block;
      background: #ECFDF5;
      border: 1px solid #A7F3D0;
      color: #065F46;
    }
    .status-alert.error {
      display: block;
      background: #FEF2F2;
      border: 1px solid #FECACA;
      color: #991B1B;
    }
    .footer {
      text-align: center;
      font-size: 12px;
      color: #94A3B8;
      margin-top: 8px;
    }
  </style>
</head>
<body>
  <div class="container">
    <div class="brand-header">
      <div class="brand-title"><span>Xerox</span>Go</div>
      <div><span class="demo-badge">Demo Mode Simulation</span></div>
    </div>

    <div class="card" id="checkout-card">
      <div class="price-hero">
        <div class="price-hero-label">Total Amount Due</div>
        <div class="price-hero-amount">₹%.2f</div>
        <div class="price-hero-sub">incl. of all taxes</div>
      </div>

      <div class="specs-grid">
        <div class="spec-cell">
          <div class="spec-label">Document Pages</div>
          <div class="spec-val">%d pages</div>
        </div>
        <div class="spec-cell">
          <div class="spec-label">Sheets to Print</div>
          <div class="spec-val">%d sheets</div>
        </div>
        <div class="spec-cell">
          <div class="spec-label">Copies</div>
          <div class="spec-val">%d</div>
        </div>
        <div class="spec-cell">
          <div class="spec-label">Sides</div>
          <div class="spec-val">%s</div>
        </div>
      </div>

      <div class="order-meta">
        <div>Order ID: <strong>%s</strong></div>
        <div>Target Kiosk: <strong>%s</strong></div>
      </div>

      <div class="demo-notice">
        <strong>Demo Mode Notice:</strong> This is a simulation payment flow. No real funds will be deducted. Tapping below verifies payment and triggers immediate printing at the kiosk.
      </div>

      <form id="pay-form" method="POST" action="/pay/%s" onsubmit="handlePaySubmit(event)">
        <button type="submit" id="pay-btn" class="btn-pay">Pay ₹%.2f (Demo Mode)</button>
      </form>
      <div id="status-msg" class="status-alert"></div>
    </div>

    <div class="footer">
      XeroxGo Automated Kiosk System &bull; Demo Simulation
    </div>
  </div>

  <script>
    async function handlePaySubmit(e) {
      e.preventDefault();
      const btn = document.getElementById('pay-btn');
      const msg = document.getElementById('status-msg');

      btn.disabled = true;
      btn.innerText = 'Processing Demo Payment...';
      msg.className = 'status-alert';
      msg.style.display = 'none';

      try {
        const resp = await fetch('/pay/%s', {
          method: 'POST',
          headers: {
            'Accept': 'application/json',
            'Content-Type': 'application/json'
          }
        });

        const data = await resp.json().catch(() => ({}));
        if (resp.ok) {
          btn.innerText = 'Payment Successful!';
          btn.style.background = '#10B981';
          msg.className = 'status-alert success';
          msg.innerHTML = '<strong>Success!</strong> Payment verified. Your documents are printing at kiosk <strong>%s</strong>.';
          msg.style.display = 'block';
        } else {
          btn.disabled = false;
          btn.innerText = 'Pay ₹%.2f (Demo Mode)';
          msg.className = 'status-alert error';
          msg.innerHTML = '<strong>Payment Error:</strong> ' + (data.error || 'Payment failed. Please try again.');
          msg.style.display = 'block';
        }
      } catch (err) {
        btn.disabled = false;
        btn.innerText = 'Pay ₹%.2f (Demo Mode)';
        msg.className = 'status-alert error';
        msg.innerHTML = '<strong>Network Error:</strong> Failed to connect to server. Please try again.';
        msg.style.display = 'block';
      }
    }
  </script>
</body>
</html>`,
		amountRupees,
		totalPages,
		totalSheets,
		copies,
		html.EscapeString(duplexStr),
		html.EscapeString(att.OrderID),
		html.EscapeString(att.KioskID),
		token,
		amountRupees,
		token,
		html.EscapeString(att.KioskID),
		amountRupees,
		amountRupees,
	)

	_, _ = w.Write([]byte(htmlContent))
}

func (s *appServer) handleDemoPaymentSubmit(w http.ResponseWriter, r *http.Request, token string) {
	att, err := s.db.GetPaymentAttemptByToken(r.Context(), token)
	if err != nil || att == nil {
		if isJSONRequest(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "payment attempt not found"})
			return
		}
		s.renderDemoErrorPage(w, http.StatusNotFound, "Payment Not Found", "The payment attempt could not be found.")
		return
	}

	now := time.Now().UTC()
	if att.AmountPaise <= 0 {
		if isJSONRequest(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "payment amount must be greater than zero"})
			return
		}
		s.renderDemoErrorPage(w, http.StatusBadRequest, "Invalid Payment", "Payment amount must be greater than zero.")
		return
	}
	if now.After(att.ExpiresAt) {
		if isJSONRequest(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusGone)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "payment session has expired"})
			return
		}
		s.renderDemoErrorPage(w, http.StatusGone, "Payment Expired", "This payment session has timed out. Please generate a new session at the kiosk.")
		return
	}

	order, _ := s.db.GetOrder(r.Context(), att.OrderID)

	// Idempotency: if already marked paid, return success immediately
	if att.Status == "PAID" {
		if isJSONRequest(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":   "already_processed",
				"ok":       true,
				"order_id": att.OrderID,
				"kiosk_id": att.KioskID,
			})
			return
		}
		s.renderDemoSuccessPage(w, att, order, "already_processed")
		return
	}

	// Capture payment atomically
	captureRes, err := s.db.CapturePaymentAndCheckDispatch(r.Context(), store.PaymentRecord{
		PaymentID:   "pay_demo_" + att.AttemptID,
		OrderID:     att.OrderID,
		KioskID:     att.KioskID,
		SessionID:   att.SessionID,
		AmountPaise: att.AmountPaise,
		Provider:    "mock_demo",
		Status:      "captured",
	}, now)
	if err != nil {
		log.Printf("[DEMO-PAY] Capture error for attempt %s: %v", att.AttemptID, err)
		if isJSONRequest(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		s.renderDemoErrorPage(w, http.StatusBadRequest, "Payment Failed", err.Error())
		return
	}

	_ = s.db.MarkPaymentAttemptPaid(r.Context(), att.AttemptID, "pay_demo_"+att.AttemptID)

	if captureRes.AlreadyProcessed {
		if isJSONRequest(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":   "already_processed",
				"ok":       true,
				"order_id": att.OrderID,
				"kiosk_id": att.KioskID,
			})
			return
		}
		s.renderDemoSuccessPage(w, att, order, "already_processed")
		return
	}

	// Physical Print Authorization Gate
	if captureRes.DispatchEligible {
		log.Printf("[DEMO-PAY] Order %s authorized for printing (session %s)", att.OrderID, captureRes.ActiveSessionID)
		if s.hub != nil {
			if len(captureRes.Commands) > 0 {
				for _, cmd := range captureRes.Commands {
					if err := s.hub.SendJob(r.Context(), cmd); err != nil {
						log.Printf("[DEMO-PAY] Failed to send print job to hub for kiosk %s: %v", cmd.KioskID, err)
						break
					}
					_ = s.db.MarkDispatchIntentSent(r.Context(), cmd.JobID)
				}
			} else if captureRes.Command != nil {
				if err := s.hub.SendJob(r.Context(), *captureRes.Command); err != nil {
					log.Printf("[DEMO-PAY] Failed to send print job to hub for kiosk %s: %v", captureRes.KioskID, err)
				} else {
					_ = s.db.MarkDispatchIntentSent(r.Context(), captureRes.Command.JobID)
				}
			}
		}

		if isJSONRequest(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":   "dispatched",
				"ok":       true,
				"order_id": att.OrderID,
				"kiosk_id": att.KioskID,
			})
			return
		}
		s.renderDemoSuccessPage(w, att, order, "dispatched")
		return
	}

	// Absent/expired kiosk session: held in PAID_WAITING
	log.Printf("[DEMO-PAY] Payment captured for order %s, but session expired/canceled. Preserved in PAID_WAITING.", att.OrderID)
	if isJSONRequest(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "paid_unprinted",
			"ok":       true,
			"order_id": att.OrderID,
			"kiosk_id": att.KioskID,
		})
		return
	}
	s.renderDemoSuccessPage(w, att, order, "paid_unprinted")
}

func (s *appServer) renderDemoSuccessPage(w http.ResponseWriter, att *store.PaymentAttempt, order *store.Order, status string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	amountRupees := float64(att.AmountPaise) / 100.0
	headline := "Payment Successful!"
	msg := fmt.Sprintf("Your payment of <strong>₹%.2f</strong> was verified. The kiosk is now printing your document.", amountRupees)
	badge := "PRINTING IN PROGRESS"
	badgeColor := "#065F46"
	badgeBg := "#ECFDF5"

	if status == "paid_unprinted" {
		headline = "Payment Captured"
		msg = fmt.Sprintf("Your payment of <strong>₹%.2f</strong> was captured, but the kiosk session timed out. Please enter your PIN at Kiosk <strong>%s</strong> to release the print job.", amountRupees, html.EscapeString(att.KioskID))
		badge = "PAID - AWAITING KIOSK CLAIM"
		badgeColor = "#92400E"
		badgeBg = "#FEF3C7"
	} else if status == "already_processed" {
		headline = "Payment Already Received"
		msg = fmt.Sprintf("This order has already been paid (<strong>₹%.2f</strong>) and sent to the kiosk for printing.", amountRupees)
		badge = "COMPLETED"
	}

	htmlContent := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0, maximum-scale=1.0, user-scalable=no">
  <title>Payment Success - XeroxGo</title>
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      background: #F8FAFC;
      color: #0F172A;
      min-height: 100vh;
      display: flex;
      flex-direction: column;
      align-items: center;
      padding: 32px 16px;
      -webkit-font-smoothing: antialiased;
    }
    .container { width: 100%%; max-width: 440px; margin: 0 auto; }
    .card {
      background: #FFFFFF;
      border: 1px solid #E2E8F0;
      border-radius: 20px;
      padding: 36px 24px;
      text-align: center;
      box-shadow: 0 4px 20px -2px rgba(15, 23, 42, 0.06);
    }
    .icon {
      width: 64px;
      height: 64px;
      background: #ECFDF5;
      color: #10B981;
      border-radius: 50%%;
      display: flex;
      align-items: center;
      justify-content: center;
      font-size: 32px;
      font-weight: 900;
      margin: 0 auto 16px auto;
    }
    .badge {
      display: inline-block;
      background: %s;
      color: %s;
      font-size: 11px;
      font-weight: 800;
      padding: 4px 12px;
      border-radius: 9999px;
      margin-bottom: 12px;
      text-transform: uppercase;
      letter-spacing: 0.05em;
    }
    .title { font-size: 22px; font-weight: 800; color: #0F172A; margin-bottom: 12px; }
    .desc { font-size: 14px; color: #475569; line-height: 1.5; margin-bottom: 24px; }
    .details {
      background: #F8FAFC;
      border: 1px solid #E2E8F0;
      border-radius: 12px;
      padding: 14px;
      text-align: left;
      font-size: 13px;
      color: #64748B;
    }
    .details div { margin-bottom: 4px; }
    .details strong { color: #1E293B; }
  </style>
</head>
<body>
  <div class="container">
    <div class="card">
      <div class="icon">&#10003;</div>
      <div class="badge">%s</div>
      <h1 class="title">%s</h1>
      <p class="desc">%s</p>

      <div class="details">
        <div>Order ID: <strong>%s</strong></div>
        <div>Target Kiosk: <strong>%s</strong></div>
        <div>Amount Paid: <strong>₹%.2f</strong></div>
      </div>
    </div>
  </div>
</body>
</html>`,
		badgeBg, badgeColor,
		badge,
		headline,
		msg,
		html.EscapeString(att.OrderID),
		html.EscapeString(att.KioskID),
		amountRupees,
	)

	_, _ = w.Write([]byte(htmlContent))
}

func (s *appServer) renderDemoErrorPage(w http.ResponseWriter, code int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)

	htmlContent := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0, maximum-scale=1.0, user-scalable=no">
  <title>%s - XeroxGo</title>
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      background: #F8FAFC;
      color: #0F172A;
      min-height: 100vh;
      display: flex;
      flex-direction: column;
      align-items: center;
      padding: 32px 16px;
      -webkit-font-smoothing: antialiased;
    }
    .container { width: 100%%; max-width: 440px; margin: 0 auto; }
    .card {
      background: #FFFFFF;
      border: 1px solid #FECACA;
      border-radius: 20px;
      padding: 36px 24px;
      text-align: center;
      box-shadow: 0 4px 20px -2px rgba(15, 23, 42, 0.06);
    }
    .icon {
      width: 64px;
      height: 64px;
      background: #FEF2F2;
      color: #DC2626;
      border-radius: 50%%;
      display: flex;
      align-items: center;
      justify-content: center;
      font-size: 28px;
      font-weight: 900;
      margin: 0 auto 16px auto;
    }
    .title { font-size: 20px; font-weight: 800; color: #991B1B; margin-bottom: 12px; }
    .desc { font-size: 14px; color: #475569; line-height: 1.5; }
  </style>
</head>
<body>
  <div class="container">
    <div class="card">
      <div class="icon">!</div>
      <h1 class="title">%s</h1>
      <p class="desc">%s</p>
    </div>
  </div>
</body>
</html>`,
		html.EscapeString(title),
		html.EscapeString(title),
		html.EscapeString(message),
	)

	_, _ = w.Write([]byte(htmlContent))
}
