package main

import (
 "context"
 "crypto/sha256"
 "encoding/hex"
 "io"
 "os"
 "path/filepath"
 "net/http"
 "net/http/httptest"
 "strings"
 "testing"

 "xeroxgo/cloud-backend/internal/store"
 "xeroxgo/pkg/contracts"
)

func TestCloudVariantOptionsStrictValidation(t *testing.T) {
 good:=docItemMeta{Copies:1,Duplex:"DUPLEX_LONG_EDGE",ColorMode:"BW",
  Orientation:"AUTO",PagesPerSheet:2}
 opts,err:=strictVariantOptions(good,false)
 if err!=nil {t.Fatal(err)}
 if opts.Duplex!=contracts.DuplexLongEdge || opts.PagesPerSheet!=2 {
  t.Fatalf("wrong normalized options: %+v",opts)
 }
 bad:=[]docItemMeta{
  {Copies:0,Duplex:"SIMPLEX",ColorMode:"BW",Orientation:"AUTO",PagesPerSheet:1},
  {Copies:1000,Duplex:"SIMPLEX",ColorMode:"BW",Orientation:"AUTO",PagesPerSheet:1},
  {Copies:1,Duplex:"SIDEWAYS",ColorMode:"BW",Orientation:"AUTO",PagesPerSheet:1},
  {Copies:1,Duplex:"SIMPLEX",ColorMode:"COLOR",Orientation:"AUTO",PagesPerSheet:1},
  {Copies:1,Duplex:"SIMPLEX",ColorMode:"BW",Orientation:"AUTO",PagesPerSheet:3},
  {Copies:1,Duplex:"SIMPLEX",ColorMode:"BW",Orientation:"UP",PagesPerSheet:1},
  {Copies:1,Duplex:"SIMPLEX",ColorMode:"BW",Orientation:"AUTO",PagesPerSheet:1,Nup:4},
 }
 for i,meta:=range bad {
  if _,err:=strictVariantOptions(meta,false);err==nil {t.Errorf("invalid options accepted case %d",i)}
 }
}

func TestVariantIdentityStableAndSensitiveToPrintSettings(t *testing.T) {
 base:=docPrintOptions{Copies:1,Duplex:contracts.DuplexSimplex,ColorMode:"BW",
  PageRange:"",Orientation:"AUTO",PagesPerSheet:1}
 a:=cloudVariantSettingsHash(base)
 if a!=cloudVariantSettingsHash(base)||len(a)!=64 {t.Fatalf("non-deterministic hash %q",a)}
 variants:=[]docPrintOptions{
  {Copies:2,Duplex:base.Duplex,ColorMode:base.ColorMode,Orientation:base.Orientation,PagesPerSheet:1},
  {Copies:1,Duplex:contracts.DuplexLongEdge,ColorMode:base.ColorMode,Orientation:base.Orientation,PagesPerSheet:1},
  {Copies:1,Duplex:base.Duplex,ColorMode:base.ColorMode,Orientation:"LANDSCAPE",PagesPerSheet:1},
  {Copies:1,Duplex:base.Duplex,ColorMode:base.ColorMode,Orientation:base.Orientation,PagesPerSheet:2},
  {Copies:1,Duplex:base.Duplex,ColorMode:base.ColorMode,Orientation:base.Orientation,PagesPerSheet:1,PageRange:"1-2"},
 }
 for i,v:=range variants {if a==cloudVariantSettingsHash(v){t.Errorf("distinct options collide: %d",i)}}
}

func TestCloudVariantPrepareOffByDefault(t *testing.T) {
 t.Setenv("XEROXGO_CLOUD_UPLOADS_ENABLED","")
 s:=&appServer{}
 w:=httptest.NewRecorder()
 req:=httptest.NewRequest(http.MethodPost,"/api/documents/variants/prepare",strings.NewReader("{}"))
 s.handleCloudVariantPrepare(w,req)
 if w.Code!=http.StatusNotFound {t.Fatalf("unexpected status %d",w.Code)}
}

func TestCloudPreparationUsesVerifiedSourceAndPrivateDestination(t *testing.T) {
 bytes:=[]byte("%PDF-1.7\nsmall test document")
 digest:=sha256.Sum256(bytes)
 var uploadCount int
 remote:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
  if r.Header.Get("Authorization")!="Bearer secret" || r.Header.Get("apikey")!="secret" {
   w.WriteHeader(http.StatusForbidden);return
  }
  if r.Method=="GET" {
   if r.URL.Path!="/storage/v1/object/authenticated/xeroxgo-originals/uploads/a/original.pdf" {
    w.WriteHeader(http.StatusNotFound);return
   }
   _,_=w.Write(bytes);return
  }
  if r.Method=="POST" {
   if r.URL.Path!="/storage/v1/object/xeroxgo-prepared/variants/v/output.pdf" ||
    r.Header.Get("Content-Type")!="application/pdf" || r.Header.Get("x-upsert")!="false" {
    w.WriteHeader(http.StatusBadRequest);return
   }
   b,_:=io.ReadAll(r.Body)
   if string(b)!=string(bytes) {w.WriteHeader(http.StatusBadRequest);return}
   uploadCount++
   w.WriteHeader(http.StatusOK);return
  }
  w.WriteHeader(http.StatusMethodNotAllowed)
 }))
 defer remote.Close()
 dir:=t.TempDir()
 source:=store.CloudDocumentUpload{ObjectKey:"uploads/a/original.pdf",
  DeclaredBytes:int64(len(bytes)),VerifiedSHA256:hex.EncodeToString(digest[:])}
 location,err:=readVerifiedOriginalToTemp(context.Background(),remote.URL,"secret",source,dir)
 if err!=nil {t.Fatal(err)}
 defer os.Remove(location)
 checksum,err:=uploadPreparedVariant(context.Background(),remote.URL,"secret","variants/v/output.pdf",location)
 if err!=nil||checksum!=source.VerifiedSHA256||uploadCount!=1 {
  t.Fatalf("prepared upload hash=%s uploads=%d err=%v",checksum,uploadCount,err)
 }
 source.VerifiedSHA256=strings.Repeat("0",64)
 if unexpected,err:=readVerifiedOriginalToTemp(context.Background(),remote.URL,"secret",source,dir);err==nil {
  _=os.Remove(unexpected);t.Fatal("changed source accepted")
 }
 if err:=os.WriteFile(filepath.Join(dir,"not_pdf.pdf"),[]byte("bad"),0600);err!=nil {t.Fatal(err)}
}

func TestCloudVariantStatusOffByDefault(t *testing.T) {
 t.Setenv("XEROXGO_CLOUD_UPLOADS_ENABLED","")
 s:=&appServer{}
 w:=httptest.NewRecorder()
 req:=httptest.NewRequest(http.MethodPost,"/api/documents/variants/status",strings.NewReader("{}"))
 s.handleCloudVariantStatus(w,req)
 if w.Code!=http.StatusNotFound {t.Fatalf("unexpected status %d",w.Code)}
}
