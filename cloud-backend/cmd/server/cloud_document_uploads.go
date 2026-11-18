package main

// Opt-in foundation for XG-CLOUD-DOCS-V2. No existing checkout route changes.
// This implementation intentionally only accepts PDFs until Office conversion
// and canonical prepared-variant lifecycle have completed their own gates.

import (
    "context"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "net/http"
    "net/url"
    "os"
    "path"
    "strings"
    "time"

    "xeroxgo/cloud-backend/internal/store"
)

const cloudDocumentMaxBytes = 25 * 1024 * 1024
const cloudOriginalBucket = "xeroxgo-originals"

type cloudUploadInitRequest struct {
    KioskID string `json:"kiosk_id"`
    Filename string `json:"filename"`
    ContentType string `json:"content_type"`
    SizeBytes int64 `json:"size_bytes"`
}

type cloudUploadCompleteRequest struct {
    UploadID string `json:"upload_id"`
    UploadToken string `json:"upload_token"`
}

func cloudUploadsEnabled() bool {
    return os.Getenv("XEROXGO_CLOUD_UPLOADS_ENABLED") == "1"
}

func validCloudUploadRequest(r cloudUploadInitRequest) error {
    if len(r.KioskID) < 1 || len(r.KioskID) > 100 ||
        strings.ContainsAny(r.KioskID, "/\\?&#") {
        return errors.New("invalid kiosk id")
    }
    if r.Filename == "" || len(r.Filename) > 200 ||
        strings.ContainsAny(r.Filename, "/\\\x00") ||
        strings.ToLower(path.Ext(r.Filename)) != ".pdf" {
        return errors.New("only PDF uploads are supported in this foundation")
    }
    if r.ContentType != "application/pdf" {
        return errors.New("content type must be application/pdf")
    }
    if r.SizeBytes <= 0 || r.SizeBytes > cloudDocumentMaxBytes {
        return errors.New("file size outside the supported 25 MiB limit")
    }
    return nil
}

func cloudStorageConfig() (string, string, error) {
    base := strings.TrimRight(strings.TrimSpace(os.Getenv("SUPABASE_URL")), "/")
    secret := strings.TrimSpace(os.Getenv("SUPABASE_SERVICE_ROLE_KEY"))
    u, err := url.Parse(base)
    if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
        u.RawQuery != "" || u.Fragment != "" || u.Path != "" || secret == "" {
        return "", "", errors.New("private Supabase Storage is not configured")
    }
    return base, secret, nil
}

func supabaseStorageRequest(ctx context.Context, method, endpoint, secret string, body io.Reader) (*http.Response, error) {
    req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
    if err != nil { return nil, err }
    req.Header.Set("Authorization", "Bearer "+secret)
    req.Header.Set("apikey", secret)
    if body != nil { req.Header.Set("Content-Type", "application/json") }
    client := &http.Client{Timeout: 75 * time.Second}
    return client.Do(req)
}

// Resolve only Supabase's own relative signed upload path; never follow an
// arbitrary URL supplied by an upstream response.
func cloudSignedUploadURL(base, raw string) (string, error) {
    u, err := url.Parse(raw)
    if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" { return "", errors.New("invalid signed upload URL") }
    if strings.HasPrefix(u.Path, "/object/upload/sign/"+cloudOriginalBucket+"/") {
        return base + "/storage/v1" + raw, nil
    }
    if strings.HasPrefix(u.Path, "/storage/v1/object/upload/sign/"+cloudOriginalBucket+"/") {
        return base + raw, nil
    }
    return "", errors.New("unexpected signed upload URL scope")
}

