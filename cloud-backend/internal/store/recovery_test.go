package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

func TestStoreRecoveryRecordsAndOrderByJobID(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "recovery_test.db")

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer s.Close()

	ctx := context.Background()

	// 1. Create an order
	order := store.Order{
		OrderID: "ord_rec_001",
		Command: contracts.PrintJobCommand{
			JobID:          "job_rec_001",
			KioskID:        "kiosk-demo",
			DisplayCode:    "1234",
			DownloadURL:    "http://example.com/pdf",
			SHA256Checksum: "abc123def456",
			TotalPages:     6,
			TotalSheets:    3,
			Copies:         2,
			Duplex:         contracts.DuplexLongEdge,
			StartFromPage:  1,
		},
		Status:     contracts.JobStatusDispatched,
		PricePaise: 450,
		ColorMode:  "BW",
	}

	if err := s.CreateOrder(ctx, order); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	// 2. Test GetOrderByJobID
	fetchedOrder, err := s.GetOrderByJobID(ctx, "job_rec_001")
	if err != nil {
		t.Fatalf("GetOrderByJobID failed: %v", err)
	}
	if fetchedOrder == nil {
		t.Fatal("expected order to be found by job_id")
	}
	if fetchedOrder.OrderID != "ord_rec_001" {
		t.Fatalf("expected orderID ord_rec_001, got %s", fetchedOrder.OrderID)
	}
	if fetchedOrder.Command.TotalSheets != 3 {
		t.Fatalf("expected TotalSheets=3, got %d", fetchedOrder.Command.TotalSheets)
	}

	// Non-existent job
	missingOrder, err := s.GetOrderByJobID(ctx, "job_missing")
	if err != nil {
		t.Fatalf("unexpected error for missing job: %v", err)
	}
	if missingOrder != nil {
		t.Fatalf("expected nil for missing job, got %+v", missingOrder)
	}

	// 3. Test GetLatestRecoverySequence initially 0
	seq0, err := s.GetLatestRecoverySequence(ctx, "job_rec_001")
	if err != nil {
		t.Fatalf("GetLatestRecoverySequence failed: %v", err)
	}
	if seq0 != 0 {
		t.Fatalf("expected initial sequence 0, got %d", seq0)
	}

	// 4. Create Recovery Record 1
	rec1 := store.RecoveryRecord{
		RecoveryID:             "rec_token_1",
		JobID:                  "job_rec_001",
		KioskID:                "kiosk-demo",
		SequenceID:             1,
		ConfirmedSheetsPrinted: 1,
		RemainingPages:         4,
		RemainingSheets:        2,
		DownloadURL:            "http://example.com/rec1.pdf",
		SHA256Checksum:         "hash111",
		Status:                 "CREATED",
	}
	if err := s.CreateRecoveryRecord(ctx, rec1); err != nil {
		t.Fatalf("CreateRecoveryRecord 1 failed: %v", err)
	}

	// Fetch back
	gotRec1, err := s.GetRecoveryRecord(ctx, "rec_token_1")
	if err != nil {
		t.Fatalf("GetRecoveryRecord failed: %v", err)
	}
	if gotRec1 == nil {
		t.Fatal("expected recovery record 1 to be found")
	}
	if gotRec1.SequenceID != 1 || gotRec1.RemainingSheets != 2 || gotRec1.ConfirmedSheetsPrinted != 1 {
		t.Fatalf("unexpected recovery record fields: %+v", gotRec1)
	}

	// Check sequence now is 1
	seq1, err := s.GetLatestRecoverySequence(ctx, "job_rec_001")
	if err != nil {
		t.Fatalf("GetLatestRecoverySequence failed: %v", err)
	}
	if seq1 != 1 {
		t.Fatalf("expected sequence 1, got %d", seq1)
	}

	// 5. Create Recovery Record 2
	rec2 := store.RecoveryRecord{
		RecoveryID:             "rec_token_2",
		JobID:                  "job_rec_001",
		KioskID:                "kiosk-demo",
		SequenceID:             2,
		ConfirmedSheetsPrinted: 2,
		RemainingPages:         2,
		RemainingSheets:        1,
		DownloadURL:            "http://example.com/rec2.pdf",
		SHA256Checksum:         "hash222",
		Status:                 "CREATED",
	}
	if err := s.CreateRecoveryRecord(ctx, rec2); err != nil {
		t.Fatalf("CreateRecoveryRecord 2 failed: %v", err)
	}

	seq2, err := s.GetLatestRecoverySequence(ctx, "job_rec_001")
	if err != nil {
		t.Fatalf("GetLatestRecoverySequence failed: %v", err)
	}
	if seq2 != 2 {
		t.Fatalf("expected sequence 2, got %d", seq2)
	}

	// 6. Idempotency / Duplicate rejection for duplicate (job_id, sequence_id) or recovery_id
	errDup := s.CreateRecoveryRecord(ctx, rec2)
	if errDup == nil {
		t.Fatal("expected duplicate CreateRecoveryRecord to fail")
	}
}
