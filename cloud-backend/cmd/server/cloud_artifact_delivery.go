package main

import (
 "crypto/sha256"
 "encoding/hex"
 "io"
 "net/http"
 "os"
 "strconv"
 "strings"
)

// Existing signed Render PDF endpoint remains the only URL given to the Pi.
// Render checks the ordered immutable checksum before returning any PDF bytes.
// Never redirect Pi to a long-lived, public or browser-accessible Storage URL.
func (s *appServer) serveCloudOrderArtifact(w http.ResponseWriter,r *http.Request,key,wantChecksum string) {
 if len(wantChecksum)!=64 || !strings.HasPrefix(key,"variants/") ||
  !strings.HasSuffix(key,"/output.pdf") || strings.Contains(key,"..") {
  http.Error(w,"invalid prepared document reference",http.StatusInternalServerError);return
 }
 base,secret,err:=cloudStorageConfig()
 if err!=nil {http.Error(w,"cloud storage unavailable",http.StatusServiceUnavailable);return}
 endpoint:=base+"/storage/v1/object/authenticated/"+cloudPreparedBucket+"/"+key
 resp,err:=supabaseStorageRequest(r.Context(),http.MethodGet,endpoint,secret,nil)
 if err!=nil {http.Error(w,"prepared document temporarily unavailable",http.StatusServiceUnavailable);return}
 defer resp.Body.Close()
 if resp.StatusCode!=http.StatusOK {
  http.Error(w,"prepared document temporarily unavailable",http.StatusServiceUnavailable);return
 }
 f,err:=os.CreateTemp(s.cfg.jobsDir,"cloud_print_*.pdf")
 if err!=nil {http.Error(w,"print download workspace unavailable",500);return}
 defer func(){_ = f.Close();_ = os.Remove(f.Name())}()
 h:=sha256.New()
 n,err:=io.Copy(io.MultiWriter(f,h),io.LimitReader(resp.Body,maxPreparedPDFBytes+1))
 if err!=nil || n<5 || n>maxPreparedPDFBytes ||
  hex.EncodeToString(h.Sum(nil))!=strings.ToLower(wantChecksum) {
  http.Error(w,"prepared document failed integrity verification",http.StatusServiceUnavailable);return
 }
 if _,err=f.Seek(0,io.SeekStart);err!=nil {
  http.Error(w,"prepared document unavailable",500);return
 }
 sig:=make([]byte,5)
 if _,err=io.ReadFull(f,sig);err!=nil || string(sig)!="%PDF-" {
  http.Error(w,"prepared document is not a PDF",500);return
 }
 if _,err=f.Seek(0,io.SeekStart);err!=nil {
  http.Error(w,"prepared document unavailable",500);return
 }
 w.Header().Set("Content-Type","application/pdf")
 w.Header().Set("Content-Length",strconv.FormatInt(n,10))
 w.Header().Set("Cache-Control","private, no-store")
 _,_=io.Copy(w,f)
}
