package store

import (
 "context"
 "path/filepath"
 "testing"
 "time"
)

func TestCloudVariantReservationAndImmutablePublish(t *testing.T) {
 db,err:=Open(filepath.Join(t.TempDir(),"variants.db"))
 if err!=nil {t.Fatal(err)}
 defer db.Close()
 _,err=db.DB().Exec(`CREATE TABLE document_variants (
  variant_id TEXT PRIMARY KEY, upload_id TEXT NOT NULL,
  settings_hash TEXT NOT NULL, normalizer_version TEXT NOT NULL,
  storage_key TEXT UNIQUE, checksum_sha256 TEXT, billable_sides INTEGER,
  total_sheets INTEGER, canonical_price_paise BIGINT, tariff_revision BIGINT,
  copies INTEGER, duplex TEXT, color_mode TEXT,
  state TEXT NOT NULL, error_code TEXT, expires_at TIMESTAMP NOT NULL,
  lease_expires_at TIMESTAMP,
  UNIQUE(upload_id,settings_hash,normalizer_version)
 )`)
 if err!=nil {t.Fatal(err)}
 _,err=db.DB().Exec(`CREATE TABLE order_documents (
  order_id TEXT, sequence_no INTEGER, variant_id TEXT, accepted_price_paise BIGINT)`)
 if err!=nil {t.Fatal(err)}
 ctx:=context.Background()
 v:=CloudDocumentVariant{ID:"variant-a",UploadID:"upload-a",
  SettingsHash:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  NormalizerVersion:"canonical-v1-tariff-1",ExpiresAt:time.Now().Add(time.Hour).UTC()}
 got,acquired,err:=db.AcquireCloudVariant(ctx,v)
 if err!=nil||!acquired||got.ID!="variant-a" {t.Fatalf("first acquire %+v %v %v",got,acquired,err)}
 v.ID="variant-b"
 got,acquired,err=db.AcquireCloudVariant(ctx,v)
 if err!=nil||acquired||got.ID!="variant-a"||got.State!="PREPARING" {
  t.Fatalf("duplicate reservation %+v %v %v",got,acquired,err)
 }
 got.StorageKey="variants/variant-a/output.pdf"
 got.Checksum="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
 got.BilledSides=2;got.TotalSheets=1;got.PricePaise=500;got.TariffRevision=1
 got.Copies=1;got.Duplex="DUPLEX_LONG_EDGE";got.ColorMode="BW"
 ok,err:=db.PublishCloudVariant(ctx,got)
 if err!=nil||!ok {t.Fatalf("first publish %v %v",ok,err)}
 ok,err=db.PublishCloudVariant(ctx,got)
 if err!=nil||ok {t.Fatalf("duplicate publish %v %v",ok,err)}
 final,err:=db.GetCloudVariant(ctx,got.UploadID,got.SettingsHash,got.NormalizerVersion)
 if err!=nil||final.State!="READY"||final.PricePaise!=500||final.Checksum!=got.Checksum {
  t.Fatalf("unexpected persisted variant %+v %v",final,err)
 }
 scoped,err:=db.GetCloudVariantByID(ctx,got.UploadID,got.ID)
 if err!=nil||scoped.ID!=got.ID||scoped.State!="READY" {t.Fatalf("scoped read %+v %v",scoped,err)}
 if _,err:=db.GetCloudVariantByID(ctx,"another-upload",got.ID);err==nil {
  t.Fatal("cross-upload variant access accepted")
 }
 if err:=db.FailCloudVariant(ctx,got.ID);err!=nil {t.Fatal(err)}
 final,err=db.GetCloudVariant(ctx,got.UploadID,got.SettingsHash,got.NormalizerVersion)
 if err!=nil||final.State!="READY" {t.Fatalf("READY mutated after fail %+v %v",final,err)}
}

func TestCloudVariantReservationRejectsIncompleteIdentity(t *testing.T) {
 db,err:=Open(filepath.Join(t.TempDir(),"invalid.db"))
 if err!=nil {t.Fatal(err)}
 defer db.Close()
 _,_,err=db.AcquireCloudVariant(context.Background(),CloudDocumentVariant{ID:"x",UploadID:"y"})
 if err==nil {t.Fatal("incomplete identity accepted")}
}


