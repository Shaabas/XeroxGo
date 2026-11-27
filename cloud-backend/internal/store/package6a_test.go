package store

import (
	"context"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestPackage6A_UpdateJobProgressRejectsInconsistentCompletionAndRegression(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	ord := Order{
		OrderID: "ord-6a-prog", Status: contracts.JobStatusPaidWaiting, PricePaise: 300,
		TotalPages: 2, TotalSheets: 2, ColorMode: "BW", PINExpiresAt: now.Add(10 * time.Minute),
		Documents: []OrderDocument{{
			JobID: "job-6a-prog", OrderID: "ord-6a-prog", DocumentIndex: 0,
			OriginalFilename: "doc.pdf", Duplex: contracts.DuplexSimplex, ColorMode: "BW",
			TotalPages: 2, TotalSheets: 2, Copies: 1, PricePaise: 300,
		}},
		Command: PrintJobCommandCompat("job-6a-prog", "ord-6a-prog", "kiosk-6a-cloud"),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	// 1. Missing/zero counts on COMPLETED must be rejected (no synthetic substitution).
	errZero := s.UpdateJobProgress(ctx, contracts.JobProgressEvent{
		JobID: "job-6a-prog", KioskID: "kiosk-6a-cloud", Status: contracts.JobStatusCompleted,
		PagesPrinted: 0, SheetsPrinted: 0, Timestamp: now.Add(time.Second),
	})
	if errZero == nil {
		t.Fatalf("expected error rejecting COMPLETED event with 0/0 counts")
	}

	// 2. Partial counts on COMPLETED must be rejected.
	errPartial := s.UpdateJobProgress(ctx, contracts.JobProgressEvent{
		JobID: "job-6a-prog", KioskID: "kiosk-6a-cloud", Status: contracts.JobStatusCompleted,
		PagesPrinted: 1, SheetsPrinted: 1, Timestamp: now.Add(2 * time.Second),
	})
	if errPartial == nil {
		t.Fatalf("expected error rejecting COMPLETED event with partial 1/2 counts")
	}

	// 3. Valid COMPLETED event with full 2/2 counts succeeds.
	if err := s.UpdateJobProgress(ctx, contracts.JobProgressEvent{
		JobID: "job-6a-prog", KioskID: "kiosk-6a-cloud", Status: contracts.JobStatusCompleted,
		PagesPrinted: 2, SheetsPrinted: 2, Timestamp: now.Add(3 * time.Second),
	}); err != nil {
		t.Fatalf("expected valid 2/2 COMPLETED event to succeed, got %v", err)
	}

	// 4. Delayed/non-COMPLETED event cannot regress a COMPLETED document.
	errRegress := s.UpdateJobProgress(ctx, contracts.JobProgressEvent{
		JobID: "job-6a-prog", KioskID: "kiosk-6a-cloud", Status: contracts.JobStatusPrinting,
		PagesPrinted: 1, SheetsPrinted: 1, Timestamp: now.Add(4 * time.Second),
	})
	if errRegress == nil {
		t.Fatalf("expected error rejecting regression of COMPLETED job to PRINTING")
	}
}

func TestPackage6A_UpdateJobProgressRejectsMonotonicCountRegressionWithNewerTimestamp(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	ord := Order{
		OrderID: "ord-6a-mono", Status: contracts.JobStatusPaidWaiting, PricePaise: 500,
		TotalPages: 5, TotalSheets: 5, ColorMode: "BW", PINExpiresAt: now.Add(10 * time.Minute),
		Documents: []OrderDocument{{
			JobID: "job-6a-mono", OrderID: "ord-6a-mono", DocumentIndex: 0,
			OriginalFilename: "doc.pdf", Duplex: contracts.DuplexSimplex, ColorMode: "BW",
			TotalPages: 5, TotalSheets: 5, Copies: 1, PricePaise: 500,
		}},
		Command: PrintJobCommandCompat("job-6a-mono", "ord-6a-mono", "kiosk-6a-mono"),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	if err := s.UpdateJobProgress(ctx, contracts.JobProgressEvent{
		JobID: "job-6a-mono", KioskID: "kiosk-6a-mono", Status: contracts.JobStatusPrinting,
		PagesPrinted: 3, SheetsPrinted: 3, Timestamp: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("initial UpdateJobProgress failed: %v", err)
	}

	errRegress := s.UpdateJobProgress(ctx, contracts.JobProgressEvent{
		JobID: "job-6a-mono", KioskID: "kiosk-6a-mono", Status: contracts.JobStatusPrinting,
		PagesPrinted: 1, SheetsPrinted: 1, Timestamp: now.Add(5 * time.Second),
	})
	if errRegress == nil {
		t.Fatal("expected error rejecting count regression even with newer timestamp")
	}

	got, err := s.GetJobProgress(ctx, "job-6a-mono")
	if err != nil || got == nil {
		t.Fatalf("GetJobProgress failed: %v", err)
	}
	if got.PagesPrinted != 3 || got.SheetsPrinted != 3 {
		t.Fatalf("expected persisted counts 3/3 preserved, got %d/%d", got.PagesPrinted, got.SheetsPrinted)
	}
}

func PrintJobCommandCompat(jobID, orderID, kioskID string) contracts.PrintJobCommand {
	return contracts.PrintJobCommand{
		JobID: jobID, OrderID: orderID, KioskID: kioskID, DisplayCode: "6262",
		TotalPages: 2, TotalSheets: 2, Copies: 1, Duplex: contracts.DuplexSimplex, ColorMode: "BW", StartFromPage: 1,
	}
}
