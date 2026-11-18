package store

import (
	"context"
	"strings"
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
		Status:      contracts.JobStatusNormalized,
		PricePaise:  750,
		TotalPages:  11,
		TotalSheets: 6,
		Documents:   docs,
		CreatedAt:   now,
		UpdatedAt:   now,
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
			PricePaise:       100,
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
			PricePaise:       100,
		},
	}

	ord := Order{
		OrderID: orderID,
		Command: contracts.PrintJobCommand{
			KioskID:     "kiosk-m1",
			DisplayCode: "1234",
		},
		Status:      contracts.JobStatusNormalized,
		PricePaise:  200,
		TotalPages:  2,
		TotalSheets: 2,
		Documents:   badDocs,
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

func TestCreateOrder_ParentTotalsEnforced(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	orderID := "ord-totals-check"
	kioskID := "kiosk-tot"

	docs := []OrderDocument{
		{
			JobID:            "job-t-0",
			OrderID:          orderID,
			DocumentIndex:    0,
			OriginalFilename: "doc0.pdf",
			DownloadURL:      "https://example.com/0.pdf",
			SHA256Checksum:   "sha0",
			TotalPages:       3,
			TotalSheets:      2,
			Copies:           1,
			Duplex:           contracts.DuplexLongEdge,
			PricePaise:       300,
		},
		{
			JobID:            "job-t-1",
			OrderID:          orderID,
			DocumentIndex:    1,
			OriginalFilename: "doc1.pdf",
			DownloadURL:      "https://example.com/1.pdf",
			SHA256Checksum:   "sha1",
			TotalPages:       2,
			TotalSheets:      1,
			Copies:           1,
			Duplex:           contracts.DuplexSimplex,
			PricePaise:       200,
		},
	}

	// 1. TotalPages mismatch (5 actual vs 4 claimed) -> must be rejected
	ordBadPages := Order{
		OrderID:     orderID,
		Command:     contracts.PrintJobCommand{KioskID: kioskID, DisplayCode: "1111"},
		Status:      contracts.JobStatusNormalized,
		PricePaise:  500,
		TotalPages:  4, // MISMATCH
		TotalSheets: 3,
		Documents:   docs,
	}
	if err := s.CreateOrder(ctx, ordBadPages); err == nil {
		t.Fatal("expected error on total_pages mismatch, got nil")
	}

	// 2. TotalSheets mismatch (3 actual vs 4 claimed) -> must be rejected
	ordBadSheets := Order{
		OrderID:     orderID,
		Command:     contracts.PrintJobCommand{KioskID: kioskID, DisplayCode: "1111"},
		Status:      contracts.JobStatusNormalized,
		PricePaise:  500,
		TotalPages:  5,
		TotalSheets: 4, // MISMATCH
		Documents:   docs,
	}
	if err := s.CreateOrder(ctx, ordBadSheets); err == nil {
		t.Fatal("expected error on total_sheets mismatch, got nil")
	}

	// 3. PricePaise mismatch (500 actual vs 450 claimed) -> must be rejected
	ordBadPrice := Order{
		OrderID:     orderID,
		Command:     contracts.PrintJobCommand{KioskID: kioskID, DisplayCode: "1111"},
		Status:      contracts.JobStatusNormalized,
		PricePaise:  450, // MISMATCH
		TotalPages:  5,
		TotalSheets: 3,
		Documents:   docs,
	}
	if err := s.CreateOrder(ctx, ordBadPrice); err == nil {
		t.Fatal("expected error on price_paise mismatch, got nil")
	}

	// 4. Exact match succeeds
	ordValid := Order{
		OrderID:     orderID,
		Command:     contracts.PrintJobCommand{KioskID: kioskID, DisplayCode: "1111"},
		Status:      contracts.JobStatusNormalized,
		PricePaise:  500,
		TotalPages:  5,
		TotalSheets: 3,
		Documents:   docs,
	}
	if err := s.CreateOrder(ctx, ordValid); err != nil {
		t.Fatalf("expected valid order with matching totals to succeed, got %v", err)
	}
}

func TestMultiDocumentPayment_AtomicallyCreatesDispatchIntentsInOrder(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	orderID := "ord-disp-multi-1"
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
			PricePaise:       100,
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
			PricePaise:       300,
		},
	}

	ord := Order{
		OrderID: orderID,
		Command: contracts.PrintJobCommand{
			KioskID:     kioskID,
			DisplayCode: "5678",
		},
		Status:      contracts.JobStatusPendingPayment,
		PricePaise:  400,
		TotalPages:  6,
		TotalSheets: 3,
		Documents:   docs,
		CreatedAt:   now,
		UpdatedAt:   now,
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
		t.Fatalf("expected multi-job order to be DispatchEligible=true after payment authorization")
	}
	if result.OrderStatus != contracts.JobStatusDispatched {
		t.Errorf("expected order status DISPATCHED, got %s", result.OrderStatus)
	}
	if len(result.Commands) != 2 {
		t.Fatalf("expected 2 dispatched commands for multi-job order, got %d", len(result.Commands))
	}
	if result.Commands[0].JobID != "job-disp-0" || result.Commands[1].JobID != "job-disp-1" {
		t.Errorf("expected commands in document index order [job-disp-0, job-disp-1], got [%s, %s]", result.Commands[0].JobID, result.Commands[1].JobID)
	}
	if result.Commands[0].TotalDocuments != 2 || result.Commands[1].TotalDocuments != 2 {
		t.Errorf("expected TotalDocuments=2 on all commands")
	}

	// Check dispatch_intents has exactly 2 records, ordered by document_index (created_at_ns ASC)
	intents, err := s.GetDispatchIntentsByOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetDispatchIntentsByOrder failed: %v", err)
	}
	if len(intents) != 2 {
		t.Fatalf("expected 2 dispatch_intents rows, got %d", len(intents))
	}
	if intents[0].JobID != "job-disp-0" || intents[1].JobID != "job-disp-1" {
		t.Errorf("expected intents [job-disp-0, job-disp-1], got [%s, %s]", intents[0].JobID, intents[1].JobID)
	}
	if intents[0].Status != "PENDING" || intents[1].Status != "PENDING" {
		t.Errorf("expected PENDING status on intents, got %s, %s", intents[0].Status, intents[1].Status)
	}
	if intents[1].CreatedAtNS < intents[0].CreatedAtNS {
		t.Errorf("expected strictly ascending CreatedAtNS: %d vs %d", intents[0].CreatedAtNS, intents[1].CreatedAtNS)
	}

	// GetPendingDispatchOrders returns all child commands in document order
	pendingCmds, err := s.GetPendingDispatchOrders(ctx, kioskID)
	if err != nil {
		t.Fatalf("GetPendingDispatchOrders failed: %v", err)
	}
	var orderCmds []contracts.PrintJobCommand
	for _, c := range pendingCmds {
		if c.OrderID == orderID {
			orderCmds = append(orderCmds, c)
		}
	}
	if len(orderCmds) != 2 {
		t.Fatalf("expected 2 pending commands for order %s, got %d", orderID, len(orderCmds))
	}
	if orderCmds[0].JobID != "job-disp-0" || orderCmds[1].JobID != "job-disp-1" {
		t.Errorf("expected pending commands in order [job-disp-0, job-disp-1], got [%s, %s]", orderCmds[0].JobID, orderCmds[1].JobID)
	}
}