func (s *appServer) handleCloudUploadInit(w http.ResponseWriter, r *http.Request) {
    if !cloudUploadsEnabled() { http.NotFound(w, r); return }
    if r.Method != http.MethodPost { http.Error(w, "method not allowed", http.StatusMethodNotAllowed); return }
    base, secret, err := cloudStorageConfig()
    if err != nil { http.Error(w, "cloud storage unavailable", http.StatusServiceUnavailable); return }
    r.Body = http.MaxBytesReader(w, r.Body, 4096)
    var input cloudUploadInitRequest
    d := json.NewDecoder(r.Body)
    d.DisallowUnknownFields()
    if d.Decode(&input) != nil || validCloudUploadRequest(input) != nil {
        http.Error(w, "invalid upload metadata", http.StatusBadRequest)
        return
    }
    id, err := randomHex(16)
    if err != nil { http.Error(w, "unable to allocate upload", 500); return }
    token, err := randomHex(32)
    if err != nil { http.Error(w, "unable to allocate upload", 500); return }
    hash := sha256.Sum256([]byte(token))
    objectKey := "uploads/" + id + "/original.pdf"
    item := store.CloudDocumentUpload{
        ID: id, TokenHash: hex.EncodeToString(hash[:]), KioskID: input.KioskID,
        OriginalName: input.Filename, ObjectKey: objectKey, ContentType: input.ContentType,
        DeclaredBytes: input.SizeBytes, State: "PENDING_UPLOAD",
        ExpiresAt: time.Now().Add(2*time.Hour),
    }
    if s.db == nil {
        http.Error(w, "document store unavailable", http.StatusServiceUnavailable)
        return
    }
    if err := s.db.ReserveCloudDocumentUpload(r.Context(), item); err != nil {
        if errors.Is(err, store.ErrCloudUploadQuota) {
            w.Header().Set("Retry-After", "3600")
            http.Error(w, "upload limit reached; try again later", http.StatusTooManyRequests)
        } else {
            http.Error(w, "cannot reserve document upload", http.StatusServiceUnavailable)
        }
        return
    }
    endpoint := base + "/storage/v1/object/upload/sign/" + cloudOriginalBucket + "/" + objectKey
    resp, err := supabaseStorageRequest(r.Context(), http.MethodPost, endpoint, secret, strings.NewReader("{}"))
    if err != nil { http.Error(w, "cannot authorize storage upload", http.StatusBadGateway); return }
    defer resp.Body.Close()
    if resp.StatusCode/100 != 2 { http.Error(w, "cannot authorize storage upload", http.StatusBadGateway); return }
    var grant struct { URL string `json:"url"`; Token string `json:"token"` }
    if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&grant) != nil {
        http.Error(w, "invalid storage authorization", http.StatusBadGateway)
        return
    }
    signedURL, err := cloudSignedUploadURL(base, grant.URL)
    if err != nil { http.Error(w, "invalid storage authorization", http.StatusBadGateway); return }
    if grant.Token != "" {
        u, parseErr := url.Parse(signedURL)
        if parseErr != nil { http.Error(w, "invalid storage authorization", http.StatusBadGateway); return }
        q := u.Query()
        if q.Get("token") == "" { q.Set("token", grant.Token); u.RawQuery = q.Encode() }
        signedURL = u.String()
    }
    w.Header().Set("Cache-Control", "no-store")
    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(map[string]any{
        "upload_id": id, "upload_token": token, "upload_url": signedURL,
        "expires_at": item.ExpiresAt.Format(time.RFC3339),
        "status": "PENDING_UPLOAD",
    })
}

// Re-read the private object from Storage; a browser's upload success event
// or uploaded metadata must never be treated as verified file contents.
func verifyPrivateCloudPDF(ctx context.Context, base, secret, key string, expected int64) (string, error) {
    endpoint := base + "/storage/v1/object/authenticated/" + cloudOriginalBucket + "/" + key
    resp, err := supabaseStorageRequest(ctx, http.MethodGet, endpoint, secret, nil)
    if err != nil { return "", err }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK { return "", fmt.Errorf("storage read status %d", resp.StatusCode) }
    if expected <= 0 || expected > cloudDocumentMaxBytes { return "", errors.New("invalid expected size") }
    h := sha256.New()
    header := make([]byte, 5)
    n, err := io.ReadFull(resp.Body, header)
    if err != nil || n != 5 || string(header) != "%PDF-" {
        return "", errors.New("invalid PDF signature")
    }
    _, _ = h.Write(header)
    remaining, err := io.Copy(h, io.LimitReader(resp.Body, cloudDocumentMaxBytes+1))
    if err != nil || remaining+5 != expected {
        return "", errors.New("cloud file size mismatch")
    }
    return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *appServer) handleCloudUploadComplete(w http.ResponseWriter, r *http.Request) {
    if !cloudUploadsEnabled() { http.NotFound(w, r); return }
    if r.Method != http.MethodPost { http.Error(w, "method not allowed", http.StatusMethodNotAllowed); return }
    base, secret, err := cloudStorageConfig()
    if err != nil { http.Error(w, "cloud storage unavailable", http.StatusServiceUnavailable); return }
    r.Body = http.MaxBytesReader(w, r.Body, 4096)
    var input cloudUploadCompleteRequest
    d := json.NewDecoder(r.Body)
    d.DisallowUnknownFields()
    if d.Decode(&input) != nil || len(input.UploadID) != 32 || len(input.UploadToken) != 64 {
        http.Error(w, "invalid upload receipt", http.StatusBadRequest)
        return
    }
    hash := sha256.Sum256([]byte(input.UploadToken))
    if s.db == nil { http.Error(w, "document store unavailable", http.StatusServiceUnavailable); return }
    item, err := s.db.AuthorizeCloudDocumentUpload(r.Context(), input.UploadID, hex.EncodeToString(hash[:]))
    if err != nil { http.Error(w, "upload not found", http.StatusNotFound); return }
    if item.State == "UPLOADED" {
        w.Header().Set("Content-Type", "application/json")
        _ = json.NewEncoder(w).Encode(map[string]string{"upload_id": item.ID, "status": "UPLOADED"})
        return
    }
    if item.State != "PENDING_UPLOAD" { http.Error(w, "invalid upload state", http.StatusConflict); return }
    checksum, err := verifyPrivateCloudPDF(r.Context(), base, secret, item.ObjectKey, item.DeclaredBytes)
    if err != nil { http.Error(w, "uploaded file could not be verified", http.StatusUnprocessableEntity); return }
    completed, err := s.db.CompleteCloudDocumentUpload(r.Context(), item.ID, item.DeclaredBytes, checksum)
    if err != nil { http.Error(w, "could not finalize upload", http.StatusInternalServerError); return }
    if !completed { http.Error(w, "upload state changed", http.StatusConflict); return }
    w.Header().Set("Cache-Control", "no-store")
    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(map[string]string{
        "upload_id": item.ID, "status": "UPLOADED", "sha256": checksum,
    })
}
