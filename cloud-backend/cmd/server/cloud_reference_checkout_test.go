package main

import (
 "net/http"
 "net/http/httptest"
 "strings"
 "testing"
)

func TestReferenceCheckoutIsOffByDefault(t *testing.T) {
 t.Setenv("XEROXGO_CLOUD_UPLOADS_ENABLED","1")
 t.Setenv("XEROXGO_CLOUD_CHECKOUT_ENABLED","")
 s:=&appServer{}
 req:=httptest.NewRequest(http.MethodPost,"/api/documents/checkout",strings.NewReader("{}"))
 w:=httptest.NewRecorder()
 s.handleCloudReferenceCheckout(w,req)
 if w.Code!=http.StatusNotFound {t.Fatalf("expected disabled endpoint, got %d",w.Code)}
}

func TestReferenceCheckoutRejectsBadInput(t *testing.T) {
 input:=cloudCheckoutRequest{
  KioskID:"demo",UploadID:strings.Repeat("a",32),
  UploadToken:strings.Repeat("a",64),
  VariantID:strings.Repeat("b",32),RequestID:strings.Repeat("c",32),
  AcceptedPricePaise:500,AcceptedTariffRevision:1,
 }
 if !validCloudCheckoutRequest(input) {t.Fatal("valid reference checkout rejected")}
 cases:=[]struct{name string;mutate func(*cloudCheckoutRequest)}{
  {"negative price",func(x *cloudCheckoutRequest){x.AcceptedPricePaise=-1}},
  {"revision missing",func(x *cloudCheckoutRequest){x.AcceptedTariffRevision=0}},
  {"missing capability",func(x *cloudCheckoutRequest){x.UploadToken=""}},
  {"invalid kiosk",func(x *cloudCheckoutRequest){x.KioskID="../another"}},
  {"missing idem",func(x *cloudCheckoutRequest){x.RequestID=""}},
  {"missing variant",func(x *cloudCheckoutRequest){x.VariantID=""}},
 }
 for _,tc:=range cases{
  t.Run(tc.name,func(t *testing.T){
   candidate:=input;tc.mutate(&candidate)
   if validCloudCheckoutRequest(candidate){t.Fatal("invalid request accepted")}
  })
 }
}

func TestCloudArtifactRejectsUntrustedPath(t *testing.T) {
 s:=&appServer{}
 w:=httptest.NewRecorder()
 req:=httptest.NewRequest(http.MethodGet,"/api/orders/ord-test/pdf",nil)
 s.serveCloudOrderArtifact(w,req,"../../secrets",strings.Repeat("a",64))
 if w.Code!=http.StatusInternalServerError {t.Fatalf("unsafe path status %d",w.Code)}
}
