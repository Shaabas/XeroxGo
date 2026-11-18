package main

// Single-prepared-variant reference checkout. A separate release gate is
// required for mixed-document output packing, durable workers and Pi rollout.
import (
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "errors"
 "net/http"
 "os"
 "strings"
 "time"

 "xeroxgo/cloud-backend/internal/store"
 "xeroxgo/pkg/contracts"
)

type cloudCheckoutRequest struct {
 KioskID string `json:"kiosk_id"`
 UploadID string `json:"upload_id"`
 UploadToken string `json:"upload_token"`
 VariantID string `json:"variant_id"`
 RequestID string `json:"request_id"`
 AcceptedPricePaise int64 `json:"accepted_total_price_paise"`
 AcceptedTariffRevision int64 `json:"accepted_tariff_revision"`
}

func validCloudCheckoutRequest(in cloudCheckoutRequest) bool {
 return len(in.UploadID)==32 && len(in.UploadToken)==64 &&
  len(in.VariantID)==32 && len(in.RequestID)==32 &&
  in.KioskID!="" && len(in.KioskID)<=100 &&
  !strings.ContainsAny(in.KioskID,"/\\?&#") &&
  in.AcceptedPricePaise>0 && in.AcceptedTariffRevision>0
}

func respondCloudOrder(w http.ResponseWriter, order *store.Order, revision int64, reused bool) {
 w.Header().Set("Cache-Control","no-store")
 w.Header().Set("Content-Type","application/json")
 _=json.NewEncoder(w).Encode(map[string]any{
  "order_id":order.OrderID,"job_id":order.Command.JobID,
  "display_code":order.Command.DisplayCode,"claim_token":order.ClaimToken,
  "recovery_token":order.ClaimToken,
  "pin_expires_at":order.PINExpiresAt.UTC().Format(time.RFC3339),
  "total_price_paise":order.PricePaise,"tariff_revision":revision,
  "total_pages":order.Command.TotalPages,"total_sheets":order.Command.TotalSheets,
  "status":string(order.Status),"reused":reused,
 })
}

func cloudCheckoutEnabled() bool { return cloudUploadsEnabled() && strings.TrimSpace(strings.ToLower(strings.TrimSpace(getCloudCheckoutEnv())))=="1" }

func getCloudCheckoutEnv() string { return os.Getenv("XEROXGO_CLOUD_CHECKOUT_ENABLED") }

