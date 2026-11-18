package main

import (
 "context"
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "errors"
 "log"
 "net/http"
 "os"
 "strconv"
 "time"

 "xeroxgo/cloud-backend/internal/store"
)

// Separate gate: original synchronous /variants/prepare remains available for
// focused tests. In async mode, that endpoint enqueues and responds quickly.
// No work is tied to the customer's connection after enqueue commits.
func cloudAsyncPreparationEnabled() bool {
 return cloudUploadsEnabled() && os.Getenv("XEROXGO_CLOUD_ASYNC_PREP_ENABLED")=="1"
}

func (s *appServer) enqueueCloudVariantPreparation(w http.ResponseWriter,r *http.Request,
 item store.CloudDocumentUpload,opts docPrintOptions,revision int64) {
 settingsHash:=cloudVariantSettingsHash(opts)
 version:=cloudVariantNormalizer+"-tariff-"+strconv.FormatInt(revision,10)
 id,err:=randomHex(16)
 if err!=nil{http.Error(w,"could not allocate preparation",500);return}
 settingsJSON,err:=json.Marshal(opts)
 if err!=nil{http.Error(w,"invalid print settings",400);return}
 job,err:=s.db.EnqueueCloudPreparation(r.Context(),store.CloudPreparationJob{
  ID:id,UploadID:item.ID,SettingsHash:settingsHash,NormalizerVersion:version,
  OptionsJSON:string(settingsJSON),TariffRevision:revision,
 })
 if err!=nil{http.Error(w,"could not queue document preparation",503);return}
 w.Header().Set("Cache-Control","no-store")
 w.Header().Set("Content-Type","application/json")
 w.WriteHeader(http.StatusAccepted)
 _=json.NewEncoder(w).Encode(map[string]any{
  "preparation_id":job.ID,"status":job.State,"variant_id":job.VariantID,
 })
}

func (s *appServer) runCloudPreparationJob(ctx context.Context,job *store.CloudPreparationJob) (variantID string,err error) {
 // Never allow a Render restart/browser disconnect to cancel persisted work.
 // All work is bounded more tightly than the database lease.
 workCtx,cancel:=context.WithTimeout(ctx,8*time.Minute)
 defer cancel()
 var opts docPrintOptions
 if json.Unmarshal([]byte(job.OptionsJSON),&opts)!=nil ||
  cloudVariantSettingsHash(opts)!=job.SettingsHash {
  return "",errors.New("stored normalization settings failed integrity check")
 }
 tariff,err:=s.loadRequestTariff(workCtx)
 if err!=nil{return "",err}
 if tariff.Revision!=job.TariffRevision ||
  job.NormalizerVersion!=cloudVariantNormalizer+"-tariff-"+strconv.FormatInt(tariff.Revision,10) {
  return "",errors.New("active tariff changed; refresh document preparation")
 }
 // Persisted input references come from a verified upload, never user paths.
 item,err:=s.db.GetVerifiedCloudUploadForWorker(workCtx,job.UploadID)
 if err!=nil{return "",err}
 id,err:=randomHex(16)
 if err!=nil{return "",err}
 candidate:=store.CloudDocumentVariant{
  ID:id,UploadID:item.ID,SettingsHash:job.SettingsHash,
  NormalizerVersion:job.NormalizerVersion,
  ExpiresAt:time.Now().UTC().Add(4*time.Hour),
 }
 variant,acquired,err:=s.db.AcquireCloudVariant(workCtx,candidate)
 if err!=nil{return "",err}
 if !acquired{
  if variant.State=="READY" && time.Now().Before(variant.ExpiresAt) {
   return variant.ID,nil
  }
  return "",errors.New("canonical variant is already being prepared")
 }
 published:=false
 defer func(){
  if !published {
   cleanupCtx,cleanupCancel:=context.WithTimeout(context.Background(),5*time.Second)
   defer cleanupCancel()
   _=s.db.FailCloudVariant(cleanupCtx,variant.ID)
  }
 }()
 base,secret,err:=cloudStorageConfig()
 if err!=nil{return "",err}
 source,err:=readVerifiedOriginalToTemp(workCtx,base,secret,item,s.cfg.jobsDir)
 if err!=nil{return "",err}
 defer os.Remove(source)
 target,err:=os.CreateTemp(s.cfg.jobsDir,"cloud_async_*.pdf")
 if err!=nil{return "",err}
 output:=target.Name();_=target.Close()
 defer os.Remove(output)
 normalized,err:=s.executeNormalizationAndPricing(workCtx,source,output,opts,tariff)
 if err!=nil{return "",err}
 key:="variants/"+variant.ID+"/output.pdf"
 checksum,err:=uploadPreparedVariant(workCtx,base,secret,key,output)
 if err!=nil{return "",err}
 variant.StorageKey=key
 variant.Checksum=checksum
 variant.BilledSides=normalized.PrintedSides
 variant.TotalSheets=normalized.NormResult.TotalSheets
 variant.PricePaise=normalized.PricePaise
 variant.TariffRevision=normalized.TariffRevision
 variant.Copies=opts.Copies
 variant.Duplex=string(opts.Duplex)
 variant.ColorMode=opts.ColorMode
 published,err=s.db.PublishCloudVariant(workCtx,variant)
 if err!=nil{return "",err}
 if !published{return "",errors.New("canonical variant lease was superseded")}
 return variant.ID,nil
}

