package store

import (
	"context"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestMultiDocumentOrderCreationAndRetrieval(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	orderID := "ord-multi-101"

	docs := []OrderDocument{
		{
			JobID:            "job-doc-0",
			OrderID:          orderID,
			DocumentIndex:    0,
			OriginalFilename: "doc0_report.pdf",
			DownloadURL:      "https://example.com/doc0.pdf",
			SHA256Checksum:   "sha0",
			TotalPages:       4,
			TotalSheets:      2,
			Copies:           1,
			Duplex:           contracts.DuplexLongEdge,
			ColorMode:        "BW",
			PricePaise:       100,
		},
		{
			JobID:            "job-doc-1",
			OrderID:          orderID,
			DocumentIndex:    1,
			OriginalFilename: "doc1_invoice.pdf",
			DownloadURL:      "https://example.com/doc1.pdf",
			SHA256Checksum:   "sha1",
			TotalPages:       1,
			TotalSheets:      1,
			Copies:           2,
			Duplex:           contracts.DuplexSimplex,
			ColorMode:        "COLOR",
			PricePaise:       500,
		},
		{
			JobID:            "job-doc-2",
			OrderID:          orderID,
			DocumentIndex:    2,
			OriginalFilename: "doc2_presentation.pdf",
			DownloadURL:      "https://example.com/doc2.pdf",
			SHA256Checksum:   "sha2",
			TotalPages:       6,
			TotalSheets:      3,
			Copies:           1,
			Duplex:           contracts.DuplexShortEdge,
			ColorMode:        "BW",
			PricePaise:       150,
		},
	}

	ord := Order{
		OrderID: orderID,
		Command: contracts.PrintJobCommand{
			JobID:       "job-doc-0", // legacy fallback field
			KioskID:     "kiosk-m1",
			DisplayCode: "1234",
		},
		Status:     contracts.JobStatusNormalized,
		PricePaise: 750,
		Documents:  docs,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	// 1. GetOrder verifies aggregated sheets and documents slice
	retrieved, err := s.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if retrieved == nil {
		t.Fatalf("expected order %s, got nil", orderID)
	}
	if retrieved.TotalPages != 11 {
		t.Errorf("expected TotalPages=11, got %d", retrieved.TotalPages)
	}
	if retrieved.TotalSheets != 6 {
		t.Errorf("expected TotalSheets=6, got %d", retrieved.TotalSheets)
	}
	if len(retrieved.Documents) != 3 {
		t.Fatalf("expected 3 documents, got %d", len(retrieved.Documents))
	}
	for i, d := range retrieved.Documents {
		if d.DocumentIndex != i {
			t.Errorf("expected doc[%d].DocumentIndex=%d, got %d", i, i, d.DocumentIndex)
		}
		if d.JobID != docs[i].JobID {
			t.Errorf("expected doc[%d].JobID=%s, got %s", i, docs[i].JobID, d.JobID)
		}
		if d.Duplex != docs[i].Duplex {
			t.Errorf("expected doc[%d].Duplex=%s, got %s", i, docs[i].Duplex, d.Duplex)
		}
		if d.ColorMode != docs[i].ColorMode {
			t.Errorf("expected doc[%d].ColorMode=%s, got %s", i, docs[i].ColorMode, d.ColorMode)
		}
	}

	// 2. GetOrderDocuments returns all child documents ordered by document_index ASC
	odList, err := s.GetOrderDocuments(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrderDocuments failed: %v", err)
	}
	if len(odList) != 3 {
		t.Fatalf("expected 3 documents from GetOrderDocuments, got %d", len(odList))
	}
	if odList[0].JobID != "job-doc-0" || odList[1].JobID != "job-doc-1" || odList[2].JobID != "job-doc-2" {
		t.Errorf("GetOrderDocuments ordering incorrect: %v", odList)
	}

	// 3. GetOrderDocument retrieves single child document by jobID
	doc1, err := s.GetOrderDocument(ctx, "job-doc-1")
	if err != nil {
		t.Fatalf("GetOrderDocument failed: %v", err)
	}
	if doc1 == nil {
		t.Fatalf("expected doc1, got nil")
	}
	if doc1.OriginalFilename != "doc1_invoice.pdf" || doc1.ColorMode != "COLOR" || doc1.Copies != 2 {
		t.Errorf("unexpected doc1 fields: %+v", doc1)
	}

	// 4. GetOrderByJobID resolves ANY child document job_id to the parent order
	for _, childJob := range []string{"job-doc-0", "job-doc-1", "job-doc-2"} {
		parentOrd, err := s.GetOrderByJobID(ctx, childJob)
		if err != nil {
			t.Fatalf("GetOrderByJobID(%s) failed: %v", childJob, err)
		}
		if parentOrd == nil || parentOrd.OrderID != orderID {
			t.Fatalf("GetOrderByJobID(%s) expected parent order %s, got %+v", childJob, orderID, parentOrd)
		}
		if len(parentOrd.Documents) != 3 {
			t.Errorf("parentOrd.Documents len = %d, want 3", len(parentOrd.Documents))
		}
	}
}

func TestMultiDocumentOrderCreationAtomicity(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	orderID := "ord-atomicity-fail"

	// Doc 1 has invalid total_pages = 0 which must fail transaction validation
	badDocs := []OrderDocument{
		{
			JobID:            "job-atom-0",
			OrderID:          orderID,
			DocumentIndex:    0,
			OriginalFilename: "good.pdf",
			DownloadURL:      "https://example.com/good.pdf",
			SHA256Checksum:   "sha0",
			TotalPages:       2,
			TotalSheets:      1,
			Copies:           1,
			Duplex:           contracts.DuplexSimplex,
		},
		{
			JobID:            "job-atom-1",
			OrderID:          orderID,
			DocumentIndex:    1,
			OriginalFilename: "bad.pdf",
			DownloadURL:      "https://example.com/bad.pdf",
			SHA256Checksum:   "sha1",
			TotalPages:       0, // INVALID!
			TotalSheets:      1,
			Copies:           1,
			Duplex:           contracts.DuplexSimplex,
		},
	}

	ord := Order{
		OrderID: orderID,
		Command: contracts.PrintJobCommand{
			KioskID:     "kiosk-m1",
			DisplayCode: "1234",
		},
		Status:     contracts.JobStatusNormalized,
		PricePaise: 200,
		Documents:  badDocs,
	}

	err := s.CreateOrder(ctx, ord)
	if err == nil {
		t.Fatal("expected CreateOrder to fail with invalid document, got nil")
	}

	// Verify nothing was persisted (orders, print_configs, order_documents all rolled back)
	retrieved, err := s.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if retrieved != nil {
		t.Fatalf("expected order %s to NOT exist after rollback, got %+v", orderID, retrieved)
	}

	var docCount int
	err = s.db.QueryRow("SELECT COUNT(1) FROM order_documents WHERE order_id = ?", orderID).Scan(&docCount)
	if err != nil {
		t.Fatalf("query doc count failed: %v", err)
	}
	if docCount != 0 {
		t.Fatalf("expected 0 order_documents after rollback, got %d", docCount)
	}
}

func TestMultiDocumentPaymentAndDispatch(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	orderID := "ord-disp-multi"
	kioskID := "kiosk-m2"
	now := time.Now().UTC()

	docs := []OrderDocument{
		{
			JobID:            "job-disp-0",
			OrderID:          orderID,
			DocumentIndex:    0,
			OriginalFilename: "file0.pdf",
			DownloadURL:      "https://example.com/file0.pdf",
			SHA256Checksum:   "sha0",
			TotalPages:       2,
			TotalSheets:      1,
			Copies:           1,
			Duplex:           contracts.DuplexSimplex,
			ColorMode:        "BW",
		},
		{
			JobID:            "job-disp-1",
			OrderID:          orderID,
			DocumentIndex:    1,
			OriginalFilename: "file1.pdf",
			DownloadURL:      "https://example.com/file1.pdf",
			SHA256Checksum:   "sha1",
			TotalPages:       4,
			TotalSheets:      2,
			Copies:           2,
			Duplex:           contracts.DuplexLongEdge,
			ColorMode:        "COLOR",
		},
	}

	ord := Order{
		OrderID: orderID,
		Command: contracts.PrintJobCommand{
			KioskID:     kioskID,
			DisplayCode: "5678",
		},
		Status:     contracts.JobStatusPendingPayment,
		PricePaise: 400,
		Documents:  docs,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	// Create active session
	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 10, 15*time.Minute, now)
	if err != nil {
		t.Fatalf("CreateOrGetKioskSessionWithPaperCheck failed: %v", err)
	}

	att := PaymentAttempt{
		AttemptID:   "att-disp-multi",
		OrderID:     orderID,
		KioskID:     kioskID,
		SessionID:   sess.SessionID,
		AmountPaise: 400,
		Status:      "CREATED",
		ExpiresAt:   now.Add(15 * time.Minute),
	}
	if err := s.CreatePaymentAttempt(ctx, att); err != nil {
		t.Fatalf("CreatePaymentAttempt failed: %v", err)
	}

	// Capture payment
	pay := PaymentRecord{
		PaymentID:   "pay-disp-1",
		OrderID:     orderID,
		KioskID:     kioskID,
		SessionID:   sess.SessionID,
		AmountPaise: 400,
		Status:      "captured",
		Provider:    "DEMO",
	}

	result, err := s.CapturePaymentAndCheckDispatch(ctx, pay, now)
	if err != nil {
		t.Fatalf("CapturePaymentAndCheckDispatch failed: %v", err)
	}
	if !result.DispatchEligible {
		t.Fatalf("expected DispatchEligible=true, got false")
	}
	if len(result.Commands) != 2 {
		t.Fatalf("expected 2 dispatched commands, got %d", len(result.Commands))
	}

	// Verify both commands have document-specific properties
	if result.Commands[0].JobID != "job-disp-0" || result.Commands[0].DocumentIndex != 0 || result.Commands[0].TotalDocuments != 2 {
		t.Errorf("unexpected command[0]: %+v", result.Commands[0])
	}
	if result.Commands[1].JobID != "job-disp-1" || result.Commands[1].DocumentIndex != 1 || result.Commands[1].ColorMode != "COLOR" {
		t.Errorf("unexpected command[1]: %+v", result.Commands[1])
	}

	// Check dispatch_intents has 2 records keyed by job_id
	var intentCount int
	err = s.db.QueryRow("SELECT COUNT(1) FROM dispatch_intents WHERE order_id = ?", orderID).Scan(&intentCount)
	if err != nil {
		t.Fatalf("query intent count failed: %v", err)
	}
	if intentCount != 2 {
		t.Fatalf("expected 2 dispatch_intents rows, got %d", intentCount)
	}

	// Mark first intent sent & acknowledged
	if err := s.MarkDispatchIntentSent(ctx, "job-disp-0"); err != nil {
		t.Fatalf("MarkDispatchIntentSent failed: %v", err)
	}
	if err := s.MarkDispatchIntentAcknowledged(ctx, "job-disp-0"); err != nil {
		t.Fatalf("MarkDispatchIntentAcknowledged failed: %v", err)
	}

	st0, ack0, err := s.GetDispatchIntentStatus(ctx, "job-disp-0")
	if err != nil {
		t.Fatalf("GetDispatchIntentStatus failed: %v", err)
	}
	if st0 != "ACKNOWLEDGED" || ack0 <= 0 {
		t.Errorf("expected ACKNOWLEDGED status with ack > 0, got %s / %d", st0, ack0)
	}
}

func TestMultiDocumentOrderCompletionInvariant(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	orderID := "ord-comp-inv"
	kioskID := "kiosk-c1"
	now := time.Now().UTC()

	docs := []OrderDocument{
		{
			JobID:            "job-comp-0",
			OrderID:          orderID,
			DocumentIndex:    0,
			OriginalFilename: "file0.pdf",
			DownloadURL:      "https://example.com/file0.pdf",
			SHA256Checksum:   "sha0",
			TotalPages:       2,
			TotalSheets:      1,
			Copies:           1,
			Duplex:           contracts.DuplexSimplex,
			ColorMode:        "BW",
		},
		{
			JobID:            "job-comp-1",
			OrderID:          orderID,
			DocumentIndex:    1,
			OriginalFilename: "file1.pdf",
			DownloadURL:      "https://example.com/file1.pdf",
			SHA256Checksum:   "sha1",
			TotalPages:       2,
			TotalSheets:      1,
			Copies:           1,
			Duplex:           contracts.DuplexSimplex,
			ColorMode:        "BW",
		},
	}

	ord := Order{
		OrderID: orderID,
		Command: contracts.PrintJobCommand{
			KioskID:     kioskID,
			DisplayCode: "9999",
		},
		Status:     contracts.JobStatusDispatched,
		PricePaise: 200,
		Documents:  docs,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	// 1. Partial completion: doc 0 completes, doc 1 is printing
	evt0 := contracts.JobProgressEvent{
		JobID:         "job-comp-0",
		OrderID:       orderID,
		KioskID:       kioskID,
		Status:        contracts.JobStatusCompleted,
		PagesPrinted:  2,
		SheetsPrinted: 1,
		Timestamp:     now.Add(1 * time.Second),
	}
	if err := s.UpdateJobProgress(ctx, evt0); err != nil {
		t.Fatalf("UpdateJobProgress for job-comp-0 failed: %v", err)
	}

	// Check doc 0 is COMPLETED in order_documents
	d0, err := s.GetOrderDocument(ctx, "job-comp-0")
	if err != nil || d0 == nil {
		t.Fatalf("GetOrderDocument(job-comp-0) failed: %v", err)
	}
	if d0.Status != contracts.JobStatusCompleted {
		t.Errorf("expected doc0 status=COMPLETED, got %s", d0.Status)
	}

	// Parent order MUST NOT be COMPLETED yet!
	parentOrd, err := s.GetOrder(ctx, orderID)
	if err != nil || parentOrd == nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if parentOrd.Status == contracts.JobStatusCompleted {
		t.Fatalf("VIOLATION: parent order marked COMPLETED while child job-comp-1 is still pending!")
	}

	// 2. Full completion: doc 1 completes
	evt1 := contracts.JobProgressEvent{
		JobID:         "job-comp-1",
		OrderID:       orderID,
		KioskID:       kioskID,
		Status:        contracts.JobStatusCompleted,
		PagesPrinted:  2,
		SheetsPrinted: 1,
		Timestamp:     now.Add(2 * time.Second),
	}
	if err := s.UpdateJobProgress(ctx, evt1); err != nil {
		t.Fatalf("UpdateJobProgress for job-comp-1 failed: %v", err)
	}

	// Now ALL child jobs (N=2) are completed -> Parent order MUST be COMPLETED!
	parentOrd, err = s.GetOrder(ctx, orderID)
	if err != nil || parentOrd == nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if parentOrd.Status != contracts.JobStatusCompleted {
		t.Errorf("expected parent order status=COMPLETED once all docs finished, got %s", parentOrd.Status)
	}
}

func TestOrderWithZeroDocsNeverCompleted(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	orderID := "ord-zero-docs"
	kioskID := "kiosk-z1"
	now := time.Now().UTC()

	// Manually insert an order directly without documents
	_, err := s.db.Exec(`
INSERT INTO orders (order_id, job_id, kiosk_id, display_code, status, price_paise, created_at_ns, updated_at_ns)
VALUES (?, 'dummy-job', ?, '0000', 'PRINTING', 100, ?, ?);
`, orderID, kioskID, now.UnixNano(), now.UnixNano())
	if err != nil {
		t.Fatalf("insert raw order failed: %v", err)
	}

	_, err = s.db.Exec(`
INSERT INTO print_configs (order_id, download_url, sha256_checksum, total_pages, total_sheets, copies, duplex, start_from_page, color_mode)
VALUES (?, 'https://example.com/dummy.pdf', 'sha', 1, 1, 1, 'SIMPLEX', 1, 'BW');
`, orderID)
	if err != nil {
		t.Fatalf("insert raw print_configs failed: %v", err)
	}

	// An event arrives for some job ID
	evt := contracts.JobProgressEvent{
		JobID:         "job-nonexistent",
		OrderID:       orderID,
		KioskID:       kioskID,
		Status:        contracts.JobStatusCompleted,
		PagesPrinted:  1,
		SheetsPrinted: 1,
		Timestamp:     now,
	}

	_ = s.UpdateJobProgress(ctx, evt)

	// Order must NOT be marked COMPLETED because total_docs is 0!
	ord, err := s.GetOrder(ctx, orderID)
	if err != nil || ord == nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if ord.Status == contracts.JobStatusCompleted {
		t.Fatalf("INVARIANT VIOLATION: order with 0 child documents was marked COMPLETED!")
	}
}

func TestMultiDocumentGetPendingDispatchOrders(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	orderID := "ord-pending-multi"
	kioskID := "kiosk-pm1"
	now := time.Now().UTC()

	docs := []OrderDocument{
		{
			JobID:            "job-pm-0",
			OrderID:          orderID,
			DocumentIndex:    0,
			OriginalFilename: "report.pdf",
			DownloadURL:      "https://example.com/report.pdf",
			SHA256Checksum:   "sha0",
			TotalPages:       10,
			TotalSheets:      5,
			Copies:           3,
			Duplex:           contracts.DuplexLongEdge,
			ColorMode:        "BW",
		},
		{
			JobID:            "job-pm-1",
			OrderID:          orderID,
			DocumentIndex:    1,
			OriginalFilename: "cover.pdf",
			DownloadURL:      "https://example.com/cover.pdf",
			SHA256Checksum:   "sha1",
			TotalPages:       1,
			TotalSheets:      1,
			Copies:           3,
			Duplex:           contracts.DuplexSimplex,
			ColorMode:        "COLOR",
		},
	}

	ord := Order{
		OrderID: orderID,
		Command: contracts.PrintJobCommand{
			KioskID:     kioskID,
			DisplayCode: "1122",
		},
		Status:     contracts.JobStatusDispatched,
		PricePaise: 800,
		Documents:  docs,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	// Insert 2 pending dispatch intents
	nowNS := now.UnixNano()
	for _, d := range docs {
		_, err := s.db.Exec(`
INSERT INTO dispatch_intents (job_id, order_id, kiosk_id, session_id, status, created_at_ns, updated_at_ns)
VALUES (?, ?, ?, '', 'PENDING', ?, ?);
`, d.JobID, orderID, kioskID, nowNS, nowNS)
		if err != nil {
			t.Fatalf("insert dispatch_intent %s failed: %v", d.JobID, err)
		}
	}

	cmds, err := s.GetPendingDispatchOrders(ctx, kioskID)
	if err != nil {
		t.Fatalf("GetPendingDispatchOrders failed: %v", err)
	}
	if len(cmds) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(cmds))
	}

	// Verify command 0
	if cmds[0].JobID != "job-pm-0" || cmds[0].DocumentIndex != 0 || cmds[0].TotalDocuments != 2 || cmds[0].Copies != 3 || cmds[0].Duplex != contracts.DuplexLongEdge || cmds[0].ColorMode != "BW" || cmds[0].OriginalFilename != "report.pdf" {
		t.Errorf("cmds[0] properties incorrect: %+v", cmds[0])
	}
	// Verify command 1
	if cmds[1].JobID != "job-pm-1" || cmds[1].DocumentIndex != 1 || cmds[1].TotalDocuments != 2 || cmds[1].Copies != 3 || cmds[1].Duplex != contracts.DuplexSimplex || cmds[1].ColorMode != "COLOR" || cmds[1].OriginalFilename != "cover.pdf" {
		t.Errorf("cmds[1] properties incorrect: %+v", cmds[1])
	}
}