func TestSingleDocumentPaymentAndDispatch_Succeeds(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	orderID := "ord-single-disp"
	kioskID := "kiosk-single"
	now := time.Now().UTC()

	ord := Order{
		OrderID: orderID,
		Command: contracts.PrintJobCommand{
			JobID:          "job-single-1",
			KioskID:        kioskID,
			DisplayCode:    "4321",
			DownloadURL:    "https://example.com/single.pdf",
			SHA256Checksum: "shasingle",
			TotalPages:     2,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		},
		Status:      contracts.JobStatusPendingPayment,
		PricePaise:  200,
		TotalPages:  2,
		TotalSheets: 1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 10, 15*time.Minute, now)
	if err != nil {
		t.Fatalf("create session failed: %v", err)
	}

	att := PaymentAttempt{
		AttemptID:   "att-single-1",
		OrderID:     orderID,
		KioskID:     kioskID,
		SessionID:   sess.SessionID,
		AmountPaise: 200,
		Status:      "CREATED",
		ExpiresAt:   now.Add(15 * time.Minute),
	}
	if err := s.CreatePaymentAttempt(ctx, att); err != nil {
		t.Fatalf("CreatePaymentAttempt failed: %v", err)
	}

	pay := PaymentRecord{
		PaymentID:   "pay-single-1",
		OrderID:     orderID,
		KioskID:     kioskID,
		SessionID:   sess.SessionID,
		AmountPaise: 200,
		Status:      "captured",
		Provider:    "DEMO",
	}

	result, err := s.CapturePaymentAndCheckDispatch(ctx, pay, now)
	if err != nil {
		t.Fatalf("CapturePaymentAndCheckDispatch failed: %v", err)
	}
	if !result.DispatchEligible {
		t.Fatalf("expected single job to be DispatchEligible=true")
	}
	if result.OrderStatus != contracts.JobStatusDispatched {
		t.Errorf("expected DISPATCHED status, got %s", result.OrderStatus)
	}
	if len(result.Commands) != 1 {
		t.Fatalf("expected 1 dispatched command, got %d", len(result.Commands))
	}

	// Verify intent is recorded
	st, _, err := s.GetJobDispatchIntentStatus(ctx, "job-single-1")
	if err != nil {
		t.Fatalf("GetJobDispatchIntentStatus failed: %v", err)
	}
	if st != "PENDING" {
		t.Errorf("expected PENDING status, got %s", st)
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
			PricePaise:       100,
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
			PricePaise:       100,
		},
	}

	ord := Order{
		OrderID: orderID,
		Command: contracts.PrintJobCommand{
			KioskID:     kioskID,
			DisplayCode: "9999",
		},
		Status:      contracts.JobStatusDispatched,
		PricePaise:  200,
		TotalPages:  4,
		TotalSheets: 2,
		Documents:   docs,
		CreatedAt:   now,
		UpdatedAt:   now,
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

func TestGetOrder_ChildDocumentReadFailure(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	orderID := "ord-corrupt-read"
	kioskID := "kiosk-cr"
	now := time.Now().UTC()

	// Insert order and print_config
	_, err := s.db.Exec(`
INSERT INTO orders (order_id, job_id, kiosk_id, display_code, status, price_paise, created_at_ns, updated_at_ns)
VALUES (?, 'job-cr-0', ?, '3333', 'PENDING_PAYMENT', 100, ?, ?);
`, orderID, kioskID, now.UnixNano(), now.UnixNano())
	if err != nil {
		t.Fatalf("insert raw order failed: %v", err)
	}
	_, err = s.db.Exec(`
INSERT INTO print_configs (order_id, download_url, sha256_checksum, total_pages, total_sheets, copies, duplex, start_from_page, color_mode)
VALUES (?, 'https://example.com/cr.pdf', 'shacr', 1, 1, 1, 'SIMPLEX', 1, 'BW');
`, orderID)
	if err != nil {
		t.Fatalf("insert raw print_configs failed: %v", err)
	}

	// Insert order_document with invalid total_sheets = NULL or invalid type that causes scan failure
	// In SQLite we can insert a non-integer string into an INTEGER column to cause Scan to fail with an error!
	_, err = s.db.Exec(`
INSERT INTO order_documents (job_id, order_id, document_index, original_filename, download_url, sha256_checksum,
    total_pages, total_sheets, copies, duplex, color_mode, price_paise, status, created_at_ns, updated_at_ns)
VALUES ('job-cr-0', ?, 0, 'file.pdf', 'https://example.com', 'sha', 'NOT_AN_INT_CORRUPT', 1, 1, 'SIMPLEX', 'BW', 100, 'NORMALIZED', ?, ?);
`, orderID, now.UnixNano(), now.UnixNano())
	if err != nil {
		t.Fatalf("insert corrupt order_document failed: %v", err)
	}

	// GetOrder must fail explicitly and NEVER return an order with silently missing documents!
	ord, err := s.GetOrder(ctx, orderID)
	if err == nil {
		t.Fatalf("expected GetOrder to fail explicitly on corrupted child document scan, got order: %+v", ord)
	}
}

func TestMultiDocumentPayment_ConcurrentCaptureCallbacks_ExactlyOnce(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	orderID := "ord-concurrent-multi"
	kioskID := "kiosk-cm1"
	now := time.Now().UTC()

	docs := []OrderDocument{
		{
			JobID:            "job-cm-0",
			OrderID:          orderID,
			DocumentIndex:    0,
			OriginalFilename: "doc0.pdf",
			DownloadURL:      "https://example.com/0.pdf",
			SHA256Checksum:   "sha0",
			TotalPages:       2,
			TotalSheets:      1,
			Copies:           1,
			Duplex:           contracts.DuplexSimplex,
			ColorMode:        "BW",
			PricePaise:       100,
		},
		{
			JobID:            "job-cm-1",
			OrderID:          orderID,
			DocumentIndex:    1,
			OriginalFilename: "doc1.pdf",
			DownloadURL:      "https://example.com/1.pdf",
			SHA256Checksum:   "sha1",
			TotalPages:       2,
			TotalSheets:      1,
			Copies:           1,
			Duplex:           contracts.DuplexSimplex,
			ColorMode:        "BW",
			PricePaise:       100,
		},
		{
			JobID:            "job-cm-2",
			OrderID:          orderID,
			DocumentIndex:    2,
			OriginalFilename: "doc2.pdf",
			DownloadURL:      "https://example.com/2.pdf",
			SHA256Checksum:   "sha2",
			TotalPages:       2,
			TotalSheets:      1,
			Copies:           1,
			Duplex:           contracts.DuplexSimplex,
			ColorMode:        "BW",
			PricePaise:       100,
		},
	}

	ord := Order{
		OrderID: orderID,
		Command: contracts.PrintJobCommand{
			KioskID:     kioskID,
			DisplayCode: "8888",
		},
		Status:      contracts.JobStatusPendingPayment,
		PricePaise:  300,
		TotalPages:  6,
		TotalSheets: 3,
		Documents:   docs,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 10, 15*time.Minute, now)
	if err != nil {
		t.Fatalf("CreateOrGetKioskSessionWithPaperCheck failed: %v", err)
	}

	att := PaymentAttempt{
		AttemptID:   "att-cm-1",
		OrderID:     orderID,
		KioskID:     kioskID,
		SessionID:   sess.SessionID,
		AmountPaise: 300,
		Status:      "CREATED",
		ExpiresAt:   now.Add(15 * time.Minute),
	}
	if err := s.CreatePaymentAttempt(ctx, att); err != nil {
		t.Fatalf("CreatePaymentAttempt failed: %v", err)
	}

	concurrency := 10
	results := make([]*PaymentCaptureResult, concurrency)
	errors := make([]error, concurrency)
	done := make(chan struct{})

	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			// Some share paymentID (duplicate webhook deliveries), some have unique paymentIDs
			pID := "pay-shared"
			if idx%2 == 1 {
				pID = "pay-unique-" + string(rune('a'+idx))
			}
			pay := PaymentRecord{
				PaymentID:   pID,
				OrderID:     orderID,
				KioskID:     kioskID,
				SessionID:   sess.SessionID,
				AmountPaise: 300,
				Status:      "captured",
				Provider:    "DEMO",
			}
			res, cErr := s.CapturePaymentAndCheckDispatch(ctx, pay, now)
			results[idx] = res
			errors[idx] = cErr
			done <- struct{}{}
		}(i)
	}

	for i := 0; i < concurrency; i++ {
		<-done
	}

	dispatchEligibleCount := 0
	for _, res := range results {
		if res != nil && res.DispatchEligible {
			dispatchEligibleCount++
		}
	}

	if dispatchEligibleCount != 1 {
		t.Fatalf("expected exactly 1 dispatch authorization under concurrent callbacks, got %d", dispatchEligibleCount)
	}

	// Invariants:
	// Exactly 3 dispatch_intents records (one per child document)
	intents, err := s.GetDispatchIntentsByOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetDispatchIntentsByOrder failed: %v", err)
	}
	if len(intents) != 3 {
		t.Fatalf("expected exactly 3 dispatch_intents, got %d", len(intents))
	}
	if intents[0].JobID != "job-cm-0" || intents[1].JobID != "job-cm-1" || intents[2].JobID != "job-cm-2" {
		t.Errorf("expected intents in sequence [job-cm-0, job-cm-1, job-cm-2], got [%s, %s, %s]",
			intents[0].JobID, intents[1].JobID, intents[2].JobID)
	}

	// Order status is DISPATCHED
	finalOrd, err := s.GetOrder(ctx, orderID)
	if err != nil || finalOrd == nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if finalOrd.Status != contracts.JobStatusDispatched {
		t.Errorf("expected final status DISPATCHED, got %s", finalOrd.Status)
	}
}