func (s *appServer) processCloudPreparationQueue(ctx context.Context) {
 if _,err:=s.db.FailExhaustedCloudPreparations(ctx,time.Now().UTC());err!=nil {
  log.Printf("[CLOUD-PREP] exhausted-worker reconciliation failed: %v",err)
  return
 }
 for i:=0;i<3 && ctx.Err()==nil;i++ {
  job,err:=s.db.ClaimNextCloudPreparation(ctx,time.Now().UTC())
  if err!=nil{log.Printf("[CLOUD-PREP] claim failure: %v",err);return}
  if job==nil{return}
  variantID,err:=s.runCloudPreparationJob(ctx,job)
  finishCtx,finishCancel:=context.WithTimeout(context.Background(),5*time.Second)
  if err!=nil{
   log.Printf("[CLOUD-PREP] attempt %d failed for preparation %s: %v",job.Attempts,job.ID,err)
   _=s.db.RetryCloudPreparation(finishCtx,job.ID,job.Attempts)
  }else{
   ok,finishErr:=s.db.FinishCloudPreparation(finishCtx,job.ID,job.Attempts,variantID,"READY")
   if finishErr!=nil||!ok{log.Printf("[CLOUD-PREP] completion lost; job will recover: %v",finishErr)}
  }
  finishCancel()
 }
}

// Polls independently of browser requests. SQL holds claim state durably,
// so a restarted Render instance can reclaim expired leases.
func (s *appServer) startCloudPreparationWorker(ctx context.Context) {
 if !cloudAsyncPreparationEnabled()||s.db==nil{return}
 ticker:=time.NewTicker(2*time.Second)
 defer ticker.Stop()
 for {
  s.processCloudPreparationQueue(ctx)
  select {
  case <-ctx.Done():return
  case <-ticker.C:
  }
 }
}

type cloudPreparationStatusRequest struct {
 UploadID string `json:"upload_id"`
 UploadToken string `json:"upload_token"`
 PreparationID string `json:"preparation_id"`
}

func (s *appServer) handleCloudPreparationStatus(w http.ResponseWriter,r *http.Request) {
 if !cloudAsyncPreparationEnabled(){http.NotFound(w,r);return}
 if r.Method!=http.MethodPost{http.Error(w,"method not allowed",405);return}
 r.Body=http.MaxBytesReader(w,r.Body,4096)
 var in cloudPreparationStatusRequest
 dec:=json.NewDecoder(r.Body);dec.DisallowUnknownFields()
 if dec.Decode(&in)!=nil||len(in.UploadID)!=32||len(in.UploadToken)!=64||
  len(in.PreparationID)!=32 {
  http.Error(w,"invalid preparation receipt",400);return
 }
 hash:=sha256.Sum256([]byte(in.UploadToken))
 upload,err:=s.db.AuthorizeCloudDocumentUpload(r.Context(),in.UploadID,hex.EncodeToString(hash[:]))
 if err!=nil {http.Error(w,"preparation not found",404);return}
 job,err:=s.db.GetCloudPreparationByID(r.Context(),upload.ID,in.PreparationID)
 if err!=nil{http.Error(w,"preparation not found",404);return}
 status:=job.State
 if status=="PROCESSING"&&!time.Now().Before(job.LeaseExpiresAt) {status="RECOVERING"}
 w.Header().Set("Cache-Control","no-store")
 w.Header().Set("Content-Type","application/json")
 result:=map[string]any{"preparation_id":job.ID,"status":status}
 if status=="READY" {
  variant,err:=s.db.GetCloudVariantByID(r.Context(),upload.ID,job.VariantID)
  if err!=nil||variant.State!="READY" {http.Error(w,"prepared document not yet available",503);return}
  result["variant_id"]=variant.ID
  result["price_paise"]=variant.PricePaise
  result["tariff_revision"]=variant.TariffRevision
  result["total_sheets"]=variant.TotalSheets
  result["billed_sides"]=variant.BilledSides
 }
 _=json.NewEncoder(w).Encode(result)
}
