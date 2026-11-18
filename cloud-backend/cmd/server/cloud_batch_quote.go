package main

// Read-only, capability-guarded multi-document quote preview. No order, PIN,
// payment, PDF merging, file deletion or printer dispatch occurs here.
import (
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "net/http"
 "os"
 "strings"

 "xeroxgo/cloud-backend/internal/store"
)

type cloudBatchDocumentInput struct {
 UploadID string `json:"upload_id"`
 UploadToken string `json:"upload_token"`
 VariantID string `json:"variant_id"`
}

type cloudBatchQuoteInput struct {
 KioskID string `json:"kiosk_id"`
 Documents []cloudBatchDocumentInput `json:"documents"`
}

func cloudBatchQuoteEnabled() bool {
 return cloudUploadsEnabled() && os.Getenv("XEROXGO_CLOUD_BATCH_QUOTE_ENABLED")=="1"
}

func validCloudBatchQuoteRequest(in cloudBatchQuoteInput) bool {
 if in.KioskID==""||len(in.KioskID)>100||strings.ContainsAny(in.KioskID,"/\\?&#")||
  len(in.Documents)<1||len(in.Documents)>20 {return false}
 seen:=map[string]bool{}
 for _,doc:=range in.Documents{
  if len(doc.UploadID)!=32||len(doc.UploadToken)!=64||len(doc.VariantID)!=32||
   seen[doc.VariantID] {return false}
  seen[doc.VariantID]=true
 }
 return true
}

func (s *appServer) handleCloudBatchQuote(w http.ResponseWriter,r *http.Request) {
 if !cloudBatchQuoteEnabled(){http.NotFound(w,r);return}
 if r.Method!=http.MethodPost{http.Error(w,"method not allowed",405);return}
 if s.db==nil{http.Error(w,"quote service unavailable",503);return}
 r.Body=http.MaxBytesReader(w,r.Body,32768)
 var input cloudBatchQuoteInput
 decoder:=json.NewDecoder(r.Body);decoder.DisallowUnknownFields()
 if decoder.Decode(&input)!=nil||!validCloudBatchQuoteRequest(input){
  http.Error(w,"invalid batch quote request",400);return
 }
 refs:=make([]store.CloudPreparedReference,0,len(input.Documents))
 for _,doc:=range input.Documents{
  sum:=sha256.Sum256([]byte(doc.UploadToken))
  upload,err:=s.db.AuthorizeCloudDocumentUpload(r.Context(),doc.UploadID,hex.EncodeToString(sum[:]))
  if err!=nil||upload.KioskID!=input.KioskID||upload.State!="UPLOADED" {
   http.Error(w,"document capability invalid",404);return
  }
  refs=append(refs,store.CloudPreparedReference{UploadID:doc.UploadID,VariantID:doc.VariantID})
 }
 plan,err:=s.db.LoadReadyCloudPrintPlan(r.Context(),input.KioskID,refs)
 if err!=nil{http.Error(w,"prepared documents are not ready or were changed",409);return}
 tariff,err:=s.loadRequestTariff(r.Context())
 if err!=nil{http.Error(w,"pricing unavailable",503);return}
 if tariff.Revision!=plan.TariffRevision{
  http.Error(w,"tariff changed; please prepare the documents again",409);return
 }
 docs:=make([]map[string]any,0,len(plan.Tasks))
 for _,task:=range plan.Tasks{
  docs=append(docs,map[string]any{
   "sequence_no":task.SequenceNo,"variant_id":task.VariantID,
   "duplex":task.Duplex,"color_mode":task.ColorMode,
   "print_copies":task.Copies,"total_sheets":task.TotalSheets,
   "billed_sides":task.TotalPages,"price_paise":task.PricePaise,
  })
 }
 w.Header().Set("Cache-Control","no-store")
 w.Header().Set("Content-Type","application/json")
 _=json.NewEncoder(w).Encode(map[string]any{
  "status":"READY","documents":docs,
  "total_price_paise":plan.TotalPricePaise,
  "total_sheets":plan.TotalSheets,"billed_sides":plan.TotalPages,
  "tariff_revision":plan.TariffRevision,
  "checkout_supported":len(plan.Tasks)==1,
 })
}
