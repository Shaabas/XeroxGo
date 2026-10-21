package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"xeroxgo/cloud-backend/internal/payments"
	"xeroxgo/cloud-backend/internal/pdf"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/cloud-backend/internal/ws"
	"xeroxgo/pkg/contracts"
)

type serverConfig struct {
	addr               string
	dbPath             string
	jobsDir            string
	pricePerSheetPaise int64
	devMode            bool
	downloadSecret     []byte
	webhookSecret      []byte
}

type appServer struct {
	cfg *serverConfig
	db  *store.Store
	hub *ws.Hub
}

const portalHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>XeroxGo — Mobile Print Portal</title>
  <style>
    :root {
      --bg: #0f172a;
      --card-bg: #1e293b;
      --border: #334155;
      --text: #f8fafc;
      --muted: #94a3b8;
      --accent: #38bdf8;
      --accent-hover: #0284c7;
      --success: #22c55e;
      --warning: #f59e0b;
      --danger: #ef4444;
    }
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      background: var(--bg);
      color: var(--text);
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      line-height: 1.5;
      padding: 16px;
      display: flex;
      justify-content: center;
    }
    .container {
      width: 100%;
      max-width: 480px;
      display: flex;
      flex-direction: column;
      gap: 16px;
    }
    header {
      text-align: center;
      padding: 12px 0;
    }
    header h1 {
      font-size: 28px;
      font-weight: 800;
      letter-spacing: -0.5px;
      color: var(--accent);
    }
    .badge {
      display: inline-block;
      padding: 4px 10px;
      border-radius: 9999px;
      font-size: 12px;
      font-weight: 600;
      background: rgba(56, 189, 248, 0.15);
      color: var(--accent);
      border: 1px solid rgba(56, 189, 248, 0.3);
      margin-top: 6px;
    }
    .card {
      background: var(--card-bg);
      border: 1px solid var(--border);
      border-radius: 12px;
      padding: 18px;
      display: flex;
      flex-direction: column;
      gap: 14px;
    }
    .card-title {
      font-size: 17px;
      font-weight: 700;
      border-bottom: 1px solid var(--border);
      padding-bottom: 8px;
      color: var(--text);
    }
    .form-group {
      display: flex;
      flex-direction: column;
      gap: 6px;
    }
    label {
      font-size: 13px;
      font-weight: 600;
      color: var(--muted);
      text-transform: uppercase;
      letter-spacing: 0.5px;
    }
    input[type="file"], select, input[type="number"] {
      width: 100%;
      padding: 10px 12px;
      background: #0f172a;
      border: 1px solid var(--border);
      border-radius: 8px;
      color: var(--text);
      font-size: 14px;
    }
    input[type="file"]:focus, select:focus, input[type="number"]:focus {
      outline: none;
      border-color: var(--accent);
    }
    .btn {
      width: 100%;
      padding: 12px;
      border: none;
      border-radius: 8px;
      font-size: 15px;
      font-weight: 700;
      cursor: pointer;
      text-align: center;
      transition: background 0.15s ease;
    }
    .btn-primary {
      background: var(--accent);
      color: #0f172a;
    }
    .btn-primary:hover { background: var(--accent-hover); }
    .btn-success {
      background: var(--success);
      color: #0f172a;
    }
    .btn-success:hover { background: #16a34a; }
    .btn:disabled {
      opacity: 0.5;
      cursor: not-allowed;
    }
    .info-row {
      display: flex;
      justify-content: space-between;
      font-size: 14px;
      padding: 4px 0;
    }
    .info-label { color: var(--muted); }
    .info-val { font-weight: 600; color: var(--text); }
    .price-total {
      font-size: 20px;
      color: var(--accent);
      font-weight: 800;
      border-top: 1px dashed var(--border);
      padding-top: 8px;
      margin-top: 4px;
    }
    .code-display {
      font-family: monospace;
      font-size: 26px;
      letter-spacing: 4px;
      text-align: center;
      padding: 10px;
      background: #0f172a;
      border-radius: 8px;
      border: 1px solid var(--accent);
      color: var(--accent);
      font-weight: bold;
    }
    .progress-bar-bg {
      width: 100%;
      height: 10px;
      background: #0f172a;
      border-radius: 5px;
      overflow: hidden;
      border: 1px solid var(--border);
    }
    .progress-bar-fill {
      height: 100%;
      width: 0%;
      background: var(--accent);
      transition: width 0.3s ease;
    }
    .status-pill {
      display: inline-block;
      padding: 4px 10px;
      border-radius: 6px;
      font-size: 13px;
      font-weight: 700;
      text-align: center;
    }
    .status-PENDING_PAYMENT { background: #d97706; color: #fff; }
    .status-PAID_WAITING { background: #2563eb; color: #fff; }
    .status-DISPATCHED { background: #7c3aed; color: #fff; }
    .status-SPOOLING { background: #0284c7; color: #fff; }
    .status-PRINTING { background: #059669; color: #fff; }
    .status-COMPLETED { background: var(--success); color: #0f172a; }
    .status-PAUSED_ERROR { background: var(--danger); color: #fff; }
    .alert {
      padding: 10px 12px;
      border-radius: 8px;
      font-size: 13px;
    }
    .alert-error {
      background: rgba(239, 68, 68, 0.15);
      border: 1px solid var(--danger);
      color: #fca5a5;
    }
    .hidden { display: none !important; }
  </style>
</head>
<body>
  <div class="container">
    <header>
      <h1>XeroxGo</h1>
      <div class="badge">Kiosk: <span id="kiosk-id-label">{{.KioskID}}</span></div>
    </header>

    <!-- Upload Section -->
    <div class="card" id="upload-card">
      <div class="card-title">1. Upload Document</div>
      <form id="upload-form">
        <div class="form-group">
          <label for="file-input">Select PDF</label>
          <input type="file" id="file-input" accept=".pdf" required>
        </div>
        <div class="form-group">
          <label for="copies-input">Copies</label>
          <input type="number" id="copies-input" min="1" max="99" value="1" required>
        </div>
        <div class="form-group">
          <label for="duplex-select">Print Side</label>
          <select id="duplex-select">
            <option value="SIMPLEX">Single-sided (Simplex)</option>
            <option value="DUPLEX_LONG_EDGE">Double-sided (Long Edge)</option>
            <option value="DUPLEX_SHORT_EDGE">Double-sided (Short Edge)</option>
          </select>
        </div>
        <button type="submit" class="btn btn-primary" id="upload-btn">Upload & Calculate Price</button>
      </form>
      <div id="upload-error" class="alert alert-error hidden"></div>
    </div>

    <!-- Pricing & Payment Section -->
    <div class="card hidden" id="order-card">
      <div class="card-title">2. Order Summary</div>
      <div class="form-group">
        <label>Display Code</label>
        <div class="code-display" id="display-code">----</div>
      </div>
      <div class="info-row">
        <span class="info-label">Logical Pages:</span>
        <span class="info-val" id="summary-pages">-</span>
      </div>
      <div class="info-row">
        <span class="info-label">Physical Sheets:</span>
        <span class="info-val" id="summary-sheets">-</span>
      </div>
      <div class="info-row">
        <span class="info-label">Copies:</span>
        <span class="info-val" id="summary-copies">-</span>
      </div>
      <div class="info-row">
        <span class="info-label">Rate per sheet:</span>
        <span class="info-val" id="summary-rate">-</span>
      </div>
      <div class="info-row price-total">
        <span>Total Payable:</span>
        <span id="summary-total">-</span>
      </div>

      {{if .DevMode}}
      <button class="btn btn-success" id="pay-btn">Simulate Payment (Dev Mode)</button>
      {{else}}
      <div class="alert" style="background:#1e293b;border:1px solid #334155;color:#94a3b8;">
        Standard payment gateway integration active. Complete payment via provider app.
      </div>
      {{end}}
      <div id="payment-error" class="alert alert-error hidden"></div>
    </div>

    <!-- Live Status Section -->
    <div class="card hidden" id="status-card">
      <div class="card-title">3. Print Status</div>
      <div class="info-row" style="align-items: center;">
        <span class="info-label">Current State:</span>
        <span class="status-pill" id="status-pill">PENDING</span>
      </div>
      <div class="progress-bar-bg">
        <div class="progress-bar-fill" id="progress-fill"></div>
      </div>
      <div class="info-row">
        <span class="info-label">Printed Pages:</span>
        <span class="info-val" id="progress-pages">0 / 0</span>
      </div>
      <div class="info-row">
        <span class="info-label">Printed Sheets:</span>
        <span class="info-val" id="progress-sheets">0 / 0</span>
      </div>
      <div id="status-error-reason" class="alert alert-error hidden"></div>
    </div>
  </div>

  <script>
    (function() {
      const kioskID = "{{.KioskID}}";
      let activeOrderID = null;
      let pollTimer = null;

      const uploadForm = document.getElementById("upload-form");
      const fileInput = document.getElementById("file-input");
      const copiesInput = document.getElementById("copies-input");
      const duplexSelect = document.getElementById("duplex-select");
      const uploadBtn = document.getElementById("upload-btn");
      const uploadError = document.getElementById("upload-error");

      const orderCard = document.getElementById("order-card");
      const displayCode = document.getElementById("display-code");
      const summaryPages = document.getElementById("summary-pages");
      const summarySheets = document.getElementById("summary-sheets");
      const summaryCopies = document.getElementById("summary-copies");
      const summaryRate = document.getElementById("summary-rate");
      const summaryTotal = document.getElementById("summary-total");
      const payBtn = document.getElementById("pay-btn");
      const paymentError = document.getElementById("payment-error");

      const statusCard = document.getElementById("status-card");
      const statusPill = document.getElementById("status-pill");
      const progressFill = document.getElementById("progress-fill");
      const progressPages = document.getElementById("progress-pages");
      const progressSheets = document.getElementById("progress-sheets");
      const statusErrorReason = document.getElementById("status-error-reason");

      function formatPaise(paise) {
        return "₹" + (paise / 100).toFixed(2);
      }

      uploadForm.addEventListener("submit", async function(e) {
        e.preventDefault();
        uploadError.classList.add("hidden");
        uploadError.textContent = "";

        const file = fileInput.files[0];
        if (!file) {
          uploadError.textContent = "Please select a PDF file.";
          uploadError.classList.remove("hidden");
          return;
        }

        const formData = new FormData();
        formData.append("file", file);
        formData.append("copies", copiesInput.value);
        formData.append("duplex", duplexSelect.value);

        uploadBtn.disabled = true;
        uploadBtn.textContent = "Processing...";

        try {
          const resp = await fetch("/api/k/" + encodeURIComponent(kioskID) + "/upload", {
            method: "POST",
            body: formData
          });

          if (!resp.ok) {
            const errText = await resp.text();
            throw new Error(errText || "Upload failed (" + resp.status + ")");
          }

          const data = await resp.json();
          activeOrderID = data.order_id;

          displayCode.textContent = data.display_code;
          summaryPages.textContent = data.total_pages;
          summarySheets.textContent = data.total_sheets;
          summaryCopies.textContent = data.copies;
          summaryRate.textContent = formatPaise(data.price_per_sheet_paise);
          summaryTotal.textContent = formatPaise(data.total_price_paise);

          orderCard.classList.remove("hidden");
          statusCard.classList.remove("hidden");
          updateStatusUI(data.status, 0, data.total_pages, 0, data.total_sheets, "");

          startPolling(activeOrderID);
        } catch (err) {
          uploadError.textContent = err.message || "Failed to process PDF.";
          uploadError.classList.remove("hidden");
        } finally {
          uploadBtn.disabled = false;
          uploadBtn.textContent = "Upload & Calculate Price";
        }
      });

      if (payBtn) {
        payBtn.addEventListener("click", async function() {
          if (!activeOrderID) return;
          paymentError.classList.add("hidden");
          payBtn.disabled = true;

          try {
            const resp = await fetch("/api/orders/" + encodeURIComponent(activeOrderID) + "/simulate-payment", {
              method: "POST"
            });
            if (!resp.ok) {
              const errText = await resp.text();
              throw new Error(errText || "Payment simulation failed");
            }
            const data = await resp.json();
            updateStatusUI(data.status, 0, 0, 0, 0, "");
          } catch (err) {
            paymentError.textContent = err.message;
            paymentError.classList.remove("hidden");
            payBtn.disabled = false;
          }
        });
      }

      function startPolling(orderID) {
        if (pollTimer) clearInterval(pollTimer);
        pollTimer = setInterval(async function() {
          try {
            const resp = await fetch("/api/orders/" + encodeURIComponent(orderID) + "/status");
            if (!resp.ok) return;
            const data = await resp.json();

            updateStatusUI(
              data.status,
              data.pages_printed,
              data.total_pages,
              data.sheets_printed,
              data.total_sheets,
              data.error_reason
            );

            if (data.status === "COMPLETED" || data.status === "PAUSED_ERROR" || data.status === "FAILED_REFUNDABLE") {
              if (data.status === "COMPLETED") {
                clearInterval(pollTimer);
              }
            }
          } catch (_) {}
        }, 1200);
      }

      function updateStatusUI(status, pages, totalPages, sheets, totalSheets, errReason) {
        statusPill.textContent = status;
        statusPill.className = "status-pill status-" + status;

        if (totalPages > 0) {
          progressPages.textContent = pages + " / " + totalPages;
          const pct = Math.min(100, Math.round((pages / totalPages) * 100));
          progressFill.style.width = pct + "%";
        }
        if (totalSheets > 0) {
          progressSheets.textContent = sheets + " / " + totalSheets;
        }

        if (errReason) {
          statusErrorReason.textContent = errReason;
          statusErrorReason.classList.remove("hidden");
        } else {
          statusErrorReason.classList.add("hidden");
        }
      }
    })();
  </script>
</body>
</html>`

func randomHex(bytesLen int) (string, error) {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func randomDisplayCode() (string, error) {
	const charset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b), nil
}

func generateSignedDownloadURL(baseURL, orderID string, secret []byte, ttl time.Duration) string {
	expires := time.Now().Add(ttl).Unix()
	msg := fmt.Sprintf("%s\n%d", orderID, expires)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(msg))
	sig := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("%s/api/orders/%s/pdf?expires=%d&sig=%s", baseURL, orderID, expires, sig)
}

func verifySignedDownloadURL(orderID string, expires int64, sig string, secret []byte) bool {
	if time.Now().Unix() > expires {
		return false
	}
	msg := fmt.Sprintf("%s\n%d", orderID, expires)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(msg))
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(strings.ToLower(sig)), []byte(strings.ToLower(expected)))
}

func (s *appServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *appServer) handlePortal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	prefix := "/k/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	kioskID := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, prefix))
	if kioskID == "" {
		http.Error(w, "kiosk id required", http.StatusBadRequest)
		return
	}

	tmpl, err := template.New("portal").Parse(portalHTML)
	if err != nil {
		http.Error(w, "failed to render portal", http.StatusInternalServerError)
		return
	}

	data := struct {
		KioskID            string
		DevMode            bool
		PricePerSheetPaise int64
	}{
		KioskID:            kioskID,
		DevMode:            s.cfg.devMode,
		PricePerSheetPaise: s.cfg.pricePerSheetPaise,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.Execute(w, data)
}

func (s *appServer) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	prefix := "/api/k/"
	suffix := "/upload"
	if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, suffix) {
		http.NotFound(w, r)
		return
	}
	kioskID := strings.TrimPrefix(r.URL.Path, prefix)
	kioskID = strings.TrimSuffix(kioskID, suffix)
	kioskID = strings.TrimSpace(kioskID)
	if kioskID == "" {
		http.Error(w, "kiosk id required", http.StatusBadRequest)
		return
	}

	const maxUploadSize = 25 * 1024 * 1024 // 25 MiB
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		http.Error(w, "request exceeds 25 MiB limit or malformed", http.StatusRequestEntityTooLarge)
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	copiesStr := r.FormValue("copies")
	copies := 1
	if copiesStr != "" {
		c, err := strconv.Atoi(copiesStr)
		if err != nil || c < 1 || c > 999 {
			http.Error(w, "copies must be between 1 and 999", http.StatusBadRequest)
			return
		}
		copies = c
	}

	duplexStr := r.FormValue("duplex")
	var duplexMode contracts.DuplexMode
	switch duplexStr {
	case string(contracts.DuplexSimplex):
		duplexMode = contracts.DuplexSimplex
	case string(contracts.DuplexLongEdge):
		duplexMode = contracts.DuplexLongEdge
	case string(contracts.DuplexShortEdge):
		duplexMode = contracts.DuplexShortEdge
	default:
		http.Error(w, "invalid duplex mode", http.StatusBadRequest)
		return
	}

	tmpInput, err := os.CreateTemp(s.cfg.jobsDir, "upload_in_*.pdf")
	if err != nil {
		http.Error(w, "failed to create temp file", http.StatusInternalServerError)
		return
	}
	tmpInputName := tmpInput.Name()
	defer func() {
		_ = tmpInput.Close()
		_ = os.Remove(tmpInputName)
	}()

	if _, err := io.Copy(tmpInput, file); err != nil {
		http.Error(w, "failed to save uploaded file", http.StatusInternalServerError)
		return
	}
	_ = tmpInput.Close()

	orderHex, err := randomHex(12)
	if err != nil {
		http.Error(w, "failed to generate order id", http.StatusInternalServerError)
		return
	}
	orderID := "ord_" + orderHex

	jobHex, err := randomHex(12)
	if err != nil {
		http.Error(w, "failed to generate job id", http.StatusInternalServerError)
		return
	}
	jobID := "job_" + jobHex

	displayCode, err := randomDisplayCode()
	if err != nil {
		http.Error(w, "failed to generate display code", http.StatusInternalServerError)
		return
	}

	h := sha256.Sum256([]byte(orderID))
	outputPath := filepath.Join(s.cfg.jobsDir, hex.EncodeToString(h[:])+".pdf")

	normRes, err := pdf.NormalizeAndCount(r.Context(), tmpInputName, outputPath, copies, duplexMode)
	if err != nil {
		_ = os.Remove(outputPath)
		http.Error(w, fmt.Sprintf("normalization failed: %v", err), http.StatusBadRequest)
		return
	}

	pricePerSheet, err := s.db.GetPricePerSheetPaise(r.Context())
	if err != nil || pricePerSheet <= 0 {
		pricePerSheet = s.cfg.pricePerSheetPaise
	}
	totalPricePaise, err := payments.CalculatePricePaise(normRes.TotalSheets, pricePerSheet)
	if err != nil {
		_ = os.Remove(outputPath)
		http.Error(w, "failed to calculate price", http.StatusInternalServerError)
		return
	}

	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	baseURL := fmt.Sprintf("%s://%s", scheme, r.Host)
	downloadURL := generateSignedDownloadURL(baseURL, orderID, s.cfg.downloadSecret, 24*time.Hour)

	cmd := contracts.PrintJobCommand{
		JobID:          jobID,
		KioskID:        kioskID,
		DisplayCode:    displayCode,
		DownloadURL:    downloadURL,
		SHA256Checksum: normRes.SHA256Checksum,
		TotalPages:     normRes.TotalPages,
		TotalSheets:    normRes.TotalSheets,
		Copies:         copies,
		Duplex:         duplexMode,
		StartFromPage:  1,
	}

	order := store.Order{
		OrderID:    orderID,
		Command:    cmd,
		Status:     contracts.JobStatusPendingPayment,
		PricePaise: totalPricePaise,
	}

	if err := s.db.CreateOrder(r.Context(), order); err != nil {
		_ = os.Remove(outputPath)
		http.Error(w, "failed to persist order", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"order_id":              orderID,
		"job_id":                jobID,
		"display_code":          displayCode,
		"total_pages":           normRes.TotalPages,
		"total_sheets":          normRes.TotalSheets,
		"copies":                copies,
		"duplex":                string(duplexMode),
		"price_per_sheet_paise": pricePerSheet,
		"total_price_paise":     totalPricePaise,
		"status":                string(contracts.JobStatusPendingPayment),
	})
}

func (s *appServer) handleDownloadPDF(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	prefix := "/api/orders/"
	suffix := "/pdf"
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

	expiresStr := r.URL.Query().Get("expires")
	sig := r.URL.Query().Get("sig")
	if expiresStr == "" || sig == "" {
		http.Error(w, "missing authentication signature", http.StatusUnauthorized)
		return
	}

	expires, err := strconv.ParseInt(expiresStr, 10, 64)
	if err != nil || !verifySignedDownloadURL(orderID, expires, sig, s.cfg.downloadSecret) {
		http.Error(w, "invalid or expired download URL", http.StatusUnauthorized)
		return
	}

	order, err := s.db.GetOrder(r.Context(), orderID)
	if err != nil || order == nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}

	h := sha256.Sum256([]byte(orderID))
	pdfPath := filepath.Join(s.cfg.jobsDir, hex.EncodeToString(h[:])+".pdf")

	f, err := os.Open(pdfPath)
	if err != nil {
		http.Error(w, "document artifact not found", http.StatusNotFound)
		return
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		http.Error(w, "failed to verify document integrity", http.StatusInternalServerError)
		return
	}
	calcSHA := hex.EncodeToString(hasher.Sum(nil))
	if calcSHA != strings.ToLower(order.Command.SHA256Checksum) {
		http.Error(w, "artifact integrity check failed", http.StatusInternalServerError)
		return
	}

	stat, _ := f.Stat()
	if stat != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	}
	w.Header().Set("Content-Type", "application/pdf")
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "seek error", http.StatusInternalServerError)
		return
	}
	_, _ = io.Copy(w, f)
}

func (s *appServer) handleSimulatePayment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !s.cfg.devMode {
		http.Error(w, "payment simulation only available in development mode", http.StatusForbidden)
		return
	}

	prefix := "/api/orders/"
	suffix := "/simulate-payment"
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

	webhookID := "evt_sim_" + orderID
	recorded, err := s.db.RecordWebhook(r.Context(), webhookID)
	if err != nil {
		http.Error(w, "failed to record webhook", http.StatusInternalServerError)
		return
	}
	if !recorded {
		// Idempotent repetition
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"order_id": orderID,
			"status":   string(order.Status),
		})
		return
	}

	if err := s.db.UpdateOrderStatus(r.Context(), orderID, contracts.JobStatusPaidWaiting); err != nil {
		http.Error(w, "failed to update order status", http.StatusInternalServerError)
		return
	}

	if err := s.hub.SendJob(r.Context(), order.Command); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"order_id": orderID,
			"status":   string(contracts.JobStatusPaidWaiting),
			"error":    fmt.Sprintf("kiosk offline or dispatch failed: %v", err),
		})
		return
	}

	_ = s.db.UpdateOrderStatus(r.Context(), orderID, contracts.JobStatusDispatched)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"order_id": orderID,
		"status":   string(contracts.JobStatusDispatched),
	})
}

func (s *appServer) handleRazorpayWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	const maxWebhookSize = 1024 * 1024 // 1 MiB
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookSize)
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read webhook body", http.StatusBadRequest)
		return
	}

	sig := r.Header.Get("X-Razorpay-Signature")
	if sig == "" || !payments.VerifyWebhookSignature(rawBody, sig, string(s.cfg.webhookSecret)) {
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	var parsed struct {
		Event   string `json:"event"`
		Payload struct {
			Payment struct {
				Entity struct {
					ID      string `json:"id"`
					OrderID string `json:"order_id"`
					Status  string `json:"status"`
					Notes   struct {
						OrderID string `json:"order_id"`
					} `json:"notes"`
				} `json:"entity"`
			} `json:"payment"`
		} `json:"payload"`
	}

	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		http.Error(w, "malformed webhook json", http.StatusBadRequest)
		return
	}

	if parsed.Event != "payment.captured" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ignored"}`))
		return
	}

	orderID := parsed.Payload.Payment.Entity.Notes.OrderID
	if orderID == "" {
		orderID = parsed.Payload.Payment.Entity.OrderID
	}
	if orderID == "" {
		http.Error(w, "order id missing in payment entity", http.StatusBadRequest)
		return
	}

	eventID := parsed.Payload.Payment.Entity.ID
	if eventID == "" {
		eventID = "evt_" + orderID
	}

	recorded, err := s.db.RecordWebhook(r.Context(), eventID)
	if err != nil {
		http.Error(w, "failed to record webhook", http.StatusInternalServerError)
		return
	}
	if !recorded {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"duplicate"}`))
		return
	}

	order, err := s.db.GetOrder(r.Context(), orderID)
	if err != nil || order == nil {
		http.Error(w, "order not found", http.StatusBadRequest)
		return
	}

	_ = s.db.UpdateOrderStatus(r.Context(), orderID, contracts.JobStatusPaidWaiting)

	if err := s.hub.SendJob(r.Context(), order.Command); err == nil {
		_ = s.db.UpdateOrderStatus(r.Context(), orderID, contracts.JobStatusDispatched)
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *appServer) handleOrderStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	prefix := "/api/orders/"
	suffix := "/status"
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

	pagesPrinted := 0
	sheetsPrinted := 0
	currentStatus := string(order.Status)
	errorReason := ""

	progress, err := s.db.GetJobProgress(r.Context(), order.Command.JobID)
	if err == nil && progress != nil {
		pagesPrinted = progress.PagesPrinted
		sheetsPrinted = progress.SheetsPrinted
		errorReason = progress.ErrorReason
		switch progress.Status {
		case contracts.JobStatusSpooling,
			contracts.JobStatusPrinting,
			contracts.JobStatusCompleted,
			contracts.JobStatusPausedError,
			contracts.JobStatusFailedRefundable:
			currentStatus = string(progress.Status)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"order_id":       order.OrderID,
		"display_code":   order.Command.DisplayCode,
		"status":         currentStatus,
		"total_pages":    order.Command.TotalPages,
		"total_sheets":   order.Command.TotalSheets,
		"copies":         order.Command.Copies,
		"duplex":         string(order.Command.Duplex),
		"pages_printed":  pagesPrinted,
		"sheets_printed": sheetsPrinted,
		"error_reason":   errorReason,
		"price_paise":    order.PricePaise,
	})
}

func main() {
	addr := os.Getenv("XEROXGO_CLOUD_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	dbPath := os.Getenv("XEROXGO_CLOUD_DB")
	if dbPath == "" {
		dbPath = "./data/xeroxgo-cloud.db"
	}

	jobsDir := os.Getenv("XEROXGO_JOBS_DIR")
	if jobsDir == "" {
		jobsDir = "./data/jobs"
	}

	devMode := os.Getenv("XEROXGO_DEV_MODE") == "1"

	dlSecretStr := os.Getenv("XEROXGO_DOWNLOAD_SECRET")
	whSecretStr := os.Getenv("XEROXGO_WEBHOOK_SECRET")

	if !devMode {
		if len(dlSecretStr) < 16 || len(whSecretStr) < 16 {
			log.Fatalf("Fatal: production mode requires non-empty XEROXGO_DOWNLOAD_SECRET and XEROXGO_WEBHOOK_SECRET of at least 16 characters")
		}
	} else {
		if dlSecretStr == "" {
			dlSecretStr = "dev_download_secret_32_bytes_long_123"
		}
		if whSecretStr == "" {
			whSecretStr = "dev_webhook_secret_32_bytes_long_123"
		}
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		log.Fatalf("failed to create db directory: %v", err)
	}
	if err := os.MkdirAll(jobsDir, 0700); err != nil {
		log.Fatalf("failed to create jobs directory: %v", err)
	}

	storeDB, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("failed to open cloud database at %s: %v", dbPath, err)
	}
	defer storeDB.Close()

	ctx := context.Background()
	var pricePaise int64
	priceEnv := os.Getenv("XEROXGO_PRICE_PER_SHEET_PAISE")
	if priceEnv != "" {
		p, err := strconv.ParseInt(priceEnv, 10, 64)
		if err != nil || p <= 0 {
			log.Fatalf("invalid XEROXGO_PRICE_PER_SHEET_PAISE: %s", priceEnv)
		}
		if err := storeDB.SetPricePerSheetPaise(ctx, p); err != nil {
			log.Fatalf("failed to persist price per sheet: %v", err)
		}
		pricePaise = p
	} else {
		p, err := storeDB.GetPricePerSheetPaise(ctx)
		if err == nil && p > 0 {
			pricePaise = p
		} else if devMode {
			pricePaise = 500
			_ = storeDB.SetPricePerSheetPaise(ctx, pricePaise)
		} else {
			log.Fatalf("pricing unconfigured in production mode")
		}
	}

	hub := ws.NewHub(storeDB)

	cfg := &serverConfig{
		addr:               addr,
		dbPath:             dbPath,
		jobsDir:            jobsDir,
		pricePerSheetPaise: pricePaise,
		devMode:            devMode,
		downloadSecret:     []byte(dlSecretStr),
		webhookSecret:      []byte(whSecretStr),
	}

	app := &appServer{
		cfg: cfg,
		db:  storeDB,
		hub: hub,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", app.handleHealthz)
	mux.HandleFunc("/k/", app.handlePortal)
	mux.HandleFunc("/api/k/", app.handleUpload)
	mux.HandleFunc("/api/orders/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/status"):
			app.handleOrderStatus(w, r)
		case strings.HasSuffix(path, "/simulate-payment"):
			app.handleSimulatePayment(w, r)
		case strings.HasSuffix(path, "/pdf"):
			app.handleDownloadPDF(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/api/webhooks/razorpay", app.handleRazorpayWebhook)
	mux.HandleFunc("/ws", hub.ServeHTTP)

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErrCh := make(chan error, 1)
	go func() {
		log.Printf("XeroxGo cloud backend listening on %s (dev_mode=%v, price=%d paise)", addr, devMode, pricePaise)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrCh <- err
		}
	}()

	select {
	case err := <-serverErrCh:
		log.Fatalf("server failure: %v", err)
	case <-sigCtx.Done():
		log.Printf("shutting down cloud backend...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown error: %v", err)
		}
	}
}
