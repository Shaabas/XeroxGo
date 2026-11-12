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

	// Active valid payment attempt: render mobile checkout page
	amountRupees := float64(att.AmountPaise) / 100.0
	var totalPages, totalSheets, copies int
	duplexStr := "Single-sided (Simplex)"
	colorModeStr := "Black & White (Mono)"

	if order != nil {
		totalPages = order.Command.TotalPages
		totalSheets = order.Command.TotalSheets
		copies = order.Command.Copies
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
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
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
		})
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	htmlContent := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>XeroxGo Demo Checkout</title>
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
      background: #F8FAFC;
      color: #1E293B;
      min-height: 100vh;
      display: flex;
      flex-direction: column;
      align-items: center;
      padding: 16px;
    }
    .container {
      width: 100%%;
      max-width: 480px;
      margin: 0 auto;
    }
    .header {
      text-align: center;
      margin-bottom: 20px;
      padding-top: 8px;
    }
    .logo-badge {
      display: inline-flex;
      align-items: center;
      gap: 8px;
      background: #EFF6FF;
      border: 1px solid #BFDBFE;
      padding: 6px 14px;
      border-radius: 9999px;
      font-size: 13px;
      font-weight: 700;
      color: #2563EB;
      text-transform: uppercase;
      letter-spacing: 0.5px;
      margin-bottom: 12px;
    }
    .demo-tag {
      background: #FEF3C7;
      color: #B45309;
      border: 1px solid #FDE68A;
      padding: 2px 8px;
      border-radius: 6px;
      font-size: 11px;
      font-weight: 800;
    }
    .title {
      font-size: 24px;
      font-weight: 800;
      color: #0F172A;
      line-height: 1.2;
    }
    .card {
      background: #FFFFFF;
      border: 1px solid #E2E8F0;
      border-radius: 16px;
      box-shadow: 0 4px 6px -1px rgba(0, 0, 0, 0.05), 0 2px 4px -2px rgba(0, 0, 0, 0.05);
      padding: 24px;
      margin-bottom: 16px;
    }
    .price-box {
      background: #EFF6FF;
      border: 1.5px solid #BFDBFE;
      border-radius: 12px;
      padding: 18px;
      text-align: center;
      margin-bottom: 20px;
    }
    .price-label {
      font-size: 13px;
      font-weight: 600;
      color: #64748B;
      text-transform: uppercase;
      letter-spacing: 0.5px;
      margin-bottom: 4px;
    }
    .price-val {
      font-size: 38px;
      font-weight: 900;
      color: #2563EB;
      letter-spacing: -0.5px;
    }
    .specs-grid {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 12px;
      margin-bottom: 20px;
    }
    .spec-item {
      background: #F8FAFC;
      border: 1px solid #F1F5F9;
      border-radius: 10px;
      padding: 10px 12px;
    }
    .spec-label {
      font-size: 11px;
      font-weight: 600;
      color: #64748B;
      text-transform: uppercase;
      margin-bottom: 2px;
    }
    .spec-val {
      font-size: 14px;
      font-weight: 700;
      color: #1E293B;
    }
    .order-meta {
      border-top: 1px solid #E2E8F0;
      padding-top: 14px;
      margin-bottom: 20px;
      font-size: 12px;
      color: #64748B;
      display: flex;
      flex-direction: column;
      gap: 4px;
    }
    .disclaimer {
      background: #FFFBEB;
      border: 1px solid #FDE68A;
      border-radius: 10px;
      padding: 12px;
      font-size: 12px;
      color: #92400E;
      line-height: 1.4;
      margin-bottom: 20px;
    }
    .btn-pay {
      display: block;
      width: 100%%;
      background: #2563EB;
      color: #FFFFFF;
      font-size: 17px;
      font-weight: 700;
      padding: 16px 20px;
      border: none;
      border-radius: 12px;
      cursor: pointer;
      text-align: center;
      transition: background 0.15s ease, transform 0.05s ease;
      box-shadow: 0 4px 12px rgba(37, 99, 235, 0.25);
    }
    .btn-pay:hover { background: #1D4ED8; }
    .btn-pay:active { transform: scale(0.98); }
    .btn-pay:disabled {
      background: #94A3B8;
      cursor: not-allowed;
      box-shadow: none;
      transform: none;
    }
    .footer {
      text-align: center;
      font-size: 12px;
      color: #94A3B8;
      margin-top: 16px;
    }
    #status-msg {
      margin-top: 14px;
      padding: 12px;
      border-radius: 10px;
      font-size: 13px;
      font-weight: 600;
      display: none;
      text-align: center;
    }
    .msg-success {
      background: #ECFDF5;
      color: #065F46;
      border: 1px solid #A7F3D0;
    }
    .msg-error {
      background: #FEF2F2;
      color: #991B1B;
      border: 1px solid #FECACA;
    }
  </style>
