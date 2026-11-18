package main

import (
 "net/http"
 "net/http/httptest"
 "strings"
 "testing"

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
