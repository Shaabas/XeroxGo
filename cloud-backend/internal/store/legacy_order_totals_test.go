package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

// A historical order can have a valid print_config and/or child documents
// while its new aggregate columns remain at an older schema's default zero.
// Backfill from authoritative document data rather than allowing customers
// to bypass physical paper accounting.
func TestLegacyOrderTotals_BackfillFromPrintConfigs(t *testing.T) {
	dbPath:=filepath.Join(t.TempDir(),"legacy_totals.db")
	s,err:=Open(dbPath)
	if err!=nil{t.Fatal(err)}
	ctx:=context.Background()
	now:=time.Now().UTC()
	const orderID,kioskID="legacy_total_order","legacy_total_kiosk"
	createTestOrderWithSheets(t,s,ctx,orderID,kioskID,"8253",4,contracts.JobStatusPendingPayment,now)
	if _,err:=s.db.ExecContext(ctx,"UPDATE orders SET total_pages=0,total_sheets=0 WHERE order_id=?",orderID);err!=nil{
		t.Fatal(err)
	}
	if err:=s.Close();err!=nil{t.Fatal(err)}
	s,err=Open(dbPath)
	if err!=nil{t.Fatal(err)}
	defer s.Close()
	ord,err:=s.GetOrder(ctx,orderID)
	if err!=nil||ord==nil||ord.TotalPages!=4||ord.TotalSheets!=4 {
		t.Fatalf("legacy totals failed to backfill: %+v %v",ord,err)
	}
	committed,err:=s.GetCommittedPaperSheets(ctx,kioskID,"",now)
	if err!=nil||committed!=4{
		t.Fatalf("restored order did not reserve paper: %d %v",committed,err)
	}
}
