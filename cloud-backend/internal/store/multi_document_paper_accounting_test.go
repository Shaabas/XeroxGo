package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

// seedMultiDocumentPaperOrder creates an order where the print-config snapshot
// describes the first document but orders.total_sheets covers every document.
func seedMultiDocumentPaperOrder(t *testing.T, s *Store, orderID, kioskID string, sheets []int, now time.Time) []string {
	t.Helper()
	var docs []OrderDocument
	var jobIDs []string
	var totalSheets int
	var totalPrice int64
	for i, count := range sheets {
		jobID := fmt.Sprintf("job_%s_%d", orderID, i)
		jobIDs = append(jobIDs, jobID)
		docs = append(docs, OrderDocument{
			JobID: jobID, OrderID: orderID, DocumentIndex: i,
			OriginalFilename: fmt.Sprintf("document-%d.pdf", i+1),
			DownloadURL: "https://storage.test/" + jobID + ".pdf",
			SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			TotalPages: count, TotalSheets: count, Copies: 1,
			Duplex: contracts.DuplexSimplex, ColorMode: "BW",
			Orientation: "AUTO", PagesPerSheet: 1,
			PricePaise: int64(count * 250), Status: contracts.JobStatusPendingPayment,
		})
		totalSheets += count
		totalPrice += int64(count * 250)
	}
	if len(docs) == 0 {
		t.Fatal("test requires at least one document")
	}
	first := docs[0]
	cmd := contracts.PrintJobCommand{
		JobID: first.JobID, OrderID: orderID, KioskID: kioskID,
		DisplayCode: "9090", TotalDocuments: len(docs),
		DownloadURL: first.DownloadURL, SHA256Checksum: first.SHA256Checksum,
		TotalPages: first.TotalPages, TotalSheets: first.TotalSheets,
		Copies: 1, Duplex: contracts.DuplexSimplex, StartFromPage: 1, ColorMode: "BW",
	}
	ord := Order{
		OrderID: orderID, Command: cmd, Documents: docs,
		TotalPages: totalSheets, TotalSheets: totalSheets, PricePaise: totalPrice,
		Status: contracts.JobStatusPendingPayment, PINExpiresAt: now.Add(30 * time.Minute),
	}
	if err := s.CreateOrder(context.Background(), ord); err != nil {
		t.Fatalf("create multi-document order: %v", err)
	}
	return jobIDs
}

func TestStore_MultiDocumentClaimUsesWholeOrderSheets(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "multi-doc-claim.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	kioskID := "kiosk-multi-doc-stock"
	orderID := "ord_multi_doc_stock"

	seedMultiDocumentPaperOrder(t, s, orderID, kioskID, []int{1, 2}, now)
	var orderSheets, configSheets int
	err = s.db.QueryRowContext(ctx, s.rebind(`SELECT o.total_sheets, pc.total_sheets
FROM orders o JOIN print_configs pc ON pc.order_id = o.order_id WHERE o.order_id = ?`), orderID).Scan(&orderSheets, &configSheets)
	if err != nil {
		t.Fatal(err)
	}
	if orderSheets != 3 || configSheets != 1 {
		t.Fatalf("test fixture must differ: order=%d first-doc-config=%d", orderSheets, configSheets)
	}

	if _, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 2, 5*time.Minute, now); !errors.Is(err, ErrInsufficientPaper) {
		t.Fatalf("2 available sheets must not claim a 3-sheet order: %v", err)
	}
	if _, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 3, 5*time.Minute, now); err != nil {
		t.Fatalf("3 available sheets should permit claim: %v", err)
	}
	if got, err := s.GetCommittedPaperSheets(ctx, kioskID, "", now); err != nil || got != 3 {
		t.Fatalf("multi-document unpaid commitment = %d, err=%v; want 3", got, err)
	}

	createTestOrderWithSheets(t, s, ctx, "ord_competing", kioskID, "9091", 1, contracts.JobStatusPendingPayment, now)
	if _, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, "ord_competing", 3, 5*time.Minute, now); !errors.Is(err, ErrInsufficientPaper) {
		t.Fatalf("competing order must be rejected when all 3 sheets committed: %v", err)
	}
	if _, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, "ord_competing", 4, 5*time.Minute, now); err != nil {
		t.Fatalf("4 sheets should allow 3+1 commitments: %v", err)
	}
}

func TestStore_MultiDocumentPaidRemainingSheets(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "multi-doc-progress.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	kioskID := "kiosk-multi-doc-progress"
	orderID := "ord_multi_doc_progress"
	jobIDs := seedMultiDocumentPaperOrder(t, s, orderID, kioskID, []int{2, 3}, now)

	if _, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 5, 5*time.Minute, now); err != nil {
		t.Fatalf("claim order: %v", err)
	}
	if err := s.UpdateOrderStatus(ctx, orderID, contracts.JobStatusPaidWaiting); err != nil {
		t.Fatalf("mark order paid-waiting: %v", err)
	}
	checkCommitment := func(want int) {
		t.Helper()
		got, err := s.GetCommittedPaperSheets(ctx, kioskID, "", now)
		if err != nil || got != want {
			t.Fatalf("paid remaining commitment = %d, err=%v; want %d", got, err, want)
		}
	}
	checkCommitment(5)

	for i, printed := range []int{1, 2} {
		_, err := s.db.ExecContext(ctx, s.rebind(`
INSERT INTO job_progress (job_id, kiosk_id, status, pages_printed, sheets_printed, error_reason, timestamp_ns)
VALUES (?, ?, 'PRINTING', ?, ?, '', ?)`), jobIDs[i], kioskID, printed, printed, now.UnixNano())
		if err != nil {
			t.Fatalf("record document %d progress: %v", i, err)
		}
	}
	// Both child jobs' progress must be deducted (5 - 1 - 2).
	checkCommitment(2)

	_, err = s.db.ExecContext(ctx, s.rebind(`
UPDATE job_progress SET sheets_printed = 3, pages_printed = 3 WHERE job_id = ?`), jobIDs[1])
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.ExecContext(ctx, s.rebind(`
UPDATE job_progress SET sheets_printed = 2, pages_printed = 2 WHERE job_id = ?`), jobIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	checkCommitment(0)
}

func TestStore_ClaimRejectsInvalidOrderSheetTotal(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "invalid-total.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	kioskID := "kiosk-invalid-total"
	orderID := "ord_invalid_total"
	createTestOrderWithSheets(t, s, ctx, orderID, kioskID, "8181", 4, contracts.JobStatusPendingPayment, now)

	// An invalid order total must never be silently replaced with first-document
	// totals, even when the printer reports enough paper.
	if _, err := s.db.ExecContext(ctx, s.rebind("UPDATE orders SET total_sheets = 0 WHERE order_id = ?"), orderID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 100, 5*time.Minute, now); err == nil {
		t.Fatal("claim must fail closed when orders.total_sheets is not positive")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, s.rebind("SELECT COUNT(*) FROM kiosk_sessions WHERE order_id = ?"), orderID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("invalid order created %d payment sessions; want 0", count)
	}
}
