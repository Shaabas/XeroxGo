package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrInvalidOrExpiredPIN is returned when the server reports HTTP 404 (unknown, expired, or completed order).
	ErrInvalidOrExpiredPIN = errors.New("invalid or expired PIN")

	// ErrUnauthorized is returned when kiosk authentication fails (HTTP 401).
	ErrUnauthorized = errors.New("unauthorized kiosk: authentication failed")

	// ErrOrderAlreadyClaimed is returned when an order is already actively claimed (HTTP 409).
	ErrOrderAlreadyClaimed = errors.New("order is already claimed by an active transaction")

	// ErrOrderExpired is returned when the order has expired (HTTP 410).
	ErrOrderExpired = errors.New("order has expired")

	// ErrSessionNotFound is returned when the session does not exist (HTTP 404).
	ErrSessionNotFound = errors.New("session not found")

	// ErrInsufficientPaper is returned when the kiosk printer has insufficient paper or empty tray (HTTP 409).
	ErrInsufficientPaper = errors.New("insufficient paper in kiosk tray")

	// ErrPrinterOffline is returned when the kiosk printer is offline or disconnected (HTTP 409/503).
	ErrPrinterOffline = errors.New("kiosk printer is offline")

	// ErrPrinterError is returned when the printer is in an error state (jam, cover open, etc.) (HTTP 409).
	ErrPrinterError = errors.New("kiosk printer hardware error")
)

// RateLimitError represents an HTTP 429 response from the cloud rate limiter.
type RateLimitError struct {
	RetryAfterSeconds int
}

func (e *RateLimitError) Error() string {
	if e.RetryAfterSeconds > 0 {
		return fmt.Sprintf("rate limited: retry after %d seconds", e.RetryAfterSeconds)
	}
	return "rate limited: too many failed attempts"
}

// NetworkError represents a connection or timeout error talking to the cloud backend.
type NetworkError struct {
	Err error
}

func (e *NetworkError) Error() string {
	return fmt.Sprintf("cloud connection failed: %v", e.Err)
}

func (e *NetworkError) Unwrap() error {
	return e.Err
}

// OrderDetails represents the safe, authoritative order summary returned by the cloud PIN lookup.
type OrderDetails struct {
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

// SessionDetails represents the transaction session state returned by the cloud backend.
type SessionDetails struct {
	SessionID        string `json:"session_id"`
	OrderID          string `json:"order_id"`
	KioskID          string `json:"kiosk_id"`
	Status           string `json:"status"`
	TotalPricePaise  int64  `json:"total_price_paise"`
	ExpiresAt        string `json:"expires_at"`
	SecondsRemaining int64  `json:"seconds_remaining"`
}

// PaymentAttemptDetails represents the payment attempt and dynamic QR payload returned by initiate-payment.
type PaymentAttemptDetails struct {
	AttemptID         string `json:"attempt_id"`
	SessionID         string `json:"session_id"`
	OrderID           string `json:"order_id"`
	KioskID           string `json:"kiosk_id"`
	Provider          string `json:"provider"`
	ProviderOrderID   string `json:"provider_order_id"`
	AmountPaise       int64  `json:"amount_paise"`
	Currency          string `json:"currency"`
	Status            string `json:"status"`
	AttemptStatus     string `json:"attempt_status"`
	QRPayload         string `json:"qr_payload"`
	QRImageURL        string `json:"qr_image_url"`
	DemoToken         string `json:"demo_token"`
	ExpiresAt         string `json:"expires_at"`
}

// PINLookupClient abstracts the retrieval and confirmation of orders and transaction sessions.
type PINLookupClient interface {
	LookupOrderByPIN(ctx context.Context, pin string) (*OrderDetails, error)
	ClaimOrder(ctx context.Context, orderID string) (*SessionDetails, error)
	ReleaseOrder(ctx context.Context, orderID string) error
	CancelSession(ctx context.Context, sessionID string) error
	GetSession(ctx context.Context, sessionID string) (*SessionDetails, error)
	InitiatePayment(ctx context.Context, sessionID string) (*PaymentAttemptDetails, error)
}

// HTTPPINClient implements PINLookupClient using HTTP REST against the cloud backend.
type HTTPPINClient struct {
	baseURL     string
	endpointURL string
	kioskID     string
	kioskSecret string
	client      *http.Client
}

// NewHTTPPINClient constructs a new HTTPPINClient.
func NewHTTPPINClient(cloudURL, kioskID, kioskSecret string, httpClient *http.Client) (*HTTPPINClient, error) {
	if strings.TrimSpace(cloudURL) == "" {
		return nil, errors.New("cloud URL cannot be empty")
	}
	if strings.TrimSpace(kioskID) == "" {
		return nil, errors.New("kiosk ID cannot be empty")
	}

	u, err := url.Parse(cloudURL)
	if err != nil {
		return nil, fmt.Errorf("invalid cloud URL: %w", err)
	}

	scheme := "http"
	if u.Scheme == "https" || u.Scheme == "wss" {
		scheme = "https"
	}

	host := u.Host
	if host == "" {
		host = u.Path
	}

	baseURL := fmt.Sprintf("%s://%s/api/k/%s", scheme, host, url.PathEscape(kioskID))
	endpoint := baseURL + "/orders/lookup"

	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 5 * time.Second,
		}
	}

	return &HTTPPINClient{
		baseURL:     baseURL,
		endpointURL: endpoint,
		kioskID:     kioskID,
		kioskSecret: strings.TrimSpace(kioskSecret),
		client:      httpClient,
	}, nil
}

