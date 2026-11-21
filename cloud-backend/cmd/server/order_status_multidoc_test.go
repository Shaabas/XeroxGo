package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

type customerOrderStatus struct {
	Status             string `json:"status"`
	TotalPages         int    `json:"total_pages"`
	TotalSheets        int    `json:"total_sheets"`
	PagesPrinted       int    `json:"pages_printed"`
	SheetsPrinted      int    `json:"sheets_printed"`
	DocumentsTotal     int    `json:"documents_total"`
	DocumentsCompleted int    `json:"documents_completed"`
	ErrorReason        string `json:"error_reason"`
}

func statusForOrder(t *testing.T, app *appServer, orderID string) customerOrderStatus {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/orders/"+orderID+"/status", nil)
	app.handleOrderStatus(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("order status for %s returned %d: %s", orderID, w.Code, w.Body.String())
	}
	var result customerOrderStatus
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil { t.Fatal(err) }
	return result
}

func seedMultiDocStatusOrder(t *testing.T, st *store.Store, orderID string) ([]store.OrderDocument, string) {
	t.Helper()
	const kioskID = "kiosk_multidoc_status"
	sizes := []struct {pages, sheets int}{{2,1},{3,2},{1,1}}
	docs := make([]store.OrderDocument,0,len(sizes))
	var totalPages,totalSheets int
	var totalPrice int64
	for i, size := range sizes {
		id := fmt.Sprintf("%s_doc_%d",orderID,i)
		price := int64(size.sheets * 200)
		docs = append(docs,store.OrderDocument{
			JobID:id,OrderID:orderID,DocumentIndex:i,
			OriginalFilename:fmt.Sprintf("document_%d.pdf",i),
			DownloadURL:fmt.Sprintf("https://storage.example.test/%s.pdf",id),
			SHA256Checksum:"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			TotalPages:size.pages,TotalSheets:size.sheets,Copies:1,
			Duplex:contracts.DuplexSimplex,ColorMode:"BW",
			Orientation:"AUTO",PagesPerSheet:1,PricePaise:price,
			Status:contracts.JobStatusDispatched,
		})
		totalPages+=size.pages
		totalSheets+=size.sheets
		totalPrice+=price
	}
	first:=docs[0]
	ord:=store.Order{
		OrderID:orderID,Status:contracts.JobStatusDispatched,
		TotalPages:totalPages,TotalSheets:totalSheets,PricePaise:totalPrice,
		PINExpiresAt:time.Now().UTC().Add(5*time.Minute),
		Documents:docs,
		Command:contracts.PrintJobCommand{
			JobID:first.JobID,OrderID:orderID,KioskID:kioskID,
			DisplayCode:"7428",DownloadURL:first.DownloadURL,
			SHA256Checksum:first.SHA256Checksum,TotalPages:first.TotalPages,
			TotalSheets:first.TotalSheets,Copies:1,
			Duplex:contracts.DuplexSimplex,StartFromPage:1,
		},
	}
	if err:=st.CreateOrder(context.Background(),ord);err!=nil{t.Fatal(err)}
	return docs,kioskID
}

func recordDocumentStatus(t *testing.T,st *store.Store,jobID,kioskID string,
	status contracts.JobStatus,pages,sheets int,reason string,at time.Time) {
	t.Helper()
	if err:=st.UpdateJobProgress(context.Background(),contracts.JobProgressEvent{
		JobID:jobID,KioskID:kioskID,Status:status,
		PagesPrinted:pages,SheetsPrinted:sheets,
		ErrorReason:reason,Timestamp:at,
	});err!=nil{t.Fatal(err)}
}

func TestOrderStatus_MultiDocumentOnlyCompletedAfterFinalChild(t *testing.T) {
	st,err:=store.Open(filepath.Join(t.TempDir(),"order_status.db"))
	if err!=nil{t.Fatal(err)}
	defer st.Close()
	const orderID="ord_status_three_documents"
	docs,kioskID:=seedMultiDocStatusOrder(t,st,orderID)
	app:=&appServer{db:st}
	now:=time.Now().UTC()

	initial:=statusForOrder(t,app,orderID)
	if initial.Status!=string(contracts.JobStatusDispatched) ||
		initial.DocumentsTotal!=3 || initial.DocumentsCompleted!=0 ||
		initial.PagesPrinted!=0 || initial.SheetsPrinted!=0 {
		t.Fatalf("initial order-wide progress mismatch: %+v",initial)
	}
	recordDocumentStatus(t,st,docs[0].JobID,kioskID,contracts.JobStatusCompleted,2,1,"",now)
	first:=statusForOrder(t,app,orderID)
	if first.Status=="COMPLETED" || first.DocumentsTotal!=3 ||
		first.DocumentsCompleted!=1 || first.PagesPrinted!=2 ||
		first.SheetsPrinted!=1 || first.TotalPages!=6 || first.TotalSheets!=4 {
		t.Fatalf("first child falsely completed whole order: %+v",first)
	}

	recordDocumentStatus(t,st,docs[1].JobID,kioskID,contracts.JobStatusPrinting,1,1,"",now.Add(time.Second))
	middle:=statusForOrder(t,app,orderID)
	if middle.Status!="PRINTING" || middle.DocumentsCompleted!=1 ||
		middle.PagesPrinted!=3 || middle.SheetsPrinted!=2 {
		t.Fatalf("middle child not included in order-wide counters: %+v",middle)
	}
	recordDocumentStatus(t,st,docs[1].JobID,kioskID,contracts.JobStatusCompleted,3,2,"",now.Add(2*time.Second))
	second:=statusForOrder(t,app,orderID)
	if second.Status=="COMPLETED" || second.DocumentsCompleted!=2 ||
		second.PagesPrinted!=5 || second.SheetsPrinted!=3 {
		t.Fatalf("second child prematurely ended customer checkout: %+v",second)
	}
	recordDocumentStatus(t,st,docs[2].JobID,kioskID,contracts.JobStatusCompleted,1,1,"",now.Add(3*time.Second))
	last:=statusForOrder(t,app,orderID)
	if last.Status!="COMPLETED" || last.DocumentsTotal!=3 ||
		last.DocumentsCompleted!=3 || last.PagesPrinted!=6 || last.SheetsPrinted!=4 {
		t.Fatalf("all documents completed but customer status wrong: %+v",last)
	}
}

