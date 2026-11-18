package store

import (
 "context"
 "database/sql"
 "errors"
 "fmt"
 "time"
)

// CloudDocumentVariant is an immutable canonical output for one verified
// upload and an exact set of rendering options.
type CloudDocumentVariant struct {
 ID, UploadID, SettingsHash, NormalizerVersion, StorageKey, Checksum, State string
 Duplex, ColorMode string
 Copies, BilledSides, TotalSheets int
 PricePaise, TariffRevision int64
 ExpiresAt time.Time
 LeaseExpiresAt time.Time
}

var ErrVariantUnavailable = errors.New("prepared variant unavailable")

// AcquireCloudVariant atomically reserves or recovers a preparation lease.
// READY output is never modified. After a crash, an expired PREPARING lease
// (or FAILED attempt) can be replaced with a new variant identity; the old
// worker cannot publish after replacement because its variant ID no longer
// matches. Existing Storage objects under the old ID require orphan cleanup.
func (s *Store) AcquireCloudVariant(ctx context.Context, v CloudDocumentVariant) (got CloudDocumentVariant, acquired bool, err error) {
 if s==nil||s.db==nil {return got,false,errors.New("database unavailable")}
 if v.ID==""||v.UploadID==""||len(v.SettingsHash)!=64||v.NormalizerVersion=="" {
  return got,false,errors.New("invalid variant identity")
 }
 now:=time.Now().UTC()
 leaseUntil:=now.Add(10*time.Minute)
 q:=s.rebind(`INSERT INTO document_variants
  (variant_id,upload_id,settings_hash,normalizer_version,state,expires_at,lease_expires_at)
  VALUES (?,?,?,?,'PREPARING',?,?)
  ON CONFLICT (upload_id,settings_hash,normalizer_version) DO NOTHING`)
 res,err:=s.db.ExecContext(ctx,q,v.ID,v.UploadID,v.SettingsHash,v.NormalizerVersion,v.ExpiresAt,leaseUntil)
 if err!=nil {return got,false,fmt.Errorf("reserve variant: %w",err)}
 count,err:=res.RowsAffected()
 if err!=nil{return got,false,err}
 if count==1 {v.State="PREPARING";return v,true,nil}

 // An abandoned reservation is reclaimable, but a live lease or READY
 // variant is never stolen. The conditional UPDATE is concurrency-safe.
 reclaim:=s.rebind(`UPDATE document_variants
   SET variant_id=?,state='PREPARING',error_code=NULL,lease_expires_at=?,expires_at=?,
       storage_key=NULL,checksum_sha256=NULL,billable_sides=NULL,total_sheets=NULL,
       canonical_price_paise=NULL,tariff_revision=NULL,copies=NULL,duplex=NULL,color_mode=NULL
   WHERE upload_id=? AND settings_hash=? AND normalizer_version=?
     AND (state='FAILED' OR
       (state='PREPARING' AND (lease_expires_at IS NULL OR lease_expires_at<=?)))
     AND NOT EXISTS (SELECT 1 FROM order_documents od
       WHERE od.variant_id=document_variants.variant_id)`)
 res,err=s.db.ExecContext(ctx,reclaim,v.ID,leaseUntil,v.ExpiresAt,
  v.UploadID,v.SettingsHash,v.NormalizerVersion,now)
 if err!=nil{return got,false,fmt.Errorf("recover variant: %w",err)}
 count,err=res.RowsAffected()
 if err!=nil{return got,false,err}
 if count==1{v.State="PREPARING";return v,true,nil}
 got,err=s.GetCloudVariant(ctx,v.UploadID,v.SettingsHash,v.NormalizerVersion)
 return got,false,err
}

