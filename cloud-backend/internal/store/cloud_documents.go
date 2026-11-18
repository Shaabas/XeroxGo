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
    ID, TokenHash, KioskID, OriginalName, ObjectKey, ContentType string
    DeclaredBytes int64
    State string
    ExpiresAt time.Time
}

var ErrCloudUploadUnauthorized = errors.New("cloud document upload not found or unauthorized")

func (s *Store) ReserveCloudDocumentUpload(ctx context.Context, u CloudDocumentUpload) error {
    if s == nil || s.db == nil {
        return errors.New("database unavailable")
    }
    if u.ID == "" || u.TokenHash == "" || u.ObjectKey == "" || u.DeclaredBytes <= 0 {
        return errors.New("invalid cloud upload record")
    }
    q := s.rebind(`INSERT INTO document_uploads
        (upload_id, upload_token_hash, kiosk_id, original_name, object_key,
         content_type, declared_bytes, state, expires_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, 'PENDING_UPLOAD', ?)`)
    _, err := s.db.ExecContext(ctx, q, u.ID, u.TokenHash, u.KioskID,
        u.OriginalName, u.ObjectKey, u.ContentType, u.DeclaredBytes, u.ExpiresAt)
    return err
}

// AuthorizeCloudDocumentUpload checks an opaque capability and its TTL.
// It never trusts client-provided Storage object keys.
func (s *Store) AuthorizeCloudDocumentUpload(ctx context.Context, id, tokenHash string) (CloudDocumentUpload, error) {
    var u CloudDocumentUpload
    if s == nil || s.db == nil || id == "" || tokenHash == "" {
        return u, ErrCloudUploadUnauthorized
    }
    q := s.rebind(`SELECT upload_id, upload_token_hash, kiosk_id, original_name,
        object_key, content_type, declared_bytes, state, expires_at
        FROM document_uploads WHERE upload_id = ?`)
    err := s.db.QueryRowContext(ctx, q, id).Scan(&u.ID, &u.TokenHash,
        &u.KioskID, &u.OriginalName, &u.ObjectKey, &u.ContentType,
        &u.DeclaredBytes, &u.State, &u.ExpiresAt)
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
