package main

import (
 "context"
 "net/http"
 "net/http/httptest"
 "strings"
 "testing"

 "xeroxgo/cloud-backend/internal/store"
)

func TestCloudCleanupStrictObjectPaths(t *testing.T){
 good:=store.CloudCleanupObject{
  Kind:"original",ID:strings.Repeat("a",32),
  Key:"uploads/"+strings.Repeat("a",32)+"/original.pdf"}
 bucket,err:=safeCloudCleanupLocation(good)
 if err!=nil||bucket!=cloudOriginalBucket{t.Fatalf("good location: %q %v",bucket,err)}
 bad:=[]store.CloudCleanupObject{
  {Kind:"original",ID:strings.Repeat("a",32),Key:"uploads/../../secret"},
  {Kind:"original",ID:"not-hex",Key:"uploads/not-hex/original.pdf"},
  {Kind:"prepared",ID:"x",Key:"../../secret"},
  {Kind:"prepared",ID:strings.Repeat("a",32),Key:"variants/"+strings.Repeat("b",32)+"/output.pdf"},
  {Kind:"other",ID:"x",Key:"uploads/x/original.pdf"},
 }
 for _,item:=range bad{
  if _,err:=safeCloudCleanupLocation(item);err==nil{t.Fatalf("unsafe key accepted: %+v",item)}
 }
}

func TestCloudCleanupUsesPrivateAuthenticatedDelete(t *testing.T){
 key:="variants/"+strings.Repeat("b",32)+"/output.pdf"
 item:=store.CloudCleanupObject{Kind:"prepared",ID:strings.Repeat("b",32),Key:key}
 calls:=0
 remote:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  calls++
  if r.Method!=http.MethodDelete||r.Header.Get("Authorization")!="Bearer secret"||
   r.Header.Get("apikey")!="secret"||
   r.URL.Path!="/storage/v1/object/xeroxgo-prepared/"+key{
   w.WriteHeader(http.StatusForbidden);return
  }
  w.WriteHeader(http.StatusNoContent)
 }))
 defer remote.Close()
 if err:=deletePrivateCloudObject(context.Background(),remote.URL,"secret",item);err!=nil{
  t.Fatal(err)
 }
 if calls!=1{t.Fatalf("wrong request count %d",calls)}
}

func TestCloudCleanupOffByDefault(t *testing.T){
 t.Setenv("XEROXGO_CLOUD_UPLOADS_ENABLED","1")
 t.Setenv("XEROXGO_CLOUD_CLEANUP_ENABLED","")
 if cloudCleanupEnabled(){t.Fatal("storage deletion on by default")}
}
