package main

import (
 "net/http"
 "net/http/httptest"
 "strings"
 "testing"
)

func TestCloudAsyncPreparationDisabledByDefault(t *testing.T) {
 t.Setenv("XEROXGO_CLOUD_UPLOADS_ENABLED","1")
 t.Setenv("XEROXGO_CLOUD_ASYNC_PREP_ENABLED","")
 if cloudAsyncPreparationEnabled(){t.Fatal("cloud background worker unexpectedly enabled")}
 s:=&appServer{}
 w:=httptest.NewRecorder()
 req:=httptest.NewRequest(http.MethodPost,"/api/documents/preparations/status",
  strings.NewReader("{}"))
 s.handleCloudPreparationStatus(w,req)
 if w.Code!=http.StatusNotFound{t.Fatalf("status not gated: %d",w.Code)}
 t.Setenv("XEROXGO_CLOUD_ASYNC_PREP_ENABLED","1")
 if !cloudAsyncPreparationEnabled(){t.Fatal("worker flag not respected")}
 t.Setenv("XEROXGO_CLOUD_UPLOADS_ENABLED","")
 if cloudAsyncPreparationEnabled(){t.Fatal("worker enabled without upload authorization")}
}

func TestCloudAsyncPreparationStatusRejectsMalformedReceipt(t *testing.T) {
 t.Setenv("XEROXGO_CLOUD_UPLOADS_ENABLED","1")
 t.Setenv("XEROXGO_CLOUD_ASYNC_PREP_ENABLED","1")
 s:=&appServer{}
 w:=httptest.NewRecorder()
 req:=httptest.NewRequest(http.MethodPost,"/api/documents/preparations/status",
  strings.NewReader(`{"upload_id":"short","upload_token":"missing"}`))
 s.handleCloudPreparationStatus(w,req)
 if w.Code!=http.StatusBadRequest{t.Fatalf("malformed queue receipt accepted: %d",w.Code)}
}
