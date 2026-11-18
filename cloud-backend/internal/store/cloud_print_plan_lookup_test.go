package store

import (
 "context"
 "errors"
 "path/filepath"
 "strings"
 "testing"
 "time"
)

func TestCloudPrintPlanLookupProtectsReadyOwnershipAndExpiration(t *testing.T){
 db,err:=Open(filepath.Join(t.TempDir(),"plan-lookup.db"))
 if err!=nil{t.Fatal(err)}
 defer db.Close()
 statements:=[]string{
  `CREATE TABLE document_uploads (
    upload_id TEXT PRIMARY KEY,kiosk_id TEXT,state TEXT,expires_at TIMESTAMP)`,
  `CREATE TABLE document_variants (
    variant_id TEXT PRIMARY KEY,upload_id TEXT,state TEXT,expires_at TIMESTAMP,
    storage_key TEXT,checksum_sha256 TEXT,copies INTEGER,duplex TEXT,
    color_mode TEXT,billable_sides INTEGER,total_sheets INTEGER,
    canonical_price_paise BIGINT,tariff_revision BIGINT)`,
 }
 for _,q:=range statements{if _,err:=db.DB().Exec(q);err!=nil{t.Fatal(err)}}
 future:=time.Now().UTC().Add(time.Hour)
 past:=time.Now().UTC().Add(-time.Hour)
 uploads:=[]struct{id,kiosk,state string;expiry time.Time}{
  {"own-1","kiosk-1","UPLOADED",future},
  {"own-2","kiosk-1","UPLOADED",future},
  {"other","kiosk-2","UPLOADED",future},
  {"expired","kiosk-1","UPLOADED",past},
 }
 for _,u:=range uploads{
  if _,err:=db.DB().Exec(`INSERT INTO document_uploads VALUES(?,?,?,?)`,
   u.id,u.kiosk,u.state,u.expiry);err!=nil{t.Fatal(err)}
 }
 variants:=[]struct{id,upload,state,duplex,color string;price int}{
  {"a","own-1","READY","SIMPLEX","BW",300},
  {"b","own-2","READY","DUPLEX_SHORT_EDGE","COLOR",400},
  {"c","other","READY","SIMPLEX","BW",500},
  {"d","expired","READY","SIMPLEX","BW",500},
  {"e","own-2","PREPARING","SIMPLEX","BW",500},
 }
 for _,v:=range variants{
  _,err:=db.DB().Exec(`INSERT INTO document_variants
    (variant_id,upload_id,state,expires_at,storage_key,checksum_sha256,
     copies,duplex,color_mode,billable_sides,total_sheets,canonical_price_paise,tariff_revision)
    VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
   v.id,v.upload,v.state,future,"variants/"+v.id+"/output.pdf",
   strings.Repeat("a",64),2,v.duplex,v.color,2,2,v.price,4)
  if err!=nil{t.Fatal(err)}
 }
 ctx:=context.Background()
 good:=[]CloudPreparedReference{{UploadID:"own-1",VariantID:"a"},
  {UploadID:"own-2",VariantID:"b"}}
 plan,err:=db.LoadReadyCloudPrintPlan(ctx,"kiosk-1",good)
 if err!=nil{t.Fatal(err)}
 if len(plan.Tasks)!=2||plan.TotalPricePaise!=700||
  plan.Tasks[0].Duplex!="SIMPLEX"||plan.Tasks[1].Duplex!="DUPLEX_SHORT_EDGE"||
  plan.Tasks[1].ColorMode!="COLOR"{
  t.Fatalf("canonical plan lost physical settings %+v",plan)
 }
 for _,ref:=range []CloudPreparedReference{
  {UploadID:"other",VariantID:"c"},
  {UploadID:"expired",VariantID:"d"},
  {UploadID:"own-2",VariantID:"e"},
  {UploadID:"own-1",VariantID:"b"},
 } {
  if _,err:=db.LoadReadyCloudPrintPlan(ctx,"kiosk-1",[]CloudPreparedReference{ref});
   !errors.Is(err,ErrVariantUnavailable) {
   t.Fatalf("unauthorized/unready variant %+v returned %v",ref,err)
  }
 }
 if _,err:=db.LoadReadyCloudPrintPlan(ctx,"kiosk-1",
  []CloudPreparedReference{good[0],good[0]});err==nil{
  t.Fatal("duplicate document checkout accepted")
 }
}
