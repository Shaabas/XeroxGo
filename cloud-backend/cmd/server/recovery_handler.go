package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"xeroxgo/cloud-backend/internal/pdf"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

func generateSignedRecoveryURL(baseURL, recoveryID string, secret []byte, ttl time.Duration) string {
	expires := time.Now().Add(ttl).Unix()
	msg := fmt.Sprintf("%s\n%d", recoveryID, expires)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(msg))
	sig := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("%s/api/recovery/%s/pdf?expires=%d&sig=%s", baseURL, recoveryID, expires, sig)
}

func verifySignedRecoveryURL(recoveryID string, expires int64, sig string, secret []byte) bool {
	if time.Now().Unix() > expires {
		return false
	}
	msg := fmt.Sprintf("%s\n%d", recoveryID, expires)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(msg))
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(strings.ToLower(sig)), []byte(strings.ToLower(expected)))
}

type recoveryRequestBody struct {
	RecoveryID             string `json:"recovery_id"`
	ConfirmedSheetsPrinted int    `json:"confirmed_sheets_printed"`
	OutputConfirmed        bool   `json:"output_confirmed"`
	Reason                 string `json:"reason,omitempty"`
}

func (s *appServer) handleRecoverJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Route format: /api/k/{kiosk_id}/jobs/{job_id}/recover
	prefix := "/api/k/"
	suffix := "/recover"
	if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, suffix) {
		http.NotFound(w, r)
		return
	}

	middle := strings.TrimPrefix(r.URL.Path, prefix)
	middle = strings.TrimSuffix(middle, suffix)
	parts := strings.Split(middle, "/jobs/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		http.Error(w, "invalid recovery path: expected /api/k/{kiosk_id}/jobs/{job_id}/recover", http.StatusBadRequest)
		return
	}
	kioskID := strings.TrimSpace(parts[0])
	jobID := strings.TrimSpace(parts[1])

	// Kiosk Authorization
	if len(s.cfg.kioskSecret) > 0 {
		var token string
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
		if token == "" {
			token = strings.TrimSpace(r.URL.Query().Get("token"))
		}
		if !hmac.Equal([]byte(token), s.cfg.kioskSecret) {
			http.Error(w, "unauthorized kiosk", http.StatusUnauthorized)
			return
		}
	}

	// Look up order
	order, err := s.db.GetOrderByJobID(r.Context(), jobID)
	if err != nil {
		http.Error(w, "failed to query order", http.StatusInternalServerError)
		return
	}
	if order == nil {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	if order.Command.KioskID != kioskID {
		http.Error(w, "kiosk id mismatch", http.StatusForbidden)
		return
	}
	if order.Status == contracts.JobStatusPendingPayment {
		http.Error(w, "cannot recover unpaid job", http.StatusBadRequest)
		return
	}
	if order.Status == contracts.JobStatusCompleted {
		http.Error(w, "job already completed", http.StatusConflict)
		return
	}

	// Parse request body
	var req recoveryRequestBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024*1024)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.RecoveryID = strings.TrimSpace(req.RecoveryID)
	if req.RecoveryID == "" {
		http.Error(w, "recovery_id is required", http.StatusBadRequest)
		return
	}

	// Constraint 10: Never automatically resume if completed physical output is uncertain.
	// Require explicit reconciliation or confirmation before creating a continuation job.
	if !req.OutputConfirmed {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": "physical output is uncertain; explicit reconciliation required",
		})
		return
	}

	if req.ConfirmedSheetsPrinted < 0 || req.ConfirmedSheetsPrinted > order.Command.TotalSheets {
		http.Error(w, fmt.Sprintf("invalid confirmed_sheets_printed: must be between 0 and %d", order.Command.TotalSheets), http.StatusBadRequest)
		return
	}

	if req.ConfirmedSheetsPrinted == order.Command.TotalSheets {
		// All physical sheets confirmed delivered. Mark job completed.
		_ = s.db.UpdateOrderStatus(r.Context(), order.OrderID, contracts.JobStatusCompleted)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  string(contracts.JobStatusCompleted),
			"message": "all sheets already printed",
			"job_id":  jobID,
		})
		return
	}

	// Idempotency check: return existing continuation command if already created
	existingRec, err := s.db.GetRecoveryRecord(r.Context(), req.RecoveryID)
	if err != nil {
		http.Error(w, "failed to check recovery status", http.StatusInternalServerError)
		return
	}
	if existingRec != nil {
		continuationJobID := fmt.Sprintf("%s-rec%d", jobID, existingRec.SequenceID)
		contCmd := contracts.PrintJobCommand{
			JobID:          continuationJobID,
			KioskID:        order.Command.KioskID,
			DisplayCode:    order.Command.DisplayCode,
			DownloadURL:    existingRec.DownloadURL,
			SHA256Checksum: existingRec.SHA256Checksum,
			TotalPages:     existingRec.RemainingPages,
			TotalSheets:    existingRec.RemainingSheets,
			Copies:         1,
			Duplex:         order.Command.Duplex,
			StartFromPage:  1,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"recovery_id":      existingRec.RecoveryID,
			"sequence_id":      existingRec.SequenceID,
			"job_id":           continuationJobID,
			"remaining_pages":  existingRec.RemainingPages,
			"remaining_sheets": existingRec.RemainingSheets,
			"status":           existingRec.Status,
			"command":          contCmd,
		})
		return
	}

	// Next durable sequence ID for this job
	lastSeq, err := s.db.GetLatestRecoverySequence(r.Context(), jobID)
	if err != nil {
		http.Error(w, "failed to determine recovery sequence", http.StatusInternalServerError)
		return
	}
	seqID := lastSeq + 1
	continuationJobID := fmt.Sprintf("%s-rec%d", jobID, seqID)

	h := sha256.Sum256([]byte(order.OrderID))
	originalPDFPath := filepath.Join(s.cfg.jobsDir, hex.EncodeToString(h[:])+".pdf")
	if _, err := os.Stat(originalPDFPath); err != nil {
		http.Error(w, "original print artifact not found", http.StatusInternalServerError)
		return
	}

	continuationPDFName := fmt.Sprintf("rec_%s_%d.pdf", jobID, seqID)
	continuationPDFPath := filepath.Join(s.cfg.jobsDir, continuationPDFName)

	contRes, err := pdf.GenerateContinuationPDF(
		r.Context(),
		originalPDFPath,
		continuationPDFPath,
		order.Command.TotalPages,
		order.Command.TotalSheets,
		order.Command.Copies,
		order.Command.Duplex,
		req.ConfirmedSheetsPrinted,
	)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to generate continuation PDF: %v", err), http.StatusInternalServerError)
		return
	}

	baseURL := os.Getenv("XEROXGO_PUBLIC_URL")
	if baseURL == "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		baseURL = fmt.Sprintf("%s://%s", scheme, r.Host)
	}
	baseURL = strings.TrimRight(baseURL, "/")
	downloadURL := generateSignedRecoveryURL(baseURL, req.RecoveryID, s.cfg.downloadSecret, 24*time.Hour)

	rec := store.RecoveryRecord{
		RecoveryID:             req.RecoveryID,
		JobID:                  jobID,
		KioskID:                kioskID,
		SequenceID:             seqID,
		ConfirmedSheetsPrinted: req.ConfirmedSheetsPrinted,
		RemainingPages:         contRes.RemainingPages,
		RemainingSheets:        contRes.RemainingSheets,
		DownloadURL:            downloadURL,
		SHA256Checksum:         contRes.SHA256Checksum,
		Status:                 "CREATED",
		CreatedAt:              time.Now().UTC(),
		UpdatedAt:              time.Now().UTC(),
	}
	if err := s.db.CreateRecoveryRecord(r.Context(), rec); err != nil {
		_ = os.Remove(continuationPDFPath)
		http.Error(w, "failed to save recovery record", http.StatusInternalServerError)
		return
	}

	contCmd := contracts.PrintJobCommand{
		JobID:          continuationJobID,
		KioskID:        order.Command.KioskID,
		DisplayCode:    order.Command.DisplayCode,
		DownloadURL:    downloadURL,
		SHA256Checksum: contRes.SHA256Checksum,
		TotalPages:     contRes.RemainingPages,
		TotalSheets:    contRes.RemainingSheets,
		Copies:         1,
		Duplex:         order.Command.Duplex,
		StartFromPage:  1,
	}

	// Dispatch to connected kiosk if available
	if s.hub != nil {
		_ = s.hub.SendJob(r.Context(), contCmd)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"recovery_id":      req.RecoveryID,
		"sequence_id":      seqID,
		"job_id":           continuationJobID,
		"remaining_pages":  contRes.RemainingPages,
		"remaining_sheets": contRes.RemainingSheets,
		"status":           "CREATED",
		"command":          contCmd,
	})
}