func TestOrderStatus_MultiDocumentFaultReportsLaterChildNotFirst(t *testing.T) {
	st,err:=store.Open(filepath.Join(t.TempDir(),"order_status_fault.db"))
	if err!=nil{t.Fatal(err)}
	defer st.Close()
	const orderID="ord_status_later_fault"
	docs,kioskID:=seedMultiDocStatusOrder(t,st,orderID)
	app:=&appServer{db:st}
	now:=time.Now().UTC()
	recordDocumentStatus(t,st,docs[0].JobID,kioskID,contracts.JobStatusCompleted,2,1,"",now)
	recordDocumentStatus(t,st,docs[1].JobID,kioskID,contracts.JobStatusFailedRefundable,1,1,
		"paper jam during second document",now.Add(time.Second))
	status:=statusForOrder(t,app,orderID)
	if status.Status!="FAILED_REFUNDABLE" || status.ErrorReason!="paper jam during second document" ||
		status.DocumentsCompleted!=1 || status.PagesPrinted!=3 || status.SheetsPrinted!=2 {
		t.Fatalf("later document fault hidden by first child's completion: %+v",status)
	}
}

func TestOrderStatus_PausedSecondDocumentOverridesFirstCompletion(t *testing.T) {
	st,err:=store.Open(filepath.Join(t.TempDir(),"order_status_paused.db"))
	if err!=nil{t.Fatal(err)}
	defer st.Close()
	const orderID="ord_status_second_paused"
	docs,kioskID:=seedMultiDocStatusOrder(t,st,orderID)
	app:=&appServer{db:st}
	now:=time.Now().UTC()
	recordDocumentStatus(t,st,docs[0].JobID,kioskID,contracts.JobStatusCompleted,2,1,"",now)
	recordDocumentStatus(t,st,docs[1].JobID,kioskID,contracts.JobStatusPausedError,1,1,
		"paper jam during second document",now.Add(time.Second))
	status:=statusForOrder(t,app,orderID)
	if status.Status!="PAUSED_ERROR" || status.ErrorReason!="paper jam during second document" ||
		status.DocumentsCompleted!=1 || status.PagesPrinted!=3 || status.SheetsPrinted!=2 {
		t.Fatalf("later document pause hidden by first child's completion: %+v",status)
	}
}

func TestOrderStatus_CorruptCompletedParentFailsClosed(t *testing.T) {
	st,err:=store.Open(filepath.Join(t.TempDir(),"order_status_invariant.db"))
	if err!=nil{t.Fatal(err)}
	defer st.Close()
	const orderID="ord_status_bad_parent"
	docs,kioskID:=seedMultiDocStatusOrder(t,st,orderID)
	now:=time.Now().UTC()
	recordDocumentStatus(t,st,docs[0].JobID,kioskID,contracts.JobStatusCompleted,2,1,"",now)
	// Simulate a bad administrative write or inconsistent older deployment.
	if _,err:=st.DB().ExecContext(context.Background(),
		"UPDATE orders SET status = 'COMPLETED' WHERE order_id = ?",orderID);err!=nil{t.Fatal(err)}
	status:=statusForOrder(t,&appServer{db:st},orderID)
	if status.Status=="COMPLETED" || status.DocumentsTotal!=3 || status.DocumentsCompleted!=1 {
		t.Fatalf("corrupt parent incorrectly exposed as a completed order: %+v",status)
	}
}

func TestOrderStatus_LegacySingleJobProgressPreserved(t *testing.T) {
	st,err:=store.Open(filepath.Join(t.TempDir(),"order_status_legacy.db"))
	if err!=nil{t.Fatal(err)}
	defer st.Close()
	ctx:=context.Background()
	const orderID,kioskID="ord_status_old_single","kiosk_old_single"
	ord:=store.Order{
		OrderID:orderID,Status:contracts.JobStatusDispatched,PricePaise:200,
		PINExpiresAt:time.Now().UTC().Add(5*time.Minute),
		Command:contracts.PrintJobCommand{
			JobID:"job_legacy_status",OrderID:orderID,KioskID:kioskID,
			DisplayCode:"3561",DownloadURL:"https://storage.example.test/legacy.pdf",
			SHA256Checksum:"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			TotalPages:2,TotalSheets:2,Copies:1,
			Duplex:contracts.DuplexSimplex,StartFromPage:1,
		},
	}
	if err:=st.CreateOrder(ctx,ord);err!=nil{t.Fatal(err)}
	// Old DBs can have print_configs and progress without child records.
	if _,err:=st.DB().ExecContext(ctx,"DELETE FROM order_documents WHERE order_id=?",orderID);err!=nil{t.Fatal(err)}
	recordDocumentStatus(t,st,ord.Command.JobID,kioskID,contracts.JobStatusPrinting,1,1,"",time.Now().UTC())
	mid:=statusForOrder(t,&appServer{db:st},orderID)
	if mid.DocumentsTotal!=0 || mid.Status!="PRINTING" || mid.PagesPrinted!=1 || mid.SheetsPrinted!=1{
		t.Fatalf("legacy single-file reporting regressed: %+v",mid)
	}
}