</head>
<body>
  <div class="container">
    <div class="header">
      <div class="logo-badge">
        <span>XeroxGo</span>
        <span class="demo-tag">SIMULATED PAYMENT</span>
      </div>
      <h1 class="title">Complete Payment</h1>
    </div>

    <div class="card">
      <div class="price-box">
        <div class="price-label">Authoritative Amount Due</div>
        <div class="price-val">₹%.2f</div>
      </div>

      <div class="specs-grid">
        <div class="spec-item">
          <div class="spec-label">Document Pages</div>
          <div class="spec-val">%d pages</div>
        </div>
        <div class="spec-item">
          <div class="spec-label">Sheets to Print</div>
          <div class="spec-val">%d sheets</div>
        </div>
        <div class="spec-item">
          <div class="spec-label">Copies</div>
          <div class="spec-val">%d</div>
        </div>
        <div class="spec-item">
          <div class="spec-label">Sides</div>
          <div class="spec-val">%s</div>
        </div>
      </div>

      <div class="order-meta">
        <div><strong>Order ID:</strong> %s</div>
        <div><strong>Target Kiosk:</strong> %s</div>
      </div>

      <div class="disclaimer">
        <strong>Demo Mode Notice:</strong> This is a demonstration payment flow. No real funds will be deducted. Clicking the button below simulates instant payment verification and starts printing at the kiosk.
      </div>

      <form id="pay-form" method="POST" action="/pay/%s" onsubmit="handlePaySubmit(event)">
        <button type="submit" id="pay-btn" class="btn-pay">Pay ₹%.2f (Demo Mode)</button>
      </form>
      <div id="status-msg"></div>
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
          msg.className = 'msg-success';
          msg.innerHTML = '<strong>Success!</strong> Payment verified. Your documents are printing at kiosk <strong>%s</strong>.';
          msg.style.display = 'block';
        } else {
          btn.disabled = false;
          btn.innerText = 'Pay ₹%.2f (Demo Mode)';
          msg.className = 'msg-error';
          msg.innerHTML = '<strong>Error:</strong> ' + (data.error || 'Payment failed. Please try again.');
          msg.style.display = 'block';
        }
      } catch (err) {
        btn.disabled = false;
        btn.innerText = 'Pay ₹%.2f (Demo Mode)';
        msg.className = 'msg-error';
        msg.innerHTML = '<strong>Network Error:</strong> Failed to connect. Please try again.';
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
	if captureRes.DispatchEligible && captureRes.Command != nil {
		log.Printf("[DEMO-PAY] Order %s authorized for printing (session %s)", att.OrderID, captureRes.ActiveSessionID)
		if s.hub != nil {
			if err := s.hub.SendJob(r.Context(), *captureRes.Command); err != nil {
				log.Printf("[DEMO-PAY] Failed to send print job to hub for kiosk %s: %v", captureRes.KioskID, err)
			} else {
				_ = s.db.MarkDispatchIntentSent(r.Context(), att.OrderID)
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
	headline := "Payment Verified & Printing"
	msg := fmt.Sprintf("Your payment of <strong>₹%.2f</strong> was successfully verified. The kiosk is now printing your documents.", amountRupees)
	badge := "PRINTING IN PROGRESS"
	badgeColor := "#10B981"
	badgeBg := "#ECFDF5"

	if status == "paid_unprinted" {
		headline = "Payment Captured (Session Expired)"
		msg = fmt.Sprintf("Your payment of <strong>₹%.2f</strong> was captured, but the kiosk session expired. Please use your PIN or recovery token at Kiosk <strong>%s</strong> to release the print job.", amountRupees, html.EscapeString(att.KioskID))
		badge = "PAID - AWAITING KIOSK CLAIM"
		badgeColor = "#D97706"
		badgeBg = "#FEF3C7"
	} else if status == "already_processed" {
		headline = "Payment Already Received"
		msg = fmt.Sprintf("This order has already been paid (<strong>₹%.2f</strong>) and sent for printing.", amountRupees)
		badge = "COMPLETED"
	}

	htmlContent := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Payment Success - XeroxGo</title>
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      background: #F8FAFC;
      color: #1E293B;
      min-height: 100vh;
      display: flex;
      flex-direction: column;
      align-items: center;
      padding: 24px 16px;
    }
    .container { width: 100%%; max-width: 480px; margin: 0 auto; }
    .card {
      background: #FFFFFF;
      border: 1px solid #E2E8F0;
      border-radius: 20px;
      padding: 32px 24px;
      text-align: center;
      box-shadow: 0 4px 6px -1px rgba(0,0,0,0.05);
    }
    .icon {
      width: 64px;
      height: 64px;
      background: %s;
      color: %s;
      border-radius: 50%%;
      display: flex;
      align-items: center;
      justify-content: center;
      font-size: 32px;
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
      letter-spacing: 0.5px;
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
      margin-bottom: 20px;
    }
    .details div { margin-bottom: 4px; }
    .details strong { color: #1E293B; }
  </style>
</head>
<body>
  <div class="container">
    <div class="card">
      <div class="icon">✓</div>
      <div class="badge">%s</div>
      <h1 class="title">%s</h1>
      <p class="desc">%s</p>

      <div class="details">
        <div><strong>Order ID:</strong> %s</div>
        <div><strong>Kiosk ID:</strong> %s</div>
        <div><strong>Amount:</strong> ₹%.2f</div>
      </div>
    </div>
  </div>
</body>
</html>`,
		badgeBg, badgeColor,
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
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>%s - XeroxGo</title>
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      background: #F8FAFC;
      color: #1E293B;
      min-height: 100vh;
      display: flex;
      flex-direction: column;
      align-items: center;
      padding: 24px 16px;
    }
    .container { width: 100%%; max-width: 480px; margin: 0 auto; }
    .card {
      background: #FFFFFF;
      border: 1px solid #FECACA;
      border-radius: 20px;
      padding: 32px 24px;
      text-align: center;
      box-shadow: 0 4px 6px -1px rgba(0,0,0,0.05);
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
      font-size: 32px;
      margin: 0 auto 16px auto;
    }
    .title { font-size: 22px; font-weight: 800; color: #991B1B; margin-bottom: 12px; }
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