// LookupOrderByPIN queries the cloud lookup endpoint for the given 4-digit PIN.
func (c *HTTPPINClient) LookupOrderByPIN(ctx context.Context, pin string) (*OrderDetails, error) {
	reqBody, err := json.Marshal(map[string]string{"pin": strings.TrimSpace(pin)})
	if err != nil {
		return nil, fmt.Errorf("marshal pin request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpointURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create pin lookup request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if c.kioskSecret != "" {
		req.Header.Set("Authorization", "Bearer "+c.kioskSecret)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, &NetworkError{Err: err}
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	switch resp.StatusCode {
	case http.StatusOK:
		var details OrderDetails
		if err := json.Unmarshal(bodyBytes, &details); err != nil {
			return nil, fmt.Errorf("unmarshal order details: %w", err)
		}
		return &details, nil

	case http.StatusNotFound:
		return nil, ErrInvalidOrExpiredPIN

	case http.StatusUnauthorized:
		return nil, ErrUnauthorized

	case http.StatusTooManyRequests:
		retrySec := 0
		if h := resp.Header.Get("Retry-After"); h != "" {
			if s, err := strconv.Atoi(h); err == nil && s > 0 {
				retrySec = s
			}
		}
		if retrySec == 0 {
			var parsed struct {
				RetryAfterSeconds int `json:"retry_after_seconds"`
			}
			_ = json.Unmarshal(bodyBytes, &parsed)
			retrySec = parsed.RetryAfterSeconds
		}
		return nil, &RateLimitError{RetryAfterSeconds: retrySec}

	case http.StatusBadRequest:
		var parsed struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(bodyBytes, &parsed)
		if parsed.Error != "" {
			return nil, fmt.Errorf("bad request: %s", parsed.Error)
		}
		return nil, errors.New("bad request: invalid pin syntax")

	default:
		return nil, fmt.Errorf("unexpected status %d from lookup: %s", resp.StatusCode, string(bodyBytes))
	}
}

// ClaimOrder claims an order and creates an active transaction session.
func (c *HTTPPINClient) ClaimOrder(ctx context.Context, orderID string) (*SessionDetails, error) {
	claimURL := fmt.Sprintf("%s/orders/%s/claim", c.baseURL, url.PathEscape(orderID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claimURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create claim request: %w", err)
	}

	if c.kioskSecret != "" {
		req.Header.Set("Authorization", "Bearer "+c.kioskSecret)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, &NetworkError{Err: err}
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	switch resp.StatusCode {
	case http.StatusOK:
		var sess SessionDetails
		if err := json.Unmarshal(bodyBytes, &sess); err != nil {
			return nil, fmt.Errorf("unmarshal session details: %w", err)
		}
		return &sess, nil
	case http.StatusConflict:
		var errResp struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		_ = json.Unmarshal(bodyBytes, &errResp)
		switch errResp.Code {
		case "INSUFFICIENT_PAPER", "PAPER_UNKNOWN":
			return nil, ErrInsufficientPaper
		case "PRINTER_OFFLINE":
			return nil, ErrPrinterOffline
		case "PRINTER_ERROR":
			return nil, ErrPrinterError
		default:
			if strings.Contains(strings.ToLower(errResp.Error), "paper") {
				return nil, ErrInsufficientPaper
			}
			return nil, ErrOrderAlreadyClaimed
		}
	case http.StatusGone:
		return nil, ErrOrderExpired
	case http.StatusNotFound:
		return nil, ErrInvalidOrExpiredPIN
	case http.StatusUnauthorized:
		return nil, ErrUnauthorized
	default:
		return nil, fmt.Errorf("claim error %d: %s", resp.StatusCode, string(bodyBytes))
	}
}

// CancelSession cancels an active transaction session.
func (c *HTTPPINClient) CancelSession(ctx context.Context, sessionID string) error {
	cancelURL := fmt.Sprintf("%s/sessions/%s/cancel", c.baseURL, url.PathEscape(sessionID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cancelURL, nil)
	if err != nil {
		return fmt.Errorf("create cancel request: %w", err)
	}

	if c.kioskSecret != "" {
		req.Header.Set("Authorization", "Bearer "+c.kioskSecret)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return &NetworkError{Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("cancel session error %d: %s", resp.StatusCode, string(bodyBytes))
}

// GetSession retrieves details for an existing transaction session.
func (c *HTTPPINClient) GetSession(ctx context.Context, sessionID string) (*SessionDetails, error) {
	sessURL := fmt.Sprintf("%s/sessions/%s", c.baseURL, url.PathEscape(sessionID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sessURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create get session request: %w", err)
	}

	if c.kioskSecret != "" {
		req.Header.Set("Authorization", "Bearer "+c.kioskSecret)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, &NetworkError{Err: err}
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	switch resp.StatusCode {
	case http.StatusOK:
		var sess SessionDetails
		if err := json.Unmarshal(bodyBytes, &sess); err != nil {
			return nil, fmt.Errorf("unmarshal session details: %w", err)
		}
		return &sess, nil
	case http.StatusNotFound:
		return nil, ErrSessionNotFound
	case http.StatusUnauthorized:
		return nil, ErrUnauthorized
	default:
		return nil, fmt.Errorf("get session error %d: %s", resp.StatusCode, string(bodyBytes))
	}
}

// ReleaseOrder requests physical print release for an already paid order.
func (c *HTTPPINClient) ReleaseOrder(ctx context.Context, orderID string) error {
	releaseURL := fmt.Sprintf("%s/orders/%s/release", c.baseURL, url.PathEscape(orderID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, releaseURL, nil)
	if err != nil {
		return fmt.Errorf("create release request: %w", err)
	}

	if c.kioskSecret != "" {
		req.Header.Set("Authorization", "Bearer "+c.kioskSecret)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return &NetworkError{Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil
	}

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	switch resp.StatusCode {
	case http.StatusConflict, http.StatusServiceUnavailable:
		var errResp struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		_ = json.Unmarshal(bodyBytes, &errResp)
		switch errResp.Code {
		case "INSUFFICIENT_PAPER", "PAPER_UNKNOWN":
			return ErrInsufficientPaper
		case "PRINTER_OFFLINE":
			return ErrPrinterOffline
		case "PRINTER_ERROR":
			return ErrPrinterError
		default:
			if strings.Contains(strings.ToLower(errResp.Error), "paper") {
				return ErrInsufficientPaper
			}
			return ErrSessionNotFound
		}
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusNotFound:
		return ErrInvalidOrExpiredPIN
	default:
		return fmt.Errorf("release error %d: %s", resp.StatusCode, string(bodyBytes))
	}
}

// InitiatePayment requests payment attempt creation / dynamic QR payload for an active session.
func (c *HTTPPINClient) InitiatePayment(ctx context.Context, sessionID string) (*PaymentAttemptDetails, error) {
	initiateURL := fmt.Sprintf("%s/sessions/%s/initiate-payment", c.baseURL, url.PathEscape(sessionID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, initiateURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create initiate payment request: %w", err)
	}

	if c.kioskSecret != "" {
		req.Header.Set("Authorization", "Bearer "+c.kioskSecret)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, &NetworkError{Err: err}
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	switch resp.StatusCode {
	case http.StatusOK:
		var att PaymentAttemptDetails
		if err := json.Unmarshal(bodyBytes, &att); err != nil {
			return nil, fmt.Errorf("unmarshal payment attempt details: %w", err)
		}
		return &att, nil
	case http.StatusConflict:
		var errResp struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		_ = json.Unmarshal(bodyBytes, &errResp)
		switch errResp.Code {
		case "INSUFFICIENT_PAPER", "PAPER_UNKNOWN":
			return nil, ErrInsufficientPaper
		case "PRINTER_OFFLINE":
			return nil, ErrPrinterOffline
		case "PRINTER_ERROR":
			return nil, ErrPrinterError
		default:
			return nil, errors.New(errResp.Error)
		}
	case http.StatusUnauthorized:
		return nil, ErrUnauthorized
	case http.StatusNotFound:
		return nil, ErrSessionNotFound
	default:
		return nil, fmt.Errorf("initiate payment error %d: %s", resp.StatusCode, string(bodyBytes))
	}
}



