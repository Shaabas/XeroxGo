package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsAllowedOrigin(t *testing.T) {
	tests := []struct {
		origin  string
		allowed bool
	}{
		{"https://xeroxgo.vercel.app", true},
		{"https://campusprint.vercel.app", true},
		{"https://campusprint-preview-123.vercel.app", true},
		{"https://xeroxgo-preview-456.vercel.app", true},
		{"http://localhost:3000", true},
		{"http://localhost:8080", true},
		{"http://localhost:5173", true},
		{"http://127.0.0.1:3000", true},
		{"http://127.0.0.1:8080", true},
		{"http://127.0.0.1:5173", true},
		{"", false},
		{"https://malicious-site.com", false},
		{"https://attacker.vercel.app", false},
		{"https://evilcampusprint.vercel.app", false},
		{"http://evil.com", false},
	}

	for _, tt := range tests {
		got := isAllowedOrigin(tt.origin)
		if got != tt.allowed {
			t.Errorf("isAllowedOrigin(%q) = %v; want %v", tt.origin, got, tt.allowed)
		}
	}
}

func TestSanitizeCSVField(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", ""},
		{"Normal Text", "Normal Text"},
		{"12345", "12345"},
		{"=1+1", "'=1+1"},
		{"-SUM(A1:A10)", "'-SUM(A1:A10)"},
		{"+cmd|' /C calc'!A0", "'+cmd|' /C calc'!A0"},
		{"@SUM(1,2)", "'@SUM(1,2)"},
		{"\tmalicious", "'\tmalicious"},
		{"\rmalicious", "'\rmalicious"},
	}

	for _, tt := range tests {
		got := sanitizeCSVField(tt.input)
		if got != tt.expected {
			t.Errorf("sanitizeCSVField(%q) = %q; want %q", tt.input, got, tt.expected)
		}
	}
}

func TestIngressHandlerProxySecret(t *testing.T) {
	dummyMux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	const secret = "test-proxy-secret-abc-123"

	t.Run("Production mode blocks direct API request without secret", func(t *testing.T) {
		h := createIngressHandler(dummyMux, secret, false)
		req := httptest.NewRequest(http.MethodPost, "/api/k/demo/upload", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404 Not Found for direct request without proxy secret, got %d", rec.Code)
		}
	})

	t.Run("Production mode blocks direct API request with wrong secret", func(t *testing.T) {
		h := createIngressHandler(dummyMux, secret, false)
		req := httptest.NewRequest(http.MethodPost, "/api/k/demo/upload", nil)
		req.Header.Set("X-XeroxGo-Proxy-Secret", "wrong-secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404 Not Found for direct request with invalid secret, got %d", rec.Code)
		}
	})

	t.Run("Production mode allows proxied API request with matching secret", func(t *testing.T) {
		h := createIngressHandler(dummyMux, secret, false)
		req := httptest.NewRequest(http.MethodPost, "/api/k/demo/upload", nil)
		req.Header.Set("X-XeroxGo-Proxy-Secret", secret)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for valid proxy secret, got %d", rec.Code)
		}
	})

	t.Run("Production mode exempts healthz probe without secret", func(t *testing.T) {
		h := createIngressHandler(dummyMux, secret, false)
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for /healthz, got %d", rec.Code)
		}
	})

	t.Run("Production mode exempts razorpay webhook without secret", func(t *testing.T) {
		h := createIngressHandler(dummyMux, secret, false)
		req := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for /api/webhooks/razorpay, got %d", rec.Code)
		}
	})

	t.Run("Production mode exempts signed PDF download route without proxy secret", func(t *testing.T) {
		h := createIngressHandler(dummyMux, secret, false)
		req := httptest.NewRequest(http.MethodGet, "/api/orders/order-123/pdf", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for pdf download route, got %d", rec.Code)
		}
	})

	t.Run("Development mode bypasses proxy secret check", func(t *testing.T) {
		h := createIngressHandler(dummyMux, secret, true) // devMode = true
		req := httptest.NewRequest(http.MethodPost, "/api/k/demo/upload", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK in devMode, got %d", rec.Code)
		}
	})

	t.Run("Unconfigured proxy secret allows requests", func(t *testing.T) {
		h := createIngressHandler(dummyMux, "", false) // empty secret
		req := httptest.NewRequest(http.MethodPost, "/api/k/demo/upload", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK when secret is unconfigured, got %d", rec.Code)
		}
	})
}

