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

func TestMultiDocumentPayment_DispatchPreventedUntilSpoolerReady(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	orderID := "ord-disp-prevent"
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

	// SAFEGUARD VERIFICATION: Multi-job orders must NOT be dispatched to the kiosk!
	if result.DispatchEligible {
		t.Fatalf("VIOLATION: multi-job order was marked DispatchEligible=true before kiosk spooler upgrade!")
	}
	if result.OrderStatus != contracts.JobStatusPaidWaiting {
		t.Errorf("expected order status PAID_WAITING, got %s", result.OrderStatus)
	}
	if len(result.Commands) != 0 {
		t.Errorf("expected 0 dispatched commands for multi-job order, got %d", len(result.Commands))
	}

	// Check dispatch_intents has 0 records (no intent created for multi-job order)
	var intentCount int
	err = s.db.QueryRow("SELECT COUNT(1) FROM dispatch_intents WHERE order_id = ?", orderID).Scan(&intentCount)
	if err != nil {
		t.Fatalf("query intent count failed: %v", err)
	}
	if intentCount != 0 {
		t.Fatalf("expected 0 dispatch_intents rows, got %d", intentCount)
	}

	// Attempting ReleasePaidOrder on multi-job order must fail explicitly
	_, err = s.ReleasePaidOrder(ctx, kioskID, orderID, sess.SessionID, now)
	if err == nil {
		t.Fatal("expected ReleasePaidOrder to fail on multi-job order, got nil")
	}

	// GetPendingDispatchOrders must NOT return commands for multi-job orders
	pendingCmds, err := s.GetPendingDispatchOrders(ctx, kioskID)
	if err != nil {
		t.Fatalf("GetPendingDispatchOrders failed: %v", err)
	}
	for _, c := range pendingCmds {
		if c.OrderID == orderID {
			t.Fatalf("VIOLATION: GetPendingDispatchOrders returned command for multi-job order %s", orderID)
		}
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
	st, _, err := s.GetDispatchIntentStatus(ctx, "job-single-1")
	if err != nil {
		t.Fatalf("GetDispatchIntentStatus failed: %v", err)
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