func TestMultiDocumentPayment_FailedTransaction_AtomicallyRollsBack(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	orderID := "ord-fail-tx"
	kioskID := "kiosk-fail"
	now := time.Now().UTC()

	docs := []OrderDocument{
		{
			JobID:            "job-ft-0",
			OrderID:          orderID,
			DocumentIndex:    0,
			OriginalFilename: "f0.pdf",
			DownloadURL:      "https://example.com/f0.pdf",
			SHA256Checksum:   "sha0",
			TotalPages:       1,
			TotalSheets:      1,
			Copies:           1,
			Duplex:           contracts.DuplexSimplex,
			ColorMode:        "BW",
			PricePaise:       100,
		},
		{
			JobID:            "job-ft-1",
			OrderID:          orderID,
			DocumentIndex:    1,
			OriginalFilename: "f1.pdf",
			DownloadURL:      "https://example.com/f1.pdf",
			SHA256Checksum:   "sha1",
			TotalPages:       1,
			TotalSheets:      1,
			Copies:           1,
			Duplex:           contracts.DuplexSimplex,
			ColorMode:        "BW",
			PricePaise:       100,
		},
	}

	ord := Order{
		OrderID:     orderID,
		Command:     contracts.PrintJobCommand{KioskID: kioskID, DisplayCode: "7777"},
		Status:      contracts.JobStatusPendingPayment,
		PricePaise:  200,
		TotalPages:  2,
		TotalSheets: 2,
		Documents:   docs,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 10, 15*time.Minute, now)
	if err != nil {
		t.Fatalf("CreateOrGetKioskSessionWithPaperCheck failed: %v", err)
	}

	// Inject a failure trigger on dispatch_intents insertion
	_, err = s.db.Exec(`CREATE TRIGGER fail_multi_intent BEFORE INSERT ON dispatch_intents BEGIN SELECT RAISE(ABORT, 'injected dispatch_intents failure'); END;`)
	if err != nil {
		t.Fatalf("create trigger failed: %v", err)
	}

	pay := PaymentRecord{
		PaymentID:   "pay-ft-1",
		OrderID:     orderID,
		KioskID:     kioskID,
		SessionID:   sess.SessionID,
		AmountPaise: 200,
		Status:      "captured",
		Provider:    "DEMO",
	}

	res, err := s.CapturePaymentAndCheckDispatch(ctx, pay, now)
	if err == nil {
		t.Fatal("expected CapturePaymentAndCheckDispatch to fail when intent insert fails, got nil")
	}
	if res != nil && res.DispatchEligible {
		t.Fatal("expected DispatchEligible=false on failed transaction")
	}

	// Check order remained in PENDING_PAYMENT
	retrievedOrd, err := s.GetOrder(ctx, orderID)
	if err != nil || retrievedOrd == nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if retrievedOrd.Status != contracts.JobStatusPendingPayment {
		t.Errorf("expected order status PENDING_PAYMENT after rollback, got %s", retrievedOrd.Status)
	}

	// Check session remained in PAYMENT_PENDING
	var sessStatus string
	_ = s.db.QueryRow("SELECT status FROM kiosk_sessions WHERE session_id = ?", sess.SessionID).Scan(&sessStatus)
	if sessStatus != string(KioskSessionPaymentPending) {
		t.Errorf("expected session status PAYMENT_PENDING, got %s", sessStatus)
	}

	// Check 0 dispatch_intents and 0 payments
	var intentCount int
	_ = s.db.QueryRow("SELECT COUNT(1) FROM dispatch_intents WHERE order_id = ?", orderID).Scan(&intentCount)
	if intentCount != 0 {
		t.Errorf("expected 0 dispatch_intents rows, got %d", intentCount)
	}

	var payCount int
	_ = s.db.QueryRow("SELECT COUNT(1) FROM payments WHERE order_id = ?", orderID).Scan(&payCount)
	if payCount != 0 {
		t.Errorf("expected 0 payments rows, got %d", payCount)
	}
}

