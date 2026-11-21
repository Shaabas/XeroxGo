package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"xeroxgo/cloud-backend/internal/payments"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

// The test double advertises a real provider identity while counting calls
// without contacting the payment network. Dev/demo modes must not bypass
// the real-provider safety gate.
type realProviderPaymentSpy struct {
	*payments.MockProvider
}

func (p *realProviderPaymentSpy) Name() string { return "razorpay" }

func seedPaymentGateOrder(t *testing.T, st *store.Store, kioskID, orderID string, documentCount int) {
	t.Helper()
	const pricePerDocument = int64(250)
	if documentCount == 1 {
		seedTestOrder(t, st, orderID, kioskID, "5639", 1,
			contracts.JobStatusPendingPayment, pricePerDocument, time.Now().UTC().Add(5*time.Minute))
		return
	}
	docs := make([]store.OrderDocument, 0, documentCount)
	for i := 0; i < documentCount; i++ {
		jobID := fmt.Sprintf("job_%s_%d", orderID, i)
		docs = append(docs, store.OrderDocument{
			JobID: jobID, OrderID: orderID, DocumentIndex: i,
			OriginalFilename: fmt.Sprintf("doc_%d.pdf", i),
			DownloadURL: "https://storage.test/" + jobID + ".pdf",
			SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			TotalPages: 1, TotalSheets: 1, Copies: 1,
			Duplex: contracts.DuplexSimplex, ColorMode: "BW",
			PricePaise: pricePerDocument, Status: contracts.JobStatusPendingPayment,
		})
	}
	first := docs[0]
	order := store.Order{
		OrderID: orderID, Documents: docs,
		TotalPages: documentCount, TotalSheets: documentCount,
		PricePaise: int64(documentCount) * pricePerDocument,
		Status: contracts.JobStatusPendingPayment,
		PINExpiresAt: time.Now().UTC().Add(5 * time.Minute),
		Command: contracts.PrintJobCommand{
			JobID: first.JobID, OrderID: orderID, KioskID: kioskID,
			DisplayCode: "5639", DocumentIndex: 0, TotalDocuments: documentCount,
			DownloadURL: first.DownloadURL, SHA256Checksum: first.SHA256Checksum,
			TotalPages: 1, TotalSheets: 1, Copies: 1,
			Duplex: contracts.DuplexSimplex, StartFromPage: 1, ColorMode: "BW",
		},
	}
	if err := st.CreateOrder(context.Background(), order); err != nil {
		t.Fatalf("seed multi-document order: %v", err)
	}
}

func TestInitiatePayment_RealMultiDocumentGate(t *testing.T) {
	cases := []struct {
		name      string
		documents int
		real      bool
		devMode   bool
		demoMode  bool
		flag      string
		wantHTTP  int
	}{
		{name: "real provider multi-document disabled by default", documents: 2, real: true, wantHTTP: http.StatusConflict},
		{name: "real provider multi-document disabled in development", documents: 2, real: true, devMode: true, wantHTTP: http.StatusConflict},
		{name: "real provider multi-document disabled in demo mode", documents: 2, real: true, demoMode: true, wantHTTP: http.StatusConflict},
		{name: "non-explicit opt-in is rejected", documents: 2, real: true, flag: "TRUE", wantHTTP: http.StatusConflict},
		{name: "explicit opt-in permits real provider checkout", documents: 2, real: true, flag: "true", wantHTTP: http.StatusOK},
		{name: "single-document real checkout unaffected", documents: 1, real: true, wantHTTP: http.StatusOK},
		{name: "multi-document mock checkout remains available", documents: 2, real: false, wantHTTP: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XEROXGO_ENABLE_PUBLIC_MULTIDOC_PAYMENTS", tc.flag)
			app, server, st, secret := setupPaperTestServer(t)
			app.cfg.devMode = tc.devMode
			app.cfg.demoMode = tc.demoMode
			mock := payments.NewMockProvider("test_webhook_secret")
			if tc.real {
				app.paymentProvider = &realProviderPaymentSpy{MockProvider: mock}
			} else {
				app.paymentProvider = mock
			}

			kioskID := "kiosk_payment_gate"
			orderID := "ord_payment_gate"
			seedPaymentGateOrder(t, st, kioskID, orderID, tc.documents)
			conn, cancel := connectKioskWithHealth(t, server.URL, kioskID, contracts.PrinterHealth{
				Online: true, State: "IDLE", PaperTrayStatus: "OK",
				EstimatedSheets: 100, UpdatedAt: time.Now().UTC(),
			})
			defer func() { cancel(); _ = conn.CloseNow() }()
			session, err := st.CreateOrGetKioskSessionWithPaperCheck(
				context.Background(), kioskID, orderID, 100, 5*time.Minute, time.Now().UTC())
			if err != nil {
				t.Fatalf("create pending payment session: %v", err)
			}
			url := fmt.Sprintf("%s/api/k/%s/sessions/%s/initiate-payment", server.URL, kioskID, session.SessionID)
			req, err := http.NewRequest(http.MethodPost, url, nil)
			if err != nil { t.Fatal(err) }
			req.Header.Set("Authorization", "Bearer "+secret)
			resp, err := http.DefaultClient.Do(req)
			if err != nil { t.Fatal(err) }
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantHTTP {
				t.Fatalf("initiate payment HTTP %d, want %d", resp.StatusCode, tc.wantHTTP)
			}
			var response map[string]interface{}
			if err := json.NewDecoder(resp.Body).Decode(&response); err != nil { t.Fatal(err) }

			activeAttempt, err := st.GetActivePaymentAttempt(context.Background(), session.SessionID, time.Now().UTC())
			if err != nil { t.Fatal(err) }
			if tc.wantHTTP == http.StatusConflict {
				if response["code"] != "MULTIDOC_PAYMENTS_DISABLED" {
					t.Fatalf("missing clear public payment gate error: %+v", response)
				}
				if mock.CreatedCount != 0 || activeAttempt != nil {
					t.Fatalf("blocked real checkout touched provider or created attempt: calls=%d attempt=%+v",
						mock.CreatedCount, activeAttempt)
				}
			} else {
				if response["attempt_id"] == nil || response["attempt_id"] == "" {
					t.Fatalf("successful initiation missing payment attempt: %+v", response)
				}
				if mock.CreatedCount != 1 || activeAttempt == nil {
					t.Fatalf("permitted checkout did not persist exactly one attempt: calls=%d attempt=%+v",
						mock.CreatedCount, activeAttempt)
				}
			}
		})
	}
}
