package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/cloud-backend/internal/ws"
)

func setupStrictBindingServer(t *testing.T) (*appServer, *httptest.Server, *store.Store, string) {
	t.Helper()
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "strict_binding.db")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}

	kioskSecret := "strict_binding_kiosk_secret_12345"
	hub := ws.NewHub(st)
	hub.SetKioskSecret([]byte(kioskSecret))

	app := &appServer{
		cfg: &serverConfig{
			addr:           ":0",
			dbPath:         dbPath,
			jobsDir:        filepath.Join(tempDir, "jobs"),
			devMode:        true,
			downloadSecret: []byte("strict_binding_download_secret_123"),
			kioskSecret:    []byte(kioskSecret),
		},
		db:         st,
		hub:        hub,
		convWake:   make(chan struct{}, 1),
		pinLimiter: NewPINRateLimiter(st, 10, time.Minute, time.Minute),
	}

	ts := httptest.NewServer(app.routes())
	t.Cleanup(func() {
		ts.Close()
		_ = st.Close()
	})
	return app, ts, st, kioskSecret
}

func doKioskPOST(t *testing.T, url, secret string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		t.Fatalf("NewRequest(%s): %v", url, err)
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do(%s): %v", url, err)
	}
	return resp
}

func claimSession(t *testing.T, tsURL, kioskID, orderID, secret string) string {
	t.Helper()
	resp := doKioskPOST(t, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", tsURL, kioskID, orderID), secret)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim expected 200 OK, got %d", resp.StatusCode)
	}
	var body struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	if body.SessionID == "" {
		t.Fatal("claim returned empty session_id")
	}
	return body.SessionID
}