func TestMultiDocumentDispatch_Reconnects_PreservesDocumentOrderAndIdentity(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	orderID := "ord-rec-seq"
	kioskID := "kiosk-rec"
	now := time.Now().UTC()

	docs := []OrderDocument{
		{
			JobID:            "job-rec-0",
			OrderID:          orderID,
			DocumentIndex:    0,
			OriginalFilename: "a.pdf",
			DownloadURL:      "https://example.com/a.pdf",
			SHA256Checksum:   "shaA",
			TotalPages:       2,
			TotalSheets:      1,
			Copies:           1,
			Duplex:           contracts.DuplexSimplex,
			ColorMode:        "BW",
			PricePaise:       100,
		},
		{
			JobID:            "job-rec-1",
			OrderID:          orderID,
			DocumentIndex:    1,
			OriginalFilename: "b.pdf",
			DownloadURL:      "https://example.com/b.pdf",
			SHA256Checksum:   "shaB",
			TotalPages:       4,
			TotalSheets:      2,
			Copies:           1,
			Duplex:           contracts.DuplexLongEdge,
			ColorMode:        "COLOR",
			PricePaise:       200,
		},
		{
			JobID:            "job-rec-2",
			OrderID:          orderID,
			DocumentIndex:    2,
			OriginalFilename: "c.pdf",
			DownloadURL:      "https://example.com/c.pdf",
			SHA256Checksum:   "shaC",
			TotalPages:       1,
			TotalSheets:      1,
			Copies:           2,
			Duplex:           contracts.DuplexSimplex,
			ColorMode:        "BW",
			PricePaise:       150,
		},
	}

	ord := Order{
		OrderID:     orderID,
		Command:     contracts.PrintJobCommand{KioskID: kioskID, DisplayCode: "1212"},
		Status:      contracts.JobStatusPendingPayment,
		PricePaise:  450,
		TotalPages:  7,
		TotalSheets: 4,
		Documents:   docs,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 10, 15*time.Minute, now)
	if err != nil {
		t.Fatalf("CreateOrGetKioskSessionWithPaperCheck failed: %v", err)
	}

	pay := PaymentRecord{
		PaymentID:   "pay-rec-1",
		OrderID:     orderID,
		KioskID:     kioskID,
		SessionID:   sess.SessionID,
		AmountPaise: 450,
		Status:      "captured",
		Provider:    "DEMO",
	}

	_, err = s.CapturePaymentAndCheckDispatch(ctx, pay, now)
	if err != nil {
		t.Fatalf("CapturePaymentAndCheckDispatch failed: %v", err)
	}

	// Simulate initial dispatch: all 3 marked SENT
	for _, d := range docs {
		if err := s.MarkDispatchIntentSent(ctx, d.JobID); err != nil {
			t.Fatalf("MarkDispatchIntentSent(%s) failed: %v", d.JobID, err)
		}
	}

	// Kiosk acknowledges doc 0
	if err := s.MarkDispatchIntentAcknowledged(ctx, "job-rec-0"); err != nil {
		t.Fatalf("MarkDispatchIntentAcknowledged(job-rec-0) failed: %v", err)
	}

	// Kiosk disconnects and reconnects: ResetUnacknowledgedDispatchIntents called
	if err := s.ResetUnacknowledgedDispatchIntents(ctx, kioskID); err != nil {
		t.Fatalf("ResetUnacknowledgedDispatchIntents failed: %v", err)
	}

	// Verify intent statuses: doc 0 stays ACKNOWLEDGED; doc 1 and 2 reset to PENDING
	st0, _, _ := s.GetJobDispatchIntentStatus(ctx, "job-rec-0")
	if st0 != "ACKNOWLEDGED" {
		t.Errorf("expected doc0 to remain ACKNOWLEDGED, got %s", st0)
	}
	st1, _, _ := s.GetJobDispatchIntentStatus(ctx, "job-rec-1")
	if st1 != "PENDING" {
		t.Errorf("expected doc1 to revert to PENDING, got %s", st1)
	}
	st2, _, _ := s.GetJobDispatchIntentStatus(ctx, "job-rec-2")
	if st2 != "PENDING" {
		t.Errorf("expected doc2 to revert to PENDING, got %s", st2)
	}

	// Reconnect reconciliation: GetPendingDispatchOrders returns remaining jobs in exact order
	pending, err := s.GetPendingDispatchOrders(ctx, kioskID)
	if err != nil {
		t.Fatalf("GetPendingDispatchOrders failed: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending jobs on reconnect, got %d", len(pending))
	}
	if pending[0].JobID != "job-rec-1" || pending[1].JobID != "job-rec-2" {
		t.Fatalf("expected pending jobs in order [job-rec-1, job-rec-2], got [%s, %s]", pending[0].JobID, pending[1].JobID)
	}
	if pending[0].DocumentIndex != 1 || pending[1].DocumentIndex != 2 {
		t.Errorf("expected DocumentIndex 1 and 2, got %d and %d", pending[0].DocumentIndex, pending[1].DocumentIndex)
	}
	if pending[0].TotalDocuments != 3 || pending[1].TotalDocuments != 3 {
		t.Errorf("expected TotalDocuments=3 on all reconnect commands")
	}
}

