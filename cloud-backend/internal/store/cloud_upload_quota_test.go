package store

import (
 "context"
 "errors"
 "fmt"
 "path/filepath"
 "testing"
 "time"
)

func quotaTestStore(t *testing.T) *Store {
 t.Helper()
 db,err:=Open(filepath.Join(t.TempDir(),"cloud-quota.sqlite"))
 if err!=nil {t.Fatal(err)}
 t.Cleanup(func(){_ =db.Close()})
 _,err=db.DB().Exec(`CREATE TABLE document_uploads (
  upload_id TEXT PRIMARY KEY,upload_token_hash TEXT NOT NULL,kiosk_id TEXT NOT NULL,
  original_name TEXT,object_key TEXT UNIQUE,content_type TEXT,
  declared_bytes BIGINT,state TEXT,expires_at TIMESTAMP,created_at TIMESTAMP,
  verified_sha256 TEXT,verified_bytes BIGINT,verified_at TIMESTAMP
 )`)
 if err!=nil {t.Fatal(err)}
 return db
}

func TestCloudUploadDurableCountQuota(t *testing.T){
 db:=quotaTestStore(t)
 ctx:=context.Background()
 for i:=0;i<cloudMaxUploadsPerKioskHour;i++{
  u:=CloudDocumentUpload{ID:fmt.Sprintf("upload%03d",i),TokenHash:"token-hash",
   KioskID:"kiosk-1",OriginalName:"doc.pdf",ObjectKey:fmt.Sprintf("uploads/%d/original.pdf",i),
   ContentType:"application/pdf",DeclaredBytes:1024,ExpiresAt:time.Now().Add(time.Hour)}
  if err:=db.ReserveCloudDocumentUpload(ctx,u);err!=nil{t.Fatalf("upload %d rejected: %v",i,err)}
 }
 extra:=CloudDocumentUpload{ID:"extra",TokenHash:"token-hash",KioskID:"kiosk-1",
  OriginalName:"doc.pdf",ObjectKey:"uploads/extra/original.pdf",
  ContentType:"application/pdf",DeclaredBytes:1024,ExpiresAt:time.Now().Add(time.Hour)}
 if err:=db.ReserveCloudDocumentUpload(ctx,extra);!errors.Is(err,ErrCloudUploadQuota){
  t.Fatalf("count cap not enforced: %v",err)
 }
 extra.KioskID="kiosk-2"
 if err:=db.ReserveCloudDocumentUpload(ctx,extra);err!=nil{
  t.Fatalf("quota cross-kiosk leakage: %v",err)
 }
}

func TestCloudUploadDurableByteQuotaAndExpiration(t *testing.T){
 db:=quotaTestStore(t)
 ctx:=context.Background()
 for i:=0;i<4;i++{
  u:=CloudDocumentUpload{ID:fmt.Sprintf("big%d",i),TokenHash:"token-hash",
   KioskID:"kiosk-1",OriginalName:"big.pdf",ObjectKey:fmt.Sprintf("uploads/big%d/original.pdf",i),
   ContentType:"application/pdf",DeclaredBytes:25*1024*1024,
   ExpiresAt:time.Now().Add(time.Hour)}
  if err:=db.ReserveCloudDocumentUpload(ctx,u);err!=nil{t.Fatal(err)}
 }
 extra:=CloudDocumentUpload{ID:"too-large-hour",TokenHash:"token-hash",KioskID:"kiosk-1",
  OriginalName:"big.pdf",ObjectKey:"uploads/overflow/original.pdf",
  ContentType:"application/pdf",DeclaredBytes:1024,ExpiresAt:time.Now().Add(time.Hour)}
 if err:=db.ReserveCloudDocumentUpload(ctx,extra);!errors.Is(err,ErrCloudUploadQuota){
  t.Fatalf("byte quota not enforced: %v",err)
 }
 // The rolling window excludes allocations created over an hour ago.
 if _,err:=db.DB().Exec(`UPDATE document_uploads SET created_at=? WHERE kiosk_id='kiosk-1'`,
  time.Now().UTC().Add(-2*time.Hour));err!=nil{t.Fatal(err)}
 if err:=db.ReserveCloudDocumentUpload(ctx,extra);err!=nil{
  t.Fatalf("old allocations blocked current hour: %v",err)
 }
}
