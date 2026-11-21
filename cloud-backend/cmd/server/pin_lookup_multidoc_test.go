package main

import (
 "context"
 "encoding/json"
 "fmt"
 "net/http"
 "net/http/httptest"
 "strings"
 "testing"
 "time"

 "xeroxgo/cloud-backend/internal/store"
 "xeroxgo/pkg/contracts"
)

func TestPINLookup_MultiDocumentAggregatePagesAndSheets(t *testing.T) {
 app, db, _ := newTestPINServer(t, "")
 const kioskID = "kiosk-multidoc-lookup"
 const orderID = "ord-multidoc-lookup"
 const pin = "5749"
 var docs []store.OrderDocument
 for i, pages := range []int{4, 7} {
  sheets := i+2
  docs = append(docs, store.OrderDocument{
   JobID: fmt.Sprintf("job-lookup-%d", i), OrderID: orderID, DocumentIndex: i,
   OriginalFilename: fmt.Sprintf("file-%d.pdf", i),
   DownloadURL: fmt.Sprintf("https://storage.test/doc-%d.pdf", i),
   SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
   TotalPages: pages, TotalSheets: sheets, Copies: 1,
   Duplex: contracts.DuplexSimplex, ColorMode: "BW",
   Orientation: "AUTO", PagesPerSheet: 1,
   PricePaise: int64(sheets*200), Status: contracts.JobStatusPendingPayment,
  })
 }
 first := docs[0]
 order := store.Order{
  OrderID: orderID, Documents: docs, TotalPages: 11, TotalSheets: 5,
  PricePaise: 1000, Status: contracts.JobStatusPendingPayment,
  PINExpiresAt: time.Now().UTC().Add(customerPINValidity),
  Command: contracts.PrintJobCommand{
   JobID: first.JobID, KioskID: kioskID, OrderID: orderID, DisplayCode: pin,
   DownloadURL: first.DownloadURL, SHA256Checksum: first.SHA256Checksum,
   TotalPages: first.TotalPages, TotalSheets: first.TotalSheets,
   Copies: 1, Duplex: contracts.DuplexSimplex, StartFromPage: 1, ColorMode: "BW",
  },
 }
 if err := db.CreateOrder(context.Background(), order); err != nil {
  t.Fatal(err)
 }
 stored, err := db.GetOrder(context.Background(), orderID)
 if err != nil || stored == nil { t.Fatalf("get order: %v", err) }
 if stored.Command.TotalPages != 4 || stored.Command.TotalSheets != 2 {
  t.Fatalf("first document command unexpectedly changed: %+v", stored.Command)
 }
 req := httptest.NewRequest(http.MethodPost,
  "/api/k/"+kioskID+"/orders/lookup", strings.NewReader("{\"pin\":\"5749\"}"))
 w := httptest.NewRecorder()
 app.handlePINLookup(w, req)
 if w.Code != http.StatusOK { t.Fatalf("lookup status %d: %s", w.Code, w.Body.String()) }
 var got pinLookupResponse
 if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil { t.Fatal(err) }
 if got.TotalPages != 11 || got.TotalSheets != 5 || got.TotalPricePaise != 1000 {
  t.Fatalf("checkout must show all documents: pages=%d sheets=%d price=%d",
   got.TotalPages, got.TotalSheets, got.TotalPricePaise)
 }
 if got.SecondsRemaining < 1 || got.SecondsRemaining > 300 {
  t.Fatalf("PIN time remaining=%d, want 1..300", got.SecondsRemaining)
 }
}