func (s *appServer) handleCloudReferenceCheckout(w http.ResponseWriter,r *http.Request) {
 if !cloudCheckoutEnabled(){http.NotFound(w,r);return}
 if r.Method!=http.MethodPost{http.Error(w,"method not allowed",http.StatusMethodNotAllowed);return}
 if s.db==nil{http.Error(w,"database unavailable",http.StatusServiceUnavailable);return}
 r.Body=http.MaxBytesReader(w,r.Body,8192)
 var in cloudCheckoutRequest
 dec:=json.NewDecoder(r.Body);dec.DisallowUnknownFields()
 if dec.Decode(&in)!=nil || !validCloudCheckoutRequest(in){
  http.Error(w,"invalid checkout request",http.StatusBadRequest);return
 }
 tokenHash:=sha256.Sum256([]byte(in.UploadToken))
 upload,err:=s.db.AuthorizeCloudDocumentUpload(r.Context(),in.UploadID,hex.EncodeToString(tokenHash[:]))
 if err!=nil || upload.KioskID!=in.KioskID {
  http.Error(w,"upload not found",http.StatusNotFound);return
 }
 // First look up the immutable committed request. Never generate another
 // PIN because the customer's previous HTTP response was lost.
 previousID,lookupErr:=s.db.LookupCloudOrderRequest(r.Context(),in.RequestID,
  in.UploadID,in.VariantID,in.KioskID,in.AcceptedPricePaise,in.AcceptedTariffRevision)
 if lookupErr==nil {
  previous,err:=s.db.GetOrder(r.Context(),previousID)
  if err!=nil || previous==nil {http.Error(w,"order recovery unavailable",503);return}
  respondCloudOrder(w,previous,in.AcceptedTariffRevision,true);return
 }
 if !errors.Is(lookupErr,store.ErrCloudOrderConflict){
  http.Error(w,"order recovery unavailable",503);return
 }
 if upload.State!="UPLOADED"{http.Error(w,"document is not ready",http.StatusConflict);return}
 variant,err:=s.db.GetCloudVariantByID(r.Context(),upload.ID,in.VariantID)
 if err!=nil || variant.State!="READY" || !time.Now().Before(variant.ExpiresAt){
  http.Error(w,"prepared file is not ready",http.StatusConflict);return
 }
 if variant.PricePaise!=in.AcceptedPricePaise ||
  variant.TariffRevision!=in.AcceptedTariffRevision ||
  len(variant.Checksum)!=64 || variant.StorageKey=="" ||
  variant.BilledSides<=0 || variant.TotalSheets<=0 ||
  variant.Copies<1 || variant.Duplex=="" || variant.ColorMode=="" {
  http.Error(w,"quote changed; please review the prepared document",http.StatusConflict);return
 }
 // The authoritative active revision is locked again in the transaction.
 tariff,err:=s.loadRequestTariff(r.Context())
 if err!=nil {http.Error(w,"pricing unavailable",http.StatusServiceUnavailable);return}
 if tariff.Revision!=variant.TariffRevision{
  http.Error(w,"tariff changed; please review your quotation",http.StatusConflict);return
 }

 var order *store.Order
 var reused bool
 for attempt:=0;attempt<25;attempt++ {
  orderID,err:=randomHex(12)
  if err!=nil {break}
  jobID,err:=randomHex(12)
  if err!=nil {break}
  claimToken,err:=randomHex(32)
  if err!=nil {break}
  pin,err:=randomDisplayCode()
  if err!=nil {break}
  expiry:=time.Now().UTC().Add(15*time.Minute)
  candidate:=store.Order{
   OrderID:"ord_"+orderID,Status:contracts.JobStatusPendingPayment,
   PricePaise:variant.PricePaise,ColorMode:variant.ColorMode,
   PINExpiresAt:expiry,ClaimToken:claimToken,
   Command:contracts.PrintJobCommand{
    JobID:"job_"+jobID,KioskID:in.KioskID,DisplayCode:pin,
    SHA256Checksum:variant.Checksum,TotalPages:variant.BilledSides,
    TotalSheets:variant.TotalSheets,Copies:1,StartFromPage:1,
    Duplex:contracts.DuplexMode(variant.Duplex),
   },
  }
  candidate.Command.DownloadURL=generateSignedDownloadURL(
   s.getBaseURL(r),candidate.OrderID,s.cfg.downloadSecret,24*time.Hour)
  priorID,created,err:=s.db.CreateCloudOrderAtomic(r.Context(),store.CloudOrderInput{
   RequestID:in.RequestID,UploadID:in.UploadID,VariantID:in.VariantID,
   AcceptedPricePaise:in.AcceptedPricePaise,
   TariffRevision:in.AcceptedTariffRevision,Order:candidate,
  })
  if err==nil {
   if created {order=&candidate} else {
    order,err=s.db.GetOrder(r.Context(),priorID)
    reused=true
    if err!=nil || order==nil {http.Error(w,"order recovery unavailable",503);return}
   }
   break
  }
  if errors.Is(err,store.ErrPINCollision){continue}
  if errors.Is(err,store.ErrCloudOrderConflict) {
   id,e:=s.db.LookupCloudOrderRequest(r.Context(),in.RequestID,in.UploadID,
    in.VariantID,in.KioskID,in.AcceptedPricePaise,in.AcceptedTariffRevision)
   if e==nil {
    order,e=s.db.GetOrder(r.Context(),id)
    if e==nil && order!=nil {reused=true;break}
   }
   http.Error(w,"order request already submitted; retry using the same request ID",409);return
  }
  if errors.Is(err,store.ErrSheetTariffRevisionConflict) ||
   errors.Is(err,store.ErrCloudQuoteConflict) ||
   errors.Is(err,store.ErrVariantUnavailable){
   http.Error(w,"quote changed or prepared document unavailable",409);return
  }
  http.Error(w,"unable to create order",http.StatusServiceUnavailable);return
 }
 if order==nil {http.Error(w,"unable to reserve a PIN",http.StatusServiceUnavailable);return}
 respondCloudOrder(w,order,in.AcceptedTariffRevision,reused)
}
