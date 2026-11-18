package store

import (
 "context"
 "errors"
 "path/filepath"
 "testing"
 "time"

 "xeroxgo/pkg/contracts"
)

func cloudCheckoutTestStore(t *testing.T) (*Store, CloudOrderInput) {
 t.Helper()
 db,err:=Open(filepath.Join(t.TempDir(),"checkout.sqlite"))
 if err!=nil {t.Fatal(err)}
 t.Cleanup(func(){_ =db.Close()})
 ctx:=context.Background()
 tariff,err:=db.ActivateSheetTariff(ctx,0,sampleSheetTariff(),"test")
 if err!=nil {t.Fatal(err)}
 statements:=[]string{
  `CREATE TABLE document_uploads(
    upload_id TEXT PRIMARY KEY, state TEXT, expires_at TIMESTAMP)`,
  `CREATE TABLE document_variants(
    variant_id TEXT PRIMARY KEY, upload_id TEXT, state TEXT, expires_at TIMESTAMP,
    canonical_price_paise BIGINT,tariff_revision BIGINT,checksum_sha256 TEXT,
    billable_sides INTEGER,total_sheets INTEGER,storage_key TEXT,
    duplex TEXT,color_mode TEXT,copies INTEGER)`,
  `CREATE TABLE order_documents(
    order_id TEXT NOT NULL REFERENCES orders(order_id),
    sequence_no INTEGER NOT NULL,variant_id TEXT NOT NULL,
    accepted_price_paise BIGINT NOT NULL,PRIMARY KEY(order_id,sequence_no))`,
  `CREATE TABLE cloud_order_requests(
    request_id TEXT PRIMARY KEY,upload_id TEXT,variant_id TEXT,
    order_id TEXT UNIQUE,kiosk_id TEXT,accepted_price_paise BIGINT,
    tariff_revision BIGINT,UNIQUE(upload_id,variant_id))`,
 }
 for _,q:=range statements {if _,err=db.DB().Exec(q);err!=nil {t.Fatal(err)}}
 expires:=time.Now().UTC().Add(4*time.Hour)
 _,err=db.DB().Exec(`INSERT INTO document_uploads(upload_id,state,expires_at)
 VALUES('upload-a','UPLOADED',?)`,expires)
 if err!=nil {t.Fatal(err)}
 hash:="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
 _,err=db.DB().Exec(`INSERT INTO document_variants
  (variant_id,upload_id,state,expires_at,canonical_price_paise,
   tariff_revision,checksum_sha256,billable_sides,total_sheets,storage_key,duplex,color_mode,copies)
  VALUES('variant-a','upload-a','READY',?,?,?,?,?,?,?,?,?,?,?)`,
  expires,500,tariff.Revision,hash,2,1,"variants/variant-a/output.pdf","DUPLEX_LONG_EDGE","BW",1)
 if err!=nil {t.Fatal(err)}
 order:=Order{OrderID:"ord-cloud-1",Status:contracts.JobStatusPendingPayment,
  PricePaise:500,ColorMode:"BW",PINExpiresAt:time.Now().UTC().Add(15*time.Minute),
  ClaimToken:"claim-secret",
  Command:contracts.PrintJobCommand{JobID:"job-cloud-1",KioskID:"kiosk-1",
   DisplayCode:"1234",DownloadURL:"https://example.test/api/orders/ord-cloud-1/pdf?sig=valid",
   SHA256Checksum:hash,TotalPages:2,TotalSheets:1,Copies:1,
   Duplex:contracts.DuplexLongEdge,StartFromPage:1}}
 return db,CloudOrderInput{RequestID:"request-a",UploadID:"upload-a",
  VariantID:"variant-a",AcceptedPricePaise:500,TariffRevision:tariff.Revision,Order:order}
}

func TestCloudCheckoutAtomicIdempotency(t *testing.T) {
 db,in:=cloudCheckoutTestStore(t)
 ctx:=context.Background()
 id,created,err:=db.CreateCloudOrderAtomic(ctx,in)
 if err!=nil||!created||id!=in.Order.OrderID {t.Fatalf("first order %q %v %v",id,created,err)}
 same:=in
 same.Order.OrderID="ord-different"
 same.Order.Command.JobID="job-different"
 same.Order.Command.DisplayCode="9999"
 id,created,err=db.CreateCloudOrderAtomic(ctx,same)
 if err!=nil||created||id!=in.Order.OrderID {t.Fatalf("retry %q %v %v",id,created,err)}
 original,err:=db.GetOrder(ctx,id)
 if err!=nil||original==nil||original.Command.DisplayCode!="1234" {
  t.Fatalf("original order/PIN not reused %+v %v",original,err)
 }
 var n int
 if err=db.DB().QueryRow(`SELECT COUNT(*) FROM orders WHERE order_id='ord-cloud-1'`).Scan(&n);err!=nil||n!=1 {
  t.Fatalf("order count %d %v",n,err)
 }
 if err=db.DB().QueryRow(`SELECT COUNT(*) FROM order_documents WHERE order_id='ord-cloud-1'`).Scan(&n);err!=nil||n!=1 {
  t.Fatalf("reference count %d %v",n,err)
 }
 key,hash,exists,err:=db.CloudOrderPreparedObject(ctx,id)
 if err!=nil||!exists||key!="variants/variant-a/output.pdf"||hash!=in.Order.Command.SHA256Checksum{
  t.Fatalf("artifact %q %q %v %v",key,hash,exists,err)
 }
}

func TestCloudCheckoutRejectsPriceAndTariffMismatchWithoutOrder(t *testing.T) {
 cases:=[]struct{name string;mutate func(*CloudOrderInput)}{
  {"wrong price",func(in *CloudOrderInput){in.AcceptedPricePaise=501;in.Order.PricePaise=501}},
  {"wrong revision",func(in *CloudOrderInput){in.TariffRevision++}},
  {"wrong duplex",func(in *CloudOrderInput){in.Order.Command.Duplex=contracts.DuplexSimplex}},
  {"wrong checksum",func(in *CloudOrderInput){in.Order.Command.SHA256Checksum="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}},
  {"wrong source",func(in *CloudOrderInput){in.UploadID="upload-other"}},
 }
 for _,tc:=range cases {
  t.Run(tc.name,func(t *testing.T){
   db,in:=cloudCheckoutTestStore(t);tc.mutate(&in)
   _,created,err:=db.CreateCloudOrderAtomic(context.Background(),in)
   if err==nil||created {t.Fatalf("accepted invalid order: %v",err)}
   var count int
   if err=db.DB().QueryRow("SELECT COUNT(*) FROM orders").Scan(&count);err!=nil||count!=0 {
    t.Fatalf("partial order persisted %d %v",count,err)
   }
  })
 }
}

func TestCloudCheckoutOneOrderPerVariantAndConflictingRetry(t *testing.T){
 db,in:=cloudCheckoutTestStore(t)
 if _,_,err:=db.CreateCloudOrderAtomic(context.Background(),in);err!=nil {t.Fatal(err)}
 different:=in;different.RequestID="different-key";different.Order.OrderID="ord-2"
 different.Order.Command.JobID="job-2";different.Order.Command.DisplayCode="9876"
 _,_,err:=db.CreateCloudOrderAtomic(context.Background(),different)
 if err==nil {t.Fatal("second order from same prepared variant accepted")}
 changed:=in;changed.VariantID="another-variant"
 _,_,err=db.CreateCloudOrderAtomic(context.Background(),changed)
 if !errors.Is(err,ErrCloudOrderConflict){t.Fatalf("reused idempotency key accepted %v",err)}
}