func TestMultiDocumentDispatch_NeverDispatchesUnpaidOrCanceled(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	kioskID := "kiosk-unpaid-check"
	now := time.Now().UTC()

	// 1. Unpaid order
	orderIDUnpaid := "ord-unpaid-1"
	docsUnpaid := []OrderDocument{
		{
			JobID:            "job-unpaid-0",
			OrderID:          orderIDUnpaid,
			DocumentIndex:    0,
			OriginalFilename: "unpaid.pdf",
			DownloadURL:      "https://example.com/u.pdf",
			SHA256Checksum:   "shau",
			TotalPages:       1,
			TotalSheets:      1,
			Copies:           1,
			Duplex:           contracts.DuplexSimplex,
			ColorMode:        "BW",
			PricePaise:       100,
		},
	}
	ordUnpaid := Order{
		OrderID:     orderIDUnpaid,
		Command:     contracts.PrintJobCommand{KioskID: kioskID, DisplayCode: "0001"},
		Status:      contracts.JobStatusPendingPayment,
		PricePaise:  100,
		TotalPages:  1,
		TotalSheets: 1,
		Documents:   docsUnpaid,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.CreateOrder(ctx, ordUnpaid); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	// Insert rogue pending dispatch intent for unpaid order
	_, err := s.db.Exec(`INSERT INTO dispatch_intents (job_id, order_id, kiosk_id, status, created_at_ns, updated_at_ns) VALUES (?, ?, ?, 'PENDING', ?, ?);`,
		"job-unpaid-0", orderIDUnpaid, kioskID, now.UnixNano(), now.UnixNano())
	if err != nil {
		t.Fatalf("insert rogue intent failed: %v", err)
	}

	// 2. Canceled order
	orderIDCanceled := "ord-canceled-1"
	docsCanceled := []OrderDocument{
		{
			JobID:            "job-canc-0",
			OrderID:          orderIDCanceled,
			DocumentIndex:    0,
			OriginalFilename: "canc.pdf",
			DownloadURL:      "https://example.com/c.pdf",
			SHA256Checksum:   "shac",
			TotalPages:       1,
			TotalSheets:      1,
			Copies:           1,
			Duplex:           contracts.DuplexSimplex,
			ColorMode:        "BW",
			PricePaise:       100,
		},
	}
	ordCanceled := Order{
		OrderID:     orderIDCanceled,
		Command:     contracts.PrintJobCommand{KioskID: kioskID, DisplayCode: "0002"},
		Status:      contracts.JobStatusFailedRefundable,
		PricePaise:  100,
		TotalPages:  1,
		TotalSheets: 1,
		Documents:   docsCanceled,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.CreateOrder(ctx, ordCanceled); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}
	_, err = s.db.Exec(`INSERT INTO dispatch_intents (job_id, order_id, kiosk_id, status, created_at_ns, updated_at_ns) VALUES (?, ?, ?, 'PENDING', ?, ?);`,
		"job-canc-0", orderIDCanceled, kioskID, now.UnixNano(), now.UnixNano())
	if err != nil {
		t.Fatalf("insert canc intent failed: %v", err)
	}

	// Query pending dispatch orders: neither must be returned!
	cmds, err := s.GetPendingDispatchOrders(ctx, kioskID)
	if err != nil {
		t.Fatalf("GetPendingDispatchOrders failed: %v", err)
	}
	for _, c := range cmds {
		if c.OrderID == orderIDUnpaid {
			t.Fatalf("VIOLATION: unpaid order %s was dispatched!", orderIDUnpaid)
		}
		if c.OrderID == orderIDCanceled {
			t.Fatalf("VIOLATION: canceled order %s was dispatched!", orderIDCanceled)
		}
	}

	// Verify canceled order's intent was cleaned up to CANCELED
	stCanc, _, _ := s.GetJobDispatchIntentStatus(ctx, "job-canc-0")
	if stCanc != "CANCELED" {
		t.Errorf("expected canceled intent status=CANCELED, got %s", stCanc)
	}
}

func TestMultiDocumentDispatch_ReleasePaidOrder_CreatesIntentsInOrder(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	orderID := "ord-rel-multi"
	kioskID := "kiosk-rel-1"
	now := time.Now().UTC()

	docs := []OrderDocument{
		{
			JobID:            "job-rel-0",
			OrderID:          orderID,
			DocumentIndex:    0,
			OriginalFilename: "part1.pdf",
			DownloadURL:      "https://example.com/p1.pdf",
			SHA256Checksum:   "sha1",
			TotalPages:       2,
			TotalSheets:      1,
			Copies:           1,
			Duplex:           contracts.DuplexSimplex,
			ColorMode:        "BW",
			PricePaise:       100,
		},
		{
			JobID:            "job-rel-1",
			OrderID:          orderID,
			DocumentIndex:    1,
			OriginalFilename: "part2.pdf",
			DownloadURL:      "https://example.com/p2.pdf",
			SHA256Checksum:   "sha2",
			TotalPages:       4,
			TotalSheets:      2,
			Copies:           1,
			Duplex:           contracts.DuplexLongEdge,
			ColorMode:        "COLOR",
			PricePaise:       250,
		},
	}

	ord := Order{
		OrderID:     orderID,
		Command:     contracts.PrintJobCommand{KioskID: kioskID, DisplayCode: "4321"},
		Status:      contracts.JobStatusPaidWaiting, // Order is paid, waiting for kiosk PIN claim
		PricePaise:  350,
		TotalPages:  6,
		TotalSheets: 3,
		Documents:   docs,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 10, 15*time.Minute, now)
	if err != nil {
		t.Fatalf("CreateOrGetKioskSessionWithPaperCheck failed: %v", err)
	}

	// User enters PIN at kiosk: ReleasePaidOrder
	releasedOrd, err := s.ReleasePaidOrder(ctx, kioskID, orderID, sess.SessionID, now)
	if err != nil {
		t.Fatalf("ReleasePaidOrder failed: %v", err)
	}
	if releasedOrd.Status != contracts.JobStatusDispatched {
		t.Errorf("expected released order status DISPATCHED, got %s", releasedOrd.Status)
	}

	// Exactly 2 dispatch intents created in document order
	intents, err := s.GetDispatchIntentsByOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetDispatchIntentsByOrder failed: %v", err)
	}
	if len(intents) != 2 {
		t.Fatalf("expected 2 dispatch intents, got %d", len(intents))
	}
	if intents[0].JobID != "job-rel-0" || intents[1].JobID != "job-rel-1" {
		t.Errorf("expected intents [job-rel-0, job-rel-1], got [%s, %s]", intents[0].JobID, intents[1].JobID)
	}
	if intents[1].CreatedAtNS < intents[0].CreatedAtNS {
		t.Errorf("expected strictly ascending CreatedAtNS: %d vs %d", intents[0].CreatedAtNS, intents[1].CreatedAtNS)
	}
}

