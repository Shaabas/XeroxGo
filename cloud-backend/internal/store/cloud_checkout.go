package store

import (
 "context"
 "database/sql"
 "errors"
 "fmt"
 "strings"
 "time"

 "xeroxgo/pkg/contracts"
)

var (
 ErrCloudQuoteConflict = errors.New("cloud quotation or tariff changed")
 ErrCloudOrderConflict = errors.New("cloud checkout request conflicts with an existing order")
)

// CloudOrderInput describes one immutable prepared PDF. This first phase only
// permits one variant; multi-file merging must be designed and tested before use.
type CloudOrderInput struct {
 RequestID string
 UploadID string
 VariantID string
 AcceptedPricePaise int64
 TariffRevision int64
 Order Order
}

// CreateCloudOrderAtomic validates a READY prepared variant and stores the
// order, PIN reservation, print config, variant reference and idempotency row
// in ONE transaction. No normalized bytes or print settings come from client.
func (s *Store) CreateCloudOrderAtomic(ctx context.Context, in CloudOrderInput) (existingOrderID string, created bool, err error) {
 if s==nil || s.db==nil {return "",false,errors.New("database unavailable")}
 o:=in.Order
 if in.RequestID=="" || in.UploadID=="" || in.VariantID=="" ||
  in.AcceptedPricePaise<=0 || in.TariffRevision<=0 ||
  o.OrderID=="" || o.Command.JobID=="" || o.Command.KioskID=="" ||
  o.Command.DisplayCode=="" || o.Command.SHA256Checksum=="" ||
  o.Command.TotalPages<1 || o.Command.TotalSheets<1 || o.Command.Copies<1 ||
  o.Command.StartFromPage!=1 || o.Command.Copies!=1 || o.Command.DownloadURL=="" ||
  o.PricePaise!=in.AcceptedPricePaise {
  return "",false,errors.New("invalid cloud order")
 }
 tx,err:=s.db.BeginTx(ctx,nil)
 if err!=nil {return "",false,err}
 defer tx.Rollback()

 // Idempotency check before reading the tariff. Retrying a completed request
 // returns the ORIGINAL, immutable order even if the tariff has since changed.
 existingQ:=s.rebind(`SELECT order_id,upload_id,variant_id,kiosk_id,accepted_price_paise,tariff_revision
  FROM cloud_order_requests WHERE request_id=?`)
 var priorID,priorUpload,priorVariant,priorKiosk string
 var priorPrice,priorRevision int64
 err=tx.QueryRowContext(ctx,existingQ,in.RequestID).Scan(
  &priorID,&priorUpload,&priorVariant,&priorKiosk,&priorPrice,&priorRevision)
 if err==nil {
  if priorUpload!=in.UploadID || priorVariant!=in.VariantID ||
   priorKiosk!=o.Command.KioskID || priorPrice!=in.AcceptedPricePaise ||
   priorRevision!=in.TariffRevision {return "",false,ErrCloudOrderConflict}
  return priorID,false,nil
 }
 if !errors.Is(err,sql.ErrNoRows) {return "",false,err}

 revisionQ:="SELECT active_revision FROM sheet_tariff_state WHERE id=1"
 if s.dialect=="postgres" {revisionQ+=" FOR UPDATE"}
 var currentRevision int64
 if err=tx.QueryRowContext(ctx,revisionQ).Scan(&currentRevision);err!=nil {return "",false,err}
 if currentRevision!=in.TariffRevision {return "",false,ErrSheetTariffRevisionConflict}

 // Hold the prepared variant stable while building the order. The uploaded
 // source and prepared file must be fresh and explicitly ready.
 variantQ:=s.rebind(`SELECT v.canonical_price_paise,v.tariff_revision,
  v.checksum_sha256,v.billable_sides,v.total_sheets,v.storage_key,
  v.duplex,v.color_mode,v.copies
  FROM document_variants v JOIN document_uploads u ON u.upload_id=v.upload_id
  WHERE v.variant_id=? AND v.upload_id=? AND v.state='READY'
   AND v.expires_at>CURRENT_TIMESTAMP AND u.state='UPLOADED'
   AND u.expires_at>CURRENT_TIMESTAMP`)
 if s.dialect=="postgres" {variantQ+=" FOR UPDATE OF v"}
 var price,revision int64
 var hash,key,duplex,color string
 var sides,sheets,copies int
 err=tx.QueryRowContext(ctx,variantQ,in.VariantID,in.UploadID).Scan(
  &price,&revision,&hash,&sides,&sheets,&key,&duplex,&color,&copies)
 if errors.Is(err,sql.ErrNoRows) {return "",false,ErrVariantUnavailable}
 if err!=nil {return "",false,err}
 if price!=in.AcceptedPricePaise||revision!=in.TariffRevision||
  hash!=o.Command.SHA256Checksum||sides!=o.Command.TotalPages||
  sheets!=o.Command.TotalSheets||key=="" || copies<1 ||
  duplex!=string(o.Command.Duplex) || color!=strings.ToUpper(o.ColorMode) {
  return "",false,ErrCloudQuoteConflict
 }

 createdAt:=time.Now().UTC()
 expiry:=o.PINExpiresAt.UTC()
 if !expiry.After(createdAt) {return "",false,errors.New("invalid PIN expiry")}
 createdNS:=createdAt.UnixNano()
 // Reclaim expired reservation just as in the existing order path.
 _,err=tx.ExecContext(ctx,s.rebind(`DELETE FROM pin_reservations
  WHERE kiosk_id=? AND expires_at_ns<=?`),o.Command.KioskID,createdNS)
 if err!=nil {return "",false,err}
 _,err=tx.ExecContext(ctx,s.rebind(`INSERT INTO pin_reservations
  (kiosk_id,pin,order_id,expires_at_ns,created_at_ns)
  VALUES (?,?,?,?,?)`),o.Command.KioskID,o.Command.DisplayCode,o.OrderID,
   expiry.UnixNano(),createdNS)
 if err!=nil {
  lower:=strings.ToLower(err.Error())
  if strings.Contains(lower,"unique")||strings.Contains(lower,"23505")||
   strings.Contains(lower,"primary key") {return "",false,ErrPINCollision}
  return "",false,err
 }

 _,err=tx.ExecContext(ctx,s.rebind(`INSERT INTO orders
  (order_id,job_id,kiosk_id,display_code,status,price_paise,
   created_at_ns,updated_at_ns,pin_expires_at_ns,claim_token)
  VALUES (?,?,?,?,?,?,?,?,?,?)`),
  o.OrderID,o.Command.JobID,o.Command.KioskID,o.Command.DisplayCode,
  string(contracts.JobStatusPendingPayment),price,createdNS,createdNS,expiry.UnixNano(),o.ClaimToken)
 if err!=nil {return "",false,fmt.Errorf("insert cloud order: %w",err)}

 color:=strings.ToUpper(strings.TrimSpace(o.ColorMode))
 if color=="" {color="BW"}
 _,err=tx.ExecContext(ctx,s.rebind(`INSERT INTO print_configs
  (order_id,download_url,sha256_checksum,total_pages,total_sheets,
   copies,duplex,start_from_page,color_mode) VALUES (?,?,?,?,?,?,?,?,?)`),
  o.OrderID,o.Command.DownloadURL,hash,sides,sheets,
  o.Command.Copies,string(o.Command.Duplex),1,color)
 if err!=nil {return "",false,err}
 _,err=tx.ExecContext(ctx,s.rebind(`INSERT INTO order_documents
  (order_id,sequence_no,variant_id,accepted_price_paise) VALUES (?,?,?,?)`),
  o.OrderID,1,in.VariantID,price)
 if err!=nil {return "",false,err}
 _,err=tx.ExecContext(ctx,s.rebind(`INSERT INTO cloud_order_requests
  (request_id,upload_id,variant_id,order_id,kiosk_id,accepted_price_paise,tariff_revision)
  VALUES (?,?,?,?,?,?,?)`),in.RequestID,in.UploadID,in.VariantID,
  o.OrderID,o.Command.KioskID,price,in.TariffRevision)
 if err!=nil {
  // A concurrent identical submission may have inserted this request; the
  // transaction rolls back fully. Caller resolves the committed request.
  lower:=strings.ToLower(err.Error())
  if strings.Contains(lower,"unique")||strings.Contains(lower,"23505") {
   return "",false,ErrCloudOrderConflict
  }
  return "",false,err
 }
 if err=tx.Commit();err!=nil {return "",false,err}
 return o.OrderID,true,nil
}

