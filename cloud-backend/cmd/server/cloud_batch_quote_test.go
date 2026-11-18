package main

import (
 "net/http"
 "net/http/httptest"
 "strings"
 "testing"
)

func TestCloudBatchQuoteOptInAndNoCheckout(t *testing.T){
 t.Setenv("XEROXGO_CLOUD_UPLOADS_ENABLED","1")
 t.Setenv("XEROXGO_CLOUD_BATCH_QUOTE_ENABLED","")
 s:=&appServer{}
 w:=httptest.NewRecorder()
 req:=httptest.NewRequest(http.MethodPost,"/api/documents/batch/quote",strings.NewReader("{}"))
 s.handleCloudBatchQuote(w,req)
 if w.Code!=http.StatusNotFound{t.Fatalf("unexpected batch endpoint status %d",w.Code)}
 t.Setenv("XEROXGO_CLOUD_BATCH_QUOTE_ENABLED","1")
 if !cloudBatchQuoteEnabled(){t.Fatal("read-only batch planning gate not enabled")}
 t.Setenv("XEROXGO_CLOUD_UPLOADS_ENABLED","")
 if cloudBatchQuoteEnabled(){t.Fatal("batch quoting exposed without upload verification")}
}

func TestCloudBatchQuoteValidatesBoundedDistinctReferences(t *testing.T){
 good:=cloudBatchQuoteInput{KioskID:"demo",
  Documents:[]cloudBatchDocumentInput{{
   UploadID:strings.Repeat("a",32),UploadToken:strings.Repeat("b",64),
   VariantID:strings.Repeat("c",32),
  },{
   UploadID:strings.Repeat("d",32),UploadToken:strings.Repeat("e",64),
   VariantID:strings.Repeat("f",32),
  }},
 }
 if !validCloudBatchQuoteRequest(good){t.Fatal("valid mixed-document quote rejected")}
 cases:=[]cloudBatchQuoteInput{
  {KioskID:"demo"},
  {KioskID:"../other",Documents:good.Documents},
  {KioskID:"demo",Documents:[]cloudBatchDocumentInput{good.Documents[0],good.Documents[0]}},
  {KioskID:"demo",Documents:[]cloudBatchDocumentInput{{
   UploadID:strings.Repeat("a",32),UploadToken:"bad",VariantID:strings.Repeat("c",32),
  }}},
 }
 for _,candidate:=range cases{
  if validCloudBatchQuoteRequest(candidate){t.Fatalf("invalid request accepted: %+v",candidate)}
 }
 t.Setenv("XEROXGO_CLOUD_UPLOADS_ENABLED","1")
 t.Setenv("XEROXGO_CLOUD_BATCH_QUOTE_ENABLED","1")
 s:=&appServer{}
 w:=httptest.NewRecorder()
 req:=httptest.NewRequest(http.MethodPost,"/api/documents/batch/quote",
  strings.NewReader(`{"documents":[]}`))
 s.handleCloudBatchQuote(w,req)
 if w.Code!=http.StatusServiceUnavailable{t.Fatalf("missing backend was not fail closed: %d",w.Code)}
}