// Strict session binding suite exercised through production app.routes().
func TestStrictSessionBinding_RoutesAndIsolation(t *testing.T) {
	_, ts, st, secret := setupStrictBindingServer(t)
	ctx := context.Background()

	t.Run("valid session generates demo token and repeated initiation is idempotent", func(t *testing.T) {
		kioskID := "kiosk-bind-valid"
		orderID := "ord_bind_valid"
		createRoutingTestOrder(t, st, kioskID, orderID, "1111", 2, 1000)

		sessID := claimSession(t, ts.URL, kioskID, orderID, secret)
		initURL := fmt.Sprintf("%s/api/k/%s/sessions/%s/initiate-payment", ts.URL, kioskID, sessID)

		r1 := doKioskPOST(t, initURL, secret)
		defer r1.Body.Close()
		if r1.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", r1.StatusCode)
		}
		var b1 map[string]interface{}
		_ = json.NewDecoder(r1.Body).Decode(&b1)
		if b1["session_id"] != sessID {
			t.Errorf("session_id = %v, want %s", b1["session_id"], sessID)
		}
		tok1, _ := b1["demo_token"].(string)
		if !strings.HasPrefix(tok1, "dmo_") || len(tok1) != 52 {
			t.Errorf("invalid demo_token %q", tok1)
		}

		// Idempotent repeat for the exact same session returns the same attempt and demo_token.
		r2 := doKioskPOST(t, initURL, secret)
		defer r2.Body.Close()
		if r2.StatusCode != http.StatusOK {
			t.Fatalf("repeat expected 200 OK, got %d", r2.StatusCode)
		}
		var b2 map[string]interface{}
		_ = json.NewDecoder(r2.Body).Decode(&b2)
		if b2["attempt_id"] != b1["attempt_id"] || b2["demo_token"] != tok1 {
			t.Errorf("repeat initiation broke idempotency: first=%v second=%v", b1, b2)
		}
	})

	t.Run("canceled Session A cannot substitute newly active Session B on delayed request", func(t *testing.T) {
		kioskID := "kiosk-bind-cancel"
		orderID := "ord_bind_cancel"
		createRoutingTestOrder(t, st, kioskID, orderID, "2222", 2, 1000)

		sessA := claimSession(t, ts.URL, kioskID, orderID, secret)

		cancelResp := doKioskPOST(t, fmt.Sprintf("%s/api/k/%s/sessions/%s/cancel", ts.URL, kioskID, sessA), secret)
		cancelResp.Body.Close()
		if cancelResp.StatusCode != http.StatusOK {
			t.Fatalf("cancel Session A expected 200, got %d", cancelResp.StatusCode)
		}

		// Session B is now active for the same order!
		sessB := claimSession(t, ts.URL, kioskID, orderID, secret)
		if sessB == sessA {
			t.Fatalf("expected fresh Session B != canceled Session A (%s)", sessA)
		}

		// Delayed initiate-payment targeting Session A MUST be rejected and MUST NOT touch Session B.
		staleResp := doKioskPOST(t, fmt.Sprintf("%s/api/k/%s/sessions/%s/initiate-payment", ts.URL, kioskID, sessA), secret)
		defer staleResp.Body.Close()
		if staleResp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 Conflict for delayed canceled Session A, got %d", staleResp.StatusCode)
		}

		// Verify no attempt was created or linked for either Session A or Session B yet.
		attA, err := st.GetActivePaymentAttempt(ctx, sessA, time.Now().UTC())
		if err != nil || attA != nil {
			t.Fatalf("expected nil attempt for Session A, got %+v (err=%v)", attA, err)
		}
		attB, err := st.GetActivePaymentAttempt(ctx, sessB, time.Now().UTC())
		if err != nil || attB != nil {
			t.Fatalf("rejected Session A request created attempt for Session B: %+v (err=%v)", attB, err)
		}

		// Session B can independently initiate its own payment attempt.
		respB := doKioskPOST(t, fmt.Sprintf("%s/api/k/%s/sessions/%s/initiate-payment", ts.URL, kioskID, sessB), secret)
		defer respB.Body.Close()
		if respB.StatusCode != http.StatusOK {
			t.Fatalf("Session B initiate-payment expected 200 OK, got %d", respB.StatusCode)
		}
		var bodyB map[string]interface{}
		_ = json.NewDecoder(respB.Body).Decode(&bodyB)
		if bodyB["session_id"] != sessB {
			t.Errorf("Session B response session_id = %v, want %s", bodyB["session_id"], sessB)
		}
	})

	t.Run("expired Session A cannot substitute newly active Session B on stale request", func(t *testing.T) {
		kioskID := "kiosk-bind-exp"
		orderID := "ord_bind_exp"
		createRoutingTestOrder(t, st, kioskID, orderID, "3333", 2, 1000)

		sessA := claimSession(t, ts.URL, kioskID, orderID, secret)

		// Force Session A past expiry so reclaiming creates Session B.
		pastNS := time.Now().UTC().Add(-5 * time.Minute).UnixNano()
		if _, err := st.DB().ExecContext(ctx, "UPDATE kiosk_sessions SET expires_at_ns = ? WHERE session_id = ?;", pastNS, sessA); err != nil {
			t.Fatalf("expire Session A in DB: %v", err)
		}

		sessB := claimSession(t, ts.URL, kioskID, orderID, secret)
		if sessB == sessA {
			t.Fatalf("expected new Session B != expired Session A (%s)", sessA)
		}

		// Stale initiate-payment targeting expired Session A MUST fail with 409 Conflict without touching Session B.
		staleResp := doKioskPOST(t, fmt.Sprintf("%s/api/k/%s/sessions/%s/initiate-payment", ts.URL, kioskID, sessA), secret)
		defer staleResp.Body.Close()
		if staleResp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 Conflict for stale expired Session A, got %d", staleResp.StatusCode)
		}

		attB, err := st.GetActivePaymentAttempt(ctx, sessB, time.Now().UTC())
		if err != nil || attB != nil {
			t.Fatalf("stale Session A request leaked attempt onto Session B: %+v (err=%v)", attB, err)
		}
	})

	t.Run("wrong kiosk session rejected with 403 without creating attempt", func(t *testing.T) {
		ownerKiosk := "kiosk-bind-owner"
		otherKiosk := "kiosk-bind-other"
		orderID := "ord_bind_wrong_kiosk"
		createRoutingTestOrder(t, st, ownerKiosk, orderID, "4444", 1, 500)

		sessID := claimSession(t, ts.URL, ownerKiosk, orderID, secret)

		resp := doKioskPOST(t, fmt.Sprintf("%s/api/k/%s/sessions/%s/initiate-payment", ts.URL, otherKiosk, sessID), secret)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden for wrong kiosk, got %d", resp.StatusCode)
		}

		att, err := st.GetActivePaymentAttempt(ctx, sessID, time.Now().UTC())
		if err != nil || att != nil {
			t.Fatalf("wrong kiosk request created attempt: %+v (err=%v)", att, err)
		}
	})

	t.Run("unknown and completed sessions rejected without creating attempt", func(t *testing.T) {
		kioskID := "kiosk-bind-states"
		unknownResp := doKioskPOST(t, fmt.Sprintf("%s/api/k/%s/sessions/sess_unknown_999/initiate-payment", ts.URL, kioskID), secret)
		unknownResp.Body.Close()
		if unknownResp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 Conflict for unknown session, got %d", unknownResp.StatusCode)
		}

		orderID := "ord_bind_completed"
		createRoutingTestOrder(t, st, kioskID, orderID, "5555", 1, 500)
		sessID := claimSession(t, ts.URL, kioskID, orderID, secret)

		if _, err := st.DB().ExecContext(ctx, "UPDATE kiosk_sessions SET status = 'COMPLETED' WHERE session_id = ?;", sessID); err != nil {
			t.Fatalf("mark session COMPLETED: %v", err)
		}

		compResp := doKioskPOST(t, fmt.Sprintf("%s/api/k/%s/sessions/%s/initiate-payment", ts.URL, kioskID, sessID), secret)
		compResp.Body.Close()
		if compResp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 Conflict for COMPLETED session, got %d", compResp.StatusCode)
		}

		att, err := st.GetActivePaymentAttempt(ctx, sessID, time.Now().UTC())
		if err != nil || att != nil {
			t.Fatalf("completed session created attempt: %+v (err=%v)", att, err)
		}
	})

	t.Run("removed order-scoped endpoint is unavailable on production routes", func(t *testing.T) {
		kioskID := "kiosk-bind-removed"
		orderID := "ord_bind_removed"
		createRoutingTestOrder(t, st, kioskID, orderID, "6666", 1, 500)
		sessID := claimSession(t, ts.URL, kioskID, orderID, secret)

		resp := doKioskPOST(t, fmt.Sprintf("%s/api/k/%s/orders/%s/initiate-payment", ts.URL, kioskID, orderID), secret)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 Not Found for removed order-scoped endpoint, got %d", resp.StatusCode)
		}

		att, err := st.GetActivePaymentAttempt(ctx, sessID, time.Now().UTC())
		if err != nil || att != nil {
			t.Fatalf("removed order endpoint created attempt: %+v (err=%v)", att, err)
		}
	})
}

func TestStrictSessionBinding_ActiveAttemptDBErrorFailsClosed(t *testing.T) {
	_, ts, st, secret := setupStrictBindingServer(t)
	ctx := context.Background()

	kioskID := "kiosk-bind-dberr"
	orderID := "ord_bind_dberr"
	createRoutingTestOrder(t, st, kioskID, orderID, "7777", 1, 500)
	sessID := claimSession(t, ts.URL, kioskID, orderID, secret)

	// Force GetActivePaymentAttempt query error on payment_attempts table.
	if _, err := st.DB().ExecContext(ctx, "DROP TABLE payment_attempts;"); err != nil {
		t.Fatalf("drop payment_attempts: %v", err)
	}

	resp := doKioskPOST(t, fmt.Sprintf("%s/api/k/%s/sessions/%s/initiate-payment", ts.URL, kioskID, sessID), secret)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 Internal Server Error when GetActivePaymentAttempt fails, got %d", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body["code"] != "DATABASE_ERROR" {
		t.Errorf("code = %q, want DATABASE_ERROR", body["code"])
	}
}
