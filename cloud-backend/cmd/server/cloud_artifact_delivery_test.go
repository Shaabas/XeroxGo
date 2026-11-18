package main

import (
 "context"
 "crypto/sha256"
 "encoding/hex"
 "net/http"
 "net/http/httptest"
 "os"
 "strings"
 "testing"
)

func TestPrivatePreparedPrintDownloadVerifiedBeforeRelease(t *testing.T){
 data:=[]byte("%PDF-1.7\ncloud artifact test payload")
 sum:=sha256.Sum256(data)
 goodHash:=hex.EncodeToString(sum[:])
 key:="variants/"+strings.Repeat("a",32)+"/output.pdf"
 remote:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  if r.Method!="GET"||r.Header.Get("Authorization")!="Bearer test-secret"||
   r.URL.Path!="/storage/v1/object/authenticated/xeroxgo-prepared/"+key {
   w.WriteHeader(403);return
  }
  _,_=w.Write(data)
 }))
 defer remote.Close()
 p,n,err:=fetchVerifiedCloudArtifact(context.Background(),remote.URL,"test-secret",key,goodHash,t.TempDir())
 if err!=nil {t.Fatal(err)}
 defer os.Remove(p)
 got,err:=os.ReadFile(p)
 if err!=nil||string(got)!=string(data)||n!=int64(len(data)){
  t.Fatalf("download bytes=%d err=%v",n,err)
 }
 p,_,err=fetchVerifiedCloudArtifact(context.Background(),remote.URL,"test-secret",key,strings.Repeat("0",64),t.TempDir())
 if err==nil {_=os.Remove(p);t.Fatal("mismatched checksum accepted")}
}

func TestPrivatePreparedPrintDownloadRejectsUnsafeReference(t *testing.T){
 for _,key:=range []string{
  "../../etc/secrets","variants/../output.pdf",
  "variants/short/output.pdf","uploads/"+strings.Repeat("b",32)+"/original.pdf",
 } {
  if safePreparedObjectKey(key){t.Errorf("accepted unsafe path %q",key)}
 }
 valid:="variants/"+strings.Repeat("b",32)+"/output.pdf"
 if !safePreparedObjectKey(valid){t.Fatal("valid variant path rejected")}
}
