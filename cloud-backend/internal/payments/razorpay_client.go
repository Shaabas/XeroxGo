package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RazorpayProvider implements the Provider interface using Razorpay's REST APIs.
type RazorpayProvider struct {
	KeyID         string
	KeySecret     string
	WebhookSecret string
	BaseURL       string
	HTTPClient    *http.Client
}

// RazorpayConfig holds configuration for initializing a RazorpayProvider.
type RazorpayConfig struct {
	KeyID         string
	KeySecret     string
	WebhookSecret string
	BaseURL       string
	HTTPClient    *http.Client
}

// NewRazorpayProvider initializes a new RazorpayProvider.
func NewRazorpayProvider(cfg RazorpayConfig) (*RazorpayProvider, error) {
	keyID := strings.TrimSpace(cfg.KeyID)
	keySecret := strings.TrimSpace(cfg.KeySecret)
	if keyID == "" || keySecret == "" {
		return nil, errors.New("razorpay provider: key_id and key_secret are required")
	}

	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.razorpay.com"
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	return &RazorpayProvider{
		KeyID:         keyID,
		KeySecret:     keySecret,
		WebhookSecret: strings.TrimSpace(cfg.WebhookSecret),
		BaseURL:       baseURL,
		HTTPClient:    client,
	}, nil
}

// Name returns the provider identifier.
func (p *RazorpayProvider) Name() string {
	return "razorpay"
}

// CreatePayment initiates a dynamic UPI QR code with Razorpay.
// If the merchant account does not have dynamic QR enabled, it falls back to Razorpay Orders API.
func (p *RazorpayProvider) CreatePayment(ctx context.Context, req CreatePaymentRequest) (*CreatePaymentResult, error) {
	if ctx == nil {
		return nil, errors.New("razorpay provider: context is nil")
	}
	if req.AmountPaise <= 0 {
		return nil, fmt.Errorf("razorpay provider: invalid amount_paise %d", req.AmountPaise)
	}
	if strings.TrimSpace(req.OrderID) == "" {
		return nil, errors.New("razorpay provider: order_id cannot be empty")
	}
	if strings.TrimSpace(req.SessionID) == "" {
		return nil, errors.New("razorpay provider: session_id cannot be empty")
	}

	notes := make(map[string]string)
	for k, v := range req.Notes {
		notes[k] = v
	}
	notes["order_id"] = req.OrderID
	notes["session_id"] = req.SessionID
	notes["kiosk_id"] = req.KioskID
	notes["attempt_id"] = req.AttemptID

	closeBy := req.ExpiresAt.Unix()
	if closeBy <= time.Now().Unix() {
		closeBy = time.Now().Add(15 * time.Minute).Unix()
	}

	// 1. Attempt dynamic single-use UPI QR code creation
	qrReqBody := map[string]interface{}{
		"type":           "upi_qr",
		"name":           fmt.Sprintf("XeroxGo Kiosk %s", req.KioskID),
		"usage":          "single_use",
		"fixed_amount":   true,
		"payment_amount": req.AmountPaise,
		"description":    fmt.Sprintf("Print Order %s", req.OrderID),
		"close_by":       closeBy,
		"notes":          notes,
	}

	rawQR, qrStatus, qrErr := p.doPost(ctx, "/v1/payments/qr_codes", qrReqBody)
	if qrErr == nil && (qrStatus == http.StatusOK || qrStatus == http.StatusCreated) {
		var qrResp struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			Payload   string `json:"payload"`
			ImageURL  string `json:"image_url"`
			CloseBy   int64  `json:"close_by"`
		}
		if err := json.Unmarshal(rawQR, &qrResp); err == nil && qrResp.ID != "" {
			qrPayload := qrResp.Payload
			if qrPayload == "" {
				// Construct standard UPI URI if payload omitted by API
				rupees := float64(req.AmountPaise) / 100.0
				pn := url.QueryEscape("XeroxGo Kiosk")
				tn := url.QueryEscape(fmt.Sprintf("Print Order %s", req.OrderID))
				qrPayload = fmt.Sprintf("upi://pay?pa=razorpay@upi&pn=%s&am=%.2f&cu=INR&tr=%s&tn=%s",
					pn, rupees, qrResp.ID, tn)
			}
			return &CreatePaymentResult{
				Provider:        "razorpay",
				ProviderOrderID: qrResp.ID,
				QRPayload:       qrPayload,
				QRImageURL:      qrResp.ImageURL,
				Status:          "CREATED",
				ExpiresAt:       time.Unix(qrResp.CloseBy, 0).UTC(),
			}, nil
		}
	}

	// 2. Fallback to standard Razorpay Orders API if QR Code API returns 400/404/feature disabled
	orderReqBody := map[string]interface{}{
		"amount":   req.AmountPaise,
		"currency": "INR",
		"receipt":  req.OrderID,
		"notes":    notes,
	}
	rawOrder, orderStatus, orderErr := p.doPost(ctx, "/v1/orders", orderReqBody)
	if orderErr != nil {
		return nil, fmt.Errorf("razorpay provider: create order failed (qr_err=%v, order_err=%w)", qrErr, orderErr)
	}
	if orderStatus != http.StatusOK && orderStatus != http.StatusCreated {
		return nil, fmt.Errorf("razorpay provider: api returned status %d: %s", orderStatus, string(rawOrder))
	}

	var orderResp struct {
		ID       string `json:"id"`
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(rawOrder, &orderResp); err != nil {
		return nil, fmt.Errorf("razorpay provider: parse order response: %w", err)
	}
	if orderResp.ID == "" {
		return nil, errors.New("razorpay provider: empty order id returned")
	}

	rupees := float64(req.AmountPaise) / 100.0
	pn := url.QueryEscape("XeroxGo Kiosk")
	tn := url.QueryEscape(fmt.Sprintf("Print Order %s", req.OrderID))
	qrPayload := fmt.Sprintf("upi://pay?pa=razorpay@upi&pn=%s&am=%.2f&cu=INR&tr=%s&tn=%s",
		pn, rupees, orderResp.ID, tn)

	return &CreatePaymentResult{
		Provider:        "razorpay",
		ProviderOrderID: orderResp.ID,
		QRPayload:       qrPayload,
		QRImageURL:      fmt.Sprintf("%s/v1/orders/%s/qr", p.BaseURL, orderResp.ID),
		Status:          "CREATED",
		ExpiresAt:       time.Unix(closeBy, 0).UTC(),
	}, nil
}

// doPost sends an authenticated POST request to Razorpay's API.
func (p *RazorpayProvider) doPost(ctx context.Context, endpoint string, payload interface{}) ([]byte, int, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}

	targetURL := p.BaseURL + endpoint
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(data))
	if err != nil {
		return nil, 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.SetBasicAuth(p.KeyID, p.KeySecret)

	resp, err := p.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return bodyBytes, resp.StatusCode, nil
}

// VerifyWebhookSignature verifies HMAC signature with the Razorpay webhook secret.
func (p *RazorpayProvider) VerifyWebhookSignature(payload []byte, signature string) bool {
	return VerifyWebhookSignature(payload, signature, p.WebhookSecret)
}

