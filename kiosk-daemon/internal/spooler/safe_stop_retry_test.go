package spooler

import (
	"context"
	"image"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/pkg/contracts"
)

// Persisted paused children, not an in-memory map, must rebuild the Pi's
// safety barrier after restart. Previously queued siblings must become
// PAUSED_ERROR without a new CUPS submission.
func TestRefundSafeStop_RestartRebuildsFaultBarrierAndPausesSibling(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "safe_stop_restart.db")
	st, err := state.Open(dbPath)
	if err != nil { t.Fatal(err) }
	const orderID, kioskID = "order_retry_stop", "kiosk_retry_stop"
	doc := func(id string, index int) contracts.PrintJobCommand {
		return contracts.PrintJobCommand{
			JobID: id, OrderID: orderID, KioskID: kioskID,
			DocumentIndex: index, TotalDocuments: 2, DisplayCode: "4283",
			DownloadURL: "https://example.invalid/"+id+".pdf",
			SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			TotalPages: 1, TotalSheets: 1, Copies: 1,
			Duplex: contracts.DuplexSimplex, StartFromPage: 1,
		}
	}
	first := doc("job_retry_stop_0",0)
	second := doc("job_retry_stop_1",1)
	if err:=st.SaveJob(ctx,first,contracts.JobStatusSpooling);err!=nil{t.Fatal(err)}
	if err:=st.SaveJob(ctx,second,contracts.JobStatusSpooling);err!=nil{t.Fatal(err)}
	if err:=st.RecordSheetProgress(ctx,contracts.JobProgressEvent{
		JobID:first.JobID,KioskID:kioskID,Status:contracts.JobStatusPausedError,
		PagesPrinted:0,SheetsPrinted:0,ErrorReason:"paper jam in first document",
		Timestamp:time.Now().UTC(),
	});err!=nil{t.Fatal(err)}
	if err:=st.Close();err!=nil{t.Fatal(err)}

	restarted,err:=state.Open(dbPath)
	if err!=nil{t.Fatal(err)}
	defer restarted.Close()
	var sentToCUPS atomic.Int32
	client,err:=NewClient(ClientConfig{
		CloudURL:"ws://localhost:12345/ws",KioskID:kioskID,
		StateStore:restarted,DownloadDir:t.TempDir(),IdleQRContent:"https://example.invalid/qr",
		ScreenSink:func(*image.RGBA)error{return nil},
		CUPSQueue:"MockQueue",
		MockCUPSPrint:func(context.Context,string,contracts.PrintJobCommand,string,func(int))error{
			sentToCUPS.Add(1);return nil
		},
	})
	if err!=nil{t.Fatal(err)}
	client.reconcilePausedOrderSafeStops(ctx)
	if _,blocked:=client.blockedPrintOrders.Load(orderID);!blocked{
		t.Fatal("restart lost the persisted paused-order safety barrier")
	}
	client.processJob(ctx,second)
	if got:=sentToCUPS.Load();got!=0{
		t.Fatalf("faulted sibling was submitted to CUPS %d times",got)
	}
	stored,err:=restarted.GetJob(ctx,second.JobID)
	if err!=nil||stored==nil||stored.Status!=contracts.JobStatusPausedError{
		t.Fatalf("sibling remains stuck or got printed after restart: %+v %v",stored,err)
	}
}