// LookupCloudOrderRequest is used only after the caller proves the upload
// capability. It returns the immutable order's identity, not a new PIN.
func (s *Store) LookupCloudOrderRequest(ctx context.Context, requestID,uploadID,variantID,kioskID string,price,revision int64) (string,error) {
 if s==nil||s.db==nil {return "",errors.New("database unavailable")}
 var orderID string
 q:=s.rebind(`SELECT order_id FROM cloud_order_requests WHERE
  request_id=? AND upload_id=? AND variant_id=? AND kiosk_id=?
  AND accepted_price_paise=? AND tariff_revision=?`)
 err:=s.db.QueryRowContext(ctx,q,requestID,uploadID,variantID,kioskID,price,revision).Scan(&orderID)
 if errors.Is(err,sql.ErrNoRows) {return "",ErrCloudOrderConflict}
 return orderID,err
}

// Used only by the signed, authenticated existing order PDF endpoint.
func (s *Store) CloudOrderPreparedObject(ctx context.Context,orderID string) (key,checksum string,exists bool,err error) {
 if s==nil||s.db==nil {return "", "",false,errors.New("database unavailable")}
 q:=s.rebind(`SELECT v.storage_key,v.checksum_sha256
  FROM order_documents d JOIN document_variants v ON v.variant_id=d.variant_id
  WHERE d.order_id=? AND d.sequence_no=1`)
 err=s.db.QueryRowContext(ctx,q,orderID).Scan(&key,&checksum)
 if errors.Is(err,sql.ErrNoRows) {return "", "",false,nil}
 if err!=nil {return "", "",false,err}
 return key,checksum,true,nil
}
