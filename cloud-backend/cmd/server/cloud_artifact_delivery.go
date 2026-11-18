package main

import (
 "context"
 "crypto/sha256"
 "encoding/hex"
 "errors"
 "io"
 "net/http"
 "os"
 "path/filepath"
 "strconv"
 "strings"
)

func safePreparedObjectKey(key string) bool {
 parts:=strings.Split(key,"/")
 if len(parts)!=3 || parts[0]!="variants" || parts[2]!="output.pdf" ||
  len(parts[1])!=32 {return false}
 _,err:=hex.DecodeString(parts[1])
 return err==nil
}

// Fetch and fully verify BEFORE exposing any bytes to the Pi. Never redirect
// to a private Storage URL or trust the claimed content type/length alone.
func fetchVerifiedCloudArtifact(ctx context.Context,base,secret,key,want,dir string) (string,int64,error) {
 if !safePreparedObjectKey(key) || len(want)!=64 {
  return "",0,errors.New("invalid cloud document identity")
 }
 endpoint:=base+"/storage/v1/object/authenticated/"+cloudPreparedBucket+"/"+key
 resp,err:=supabaseStorageRequest(ctx,http.MethodGet,endpoint,secret,nil)
 if err!=nil{return "",0,err}
 defer resp.Body.Close()
 if resp.StatusCode!=http.StatusOK{return "",0,errors.New("prepared document unavailable")}
 f,err:=os.CreateTemp(dir,"cloud_print_*.pdf")
 if err!=nil{return "",0,err}
 location:=f.Name()
 cleanup:=true
 defer func(){_ =f.Close();if cleanup {_=os.Remove(location)}}()
 h:=sha256.New()
 n,err:=io.Copy(io.MultiWriter(f,h),io.LimitReader(resp.Body,maxPreparedPDFBytes+1))
 if err!=nil||n<5||n>maxPreparedPDFBytes||
  hex.EncodeToString(h.Sum(nil))!=strings.ToLower(want) {
  return "",0,errors.New("cloud prepared file failed integrity check")
 }
 if _,err=f.Seek(0,io.SeekStart);err!=nil{return "",0,err}
 header:=make([]byte,5)
 if _,err=io.ReadFull(f,header);err!=nil||string(header)!="%PDF-"{
  return "",0,errors.New("cloud prepared object is not PDF")
 }
 if err=f.Close();err!=nil{return "",0,err}
 cleanup=false
 return location,n,nil
}

// Existing signed Render PDF endpoint remains the only URL given to the Pi.
func (s *appServer) serveCloudOrderArtifact(w http.ResponseWriter,r *http.Request,key,wantChecksum string) {
 if !safePreparedObjectKey(key) || len(wantChecksum)!=64 {
  http.Error(w,"invalid prepared document reference",500);return
 }
 base,secret,err:=cloudStorageConfig()
 if err!=nil {http.Error(w,"cloud storage unavailable",503);return}
 dir:=s.cfg.jobsDir
 if dir=="" {http.Error(w,"print workspace unavailable",503);return}
 file,n,err:=fetchVerifiedCloudArtifact(r.Context(),base,secret,key,wantChecksum,dir)
 if err!=nil {http.Error(w,"prepared document unavailable or failed integrity verification",503);return}
 defer os.Remove(file)
 f,err:=os.Open(filepath.Clean(file))
 if err!=nil {http.Error(w,"prepared document unavailable",503);return}
 defer f.Close()
 w.Header().Set("Content-Type","application/pdf")
 w.Header().Set("Content-Length",strconv.FormatInt(n,10))
 w.Header().Set("Cache-Control","private, no-store")
 _,_=io.Copy(w,f)
}
