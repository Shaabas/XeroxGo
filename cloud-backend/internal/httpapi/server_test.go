package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthzSuccess(t *testing.T) {
	handler := NewHandler()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", rec.Code, http.StatusOK)
	}

	contentType := rec.Header().Get("Content-Type")
	wantContentType := "application/json; charset=utf-8"
	if contentType != wantContentType {
		t.Fatalf("Content-Type = %q, want %q", contentType, wantContentType)
	}

	rawBody := strings.TrimSpace(rec.Body.String())
	wantBody := `{"status":"ok"}`
	if rawBody != wantBody {
		t.Fatalf("body = %q, want %q", rawBody, wantBody)
	}

	var resp healthResponse
	if err := json.Unmarshal([]byte(rawBody), &resp); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}

	if resp.Status != "ok" {
		t.Fatalf("response status = %q, want %q", resp.Status, "ok")
	}
}

func TestHealthzSuccessWithQueryParams(t *testing.T) {
	handler := NewHandler()

	req := httptest.NewRequest(http.MethodGet, "/healthz?source=k8s", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", rec.Code, http.StatusOK)
	}

	contentType := rec.Header().Get("Content-Type")
	wantContentType := "application/json; charset=utf-8"
	if contentType != wantContentType {
		t.Fatalf("Content-Type = %q, want %q", contentType, wantContentType)
	}

	var resp healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}

	if resp.Status != "ok" {
		t.Fatalf("response status = %q, want %q", resp.Status, "ok")
	}
}

func TestHealthzPostMethodNotAllowed(t *testing.T) {
	handler := NewHandler()

	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status code = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}

	allowHeader := rec.Header().Get("Allow")
	if allowHeader != http.MethodGet {
		t.Fatalf("Allow header = %q, want %q", allowHeader, http.MethodGet)
	}

	contentType := rec.Header().Get("Content-Type")
	wantContentType := "application/json; charset=utf-8"
	if contentType != wantContentType {
		t.Fatalf("Content-Type = %q, want %q", contentType, wantContentType)
	}

	var resp errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}

	if resp.Error != "method not allowed" {
		t.Fatalf("error message = %q, want %q", resp.Error, "method not allowed")
	}
}

func TestHealthzUnsupportedMethods(t *testing.T) {
	methods := []string{
		http.MethodPost,
		http.MethodPut,
		http.MethodDelete,
		http.MethodPatch,
		http.MethodHead,
		http.MethodOptions,
	}

	handler := NewHandler()

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/healthz", nil)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s /healthz status code = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
			}

			allow := rec.Header().Get("Allow")
			if allow != http.MethodGet {
				t.Fatalf("%s /healthz Allow header = %q, want %q", method, allow, http.MethodGet)
			}

			contentType := rec.Header().Get("Content-Type")
			wantContentType := "application/json; charset=utf-8"
			if contentType != wantContentType {
				t.Fatalf("%s /healthz Content-Type = %q, want %q", method, contentType, wantContentType)
			}

			var resp errorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("%s /healthz failed to decode JSON: %v", method, err)
			}

			if resp.Error != "method not allowed" {
				t.Fatalf("%s /healthz error = %q, want %q", method, resp.Error, "method not allowed")
			}
		})
	}
}

func TestUnknownPathsReturnNotFound(t *testing.T) {
	paths := []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/"},
		{method: http.MethodGet, path: "/health"},
		{method: http.MethodGet, path: "/healthz/"},
		{method: http.MethodGet, path: "/unknown"},
		{method: http.MethodPost, path: "/unknown"},
		{method: http.MethodGet, path: "/api/v1/jobs"},
	}

	handler := NewHandler()

	for _, tc := range paths {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s status code = %d, want %d", tc.method, tc.path, rec.Code, http.StatusNotFound)
			}
		})
	}
}