func TestCloudVariantReclaimsAbandonedLeaseWithoutOverwritingReady(t *testing.T) {
 db,err:=Open(filepath.Join(t.TempDir(),"lease.sqlite"))
 if err!=nil {t.Fatal(err)}
 defer db.Close()
 for _,query:=range []string{
  `CREATE TABLE document_variants (
   variant_id TEXT PRIMARY KEY,upload_id TEXT,settings_hash TEXT,normalizer_version TEXT,
   state TEXT,error_code TEXT,expires_at TIMESTAMP,lease_expires_at TIMESTAMP,
   storage_key TEXT,checksum_sha256 TEXT,billable_sides INTEGER,total_sheets INTEGER,
   canonical_price_paise BIGINT,tariff_revision BIGINT,copies INTEGER,duplex TEXT,color_mode TEXT,
   UNIQUE(upload_id,settings_hash,normalizer_version))`,
  `CREATE TABLE order_documents (order_id TEXT,sequence_no INTEGER,variant_id TEXT,accepted_price_paise BIGINT)`,
 }{
  if _,err:=db.DB().Exec(query);err!=nil{t.Fatal(err)}
 }
 ctx:=context.Background()
 ident:=CloudDocumentVariant{ID:"v1",UploadID:"u1",
  SettingsHash:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  NormalizerVersion:"normalizer-v1",ExpiresAt:time.Now().Add(time.Hour)}
 _,acquired,err:=db.AcquireCloudVariant(ctx,ident)
 if err!=nil||!acquired {t.Fatalf("first reservation %v %v",acquired,err)}
 next:=ident;next.ID="v2"
 got,acquired,err:=db.AcquireCloudVariant(ctx,next)
 if err!=nil||acquired||got.ID!="v1" {t.Fatalf("live lease stolen %+v %v %v",got,acquired,err)}
 if !got.LeaseExpiresAt.After(time.Now()) || got.LeaseExpiresAt.After(time.Now().Add(11*time.Minute)) {
  t.Fatalf("invalid live lease deadline %s",got.LeaseExpiresAt)
 }
 if _,err:=db.DB().Exec("UPDATE document_variants SET lease_expires_at=? WHERE variant_id='v1'",
  time.Now().Add(-time.Minute));err!=nil{t.Fatal(err)}
 got,acquired,err=db.AcquireCloudVariant(ctx,next)
 if err!=nil||!acquired||got.ID!="v2" {t.Fatalf("expired lease not recovered %+v %v %v",got,acquired,err)}
 // A former worker must be unable to publish a stale result after takeover.
 obsolete:=CloudDocumentVariant{ID:"v1",StorageKey:"variants/v1/output.pdf",
  Checksum:"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  BilledSides:1,TotalSheets:1,PricePaise:300,TariffRevision:1,
  Copies:1,Duplex:"SIMPLEX",ColorMode:"BW"}
 ok,err:=db.PublishCloudVariant(ctx,obsolete)
 if err!=nil||ok {t.Fatalf("stale result published %v %v",ok,err)}
 obsolete.ID="v2";obsolete.StorageKey="variants/v2/output.pdf"
 ok,err=db.PublishCloudVariant(ctx,obsolete)
 if err!=nil||!ok {t.Fatalf("new worker could not publish %v %v",ok,err)}
 third:=ident;third.ID="v3"
 got,acquired,err=db.AcquireCloudVariant(ctx,third)
 if err!=nil||acquired||got.ID!="v2"||got.State!="READY" {
  t.Fatalf("published artifact was replaced %+v %v %v",got,acquired,err)
 }
 // A known failed attempt can be retried without waiting for lease expiry.
 failed:=ident
 failed.ID="v4"
 failed.SettingsHash="cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
 got,acquired,err=db.AcquireCloudVariant(ctx,failed)
 if err!=nil||!acquired{t.Fatalf("failed-case reservation %v %v",acquired,err)}
 if err:=db.FailCloudVariant(ctx,got.ID);err!=nil{t.Fatal(err)}
 retry:=failed
 retry.ID="v5"
 got,acquired,err=db.AcquireCloudVariant(ctx,retry)
 if err!=nil||!acquired||got.ID!="v5"{
  t.Fatalf("FAILED attempt not recoverable %+v %v %v",got,acquired,err)
 }
}
