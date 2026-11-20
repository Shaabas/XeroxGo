package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket/wsjson"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

func TestMultiDocumentPaperCheckAndMockPaymentQR(t *testing.T) {
	_, server, st, secret := setupPaperTestServer(t)
	kioskID := "kiosk-multidoc-payment"
	orderID := "ord_multidoc_payment"
	now := time.Now().UTC()
	docs := make([]store.OrderDocument, 0, 2)
	for i, sheets := range []int{1, 2} {
		docID := fmt.Sprintf("job_multidoc_%d", i)
		docs = append(docs, store.OrderDocument{
			JobID: docID, OrderID: orderID, DocumentIndex: i,
			OriginalFilename: fmt.Sprintf("doc_%d.pdf", i),
			DownloadURL: "https://storage.test/" + docID + ".pdf",
			SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			TotalPages: sheets, TotalSheets: sheets, Copies: 1,
			Duplex: contracts.DuplexSimplex, ColorMode: "BW",
			Orientation: "AUTO", PagesPerSheet: 1,
			PricePaise: int64(sheets * 250), Status: contracts.JobStatusPendingPayment,
		})
	}
	first := docs[0]
	order := store.Order{
		OrderID: orderID, Documents: docs,
		TotalPages: 3, TotalSheets: 3, PricePaise: 750,
		Status: contracts.JobStatusPendingPayment, PINExpiresAt: now.Add(15 * time.Minute),
		Command: contracts.PrintJobCommand{
			JobID: first.JobID, OrderID: orderID, KioskID: kioskID,
			DisplayCode: "8181", TotalDocuments: 2,
			DownloadURL: first.DownloadURL, SHA256Checksum: first.SHA256Checksum,
			TotalPages: 1, TotalSheets: 1, Copies: 1,
			Duplex: contracts.DuplexSimplex, StartFromPage: 1, ColorMode: "BW",
		},
	}
	if err := st.CreateOrder(context.Background(), order); err != nil {
		t.Fatalf("seed two-document order: %v", err)
	}

	conn, cancel := connectKioskWithHealth(t, server.URL, kioskID, contracts.PrinterHealth{
		Online: true, State: "IDLE", PaperTrayStatus: "LOW",
		EstimatedSheets: 2, UpdatedAt: time.Now().UTC(),
	})
	defer func() { cancel(); _ = conn.CloseNow() }()

	post := func(path string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	claimPath := fmt.Sprintf("/api/k/%s/orders/%s/claim", kioskID, orderID)
	resp := post(claimPath)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("claim with two sheets: got %d, want 409", resp.StatusCode)
	}
	var rejected struct{ Code string `json:"code"` }
	if err := json.NewDecoder(resp.Body).Decode(&rejected); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if rejected.Code != "INSUFFICIENT_PAPER" {
		t.Fatalf("unexpected shortage code: %s", rejected.Code)
	}

	// Three available sheets can satisfy both child documents.
	err := wsjson.Write(context.Background(), conn, wireMsg{
		Type: "health", Health: &contracts.PrinterHealth{
			Online: true, State: "IDLE", PaperTrayStatus: "LOW",
			EstimatedSheets: 3, UpdatedAt: time.Now().UTC(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	resp = post(claimPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim with three sheets: got %d, want 200", resp.StatusCode)
	}
	var sess sessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if sess.SessionID == "" || sess.TotalPricePaise != 750 {
		t.Fatalf("invalid claimed session: id_empty=%v price=%d", sess.SessionID == "", sess.TotalPricePaise)
	}

	resp = post(fmt.Sprintf("/api/k/%s/sessions/%s/initiate-payment", kioskID, sess.SessionID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initiate mock payment: got %d, want 200", resp.StatusCode)
	}
	var attempt struct {
		Provider string `json:"provider"`
		QRPayload string `json:"qr_payload"`
		AmountPaise int64 `json:"amount_paise"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&attempt); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if attempt.Provider != "mock" || attempt.QRPayload == "" || attempt.AmountPaise != 750 {
		t.Fatalf("invalid mock payment QR response: provider=%q qr_empty=%v amount=%d", attempt.Provider, attempt.QRPayload == "", attempt.AmountPaise)
	}
}
