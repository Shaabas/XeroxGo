package store

import (
 "context"
 "database/sql"
 "errors"
 "sort"
 "time"
)

// LoadReadyCloudPrintPlanTx validates and locks all source rows inside the
// CALLER'S checkout transaction. The caller must first authenticate every
// upload capability, then persist the order and task rows in this SAME tx.
// Do not use the read-only LoadReadyCloudPrintPlan result for purchasing.
//
// Locks are acquired in stable variant-ID order to reduce cross-checkout
// deadlocks. Returned tasks retain the customer's original document order.
func (s *Store) LoadReadyCloudPrintPlanTx(ctx context.Context, tx *sql.Tx,
 kiosk string, refs []CloudPreparedReference) (CloudPrintPlan, error) {
 var empty CloudPrintPlan
 if s == nil || s.db == nil || tx == nil || kiosk == "" ||
  len(refs) == 0 || len(refs) > 20 {
  return empty, errors.New("invalid transactional document batch")
 }
 seenUploads := make(map[string]bool, len(refs))
 seenVariants := make(map[string]bool, len(refs))
 for _, ref := range refs {
  if ref.UploadID == "" || ref.VariantID == "" ||
   seenUploads[ref.UploadID] || seenVariants[ref.VariantID] {
   return empty, errors.New("invalid or duplicate document reference")
  }
  seenUploads[ref.UploadID] = true
  seenVariants[ref.VariantID] = true
 }

 // Lock the tariff first, just as the single-document checkout does. All
 // selected variants must have been prepared under this active revision.
 tariffQuery := "SELECT active_revision FROM sheet_tariff_state WHERE id=1"
 if s.dialect == "postgres" { tariffQuery += " FOR UPDATE" }
 var currentRevision int64
 if err := tx.QueryRowContext(ctx, tariffQuery).Scan(&currentRevision); err != nil {
  return empty, err
 }

 indices := make([]int, len(refs))
 for i := range refs { indices[i] = i }
 sort.Slice(indices, func(i, j int) bool {
  return refs[indices[i]].VariantID < refs[indices[j]].VariantID
 })
 variants := make([]CloudDocumentVariant, len(refs))
 now := time.Now().UTC()
 query := s.rebind(`SELECT v.variant_id,v.state,v.storage_key,v.checksum_sha256,
  v.copies,v.duplex,v.color_mode,v.billable_sides,v.total_sheets,
  v.canonical_price_paise,v.tariff_revision
  FROM document_variants v
  JOIN document_uploads u ON u.upload_id=v.upload_id
  WHERE u.upload_id=? AND v.variant_id=? AND u.kiosk_id=?
   AND u.state='UPLOADED' AND v.state='READY'
   AND u.expires_at>? AND v.expires_at>?`)
 if s.dialect == "postgres" { query += " FOR UPDATE OF u,v" }
 for _, i := range indices {
  ref := refs[i]
  var variant CloudDocumentVariant
  err := tx.QueryRowContext(ctx, query,
   ref.UploadID, ref.VariantID, kiosk, now, now).
   Scan(&variant.ID, &variant.State, &variant.StorageKey,
    &variant.Checksum, &variant.Copies, &variant.Duplex,
    &variant.ColorMode, &variant.BilledSides, &variant.TotalSheets,
    &variant.PricePaise, &variant.TariffRevision)
  if errors.Is(err, sql.ErrNoRows) { return empty, ErrVariantUnavailable }
  if err != nil { return empty, err }
  if variant.TariffRevision != currentRevision {
   return empty, ErrSheetTariffRevisionConflict
  }
  variants[i] = variant
 }
 return BuildCloudPrintPlan(variants)
}
