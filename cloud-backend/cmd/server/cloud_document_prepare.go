package main

// Cloud-first canonical preparation is deliberately opt-in. This endpoint never
// creates an order or PIN. The existing checkout remains untouched.
import (
 "context"
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "errors"
 "fmt"
 "io"
 "net/http"
 "os"
 "strconv"
 "strings"
 "time"

 "xeroxgo/cloud-backend/internal/store"
 "xeroxgo/pkg/contracts"
)

const cloudPreparedBucket = "xeroxgo-prepared"
const maxPreparedPDFBytes int64 = 50 * 1024 * 1024
const cloudVariantNormalizer = "canonical-v1"

type prepareCloudVariantRequest struct {
 UploadID string `json:"upload_id"`
 UploadToken string `json:"upload_token"`
 Settings docItemMeta `json:"settings"`
}

func strictVariantOptions(m docItemMeta, colorEnabled bool) (docPrintOptions, error) {
 var opts docPrintOptions
 if m.Copies < 1 || m.Copies > 999 { return opts, errors.New("invalid copies") }
 if m.PagesPerSheet != 1 && m.PagesPerSheet != 2 && m.PagesPerSheet != 4 && m.PagesPerSheet != 6 {
  return opts, errors.New("invalid pages_per_sheet")
 }
 if m.Nup != 0 { return opts, errors.New("use pages_per_sheet only") }
 duplex, ok := m.Duplex.(string)
 if !ok { return opts, errors.New("duplex must be specified") }
 switch strings.ToUpper(duplex) {
 case "SIMPLEX", "DUPLEX_LONG_EDGE", "DUPLEX_SHORT_EDGE":
 default: return opts, errors.New("invalid duplex")
 }
 if m.ColorMode != "BW" && m.ColorMode != "COLOR" { return opts, errors.New("invalid color_mode") }
 if m.ColorMode == "COLOR" && !colorEnabled { return opts, errors.New("color printing is disabled") }
 if m.Orientation != "AUTO" && m.Orientation != "PORTRAIT" && m.Orientation != "LANDSCAPE" {
  return opts, errors.New("invalid orientation")
 }
 opts, err := parseMetadataPrintOptions(m, colorEnabled, 1)
 if err != nil { return opts, err }
 if opts.Duplex != contracts.DuplexSimplex && opts.Duplex != contracts.DuplexLongEdge &&
  opts.Duplex != contracts.DuplexShortEdge { return opts, errors.New("invalid duplex") }
 return opts, nil
}

func cloudVariantSettingsHash(opts docPrintOptions) string {
 normalized := []string{strconv.Itoa(opts.Copies),string(opts.Duplex),opts.ColorMode,
  strings.TrimSpace(opts.PageRange),opts.Orientation,strconv.Itoa(opts.PagesPerSheet)}
 h := sha256.Sum256([]byte(strings.Join(normalized,"\x00")))
 return hex.EncodeToString(h[:])
}

func readVerifiedOriginalToTemp(ctx context.Context, base, secret string, item store.CloudDocumentUpload, dir string) (string, error) {
 if item.VerifiedSHA256 == "" || len(item.VerifiedSHA256) != 64 || item.DeclaredBytes <= 0 ||
  item.DeclaredBytes > cloudDocumentMaxBytes { return "", errors.New("missing verified source") }
 endpoint := base+"/storage/v1/object/authenticated/"+cloudOriginalBucket+"/"+item.ObjectKey
 resp, err := supabaseStorageRequest(ctx,http.MethodGet,endpoint,secret,nil)
 if err != nil { return "", err }
 defer resp.Body.Close()
 if resp.StatusCode != http.StatusOK { return "", fmt.Errorf("source retrieval returned %d",resp.StatusCode) }
 f, err := os.CreateTemp(dir,"cloud_source_*.pdf")
 if err != nil { return "",err }
 location := f.Name()
 defer func() { _ = f.Close() }()
 h := sha256.New()
 n, err := io.Copy(io.MultiWriter(f,h),io.LimitReader(resp.Body,cloudDocumentMaxBytes+1))
 if err != nil || n != item.DeclaredBytes || n > cloudDocumentMaxBytes ||
  hex.EncodeToString(h.Sum(nil)) != item.VerifiedSHA256 {
  _ = os.Remove(location)
  return "",errors.New("verified source changed or has invalid length")
 }
 if err := f.Close(); err != nil { _ = os.Remove(location); return "",err }
 return location,nil
}

func uploadPreparedVariant(ctx context.Context, base, secret, key, file string) (string,error) {
 f, err := os.Open(file)
 if err != nil { return "",err }
 defer f.Close()
 stat, err := f.Stat()
 if err != nil || stat.Size() <= 0 || stat.Size() > maxPreparedPDFBytes {
  return "",errors.New("normalized PDF exceeds storage limit")
 }
 sum := sha256.New()
 if _,err = io.Copy(sum,f); err != nil { return "",err }
 if _,err = f.Seek(0,io.SeekStart); err != nil { return "",err }
 endpoint := base+"/storage/v1/object/"+cloudPreparedBucket+"/"+key
 req,err := http.NewRequestWithContext(ctx,http.MethodPost,endpoint,f)
 if err != nil { return "",err }
 req.ContentLength=stat.Size()
 req.Header.Set("Authorization","Bearer "+secret)
 req.Header.Set("apikey",secret)
 req.Header.Set("Content-Type","application/pdf")
 req.Header.Set("x-upsert","false")
 client:=&http.Client{Timeout:180*time.Second}
 resp,err:=client.Do(req)
 if err!=nil {return "",err}
 defer resp.Body.Close()
 if resp.StatusCode/100!=2 {return "",fmt.Errorf("prepared Storage upload returned %d",resp.StatusCode)}
 return hex.EncodeToString(sum.Sum(nil)),nil
}