func TestDispatchIntent_JobScopedMutations_DoesNotAffectSiblings(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	orderID := "ord-sib-test"
	kioskID := "kiosk-sib"
	now := time.Now().UTC()

	docs := []OrderDocument{
		{JobID: "job-sib-0", OrderID: orderID, DocumentIndex: 0, TotalPages: 1, TotalSheets: 1, Copies: 1, Duplex: contracts.DuplexSimplex, PricePaise: 100},
		{JobID: "job-sib-1", OrderID: orderID, DocumentIndex: 1, TotalPages: 1, TotalSheets: 1, Copies: 1, Duplex: contracts.DuplexSimplex, PricePaise: 100},
		{JobID: "job-sib-2", OrderID: orderID, DocumentIndex: 2, TotalPages: 1, TotalSheets: 1, Copies: 1, Duplex: contracts.DuplexSimplex, PricePaise: 100},
	}
	ord := Order{
		OrderID:     orderID,
		Command:     contracts.PrintJobCommand{KioskID: kioskID, DisplayCode: "1122"},
		Status:      contracts.JobStatusPendingPayment,
		PricePaise:  300,
		TotalPages:  3,
		TotalSheets: 3,
		Documents:   docs,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}
	sess, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, orderID, 10, 15*time.Minute, now)
	if err != nil {
		t.Fatalf("CreateOrGetKioskSessionWithPaperCheck failed: %v", err)
	}

	pay := PaymentRecord{
		PaymentID:   "pay-sib-1",
		OrderID:     orderID,
		KioskID:     kioskID,
		SessionID:   sess.SessionID,
		AmountPaise: 300,
		Status:      "captured",
		Provider:    "DEMO",
	}
	res, err := s.CapturePaymentAndCheckDispatch(ctx, pay, now)
	if err != nil || !res.DispatchEligible {
		t.Fatalf("CapturePaymentAndCheckDispatch failed: %v", err)
	}

	// All 3 intents start as PENDING
	for _, id := range []string{"job-sib-0", "job-sib-1", "job-sib-2"} {
		st, _, err := s.GetJobDispatchIntentStatus(ctx, id)
		if err != nil || st != "PENDING" {
			t.Fatalf("expected job %s to be PENDING, got %s (err=%v)", id, st, err)
		}
	}

	// 1. Attempting to pass orderID to MarkDispatchIntentSent must NOT mutate any child jobs!
	_ = s.MarkDispatchIntentSent(ctx, orderID)
	for _, id := range []string{"job-sib-0", "job-sib-1", "job-sib-2"} {
		st, _, _ := s.GetJobDispatchIntentStatus(ctx, id)
		if st != "PENDING" {
			t.Fatalf("VIOLATION: orderID mutation caused child %s to transition to %s!", id, st)
		}
	}

	// 2. Mark job-sib-0 as SENT: siblings job-sib-1 and job-sib-2 must remain PENDING
	if err := s.MarkDispatchIntentSent(ctx, "job-sib-0"); err != nil {
		t.Fatalf("MarkDispatchIntentSent(job-sib-0) failed: %v", err)
	}
	st0, _, _ := s.GetJobDispatchIntentStatus(ctx, "job-sib-0")
	if st0 != "SENT" {
		t.Errorf("expected job-sib-0 to be SENT, got %s", st0)
	}
	st1, _, _ := s.GetJobDispatchIntentStatus(ctx, "job-sib-1")
	st2, _, _ := s.GetJobDispatchIntentStatus(ctx, "job-sib-2")
	if st1 != "PENDING" || st2 != "PENDING" {
		t.Errorf("sibling jobs mutated accidentally: st1=%s, st2=%s", st1, st2)
	}

	// 3. Attempting to pass orderID to MarkDispatchIntentAcknowledged must NOT mutate child jobs!
	_ = s.MarkDispatchIntentAcknowledged(ctx, orderID)
	st0, _, _ = s.GetJobDispatchIntentStatus(ctx, "job-sib-0")
	if st0 != "SENT" {
		t.Errorf("job-sib-0 mutated by orderID ack: got %s", st0)
	}

	// 4. Mark job-sib-0 as ACKNOWLEDGED: siblings remain untouched
	if err := s.MarkDispatchIntentAcknowledged(ctx, "job-sib-0"); err != nil {
		t.Fatalf("MarkDispatchIntentAcknowledged(job-sib-0) failed: %v", err)
	}
	st0, _, _ = s.GetJobDispatchIntentStatus(ctx, "job-sib-0")
	if st0 != "ACKNOWLEDGED" {
		t.Errorf("expected job-sib-0 ACKNOWLEDGED, got %s", st0)
	}
	st1, _, _ = s.GetJobDispatchIntentStatus(ctx, "job-sib-1")
	if st1 != "PENDING" {
		t.Errorf("job-sib-1 mutated accidentally: %s", st1)
	}

	// 5. Attempting to pass orderID to MarkDispatchIntentCompleted must NOT mutate child jobs!
	_ = s.MarkDispatchIntentCompleted(ctx, orderID)
	st0, _, _ = s.GetJobDispatchIntentStatus(ctx, "job-sib-0")
	st1, _, _ = s.GetJobDispatchIntentStatus(ctx, "job-sib-1")
	if st0 != "ACKNOWLEDGED" || st1 != "PENDING" {
		t.Errorf("orderID complete mutated child jobs: st0=%s, st1=%s", st0, st1)
	}

	// 6. Mark job-sib-0 COMPLETED: job-sib-1 remains PENDING
	if err := s.MarkDispatchIntentCompleted(ctx, "job-sib-0"); err != nil {
		t.Fatalf("MarkDispatchIntentCompleted(job-sib-0) failed: %v", err)
	}
	st0, _, _ = s.GetJobDispatchIntentStatus(ctx, "job-sib-0")
	if st0 != "COMPLETED" {
		t.Errorf("expected job-sib-0 COMPLETED, got %s", st0)
	}
	st1, _, _ = s.GetJobDispatchIntentStatus(ctx, "job-sib-1")
	if st1 != "PENDING" {
		t.Errorf("job-sib-1 mutated accidentally: %s", st1)
	}

	// 7. GetJobDispatchIntentStatus with orderID fails cleanly
	_, _, err = s.GetJobDispatchIntentStatus(ctx, orderID)
	if err == nil {
		t.Errorf("expected GetJobDispatchIntentStatus(%s) to fail with not found, got nil", orderID)
	}

	// 8. OrderDispatchSummary reflects exact counts
	summary, err := s.GetOrderDispatchSummary(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrderDispatchSummary failed: %v", err)
	}
	if summary.TotalCount != 3 || summary.CompletedCount != 1 || summary.PendingCount != 2 {
		t.Errorf("unexpected summary: %+v", summary)
	}
}