func (s *Store) GetCloudVariant(ctx context.Context, uploadID, settingsHash, version string) (CloudDocumentVariant, error) {
 var v CloudDocumentVariant
 var lease sql.NullTime
 if s == nil || s.db == nil { return v, errors.New("database unavailable") }
 q := s.rebind(`SELECT variant_id, upload_id, settings_hash, normalizer_version, state,
   COALESCE(storage_key,''), COALESCE(checksum_sha256,''),
   COALESCE(billable_sides,0), COALESCE(total_sheets,0),
   COALESCE(canonical_price_paise,0), COALESCE(tariff_revision,0), expires_at,
   COALESCE(copies,0), COALESCE(duplex,''), COALESCE(color_mode,''),
   lease_expires_at
   FROM document_variants WHERE upload_id=? AND settings_hash=? AND normalizer_version=?`)
 err := s.db.QueryRowContext(ctx, q, uploadID, settingsHash, version).Scan(
  &v.ID, &v.UploadID, &v.SettingsHash, &v.NormalizerVersion, &v.State,
  &v.StorageKey, &v.Checksum, &v.BilledSides, &v.TotalSheets, &v.PricePaise,
  &v.TariffRevision, &v.ExpiresAt, &v.Copies, &v.Duplex, &v.ColorMode, &lease)
 if lease.Valid {v.LeaseExpiresAt=lease.Time}
 if errors.Is(err, sql.ErrNoRows) { return CloudDocumentVariant{}, ErrVariantUnavailable }
 return v, err
}

// PublishCloudVariant is a one-way CAS. A variant is never overwritten.
// DB write failure after Storage upload leaves only an orphan private object,
// NOT an order containing a partially prepared file.
func (s *Store) PublishCloudVariant(ctx context.Context, v CloudDocumentVariant) (bool, error) {
 if s == nil || s.db == nil { return false, errors.New("database unavailable") }
 if len(v.Checksum) != 64 || v.StorageKey == "" || v.BilledSides < 1 || v.TotalSheets < 1 ||
  v.PricePaise < 1 || v.TariffRevision < 1 || v.Copies < 1 || v.Duplex == "" || v.ColorMode == "" { return false, errors.New("invalid canonical variant") }
 q := s.rebind(`UPDATE document_variants SET state='READY', storage_key=?,
  checksum_sha256=?, billable_sides=?, total_sheets=?, canonical_price_paise=?, tariff_revision=?, copies=?, duplex=?, color_mode=?
  WHERE variant_id=? AND state='PREPARING' AND expires_at > CURRENT_TIMESTAMP`)
 res, err := s.db.ExecContext(ctx, q, v.StorageKey, v.Checksum, v.BilledSides,
  v.TotalSheets, v.PricePaise, v.TariffRevision, v.Copies, v.Duplex, v.ColorMode, v.ID)
 if err != nil { return false, err }
 n, err := res.RowsAffected()
 return n == 1, err
}

func (s *Store) FailCloudVariant(ctx context.Context, id string) error {
 if s == nil || s.db == nil { return errors.New("database unavailable") }
 q := s.rebind("UPDATE document_variants SET state='FAILED', error_code='PREPARATION_FAILED' WHERE variant_id=? AND state='PREPARING'")
 _, err := s.db.ExecContext(ctx, q, id)
 return err
}

func (s *Store) GetCloudVariantByID(ctx context.Context, uploadID, variantID string) (CloudDocumentVariant, error) {
 var v CloudDocumentVariant
 var lease sql.NullTime
 if s == nil || s.db == nil { return v, errors.New("database unavailable") }
 if uploadID == "" || variantID == "" { return v, ErrVariantUnavailable }
 q := s.rebind(`SELECT variant_id, upload_id, settings_hash, normalizer_version, state,
   COALESCE(storage_key,''), COALESCE(checksum_sha256,''),
   COALESCE(billable_sides,0), COALESCE(total_sheets,0),
   COALESCE(canonical_price_paise,0), COALESCE(tariff_revision,0), expires_at,
   COALESCE(copies,0), COALESCE(duplex,''), COALESCE(color_mode,''),
   lease_expires_at
   FROM document_variants WHERE upload_id=? AND variant_id=?`)
 err := s.db.QueryRowContext(ctx, q, uploadID, variantID).Scan(
  &v.ID, &v.UploadID, &v.SettingsHash, &v.NormalizerVersion, &v.State,
  &v.StorageKey, &v.Checksum, &v.BilledSides, &v.TotalSheets, &v.PricePaise,
  &v.TariffRevision, &v.ExpiresAt, &v.Copies, &v.Duplex, &v.ColorMode, &lease)
 if lease.Valid {v.LeaseExpiresAt=lease.Time}
 if errors.Is(err, sql.ErrNoRows) { return CloudDocumentVariant{}, ErrVariantUnavailable }
 return v, err
}