func (s *appServer) handleDownloadRecoveryPDF(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	prefix := "/api/recovery/"
	suffix := "/pdf"
	if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, suffix) {
		http.NotFound(w, r)
		return
	}
	recoveryID := strings.TrimPrefix(r.URL.Path, prefix)
	recoveryID = strings.TrimSuffix(recoveryID, suffix)
	recoveryID = strings.TrimSpace(recoveryID)
	if recoveryID == "" {
		http.Error(w, "recovery id required", http.StatusBadRequest)
		return
	}

	expiresStr := r.URL.Query().Get("expires")
	sig := r.URL.Query().Get("sig")
	if expiresStr == "" || sig == "" {
		http.Error(w, "missing authentication signature", http.StatusUnauthorized)
		return
	}

	expires, err := strconv.ParseInt(expiresStr, 10, 64)
	if err != nil || !verifySignedRecoveryURL(recoveryID, expires, sig, s.cfg.downloadSecret) {
		http.Error(w, "invalid or expired download URL", http.StatusUnauthorized)
		return
	}

	rec, err := s.db.GetRecoveryRecord(r.Context(), recoveryID)
	if err != nil || rec == nil {
		http.Error(w, "recovery record not found", http.StatusNotFound)
		return
	}

	pdfPath := filepath.Join(s.cfg.jobsDir, fmt.Sprintf("rec_%s_%d.pdf", rec.JobID, rec.SequenceID))
	f, err := os.Open(pdfPath)
	if err != nil {
		http.Error(w, "continuation artifact not found", http.StatusNotFound)
		return
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		http.Error(w, "failed to verify continuation artifact integrity", http.StatusInternalServerError)
		return
	}
	calcSHA := hex.EncodeToString(hasher.Sum(nil))
	if calcSHA != strings.ToLower(rec.SHA256Checksum) {
		http.Error(w, "artifact integrity check failed", http.StatusInternalServerError)
		return
	}

	stat, _ := f.Stat()
	if stat != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	}
	w.Header().Set("Content-Type", "application/pdf")
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "seek error", http.StatusInternalServerError)
		return
	}
	_, _ = io.Copy(w, f)
}
