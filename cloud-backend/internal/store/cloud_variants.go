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
 BilledSides, TotalSheets int
 PricePaise, TariffRevision int64
 ExpiresAt time.Time
}

var ErrVariantUnavailable = errors.New("prepared variant unavailable")

// AcquireCloudVariant returns an existing variant or exclusively reserves a
// new PREPARING row. The unique index is the cross-Render-instance lock.
// The caller must never process a row unless acquired is true.
func (s *Store) AcquireCloudVariant(ctx context.Context, v CloudDocumentVariant) (got CloudDocumentVariant, acquired bool, err error) {
 if s == nil || s.db == nil { return got, false, errors.New("database unavailable") }
 if v.ID == "" || v.UploadID == "" || len(v.SettingsHash) != 64 || v.NormalizerVersion == "" {
  return got, false, errors.New("invalid variant identity")
 }
 q := s.rebind(`INSERT INTO document_variants
   (variant_id, upload_id, settings_hash, normalizer_version, state, expires_at)
   VALUES (?, ?, ?, ?, 'PREPARING', ?) ON CONFLICT (upload_id, settings_hash, normalizer_version) DO NOTHING`)
 res, err := s.db.ExecContext(ctx, q, v.ID, v.UploadID, v.SettingsHash, v.NormalizerVersion, v.ExpiresAt)
 if err != nil { return got, false, fmt.Errorf("reserve variant: %w", err) }
 rows, err := res.RowsAffected()
 if err != nil { return got, false, err }
 if rows == 1 { v.State = "PREPARING"; return v, true, nil }
 got, err = s.GetCloudVariant(ctx, v.UploadID, v.SettingsHash, v.NormalizerVersion)
 return got, false, err
}

func (s *Store) GetCloudVariant(ctx context.Context, uploadID, settingsHash, version string) (CloudDocumentVariant, error) {
 var v CloudDocumentVariant
 if s == nil || s.db == nil { return v, errors.New("database unavailable") }
 q := s.rebind(`SELECT variant_id, upload_id, settings_hash, normalizer_version, state,
   COALESCE(storage_key,''), COALESCE(checksum_sha256,''),
   COALESCE(billable_sides,0), COALESCE(total_sheets,0),
   COALESCE(canonical_price_paise,0), COALESCE(tariff_revision,0), expires_at
   FROM document_variants WHERE upload_id=? AND settings_hash=? AND normalizer_version=?`)
 err := s.db.QueryRowContext(ctx, q, uploadID, settingsHash, version).Scan(
  &v.ID, &v.UploadID, &v.SettingsHash, &v.NormalizerVersion, &v.State,
  &v.StorageKey, &v.Checksum, &v.BilledSides, &v.TotalSheets, &v.PricePaise,
  &v.TariffRevision, &v.ExpiresAt)
 if errors.Is(err, sql.ErrNoRows) { return CloudDocumentVariant{}, ErrVariantUnavailable }
 return v, err
}

// PublishCloudVariant is a one-way CAS. A variant is never overwritten.
// DB write failure after Storage upload leaves only an orphan private object,
// NOT an order containing a partially prepared file.
func (s *Store) PublishCloudVariant(ctx context.Context, v CloudDocumentVariant) (bool, error) {
 if s == nil || s.db == nil { return false, errors.New("database unavailable") }
 if len(v.Checksum) != 64 || v.StorageKey == "" || v.BilledSides < 1 || v.TotalSheets < 1 ||
  v.PricePaise < 1 || v.TariffRevision < 1 { return false, errors.New("invalid canonical variant") }
 q := s.rebind(`UPDATE document_variants SET state='READY', storage_key=?,
  checksum_sha256=?, billable_sides=?, total_sheets=?, canonical_price_paise=?, tariff_revision=?
  WHERE variant_id=? AND state='PREPARING' AND expires_at > CURRENT_TIMESTAMP`)
 res, err := s.db.ExecContext(ctx, q, v.StorageKey, v.Checksum, v.BilledSides,
  v.TotalSheets, v.PricePaise, v.TariffRevision, v.ID)
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
