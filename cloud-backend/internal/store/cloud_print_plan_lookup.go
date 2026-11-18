package store

import (
 "context"
 "database/sql"
 "errors"
 "time"
)

// CloudPreparedReference is a pair of opaque IDs supplied during checkout.
// The server must separately validate each upload capability before invoking
// this read-only routine; the kiosk ID is not an authentication credential.
type CloudPreparedReference struct {
 UploadID string
 VariantID string
}

// LoadReadyCloudPrintPlan reads authoritative, unexpired variant metadata.
// It does NOT accept client-supplied totals, duplex, color, or Storage paths.
// Only a future single transaction may create an order from this result.
func (s *Store) LoadReadyCloudPrintPlan(ctx context.Context,kiosk string,refs []CloudPreparedReference)(CloudPrintPlan,error){
 var empty CloudPrintPlan
 if s==nil||s.db==nil{return empty,errors.New("database unavailable")}
 if kiosk==""||len(refs)<1||len(refs)>20{return empty,errors.New("invalid document batch")}
 variants:=make([]CloudDocumentVariant,0,len(refs))
 seen:=make(map[string]bool,len(refs))
 for _,ref:=range refs{
  if ref.UploadID==""||ref.VariantID==""||seen[ref.VariantID]{
   return empty,errors.New("invalid or duplicate document reference")
  }
  seen[ref.VariantID]=true
  q:=s.rebind(`SELECT v.variant_id,v.state,v.storage_key,v.checksum_sha256,
   v.copies,v.duplex,v.color_mode,v.billable_sides,v.total_sheets,
   v.canonical_price_paise,v.tariff_revision
   FROM document_variants v JOIN document_uploads u ON u.upload_id=v.upload_id
   WHERE u.upload_id=? AND v.variant_id=? AND u.kiosk_id=?
    AND u.state='UPLOADED' AND v.state='READY'
    AND u.expires_at>? AND v.expires_at>?`)
  now:=time.Now().UTC()
  var v CloudDocumentVariant
  err:=s.db.QueryRowContext(ctx,q,ref.UploadID,ref.VariantID,kiosk,now,now).
   Scan(&v.ID,&v.State,&v.StorageKey,&v.Checksum,&v.Copies,
    &v.Duplex,&v.ColorMode,&v.BilledSides,&v.TotalSheets,&v.PricePaise,&v.TariffRevision)
  if errors.Is(err,sql.ErrNoRows){return empty,ErrVariantUnavailable}
  if err!=nil{return empty,err}
  variants=append(variants,v)
 }
 return BuildCloudPrintPlan(variants)
}
