package store

import (
 "context"
 "database/sql"
 "errors"
 "strings"
 "testing"
 "time"
)

// This uses the same SQLite store fixture as the existing atomic checkout
// tests, proving that transaction ownership remains with the caller.
func setupCloudMultiPlanTxTest(t *testing.T) (*Store, []CloudPreparedReference, int64) {
 t.Helper()
 db, in := cloudCheckoutTestStore(t)
 if _, err := db.DB().Exec("ALTER TABLE document_uploads ADD COLUMN kiosk_id TEXT"); err != nil {
  t.Fatal(err)
 }
 if _, err := db.DB().Exec("UPDATE document_uploads SET kiosk_id='kiosk-1'"); err != nil {
  t.Fatal(err)
 }
 expiry := time.Now().UTC().Add(time.Hour)
 if _, err := db.DB().Exec(`INSERT INTO document_uploads
  (upload_id,state,expires_at,kiosk_id) VALUES ('upload-b','UPLOADED',?,'kiosk-1')`, expiry); err != nil {
  t.Fatal(err)
 }
 if _, err := db.DB().Exec(`INSERT INTO document_variants
  (variant_id,upload_id,state,expires_at,canonical_price_paise,
   tariff_revision,checksum_sha256,billable_sides,total_sheets,
   storage_key,duplex,color_mode,copies)
  VALUES ('variant-b','upload-b','READY',?,?,?,?,?,?,?,?,?,?)`,
  expiry, 600, in.TariffRevision, strings.Repeat("b",64),
  3, 2, "variants/variant-b/output.pdf", "SIMPLEX", "BW", 2); err != nil {
  t.Fatal(err)
 }
 return db, []CloudPreparedReference{
  {UploadID:"upload-b",VariantID:"variant-b"},
  {UploadID:"upload-a",VariantID:"variant-a"},
 }, in.TariffRevision
}

func withinCloudCheckoutTx(t *testing.T, db *Store, fn func(*sql.Tx)) {
 t.Helper()
 tx, err := db.DB().BeginTx(context.Background(), nil)
 if err != nil { t.Fatal(err) }
 defer tx.Rollback()
 fn(tx)
}

func TestCloudPlanTxLocksAuthoritativeInputsAndRetainsOrder(t *testing.T) {
 db, refs, revision := setupCloudMultiPlanTxTest(t)
 withinCloudCheckoutTx(t, db, func(tx *sql.Tx) {
  plan, err := db.LoadReadyCloudPrintPlanTx(context.Background(), tx, "kiosk-1", refs)
  if err != nil { t.Fatal(err) }
  if len(plan.Tasks)!=2 || plan.TotalPricePaise!=1100 ||
   plan.TariffRevision!=revision || plan.Tasks[0].VariantID!="variant-b" ||
   plan.Tasks[1].VariantID!="variant-a" ||
   plan.Tasks[0].Duplex!="SIMPLEX" ||
   plan.Tasks[1].Duplex!="DUPLEX_LONG_EDGE" ||
   plan.Tasks[0].Copies!=1 {
   t.Fatalf("plan changed source order or settings: %+v", plan)
  }
 })
}

func TestCloudPlanTxRejectsExpiredForeignAndUnreadyInputs(t *testing.T) {
 cases := []struct {
  name string
  change func(*testing.T,*Store,[]CloudPreparedReference) []CloudPreparedReference
  want error
 }{
  {"wrong kiosk",func(t *testing.T,db *Store,r []CloudPreparedReference) []CloudPreparedReference{
   return r
  },ErrVariantUnavailable},
  {"expired",func(t *testing.T,db *Store,r []CloudPreparedReference) []CloudPreparedReference{
   if _,err:=db.DB().Exec("UPDATE document_uploads SET expires_at=? WHERE upload_id='upload-b'",time.Now().UTC().Add(-time.Minute));err!=nil{t.Fatal(err)}
   return r
  },ErrVariantUnavailable},
  {"unready",func(t *testing.T,db *Store,r []CloudPreparedReference) []CloudPreparedReference{
   if _,err:=db.DB().Exec("UPDATE document_variants SET state='PREPARING' WHERE variant_id='variant-b'");err!=nil{t.Fatal(err)}
   return r
  },ErrVariantUnavailable},
  {"stale tariff",func(t *testing.T,db *Store,r []CloudPreparedReference) []CloudPreparedReference{
   if _,err:=db.DB().Exec("UPDATE document_variants SET tariff_revision=999 WHERE variant_id='variant-b'");err!=nil{t.Fatal(err)}
   return r
  },ErrSheetTariffRevisionConflict},
  {"wrong pairing",func(t *testing.T,db *Store,r []CloudPreparedReference) []CloudPreparedReference{
   r[0].UploadID="upload-unknown"
   return r
  },ErrVariantUnavailable},
 }
 for _,tc:=range cases {
  t.Run(tc.name,func(t *testing.T){
   db,refs,_:=setupCloudMultiPlanTxTest(t)
   refs=tc.change(t,db,refs)
   kiosk:="kiosk-1"
   if tc.name=="wrong kiosk" {kiosk="kiosk-other"}
   withinCloudCheckoutTx(t,db,func(tx *sql.Tx){
    _,err:=db.LoadReadyCloudPrintPlanTx(context.Background(),tx,kiosk,refs)
    if !errors.Is(err,tc.want){t.Fatalf("expected %v, got %v",tc.want,err)}
   })
   var count int
   if err:=db.DB().QueryRow("SELECT COUNT(*) FROM orders").Scan(&count);err!=nil||count!=0 {
    t.Fatalf("unexpected persisted order: %d %v",count,err)
   }
  })
 }
}

func TestCloudPlanTxRejectsDuplicateUploadAndBadTransaction(t *testing.T) {
 db,refs,_:=setupCloudMultiPlanTxTest(t)
 withinCloudCheckoutTx(t,db,func(tx *sql.Tx){
  duplicate:=[]CloudPreparedReference{refs[0],refs[0]}
  if _,err:=db.LoadReadyCloudPrintPlanTx(context.Background(),tx,"kiosk-1",duplicate);err==nil{
   t.Fatal("duplicate document accepted")
  }
 })
 if _,err:=db.LoadReadyCloudPrintPlanTx(context.Background(),nil,"kiosk-1",refs);err==nil{
  t.Fatal("transaction-free checkout validation accepted")
 }
}
