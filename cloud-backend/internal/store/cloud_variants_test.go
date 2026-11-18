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
  UNIQUE(upload_id,settings_hash,normalizer_version)
 )`)
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
