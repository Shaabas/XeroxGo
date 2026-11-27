package store

import (
	"context"
	"sync"
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

func TestPackage6A_ConcurrentUpdateJobProgress_OrderingsImmutabilityAndMultiDocConsistency(t *testing.T) {
	t.Run("concurrent_progress_updates_in_both_orderings", func(t *testing.T) {
		for _, iter := range []int{0, 1, 2, 3, 4} {
			s, _ := newTestStore(t)
			ctx := context.Background()
			now := time.Now().UTC()
			ordID := "ord-6a-conc-ord"
			jobID := "job-6a-conc-ord"
			ord := Order{
				OrderID: ordID, Status: contracts.JobStatusPaidWaiting, PricePaise: 500,
				TotalPages: 5, TotalSheets: 5, ColorMode: "BW", PINExpiresAt: now.Add(10 * time.Minute),
				Documents: []OrderDocument{{
					JobID: jobID, OrderID: ordID, DocumentIndex: 0,
					OriginalFilename: "doc.pdf", Duplex: contracts.DuplexSimplex, ColorMode: "BW",
					TotalPages: 5, TotalSheets: 5, Copies: 1, PricePaise: 500,
				}},
				Command:   PrintJobCommandCompat(jobID, ordID, "kiosk-6a-conc"),
				CreatedAt: now, UpdatedAt: now,
			}
			if err := s.CreateOrder(ctx, ord); err != nil {
				t.Fatalf("iter %d CreateOrder failed: %v", iter, err)
			}

			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				_ = s.UpdateJobProgress(ctx, contracts.JobProgressEvent{
					JobID: jobID, KioskID: "kiosk-6a-conc", Status: contracts.JobStatusPrinting,
					PagesPrinted: 1, SheetsPrinted: 1, Timestamp: now.Add(time.Second),
				})
			}()
			go func() {
				defer wg.Done()
				<-start
				_ = s.UpdateJobProgress(ctx, contracts.JobProgressEvent{
					JobID: jobID, KioskID: "kiosk-6a-conc", Status: contracts.JobStatusPrinting,
					PagesPrinted: 3, SheetsPrinted: 3, Timestamp: now.Add(2 * time.Second),
				})
			}()
			close(start)
			wg.Wait()

			got, err := s.GetJobProgress(ctx, jobID)
			if err != nil || got == nil {
				t.Fatalf("iter %d GetJobProgress failed: %v", iter, err)
			}
			if got.PagesPrinted != 3 || got.SheetsPrinted != 3 {
				t.Fatalf("iter %d expected final progress 3/3 regardless of arrival order, got %d/%d", iter, got.PagesPrinted, got.SheetsPrinted)
			}
		}
	})

	t.Run("concurrent_completed_and_printing_never_regress_completed", func(t *testing.T) {
		for _, iter := range []int{0, 1, 2, 3, 4} {
			s, _ := newTestStore(t)
			ctx := context.Background()
			now := time.Now().UTC()
			ordID := "ord-6a-conc-comp"
			jobID := "job-6a-conc-comp"
			ord := Order{
				OrderID: ordID, Status: contracts.JobStatusPaidWaiting, PricePaise: 200,
				TotalPages: 2, TotalSheets: 2, ColorMode: "BW", PINExpiresAt: now.Add(10 * time.Minute),
				Documents: []OrderDocument{{
					JobID: jobID, OrderID: ordID, DocumentIndex: 0,
					OriginalFilename: "doc.pdf", Duplex: contracts.DuplexSimplex, ColorMode: "BW",
					TotalPages: 2, TotalSheets: 2, Copies: 1, PricePaise: 200,
				}},
				Command:   PrintJobCommandCompat(jobID, ordID, "kiosk-6a-conc2"),
				CreatedAt: now, UpdatedAt: now,
			}
			if err := s.CreateOrder(ctx, ord); err != nil {
				t.Fatalf("iter %d CreateOrder failed: %v", iter, err)
			}

			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				_ = s.UpdateJobProgress(ctx, contracts.JobProgressEvent{
					JobID: jobID, KioskID: "kiosk-6a-conc2", Status: contracts.JobStatusCompleted,
					PagesPrinted: 2, SheetsPrinted: 2, Timestamp: now.Add(5 * time.Second),
				})
			}()
			go func() {
				defer wg.Done()
				<-start
				_ = s.UpdateJobProgress(ctx, contracts.JobProgressEvent{
					JobID: jobID, KioskID: "kiosk-6a-conc2", Status: contracts.JobStatusPrinting,
					PagesPrinted: 1, SheetsPrinted: 1, Timestamp: now.Add(2 * time.Second),
				})
			}()
			close(start)
			wg.Wait()

			// Even a subsequent PRINTING event with a newer timestamp cannot regress COMPLETED.
			if err := s.UpdateJobProgress(ctx, contracts.JobProgressEvent{
				JobID: jobID, KioskID: "kiosk-6a-conc2", Status: contracts.JobStatusPrinting,
				PagesPrinted: 1, SheetsPrinted: 1, Timestamp: now.Add(10 * time.Second),
			}); err == nil {
				t.Fatalf("iter %d expected error regressing COMPLETED to PRINTING", iter)
			}

			got, err := s.GetJobProgress(ctx, jobID)
			if err != nil || got == nil {
				t.Fatalf("iter %d GetJobProgress failed: %v", iter, err)
			}
			if got.Status != contracts.JobStatusCompleted || got.PagesPrinted != 2 || got.SheetsPrinted != 2 {
				t.Fatalf("iter %d expected COMPLETED 2/2, got %+v", iter, got)
			}
			gotOrd, err := s.GetOrder(ctx, ordID)
			if err != nil || gotOrd == nil || gotOrd.Status != contracts.JobStatusCompleted {
				t.Fatalf("iter %d expected order COMPLETED, got %+v (err=%v)", iter, gotOrd, err)
			}
		}
	})

	t.Run("atomic_multi_doc_order_status_consistency", func(t *testing.T) {
		s, _ := newTestStore(t)
		ctx := context.Background()
		now := time.Now().UTC()
		ordID := "ord-6a-multidoc"
		ord := Order{
			OrderID: ordID, Status: contracts.JobStatusPaidWaiting, PricePaise: 400,
			TotalPages: 4, TotalSheets: 4, ColorMode: "BW", PINExpiresAt: now.Add(10 * time.Minute),
			Documents: []OrderDocument{
				{
					JobID: "job-6a-md-0", OrderID: ordID, DocumentIndex: 0,
					OriginalFilename: "a.pdf", Duplex: contracts.DuplexSimplex, ColorMode: "BW",
					TotalPages: 2, TotalSheets: 2, Copies: 1, PricePaise: 200,
				},
				{
					JobID: "job-6a-md-1", OrderID: ordID, DocumentIndex: 1,
					OriginalFilename: "b.pdf", Duplex: contracts.DuplexSimplex, ColorMode: "BW",
					TotalPages: 2, TotalSheets: 2, Copies: 1, PricePaise: 200,
				},
			},
			Command:   PrintJobCommandCompat("job-6a-md-0", ordID, "kiosk-6a-md"),
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateOrder(ctx, ord); err != nil {
			t.Fatalf("CreateOrder failed: %v", err)
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if err := s.UpdateJobProgress(ctx, contracts.JobProgressEvent{
				JobID: "job-6a-md-0", KioskID: "kiosk-6a-md", Status: contracts.JobStatusCompleted,
				PagesPrinted: 2, SheetsPrinted: 2, Timestamp: now.Add(time.Second),
			}); err != nil {
				t.Errorf("doc 0 complete failed: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if err := s.UpdateJobProgress(ctx, contracts.JobProgressEvent{
				JobID: "job-6a-md-1", KioskID: "kiosk-6a-md", Status: contracts.JobStatusPrinting,
				PagesPrinted: 1, SheetsPrinted: 1, Timestamp: now.Add(2 * time.Second),
			}); err != nil {
				t.Errorf("doc 1 printing failed: %v", err)
			}
		}()
		close(start)
		wg.Wait()

		gotOrd, err := s.GetOrder(ctx, ordID)
		if err != nil || gotOrd.Status != contracts.JobStatusPrinting {
			t.Fatalf("expected order status PRINTING when 1 of 2 docs completed, got %q (err=%v)", gotOrd.Status, err)
		}

		if err := s.UpdateJobProgress(ctx, contracts.JobProgressEvent{
			JobID: "job-6a-md-1", KioskID: "kiosk-6a-md", Status: contracts.JobStatusCompleted,
			PagesPrinted: 2, SheetsPrinted: 2, Timestamp: now.Add(3 * time.Second),
		}); err != nil {
			t.Fatalf("doc 1 complete failed: %v", err)
		}

		gotOrd, err = s.GetOrder(ctx, ordID)
		if err != nil || gotOrd.Status != contracts.JobStatusCompleted {
			t.Fatalf("expected order status COMPLETED when all docs completed, got %q (err=%v)", gotOrd.Status, err)
		}
	})
}

func PrintJobCommandCompat(jobID, orderID, kioskID string) contracts.PrintJobCommand {
	return contracts.PrintJobCommand{
		JobID: jobID, OrderID: orderID, KioskID: kioskID, DisplayCode: "6262",
		TotalPages: 2, TotalSheets: 2, Copies: 1, Duplex: contracts.DuplexSimplex, ColorMode: "BW", StartFromPage: 1,
	}
}
