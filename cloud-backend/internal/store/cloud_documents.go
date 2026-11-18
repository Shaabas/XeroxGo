package store

import (
    "context"
    "crypto/subtle"
    "errors"
    "fmt"
    "time"
)

// CloudDocumentUpload is the server-owned upload capability record.
// No upload token, object key or private file is exposed via the Data API.
type CloudDocumentUpload struct {
    ID, TokenHash, KioskID, OriginalName, ObjectKey, ContentType, VerifiedSHA256 string
    DeclaredBytes int64
    State string
    ExpiresAt time.Time
}

var ErrCloudUploadUnauthorized = errors.New("cloud document upload not found or unauthorized")
var ErrCloudUploadQuota = errors.New("cloud document upload hourly quota exceeded")

const cloudMaxUploadsPerKioskHour = 20
const cloudMaxDeclaredBytesPerKioskHour int64 = 100 * 1024 * 1024

func (s *Store) ReserveCloudDocumentUpload(ctx context.Context, u CloudDocumentUpload) error {
    if s == nil || s.db == nil {
        return errors.New("database unavailable")
    }
    if u.ID == "" || u.TokenHash == "" || u.ObjectKey == "" || u.DeclaredBytes <= 0 {
        return errors.New("invalid cloud upload record")
    }
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil { return err }
    defer tx.Rollback()

    // This transaction serializes quota checks per kiosk across Render
    // instances; SQLite already serializes writers. A kiosk ID is NOT an
    // authentication credential, so edge/IP quotas are still necessary.
    if s.dialect == "postgres" {
        if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "cloud-upload:"+u.KioskID); err != nil { return err }
    }
    quotaQ := s.rebind(`SELECT COUNT(*), COALESCE(SUM(declared_bytes), 0)
        FROM document_uploads WHERE kiosk_id=? AND created_at>=?`)
    var count int64
    var bytesUsed int64
    err = tx.QueryRowContext(ctx, quotaQ, u.KioskID, time.Now().UTC().Add(-time.Hour)).Scan(&count, &bytesUsed)
    if err != nil { return err }
    if count >= cloudMaxUploadsPerKioskHour ||
        bytesUsed > cloudMaxDeclaredBytesPerKioskHour-u.DeclaredBytes {
        return ErrCloudUploadQuota
    }
    q := s.rebind(`INSERT INTO document_uploads
        (upload_id, upload_token_hash, kiosk_id, original_name, object_key,
         content_type, declared_bytes, state, expires_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, 'PENDING_UPLOAD', ?)`)
    if _, err = tx.ExecContext(ctx, q, u.ID, u.TokenHash, u.KioskID,
        u.OriginalName, u.ObjectKey, u.ContentType, u.DeclaredBytes, u.ExpiresAt); err != nil { return err }
    return tx.Commit()
}

// AuthorizeCloudDocumentUpload checks an opaque capability and its TTL.
// It never trusts client-provided Storage object keys.
func (s *Store) AuthorizeCloudDocumentUpload(ctx context.Context, id, tokenHash string) (CloudDocumentUpload, error) {
    var u CloudDocumentUpload
    if s == nil || s.db == nil || id == "" || tokenHash == "" {
        return u, ErrCloudUploadUnauthorized
    }
    q := s.rebind(`SELECT upload_id, upload_token_hash, kiosk_id, original_name,
        object_key, content_type, declared_bytes, state, expires_at, COALESCE(verified_sha256,'')
        FROM document_uploads WHERE upload_id = ?`)
    err := s.db.QueryRowContext(ctx, q, id).Scan(&u.ID, &u.TokenHash,
        &u.KioskID, &u.OriginalName, &u.ObjectKey, &u.ContentType,
        &u.DeclaredBytes, &u.State, &u.ExpiresAt, &u.VerifiedSHA256)
    if err != nil || subtle.ConstantTimeCompare([]byte(u.TokenHash), []byte(tokenHash)) != 1 ||
        !time.Now().Before(u.ExpiresAt) {
        return CloudDocumentUpload{}, ErrCloudUploadUnauthorized
    }
    return u, nil
}

// Finalization can only advance PENDING_UPLOAD -> UPLOADED once.
// A second finalization can safely observe UPLOADED without rewriting a hash.
func (s *Store) CompleteCloudDocumentUpload(ctx context.Context, id string, bytes int64, checksum string) (bool, error) {
    if bytes <= 0 || len(checksum) != 64 {
        return false, errors.New("invalid verified cloud file")
    }
    q := s.rebind(`UPDATE document_uploads
        SET state = 'UPLOADED', verified_bytes = ?, verified_sha256 = ?, verified_at = CURRENT_TIMESTAMP
        WHERE upload_id = ? AND state = 'PENDING_UPLOAD' AND declared_bytes = ?`)
    res, err := s.db.ExecContext(ctx, q, bytes, checksum, id, bytes)
    if err != nil {
        return false, fmt.Errorf("complete cloud upload: %w", err)
    }
    rows, err := res.RowsAffected()
    return rows == 1, err
}
