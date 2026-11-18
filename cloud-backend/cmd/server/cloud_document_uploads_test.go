package main

import (
    "context"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
)

func TestCloudUploadRequestValidation(t *testing.T) {
    good := cloudUploadInitRequest{KioskID: "demo", Filename: "document.pdf",
        ContentType: "application/pdf", SizeBytes: 2048}
    if err := validCloudUploadRequest(good); err != nil { t.Fatal(err) }
    invalid := []cloudUploadInitRequest{
        {KioskID: "demo", Filename: "../private.pdf", ContentType: "application/pdf", SizeBytes: 2048},
        {KioskID: "demo", Filename: "notes.docx", ContentType: "application/pdf", SizeBytes: 2048},
        {KioskID: "demo", Filename: "document.pdf", ContentType: "application/pdf", SizeBytes: 0},
        {KioskID: "demo", Filename: "document.pdf", ContentType: "application/pdf", SizeBytes: cloudDocumentMaxBytes+1},
        {KioskID: "../demo", Filename: "document.pdf", ContentType: "application/pdf", SizeBytes: 2048},
        {KioskID: "demo", Filename: "document.pdf", ContentType: "text/plain", SizeBytes: 2048},
    }
    for i, item := range invalid {
        if validCloudUploadRequest(item) == nil { t.Errorf("case %d incorrectly accepted", i) }
    }
}

func TestCloudSignedURLScope(t *testing.T) {
    base := "https://example.supabase.co"
    url, err := cloudSignedUploadURL(base, "/object/upload/sign/xeroxgo-originals/uploads/abc/original.pdf?token=test")
    if err != nil { t.Fatal(err) }
    if url != base+"/storage/v1/object/upload/sign/xeroxgo-originals/uploads/abc/original.pdf?token=test" {
        t.Fatalf("unexpected URL %s", url)
    }
    for _, unsafe := range []string{
        "https://evil.test/object/upload/sign/xeroxgo-originals/a",
        "//evil.test/object/upload/sign/xeroxgo-originals/a",
        "/object/upload/sign/xeroxgo-prepared/orders/a",
        "/storage/v1/object/authenticated/xeroxgo-originals/a",
        "/object/upload/sign/xeroxgo-originals/a#fragment",
    } {
        if _, err := cloudSignedUploadURL(base, unsafe); err == nil {
            t.Errorf("unsafe signed URL accepted: %q", unsafe)
        }
    }
}

func TestCloudUploadsFeatureDisabled(t *testing.T) {
    t.Setenv("XEROXGO_CLOUD_UPLOADS_ENABLED", "0")
    srv := &appServer{}
    for _, handler := range []http.HandlerFunc{srv.handleCloudUploadInit, srv.handleCloudUploadComplete} {
        req := httptest.NewRequest(http.MethodPost, "/api/documents/uploads/init", strings.NewReader("{}"))
        rec := httptest.NewRecorder()
        handler(rec, req)
        if rec.Code != http.StatusNotFound { t.Errorf("disabled endpoint status=%d", rec.Code) }
    }
}

func TestCloudVerifyPrivatePDF(t *testing.T) {
    contents := "%PDF-1.7\nverifiable test PDF"
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("apikey") != "secret" {
            w.WriteHeader(http.StatusForbidden); return
        }
        if !strings.Contains(r.URL.Path, "/storage/v1/object/authenticated/xeroxgo-originals/uploads/abc/original.pdf") {
            w.WriteHeader(http.StatusNotFound); return
        }
        _, _ = w.Write([]byte(contents))
    }))
    defer srv.Close()
    h, err := verifyPrivateCloudPDF(context.Background(), srv.URL, "secret", "uploads/abc/original.pdf", int64(len(contents)))
    if err != nil || len(h) != 64 { t.Fatalf("verification failed: hash=%q err=%v", h, err) }
    if _, err = verifyPrivateCloudPDF(context.Background(), srv.URL, "secret", "uploads/abc/original.pdf", 2); err == nil {
        t.Fatal("size mismatch accepted")
    }
}