// POST /api/documents/variants/prepare
// Synchronous for this guarded foundation: a durable async worker, retry lease,
// expiry cleanup, multi-file packing and checkout remain separate stages.
func (s *appServer) handleCloudVariantPrepare(w http.ResponseWriter,r *http.Request) {
 if !cloudUploadsEnabled(){http.NotFound(w,r);return}
 if r.Method!=http.MethodPost{http.Error(w,"method not allowed",405);return}
 if s.db==nil {http.Error(w,"database unavailable",503);return}
 r.Body=http.MaxBytesReader(w,r.Body,8192)
 var input prepareCloudVariantRequest
 decoder:=json.NewDecoder(r.Body); decoder.DisallowUnknownFields()
 if decoder.Decode(&input)!=nil || len(input.UploadID)!=32 || len(input.UploadToken)!=64 {
  http.Error(w,"invalid preparation request",400);return
 }
 sum:=sha256.Sum256([]byte(input.UploadToken))
 item,err:=s.db.AuthorizeCloudDocumentUpload(r.Context(),input.UploadID,hex.EncodeToString(sum[:]))
 if err!=nil {http.Error(w,"upload not found",404);return}
 if item.State!="UPLOADED" {http.Error(w,"document is not uploaded and verified",409);return}
 tariff,err:=s.loadRequestTariff(r.Context())
 if err!=nil {http.Error(w,"pricing unavailable",503);return}
 opts,err:=strictVariantOptions(input.Settings,tariff.ColorEnabled)
 if err!=nil {http.Error(w,err.Error(),400);return}
 base,secret,err:=cloudStorageConfig()
 if err!=nil {http.Error(w,"cloud storage unavailable",503);return}
 key:=cloudVariantSettingsHash(opts)
 // A variant is tied to a tariff revision until independent variant
 // repricing has been implemented. Existing output bytes are immutable.
 version:=cloudVariantNormalizer+"-tariff-"+strconv.FormatInt(tariff.Revision,10)
 id,err:=randomHex(16)
 if err!=nil {http.Error(w,"variant allocation failed",500);return}
 candidate:=store.CloudDocumentVariant{ID:id,UploadID:item.ID,SettingsHash:key,
  NormalizerVersion:version,ExpiresAt:time.Now().UTC().Add(4*time.Hour)}
 variant,acquired,err:=s.db.AcquireCloudVariant(r.Context(),candidate)
 if err!=nil {http.Error(w,"could not reserve variant",503);return}
 if !acquired {
  w.Header().Set("Cache-Control","no-store")
  w.Header().Set("Content-Type","application/json")
  if variant.State=="READY" && time.Now().Before(variant.ExpiresAt) {
   _=json.NewEncoder(w).Encode(map[string]any{
    "variant_id":variant.ID,"status":"READY","price_paise":variant.PricePaise,
    "tariff_revision":variant.TariffRevision,"total_sheets":variant.TotalSheets,
    "billed_sides":variant.BilledSides,
   });return
  }
  if variant.State=="PREPARING" {w.WriteHeader(http.StatusAccepted)
   _=json.NewEncoder(w).Encode(map[string]any{"variant_id":variant.ID,"status":"PREPARING"});return}
  http.Error(w,"variant is unavailable; upload a new document",409);return
 }
 published:=false
 defer func(){if !published {_=s.db.FailCloudVariant(context.Background(),variant.ID)}}()
 source,err:=readVerifiedOriginalToTemp(r.Context(),base,secret,item,s.cfg.jobsDir)
 if err!=nil {http.Error(w,"cloud source could not be verified",422);return}
 defer os.Remove(source)
 output,err:=os.CreateTemp(s.cfg.jobsDir,"cloud_variant_*.pdf")
 if err!=nil {http.Error(w,"cannot create normalization workspace",500);return}
 outputPath:=output.Name();_ = output.Close()
 defer os.Remove(outputPath)
 normalized,err:=s.executeNormalizationAndPricing(r.Context(),source,outputPath,opts,tariff)
 if err!=nil {http.Error(w,"document normalization failed",422);return}
 storageKey:="variants/"+variant.ID+"/output.pdf"
 checksum,err:=uploadPreparedVariant(r.Context(),base,secret,storageKey,outputPath)
 if err!=nil {http.Error(w,"prepared file could not be stored",502);return}
 variant.StorageKey=storageKey
 variant.Checksum=checksum
 variant.BilledSides=normalized.PrintedSides
 variant.TotalSheets=normalized.NormResult.TotalSheets
 variant.PricePaise=normalized.PricePaise
 variant.TariffRevision=normalized.TariffRevision
 published,err=s.db.PublishCloudVariant(r.Context(),variant)
 if err!=nil || !published {http.Error(w,"prepared file could not be registered",503);return}
 w.Header().Set("Cache-Control","no-store")
 w.Header().Set("Content-Type","application/json")
 _=json.NewEncoder(w).Encode(map[string]any{
  "variant_id":variant.ID,"status":"READY","price_paise":variant.PricePaise,
  "tariff_revision":variant.TariffRevision,"total_sheets":variant.TotalSheets,
  "billed_sides":variant.BilledSides,
 })
}