func TestCapturePayment_ConflictingPaymentIDReuse_Rejected(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	kioskID := "kiosk-conf"

	// Create Order A
	ordA := Order{
		OrderID:     "ord-conf-A",
		Command:     contracts.PrintJobCommand{KioskID: kioskID, DisplayCode: "1111"},
		Status:      contracts.JobStatusPendingPayment,
		PricePaise:  200,
		TotalPages:  1,
		TotalSheets: 1,
		Documents:   []OrderDocument{{JobID: "job-conf-A", OrderID: "ord-conf-A", DocumentIndex: 0, TotalPages: 1, TotalSheets: 1, Copies: 1, Duplex: contracts.DuplexSimplex, PricePaise: 200}},
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.CreateOrder(ctx, ordA); err != nil {
		t.Fatalf("CreateOrder A failed: %v", err)
	}

	// Create Order B
	ordB := Order{
		OrderID:     "ord-conf-B",
		Command:     contracts.PrintJobCommand{KioskID: kioskID, DisplayCode: "2222"},
		Status:      contracts.JobStatusPendingPayment,
		PricePaise:  200,
		TotalPages:  1,
		TotalSheets: 1,
		Documents:   []OrderDocument{{JobID: "job-conf-B", OrderID: "ord-conf-B", DocumentIndex: 0, TotalPages: 1, TotalSheets: 1, Copies: 1, Duplex: contracts.DuplexSimplex, PricePaise: 200}},
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.CreateOrder(ctx, ordB); err != nil {
		t.Fatalf("CreateOrder B failed: %v", err)
	}

	sessA, err := s.CreateOrGetKioskSessionWithPaperCheck(ctx, kioskID, "ord-conf-A", 10, 15*time.Minute, now)
	if err != nil {
		t.Fatalf("CreateOrGetKioskSessionWithPaperCheck A failed: %v", err)
	}

	// Capture payment for Order A
	payA := PaymentRecord{
		PaymentID:   "pay-unique-123",
		OrderID:     "ord-conf-A",
		KioskID:     kioskID,
		SessionID:   sessA.SessionID,
		AmountPaise: 200,
		Status:      "captured",
		Provider:    "razorpay",
	}
	resA, err := s.CapturePaymentAndCheckDispatch(ctx, payA, now)
	if err != nil || !resA.DispatchEligible {
		t.Fatalf("CapturePaymentAndCheckDispatch A failed: %v", err)
	}

	// Attempting to reuse same paymentID for Order B must be explicitly rejected!
	payBConflicting := PaymentRecord{
		PaymentID:   "pay-unique-123",
		OrderID:     "ord-conf-B",
		KioskID:     kioskID,
		AmountPaise: 200,
		Status:      "captured",
		Provider:    "razorpay",
	}
	_, err = s.CapturePaymentAndCheckDispatch(ctx, payBConflicting, now)
	if err == nil {
		t.Fatal("expected error on conflicting payment ID reuse for different order, got nil")
	}
	if !strings.Contains(err.Error(), "conflicting order") {
		t.Errorf("expected error message to mention conflicting order, got %v", err)
	}

	// Attempting to reuse same paymentID for Order A but with mismatched amount must be rejected!
	payAMismatchedAmount := PaymentRecord{
		PaymentID:   "pay-unique-123",
		OrderID:     "ord-conf-A",
		KioskID:     kioskID,
		AmountPaise: 500, // Mismatched!
		Status:      "captured",
		Provider:    "razorpay",
	}
	_, err = s.CapturePaymentAndCheckDispatch(ctx, payAMismatchedAmount, now)
	if err == nil {
		t.Fatal("expected error on payment amount mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "amount mismatch") {
		t.Errorf("expected error message to mention amount mismatch, got %v", err)
	}
}

func TestGetPendingDispatchOrders_ChildDocumentReadFailure_FailsSafely(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	kioskID := "kiosk-missing-doc"
	orderID := "ord-missing-doc-1"

	// Create order in DISPATCHED status
	ord := Order{
		OrderID:     orderID,
		Command:     contracts.PrintJobCommand{JobID: "parent-job", KioskID: kioskID, DisplayCode: "5555", TotalPages: 1, TotalSheets: 1, Copies: 1, Duplex: contracts.DuplexSimplex, StartFromPage: 1},
		Status:      contracts.JobStatusDispatched,
		PricePaise:  100,
		TotalPages:  1,
		TotalSheets: 1,
		Documents:   nil, // No documents!
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	// Add verified captured payment
	_, err := s.db.Exec(`INSERT INTO payments (payment_id, order_id, kiosk_id, amount_paise, status, provider, created_at_ns) VALUES ('pay-m-1', ?, ?, 100, 'captured', 'DEMO', ?);`,
		orderID, kioskID, now.UnixNano())
	if err != nil {
		t.Fatalf("insert payment failed: %v", err)
	}

	// Insert dispatch intent referencing a child job that does NOT exist in order_documents
	_, err = s.db.Exec(`INSERT INTO dispatch_intents (job_id, order_id, kiosk_id, status, created_at_ns, updated_at_ns) VALUES ('nonexistent-job-xyz', ?, ?, 'PENDING', ?, ?);`,
		orderID, kioskID, now.UnixNano(), now.UnixNano())
	if err != nil {
		t.Fatalf("insert intent failed: %v", err)
	}

	// GetPendingDispatchOrders MUST FAIL SAFELY and NEVER fall back to ord.Command!
	cmds, err := s.GetPendingDispatchOrders(ctx, kioskID)
	if err == nil {
		t.Fatalf("expected GetPendingDispatchOrders to fail on missing child document, got cmds: %+v", cmds)
	}
	if !strings.Contains(err.Error(), "not found in order_documents") {
		t.Errorf("expected error to mention child document not found in order_documents, got: %v", err)
	}
}

func TestGetPendingDispatchOrders_FailedPaymentStatus_NotDispatched(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	kioskID := "kiosk-pay-status"
	orderID := "ord-pay-failed-1"

	docs := []OrderDocument{
		{JobID: "job-pf-0", OrderID: orderID, DocumentIndex: 0, TotalPages: 1, TotalSheets: 1, Copies: 1, Duplex: contracts.DuplexSimplex, PricePaise: 100},
	}
	ord := Order{
		OrderID:     orderID,
		Command:     contracts.PrintJobCommand{KioskID: kioskID, DisplayCode: "9988"},
		Status:      contracts.JobStatusDispatched,
		PricePaise:  100,
		TotalPages:  1,
		TotalSheets: 1,
		Documents:   docs,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	// Add payment with status "failed"
	_, err := s.db.Exec(`INSERT INTO payments (payment_id, order_id, kiosk_id, amount_paise, status, provider, created_at_ns) VALUES ('pay-failed-1', ?, ?, 100, 'failed', 'DEMO', ?);`,
		orderID, kioskID, now.UnixNano())
	if err != nil {
		t.Fatalf("insert payment failed: %v", err)
	}

	// Insert pending intent
	_, err = s.db.Exec(`INSERT INTO dispatch_intents (job_id, order_id, kiosk_id, status, created_at_ns, updated_at_ns) VALUES ('job-pf-0', ?, ?, 'PENDING', ?, ?);`,
		orderID, kioskID, now.UnixNano(), now.UnixNano())
	if err != nil {
		t.Fatalf("insert intent failed: %v", err)
	}

	// GetPendingDispatchOrders must skip this order because payment status is failed
	cmds, err := s.GetPendingDispatchOrders(ctx, kioskID)
	if err != nil {
		t.Fatalf("GetPendingDispatchOrders failed: %v", err)
	}
	for _, c := range cmds {
		if c.OrderID == orderID {
			t.Fatalf("VIOLATION: order with failed payment was dispatched!")
		}
	}
}
