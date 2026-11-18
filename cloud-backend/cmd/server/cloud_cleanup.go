package main

import (
 "context"
 "encoding/hex"
 "errors"
 "fmt"
 "log"
 "net/http"
 "os"
 "time"

 "xeroxgo/cloud-backend/internal/store"
)

// Cleanup stays OFF until bucket creation, quotas, retention policy and Pi
// recovery lifecycle are reviewed. Every deletion is computed from database
// state; a client never chooses a bucket or key.
func cloudCleanupEnabled() bool {
 return cloudUploadsEnabled() && os.Getenv("XEROXGO_CLOUD_CLEANUP_ENABLED")=="1"
}

func safeCloudCleanupLocation(item store.CloudCleanupObject) (bucket string,err error) {
 switch item.Kind {
 case "prepared":
  if len(item.ID)!=32 || item.Key!="variants/"+item.ID+"/output.pdf" || !safePreparedObjectKey(item.Key) {
   return "",errors.New("invalid private variant key")
  }
  return cloudPreparedBucket,nil
 case "original":
  if len(item.ID)!=32 {return "",errors.New("invalid original identity")}
  if _,err=hex.DecodeString(item.ID);err!=nil{return "",errors.New("invalid original identity")}
  if item.Key!="uploads/"+item.ID+"/original.pdf" {return "",errors.New("invalid original key")}
  return cloudOriginalBucket,nil
 default:
  return "",errors.New("invalid object kind")
 }
}

func deletePrivateCloudObject(ctx context.Context,base,secret string,item store.CloudCleanupObject) error {
 bucket,err:=safeCloudCleanupLocation(item)
 if err!=nil{return err}
 endpoint:=base+"/storage/v1/object/"+bucket+"/"+item.Key
 resp,err:=supabaseStorageRequest(ctx,http.MethodDelete,endpoint,secret,nil)
 if err!=nil{return err}
 defer resp.Body.Close()
 if resp.StatusCode==http.StatusNotFound || (resp.StatusCode>=200&&resp.StatusCode<300) {
  return nil
 }
 return fmt.Errorf("private Storage deletion failed (HTTP %d)",resp.StatusCode)
}

// Once per cleanup tick, process a bounded batch. Missing Storage objects
// count as already deleted, but transient failures remain retryable in the DB.
func (s *appServer) cleanupExpiredCloudDocuments(ctx context.Context) {
 if !cloudCleanupEnabled()||s.db==nil{return}
 base,secret,err:=cloudStorageConfig()
 if err!=nil {log.Printf("[CLOUD-CLEANUP] missing Storage configuration");return}
 cleanupCtx,cancel:=context.WithTimeout(ctx,45*time.Second)
 defer cancel()
 candidates,err:=s.db.ListExpiredCloudObjects(cleanupCtx,time.Now().UTC(),25)
 if err!=nil {log.Printf("[CLOUD-CLEANUP] candidate query failed: %v",err);return}
 for _,candidate:=range candidates {
  if cleanupCtx.Err()!=nil{return}
  if err=deletePrivateCloudObject(cleanupCtx,base,secret,candidate);err!=nil {
   log.Printf("[CLOUD-CLEANUP] deletion failed for %s: %v",candidate.Kind,err)
   continue
  }
  _,err=s.db.MarkCloudObjectExpired(cleanupCtx,candidate,time.Now().UTC())
  if err!=nil {log.Printf("[CLOUD-CLEANUP] database update failed: %v",err)}
 }
}
