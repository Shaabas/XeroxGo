package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/big"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"xeroxgo/cloud-backend/internal/pricing"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/cloud-backend/internal/ws"
	"xeroxgo/pkg/contracts"
)

type serverConfig struct {
	addr           string
	dbPath         string
	jobsDir        string
	devMode        bool
	duplexEnabled  bool
	colorEnabled   bool
	policy         *contracts.ProductCapabilityPolicy
	downloadSecret []byte
	kioskSecret    []byte
}

type appServer struct {
	cfg        *serverConfig
	db         *store.Store
	hub        *ws.Hub
	convWake   chan struct{}
	pinLimiter *PINRateLimiter
}

func (app *appServer) policy() contracts.ProductCapabilityPolicy {
	if app != nil && app.cfg != nil && app.cfg.policy != nil {
		return *app.cfg.policy
	}
	pol := contracts.DefaultDemoProductPolicy()
	if app != nil && app.cfg != nil {
		if app.cfg.duplexEnabled {
			pol.AllowDuplex = true
		}
		if app.cfg.colorEnabled {
			pol.AllowColor = true
		}
	}
	return pol
}

func (s *appServer) getBaseURL(r *http.Request) string {
	baseURL := strings.TrimSpace(os.Getenv("XEROXGO_PUBLIC_URL"))
	if baseURL != "" {
		return strings.TrimRight(baseURL, "/")
	}
	scheme := "https"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS != nil {
		scheme = "https"
	} else if s.cfg != nil && s.cfg.devMode && (strings.HasPrefix(r.Host, "127.0.0.1") || strings.HasPrefix(r.Host, "localhost")) {
		scheme = "http"
	}
	host := r.Host
	if host == "" {
		host = "127.0.0.1:8080"
	}
	return fmt.Sprintf("%s://%s", scheme, host)
}

func randomHex(bytesLen int) (string, error) {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func randomDisplayCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(10000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%04d", n.Int64()), nil
}

func (s *appServer) generateUniquePIN(ctx context.Context, kioskID string) (string, error) {
	if s.db == nil {
		return randomDisplayCode()
	}
	for attempt := 0; attempt < 25; attempt++ {
		pin, err := randomDisplayCode()
		if err != nil {
			return "", fmt.Errorf("generate pin: %w", err)
		}
		inUse, err := s.db.IsPINInUse(ctx, kioskID, pin, time.Now())
		if err != nil {
			return "", fmt.Errorf("check pin collision: %w", err)
		}
		if !inUse {
			return pin, nil
		}
	}
	return "", errors.New("unable to generate unique PIN after max attempts")
}

// customerPINValidity defines the exact 10-minute lifetime of mobile-issued unpaid PINs.
const customerPINValidity = 10 * time.Minute

func (s *appServer) createOrderWithPINRetry(ctx context.Context, order *store.Order, maxAttempts int) error {
	return s.createOrderWithPINRetryAndPaperCheck(ctx, order, maxAttempts, -1)
}

func (s *appServer) createOrderWithPINRetryAndPaperCheck(ctx context.Context, order *store.Order, maxAttempts, availableStock int) error {
	if s.db == nil {
		return errors.New("database not initialized")
	}
	if maxAttempts < 1 {
		maxAttempts = 25
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		pin, err := randomDisplayCode()
		if err != nil {
			return fmt.Errorf("generate pin: %w", err)
		}
		order.Command.DisplayCode = pin
		if availableStock >= 0 {
			err = s.db.CreateOrderWithPaperCheck(ctx, *order, availableStock)
		} else {
			err = s.db.CreateOrder(ctx, *order)
		}
		if err == nil {
			return nil
		}
		if errors.Is(err, store.ErrPINCollision) {
			continue
		}
		return err
	}
	return errors.New("unable to reserve unique PIN after maximum collision retries")
}

func generateSignedDownloadURL(baseURL, orderID string, secret []byte, ttl time.Duration) string {
	expires := time.Now().Add(ttl).Unix()
	msg := fmt.Sprintf("%s\n%d", orderID, expires)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(msg))
	sig := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("%s/api/orders/%s/pdf?expires=%d&sig=%s", baseURL, orderID, expires, sig)
}

func verifySignedDownloadURL(orderID string, expires int64, sig string, secret []byte) bool {
	if time.Now().Unix() > expires {
		return false
	}
	msg := fmt.Sprintf("%s\n%d", orderID, expires)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(msg))
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(strings.ToLower(sig)), []byte(strings.ToLower(expected)))
}

func generateSignedJobDownloadURL(baseURL, orderID, jobID string, secret []byte, ttl time.Duration) string {
	expires := time.Now().Add(ttl).Unix()
	msg := fmt.Sprintf("%s\n%s\n%d", orderID, jobID, expires)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(msg))
	sig := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("%s/api/orders/%s/jobs/%s/pdf?expires=%d&sig=%s", baseURL, orderID, jobID, expires, sig)
}

func verifySignedJobDownloadURL(orderID, jobID string, expires int64, sig string, secret []byte) bool {
	if time.Now().Unix() > expires {
		return false
	}
	msg := fmt.Sprintf("%s\n%s\n%d", orderID, jobID, expires)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(msg))
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(strings.ToLower(sig)), []byte(strings.ToLower(expected)))
}

func linkOrCopyFile(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(dst)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		return closeErr
	}
	return nil
}

func (s *appServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
	})
}

func parseDuplex(val interface{}) contracts.DuplexMode {
	if val == nil {
		return contracts.DuplexSimplex
	}
	switch v := val.(type) {
	case bool:
		if v {
			return contracts.DuplexLongEdge
		}
		return contracts.DuplexSimplex
	case string:
		upper := strings.ToUpper(strings.TrimSpace(v))
		switch upper {
		case string(contracts.DuplexLongEdge), "TRUE", "DUPLEX", "DOUBLE", "DOUBLE-SIDED", "LONG_EDGE":
			return contracts.DuplexLongEdge
		case string(contracts.DuplexShortEdge), "SHORT_EDGE":
			return contracts.DuplexShortEdge
		default:
			return contracts.DuplexSimplex
		}
	default:
		return contracts.DuplexSimplex
	}
}

func (s *appServer) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	prefix := "/api/k/"
	suffix := "/upload"
	if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, suffix) {
		http.NotFound(w, r)
		return
	}
	kioskID := strings.TrimPrefix(r.URL.Path, prefix)
	kioskID = strings.TrimSuffix(kioskID, suffix)
	kioskID = strings.TrimSpace(kioskID)
	if kioskID == "" {
		http.Error(w, "kiosk id required", http.StatusBadRequest)
		return
	}

	const maxUploadSize = 50 * 1024 * 1024 // 50 MiB
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		http.Error(w, "request exceeds 50 MiB limit or malformed", http.StatusRequestEntityTooLarge)
		return
	}

	var fileHeaders []*multipart.FileHeader
	if r.MultipartForm != nil {
		if files, ok := r.MultipartForm.File["files"]; ok && len(files) > 0 {
			fileHeaders = files
		} else if fileList, ok := r.MultipartForm.File["file"]; ok && len(fileList) > 0 {
			fileHeaders = fileList
		}
	}
	if len(fileHeaders) == 0 {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}

	metadataRaw := strings.TrimSpace(r.FormValue("metadata"))
	var metaItems []docItemMeta
	if metadataRaw != "" {
		if err := json.Unmarshal([]byte(metadataRaw), &metaItems); err != nil {
			http.Error(w, fmt.Sprintf("invalid metadata JSON: %v", err), http.StatusBadRequest)
			return
		}
		if len(metaItems) != len(fileHeaders) {
			http.Error(w, fmt.Sprintf("metadata count (%d) must match uploaded files count (%d)", len(metaItems), len(fileHeaders)), http.StatusBadRequest)
			return
		}
	}

	pricing, err := s.db.GetPricing(r.Context())
	if err != nil {
		http.Error(w, "pricing unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	productPolicy := s.policy()
	colorEnabled := pricing.ColorEnabled && productPolicy.AllowColor
	duplexEnabled := productPolicy.AllowDuplex

	orderHex, err := randomHex(12)
	if err != nil {
		http.Error(w, "failed to generate order id", http.StatusInternalServerError)
		return
	}
	orderID := "ord_" + orderHex

	baseURL := os.Getenv("XEROXGO_PUBLIC_URL")
	if baseURL == "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		baseURL = fmt.Sprintf("%s://%s", scheme, r.Host)
	}
	baseURL = strings.TrimRight(baseURL, "/")

	var createdJobFiles []string
	var success bool
	defer func() {
		if !success {
			for _, p := range createdJobFiles {
				_ = os.Remove(p)
			}
		}
	}()

	var docs []store.OrderDocument
	var totalOrderPricePaise int64
	var totalOrderPages int
	var totalOrderSheets int
	anyDuplex := false
	anyColor := false

	isMultiFile := len(fileHeaders) > 1

	for idx, fh := range fileHeaders {
		originalFilename := fh.Filename
		f, err := fh.Open()
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to open uploaded file %q: %v", originalFilename, err), http.StatusBadRequest)
			return
		}

		docNum := idx + 1
		if !isMultiFile && len(metaItems) == 0 {
			docNum = 0
		}

		var docOpts docPrintOptions
		if idx < len(metaItems) {
			docOpts, err = parseMetadataPrintOptions(metaItems[idx], colorEnabled, duplexEnabled, docNum)
		} else {
			docOpts, err = parseFormPrintOptions(r, colorEnabled, duplexEnabled, docNum)
		}
		if err != nil {
			_ = f.Close()
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		prep, err := prepareInputPDF(r.Context(), f, originalFilename, s.cfg.jobsDir, "upload_in_*", docNum)
		_ = f.Close()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer prep.cleanup()

		jobHex, err := randomHex(12)
		if err != nil {
			http.Error(w, "failed to generate job id", http.StatusInternalServerError)
			return
		}
		jobID := "job_" + jobHex

		hJob := sha256.Sum256([]byte(jobID))
		jobPDFPath := filepath.Join(s.cfg.jobsDir, hex.EncodeToString(hJob[:])+".pdf")
		createdJobFiles = append(createdJobFiles, jobPDFPath)

		res, err := s.executeNormalizationAndPricing(r.Context(), prep.PDFPath, jobPDFPath, docOpts, pricing)
		if err != nil {
			http.Error(w, fmt.Sprintf("normalization failed for %q: %v", originalFilename, err), http.StatusBadRequest)
			return
		}

		jobDownloadURL := generateSignedJobDownloadURL(baseURL, orderID, jobID, s.cfg.downloadSecret, 24*time.Hour)

		doc := store.OrderDocument{
			JobID:            jobID,
			OrderID:          orderID,
			DocumentIndex:    idx,
			OriginalFilename: originalFilename,
			DownloadURL:      jobDownloadURL,
			SHA256Checksum:   res.NormResult.SHA256Checksum,
			TotalPages:       res.NormResult.TotalPages,
			TotalSheets:      res.NormResult.TotalSheets,
			Copies:           docOpts.Copies,
			Duplex:           docOpts.Duplex,
			ColorMode:        docOpts.ColorMode,
			PageRange:        docOpts.PageRange,
			Orientation:      docOpts.Orientation,
			PagesPerSheet:    docOpts.PagesPerSheet,
			PricePaise:       res.PricePaise,
			Status:           contracts.JobStatusPendingPayment,
		}
		docs = append(docs, doc)

		totalOrderPricePaise += res.PricePaise
		totalOrderPages += res.NormResult.TotalPages
		totalOrderSheets += res.NormResult.TotalSheets

		if docOpts.Duplex != contracts.DuplexSimplex {
			anyDuplex = true
		}
		if docOpts.ColorMode == "COLOR" {
			anyColor = true
		}
	}

	// For single-document orders, also create the order-level PDF link/file for backward compatibility with existing tests
	if !isMultiFile && len(docs) == 1 {
		hOrd := sha256.Sum256([]byte(orderID))
		orderPDFPath := filepath.Join(s.cfg.jobsDir, hex.EncodeToString(hOrd[:])+".pdf")
		createdJobFiles = append(createdJobFiles, orderPDFPath)
		hJob := sha256.Sum256([]byte(docs[0].JobID))
		jobPDFPath := filepath.Join(s.cfg.jobsDir, hex.EncodeToString(hJob[:])+".pdf")
		if err := linkOrCopyFile(jobPDFPath, orderPDFPath); err != nil {
			http.Error(w, fmt.Sprintf("failed to create order-level artifact: %v", err), http.StatusInternalServerError)
			return
		}
	}

	overallDuplex := contracts.DuplexSimplex
	if anyDuplex {
		overallDuplex = contracts.DuplexLongEdge
	}
	overallColor := "BW"
	if anyColor {
		overallColor = "COLOR"
	}

	claimToken, err := randomHex(32)
	if err != nil {
		http.Error(w, "failed to generate recovery credential", http.StatusInternalServerError)
		return
	}

	pinExpiresAt := time.Now().UTC().Add(customerPINValidity)

	cmd := contracts.PrintJobCommand{
		JobID:            docs[0].JobID,
		OrderID:          orderID,
		DocumentIndex:    0,
		TotalDocuments:   len(docs),
		OriginalFilename: docs[0].OriginalFilename,
		ColorMode:        docs[0].ColorMode,
		KioskID:          kioskID,
		DisplayCode:      "",
		DownloadURL:      docs[0].DownloadURL,
		SHA256Checksum:   docs[0].SHA256Checksum,
		TotalPages:       totalOrderPages,
		TotalSheets:      totalOrderSheets,
		Copies:           docs[0].Copies,
		Duplex:           docs[0].Duplex,
		StartFromPage:    1,
	}

	order := store.Order{
		OrderID:      orderID,
		Command:      cmd,
		Status:       contracts.JobStatusPendingPayment,
		PricePaise:   totalOrderPricePaise,
		ColorMode:    overallColor,
		TotalPages:   totalOrderPages,
		TotalSheets:  totalOrderSheets,
		Documents:    docs,
		PINExpiresAt: pinExpiresAt,
		ClaimToken:   claimToken,
	}

	// Validate fresh kiosk/printer health and estimated paper stock before issuing PIN
	availableStock, condErr := s.checkPrinterCondition(kioskID)
	if condErr != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(condErr.StatusCode)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": condErr.Message,
			"code":  condErr.Code,
		})
		return
	}

	if err := s.createOrderWithPINRetryAndPaperCheck(r.Context(), &order, 25, availableStock); err != nil {
		if errors.Is(err, store.ErrInsufficientPaper) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "not enough paper is available for this order; try a smaller order or another kiosk",
				"code":  "INSUFFICIENT_PAPER",
			})
			return
		}
		log.Printf("[UPLOAD] Failed to persist reserved order: %v", err)
		http.Error(w, "failed to reserve paper and issue PIN", http.StatusInternalServerError)
		return
	}
	success = true
	displayCode := order.Command.DisplayCode

	w.Header().Set("Content-Type", "application/json")
	resp := map[string]interface{}{
		"order_id":          orderID,
		"job_id":            docs[0].JobID,
		"display_code":      displayCode,
		"claim_token":       claimToken,
		"recovery_token":    claimToken,
		"pin_expires_at":    pinExpiresAt.Format(time.RFC3339),
		"color_mode":        overallColor,
		"total_pages":       totalOrderPages,
		"total_sheets":      totalOrderSheets,
		"copies":            docs[0].Copies,
		"duplex":            string(overallDuplex),
		"documents_count":   len(docs),
		"total_price_paise": totalOrderPricePaise,
		"status":            string(contracts.JobStatusPendingPayment),
	}
	docSummaries := make([]map[string]interface{}, len(docs))
	for i, d := range docs {
		docSummaries[i] = map[string]interface{}{
			"job_id":            d.JobID,
			"document_index":    d.DocumentIndex,
			"original_filename": d.OriginalFilename,
			"download_url":      d.DownloadURL,
			"sha256_checksum":   d.SHA256Checksum,
			"total_pages":       d.TotalPages,
			"total_sheets":      d.TotalSheets,
			"copies":            d.Copies,
			"duplex":            string(d.Duplex),
			"color_mode":        d.ColorMode,
			"price_paise":       d.PricePaise,
		}
	}
	resp["documents"] = docSummaries
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *appServer) streamPDF(w http.ResponseWriter, pdfPath, expectedChecksum string) {
	f, err := os.Open(pdfPath)
	if err != nil {
		http.Error(w, "document artifact not found", http.StatusNotFound)
		return
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		http.Error(w, "failed to verify document integrity", http.StatusInternalServerError)
		return
	}
	calcSHA := hex.EncodeToString(hasher.Sum(nil))
	if expectedChecksum != "" && strings.ToLower(calcSHA) != strings.ToLower(expectedChecksum) {
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

func (s *appServer) handleDownloadPDF(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	prefix := "/api/orders/"
	suffix := "/pdf"
	if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, suffix) {
		http.NotFound(w, r)
		return
	}

	trimmed := strings.TrimPrefix(r.URL.Path, prefix)
	trimmed = strings.TrimSuffix(trimmed, suffix)
	trimmed = strings.Trim(trimmed, "/")

	expiresStr := r.URL.Query().Get("expires")
	sig := r.URL.Query().Get("sig")
	if expiresStr == "" || sig == "" {
		http.Error(w, "missing authentication signature", http.StatusUnauthorized)
		return
	}
	expires, err := strconv.ParseInt(expiresStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid expiration", http.StatusUnauthorized)
		return
	}

	// Check if this is a per-job download route: /api/orders/{orderID}/jobs/{jobID}/pdf
	if strings.Contains(trimmed, "/jobs/") {
		parts := strings.Split(trimmed, "/jobs/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			http.Error(w, "invalid job download path", http.StatusBadRequest)
			return
		}
		orderID := strings.TrimSpace(parts[0])
		jobID := strings.TrimSpace(parts[1])

		if !verifySignedJobDownloadURL(orderID, jobID, expires, sig, s.cfg.downloadSecret) {
			http.Error(w, "invalid or expired download URL", http.StatusUnauthorized)
			return
		}

		doc, err := s.db.GetOrderDocument(r.Context(), jobID)
		if err != nil || doc == nil {
			http.Error(w, "document not found", http.StatusNotFound)
			return
		}

		// Security: prevent exposing other orders' documents
		if doc.OrderID != orderID {
			http.Error(w, "document does not belong to specified order", http.StatusForbidden)
			return
		}

		h := sha256.Sum256([]byte(jobID))
		pdfPath := filepath.Join(s.cfg.jobsDir, hex.EncodeToString(h[:])+".pdf")
		s.streamPDF(w, pdfPath, doc.SHA256Checksum)
		return
	}

	// Order-level download route: /api/orders/{orderID}/pdf
	orderID := strings.TrimSpace(trimmed)
	if orderID == "" {
		http.Error(w, "order id required", http.StatusBadRequest)
		return
	}

	if !verifySignedDownloadURL(orderID, expires, sig, s.cfg.downloadSecret) {
		http.Error(w, "invalid or expired download URL", http.StatusUnauthorized)
		return
	}

	order, err := s.db.GetOrder(r.Context(), orderID)
	if err != nil || order == nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}

	if len(order.Documents) > 1 {
		http.Error(w, "multi-document order requires per-job download URL", http.StatusBadRequest)
		return
	}

	var expectedChecksum string
	var pdfPath string

	if len(order.Documents) == 1 {
		expectedChecksum = order.Documents[0].SHA256Checksum
		jobID := order.Documents[0].JobID
		hJob := sha256.Sum256([]byte(jobID))
		candJobPath := filepath.Join(s.cfg.jobsDir, hex.EncodeToString(hJob[:])+".pdf")
		if _, err := os.Stat(candJobPath); err == nil {
			pdfPath = candJobPath
		}
	}

	if pdfPath == "" {
		expectedChecksum = order.Command.SHA256Checksum
		hOrd := sha256.Sum256([]byte(orderID))
		pdfPath = filepath.Join(s.cfg.jobsDir, hex.EncodeToString(hOrd[:])+".pdf")
	}

	s.streamPDF(w, pdfPath, expectedChecksum)
}

// handleDocumentPreview handles POST /api/documents/preview.
// It accepts a single document and print settings, validates them, runs the canonical
// normalization pipeline, and returns the print-ready PDF along with authoritative pricing
// and sheet count metadata headers. If format=json is requested, it returns JSON metadata.
func (s *appServer) handleDocumentPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	const maxUploadSize = 25 * 1024 * 1024 // 25 MiB
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		http.Error(w, "file exceeds 25 MiB limit or malformed", http.StatusRequestEntityTooLarge)
		return
	}

	var file multipart.File
	var filename string

	if f, header, err := r.FormFile("file"); err == nil {
		file = f
		filename = header.Filename
	} else if f, header, err := r.FormFile("document"); err == nil {
		file = f
		filename = header.Filename
	} else if r.MultipartForm != nil {
		if files, ok := r.MultipartForm.File["files"]; ok && len(files) > 0 {
			f, err := files[0].Open()
			if err != nil {
				http.Error(w, "failed to open uploaded file", http.StatusBadRequest)
				return
			}
			file = f
			filename = files[0].Filename
		}
	}

	if file == nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	pricing, err := s.db.GetPricing(r.Context())
	if err != nil {
		http.Error(w, "pricing unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}

	productPolicy := s.policy()
	colorEnabled := pricing.ColorEnabled && productPolicy.AllowColor
	duplexEnabled := productPolicy.AllowDuplex
	opts, err := parseFormPrintOptions(r, colorEnabled, duplexEnabled, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	prep, err := prepareInputPDF(r.Context(), file, filename, s.cfg.jobsDir, "preview_in_*", 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer prep.cleanup()

	tmpOutput, err := os.CreateTemp(s.cfg.jobsDir, "preview_out_*.pdf")
	if err != nil {
		http.Error(w, "failed to create output file", http.StatusInternalServerError)
		return
	}
	tmpOutputName := tmpOutput.Name()
	defer func() {
		_ = tmpOutput.Close()
		_ = os.Remove(tmpOutputName)
	}()
	_ = tmpOutput.Close()

	res, err := s.executeNormalizationAndPricing(r.Context(), prep.PDFPath, tmpOutputName, opts, pricing)
	if err != nil {
		http.Error(w, fmt.Sprintf("preview generation failed: %v", err), http.StatusBadRequest)
		return
	}
	format := strings.ToLower(strings.TrimSpace(r.FormValue("format")))
	if format == "" {
		format = strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	}
	isJSON := format == "json" || (format == "" && strings.Contains(r.Header.Get("Accept"), "application/json"))

	if isJSON {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"billed_sides":      res.PrintedSides,
			"total_sheets":      res.NormResult.TotalSheets,
			"copies":            opts.Copies,
			"total_price_paise": res.PricePaise,
			"normalized_pages":  res.NormResult.TotalPages,
			"padded_pages":      res.NormResult.PaddedPages,
			"sha256_checksum":   res.NormResult.SHA256Checksum,
			"color_mode":        opts.ColorMode,
			"duplex":            string(opts.Duplex),
			"orientation":       opts.Orientation,
			"pages_per_sheet":   opts.PagesPerSheet,
		})
		return
	}

	pdfBytes, err := os.ReadFile(tmpOutputName)
	if err != nil {
		http.Error(w, "failed to read generated preview", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Length", strconv.Itoa(len(pdfBytes)))
	w.Header().Set("X-Billed-Sides", strconv.Itoa(res.PrintedSides))
	w.Header().Set("X-Total-Sheets", strconv.Itoa(res.NormResult.TotalSheets))
	w.Header().Set("X-Copies", strconv.Itoa(opts.Copies))
	w.Header().Set("X-Total-Price-Paise", strconv.FormatInt(res.PricePaise, 10))
	w.Header().Set("X-Normalized-Pages", strconv.Itoa(res.NormResult.TotalPages))
	w.Header().Set("X-Padded-Pages", strconv.Itoa(res.NormResult.PaddedPages))
	w.Header().Set("X-SHA256-Checksum", res.NormResult.SHA256Checksum)
	w.Header().Set("Access-Control-Expose-Headers", "Content-Length, X-Billed-Sides, X-Total-Sheets, X-Copies, X-Total-Price-Paise, X-Normalized-Pages, X-Padded-Pages, X-SHA256-Checksum")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pdfBytes)
}

func (s *appServer) handleOrderStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	prefix := "/api/orders/"
	suffix := "/status"
	if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, suffix) {
		http.NotFound(w, r)
		return
	}
	orderID := strings.TrimPrefix(r.URL.Path, prefix)
	orderID = strings.TrimSuffix(orderID, suffix)
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		http.Error(w, "order id required", http.StatusBadRequest)
		return
	}

	order, err := s.db.GetOrder(r.Context(), orderID)
	if err != nil || order == nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}

	pagesPrinted := 0
	sheetsPrinted := 0
	currentStatus := string(order.Status)
	errorReason := ""

	progress, err := s.db.GetJobProgress(r.Context(), order.Command.JobID)
	if err == nil && progress != nil {
		pagesPrinted = progress.PagesPrinted
		sheetsPrinted = progress.SheetsPrinted
		errorReason = progress.ErrorReason
		switch progress.Status {
		case contracts.JobStatusSpooling,
			contracts.JobStatusPrinting,
			contracts.JobStatusCompleted,
			contracts.JobStatusPausedError,
			contracts.JobStatusFailedRefundable:
			currentStatus = string(progress.Status)
		}
	}

	secondsRemaining := int64(0)
	now := time.Now().UTC()
	if !order.PINExpiresAt.IsZero() {
		if !now.Before(order.PINExpiresAt) {
			secondsRemaining = 0
			if currentStatus == string(contracts.JobStatusPendingPayment) {
				currentStatus = "EXPIRED"
			}
		} else {
			rem := order.PINExpiresAt.Sub(now)
			secondsRemaining = int64(math.Ceil(rem.Seconds()))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"order_id":          order.OrderID,
		"display_code":      order.Command.DisplayCode,
		"status":            currentStatus,
		"total_pages":       order.Command.TotalPages,
		"total_sheets":      order.Command.TotalSheets,
		"copies":            order.Command.Copies,
		"duplex":            string(order.Command.Duplex),
		"pages_printed":     pagesPrinted,
		"sheets_printed":    sheetsPrinted,
		"error_reason":      errorReason,
		"price_paise":       order.PricePaise,
		"pin_expires_at":    order.PINExpiresAt.Format(time.RFC3339),
		"seconds_remaining": secondsRemaining,
	})
}

// initPricing ensures database has pricing configured before serving traffic.
// Default tariff seeding is strictly restricted to an explicitly unconfigured,
// genuinely new local SQLite database in dev mode.
// PostgreSQL databases, existing databases (even with zero orders), database connectivity failures,
// query errors, and invalid existing rows MUST fail closed and never seed or rewrite tariffs automatically.
func initPricing(ctx context.Context, s *store.Store, isPostgres bool, devMode bool, isNewSQLite bool) error {
	if s == nil {
		return fmt.Errorf("pricing initialization: store is nil")
	}

	_, err := s.GetPricing(ctx)
	if err == nil {
		// Valid pricing exists. Never overwrite existing tariffs or color_enabled.
		return nil
	}

	// Fail closed on connectivity failures, canceled contexts, or invalid/corrupted existing rows
	if !errors.Is(err, store.ErrPricingNotConfigured) {
		return fmt.Errorf("pricing check failed closed: %w", err)
	}

	// Never seed or rewrite PostgreSQL tariff records automatically, even when dev mode is enabled.
	if isPostgres || s.Dialect() != "sqlite" {
		return fmt.Errorf("pricing unconfigured in PostgreSQL database: manual operator configuration required (%w)", err)
	}

	// For SQLite, default seeding is only allowed in dev mode.
	if !devMode {
		return fmt.Errorf("pricing unconfigured in production SQLite database: %w", err)
	}

	// An existing unconfigured SQLite database must fail closed, even if it contains no orders.
	if !isNewSQLite {
		return fmt.Errorf("pricing unconfigured in existing SQLite database: manual configuration required (%w)", err)
	}

	// Allow default seeding only for an explicitly unconfigured, newly created local SQLite database.
	// Verify pricing table has 0 rows and orders table has 0 rows.
	var pricingRows int
	if countErr := s.DB().QueryRowContext(ctx, "SELECT COUNT(1) FROM pricing;").Scan(&pricingRows); countErr != nil {
		return fmt.Errorf("failed to check pricing table row count: %w", countErr)
	}
	if pricingRows > 0 {
		return fmt.Errorf("pricing table has %d rows but GetPricing failed: failing closed", pricingRows)
	}

	// An error from SELECT COUNT(1) FROM orders must cause immediate failure, not be ignored.
	var orderRows int
	if orderErr := s.DB().QueryRowContext(ctx, "SELECT COUNT(1) FROM orders;").Scan(&orderRows); orderErr != nil {
		return fmt.Errorf("failed to check orders table row count: %w", orderErr)
	}
	if orderRows > 0 {
		return fmt.Errorf("pricing missing in SQLite database with %d orders: failing closed", orderRows)
	}

	def := pricing.DefaultTariff()
	if setErr := s.SetDetailedPricing(ctx, def.BWSimplexPricePaise, def.BWDuplexPricePaise, def.ColorSimplexPricePaise, def.ColorDuplexPricePaise, def.ColorEnabled); setErr != nil {
		return fmt.Errorf("failed to initialize default dev pricing: %w", setErr)
	}
	log.Printf("[INFO] Initialized default canonical pricing for newly created local SQLite database")
	return nil
}

func resolveDownloadSecret(dlSecretEnv string, devMode bool) ([]byte, error) {
	if !devMode {
		if len(dlSecretEnv) < 16 {
			return nil, errors.New("production mode requires non-empty XEROXGO_DOWNLOAD_SECRET of at least 16 characters")
		}
		return []byte(dlSecretEnv), nil
	}
	if dlSecretEnv == "" {
		return []byte("dev_download_secret_32_bytes_long_123"), nil
	}
	return []byte(dlSecretEnv), nil
}

func resolveKioskSecret(kioskSecretEnv string, devMode bool) ([]byte, error) {
	if kioskSecretEnv == "" {
		if devMode {
			return []byte("dev_kiosk_secret_32_bytes_long_1234"), nil
		}
		return nil, errors.New("XEROXGO_KIOSK_SECRET required in production mode")
	}
	return []byte(kioskSecretEnv), nil
}

func main() {
	addrFlag := flag.String("addr", "", "Server listen address (overrides XEROXGO_CLOUD_ADDR / PORT)")
	flag.Parse()

	addr := strings.TrimSpace(*addrFlag)
	if addr == "" {
		addr = os.Getenv("XEROXGO_CLOUD_ADDR")
	}
	if addr == "" {
		if p := os.Getenv("PORT"); p != "" {
			addr = ":" + p
		} else {
			addr = ":8080"
		}
	}

	dbPath := os.Getenv("DATABASE_URL")
	if dbPath == "" {
		dbPath = os.Getenv("XEROXGO_CLOUD_DB")
	}
	if dbPath == "" {
		dbPath = "./data/xeroxgo-cloud.db"
	}

	jobsDir := os.Getenv("XEROXGO_JOBS_DIR")
	if jobsDir == "" {
		jobsDir = "./data/jobs"
	}

	devMode := os.Getenv("XEROXGO_DEV_MODE") == "1" || os.Getenv("DEV_MODE") == "1"

	dlSecretBytes, err := resolveDownloadSecret(os.Getenv("XEROXGO_DOWNLOAD_SECRET"), devMode)
	if err != nil {
		log.Fatalf("Fatal: %v", err)
	}

	isPostgres := strings.HasPrefix(dbPath, "postgres://") || strings.HasPrefix(dbPath, "postgresql://")
	isNewSQLite := false
	if !isPostgres {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
			log.Fatalf("failed to create db directory: %v", err)
		}
		if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
			isNewSQLite = true
		}
	}
	if err := os.MkdirAll(jobsDir, 0700); err != nil {
		log.Fatalf("failed to create jobs directory: %v", err)
	}

	storeDB, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("failed to open cloud database at %s: %v", dbPath, err)
	}
	defer storeDB.Close()

	ctx := context.Background()
	if err := initPricing(ctx, storeDB, isPostgres, devMode, isNewSQLite); err != nil {
		log.Fatalf("failed to initialize pricing: %v", err)
	}

	// Initialize default promo ads & bootstrap admin user
	bootstrapCtx, bootstrapCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer bootstrapCancel()
	if err := storeDB.SeedDefaultAds(bootstrapCtx); err != nil {
		log.Printf("[WARN] Failed to seed default ads: %v", err)
	}
	adminUser := os.Getenv("XEROXGO_ADMIN_USER")
	if adminUser == "" {
		adminUser = "admin"
	}
	adminPass := os.Getenv("XEROXGO_ADMIN_PASSWORD")
	if adminPass == "" {
		adminPass = "xeroxgo2026"
	}
	if err := storeDB.BootstrapAdmin(bootstrapCtx, adminUser, adminPass); err != nil {
		log.Printf("[WARN] Failed to bootstrap admin: %v", err)
	}

	hub := ws.NewHub(storeDB)
	kioskSecretBytes, err := resolveKioskSecret(os.Getenv("XEROXGO_KIOSK_SECRET"), devMode)
	if err != nil {
		log.Fatalf("Fatal: %v", err)
	}
	if len(kioskSecretBytes) > 0 {
		hub.SetKioskSecret(kioskSecretBytes)
	}

	cfg := &serverConfig{
		addr:           addr,
		dbPath:         dbPath,
		jobsDir:        jobsDir,
		devMode:        devMode,
		downloadSecret: dlSecretBytes,
		kioskSecret:    kioskSecretBytes,
	}

	pinLimiter := NewPINRateLimiter(storeDB, 5, 2*time.Minute, 2*time.Minute)
	app := &appServer{
		cfg:        cfg,
		db:         storeDB,
		hub:        hub,
		convWake:   make(chan struct{}, 1),
		pinLimiter: pinLimiter,
	}

	mux := app.routes()

	proxySecret := strings.TrimSpace(os.Getenv("XEROXGO_PROXY_SECRET"))
	handler := createIngressHandler(mux, proxySecret, devMode)

	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go app.startConversionWorker(sigCtx)

	serverErrCh := make(chan error, 1)
	go func() {
		log.Printf("XeroxGo cloud backend listening on %s (dev_mode=%v)", addr, devMode)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrCh <- err
		}
	}()

	select {
	case err := <-serverErrCh:
		log.Fatalf("server failure: %v", err)
	case <-sigCtx.Done():
		log.Printf("shutting down cloud backend...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown error: %v", err)
		}
	}
}

// routes registers and returns the standard HTTP request multiplexer for the cloud backend.
func (app *appServer) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", app.handleHealthz)
	mux.HandleFunc("/api/documents/preview", app.handleDocumentPreview)
	mux.HandleFunc("/api/conversions", app.handleCreateConversion)
	mux.HandleFunc("/api/conversions/", app.handleConversionRoutes)
	mux.HandleFunc("/api/k/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/upload"):
			app.handleUpload(w, r)
		case strings.HasSuffix(path, "/recover"):
			app.handleRecoverJob(w, r)
		case strings.HasSuffix(path, "/orders/lookup"), strings.HasSuffix(path, "/pin/lookup"):
			app.handlePINLookup(w, r)
		case strings.HasSuffix(path, "/claim"), strings.HasSuffix(path, "/confirm"):
			app.handleOrderClaim(w, r)
		case strings.HasSuffix(path, "/release"):
			app.handleOrderRelease(w, r)
		case strings.HasSuffix(path, "/recovery-pin"):
			app.handleRecoveryPIN(w, r)
		case strings.Contains(path, "/sessions/") && strings.HasSuffix(path, "/initiate-payment"):
			app.handleInitiatePayment(w, r)
		case strings.Contains(path, "/sessions/") && (strings.HasSuffix(path, "/cancel") || strings.HasSuffix(path, "/cancel-session")):
			app.handleSessionCancel(w, r)
		case strings.Contains(path, "/sessions/"):
			app.handleGetSession(w, r)
		case strings.HasSuffix(path, "/refill"):
			app.handleKioskRefill(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/api/recovery/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pdf") {
			app.handleDownloadRecoveryPDF(w, r)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/api/orders/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/status"):
			app.handleOrderStatus(w, r)
		case strings.HasSuffix(path, "/recovery-pin"):
			app.handleRecoveryPIN(w, r)
		case strings.HasSuffix(path, "/pdf"):
			app.handleDownloadPDF(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	if app.hub != nil {
		mux.HandleFunc("/ws", app.hub.ServeHTTP)
	}
	mux.HandleFunc("/pay/", app.handleDemoPayRoutes)

	// Public Ads API
	mux.HandleFunc("/api/ads", app.handlePublicAds)
	mux.HandleFunc("/api/ads/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/ads/images/") {
			app.handleServeAdImage(w, r)
			return
		}
		app.handlePublicAds(w, r)
	})

	// Public Pricing API
	mux.HandleFunc("/api/pricing", app.handlePublicPricing)

	// Protected Admin Dashboard API
	mux.HandleFunc("/api/admin/login", app.handleAdminLogin)
	mux.HandleFunc("/api/admin/logout", app.handleAdminLogout)
	mux.HandleFunc("/api/admin/me", app.handleAdminMe)
	mux.HandleFunc("/api/admin/password", app.handleAdminPassword)
	mux.HandleFunc("/api/admin/upload-image", app.handleAdminUploadImage)
	mux.HandleFunc("/api/admin/ads", app.handleAdminAds)
	mux.HandleFunc("/api/admin/ads/", app.handleAdminAds)
	mux.HandleFunc("/api/admin/pricing", app.handleAdminPricing)
	mux.HandleFunc("/api/admin/analytics", app.handleAdminAnalytics)
	mux.HandleFunc("/api/admin/orders", app.handleAdminOrders)
	mux.HandleFunc("/api/admin/orders/export", app.handleAdminOrdersExport)

	return mux
}

// createIngressHandler wraps the root HTTP mux with CORS headers and proxy secret validation.
func createIngressHandler(mux http.Handler, proxySecret string, devMode bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ingress proxy secret verification for frontend-facing API routes:
		// When XEROXGO_PROXY_SECRET is configured, requests to frontend API routes
		// must carry the secret header injected by Vercel Edge.
		// Excluded routes: /healthz, /ws (has kiosk auth),
		// and /api/orders/:id/pdf (has HMAC download signature).
		if proxySecret != "" && !devMode {
			path := r.URL.Path
			isExempt := path == "/healthz" ||
				path == "/ws" ||
				(strings.HasPrefix(path, "/api/orders/") && (strings.HasSuffix(path, "/pdf") || strings.HasSuffix(path, "/recovery-pin"))) ||
				(strings.HasPrefix(path, "/api/recovery/") && strings.HasSuffix(path, "/pdf")) ||
				(strings.HasPrefix(path, "/api/k/") && (strings.HasSuffix(path, "/recover") || strings.HasSuffix(path, "/orders/lookup") || strings.HasSuffix(path, "/pin/lookup") || strings.HasSuffix(path, "/claim") || strings.HasSuffix(path, "/confirm") || strings.HasSuffix(path, "/release") || strings.HasSuffix(path, "/recovery-pin") || strings.Contains(path, "/sessions/")))

			if !isExempt && strings.HasPrefix(path, "/api/") {
				reqSecret := strings.TrimSpace(r.Header.Get("X-XeroxGo-Proxy-Secret"))
				if !hmac.Equal([]byte(reqSecret), []byte(proxySecret)) {
					http.NotFound(w, r)
					return
				}
			}
		}

		origin := r.Header.Get("Origin")
		if origin != "" && isAllowedOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With, X-XeroxGo-Proxy-Secret")
			w.Header().Set("Access-Control-Expose-Headers", "Content-Length, X-Billed-Sides, X-Total-Sheets, X-Copies, X-Total-Price-Paise, X-Normalized-Pages, X-Padded-Pages, X-SHA256-Checksum")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			if origin != "" && !isAllowedOrigin(origin) {
				http.Error(w, "Forbidden origin", http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// isAllowedOrigin checks if an incoming Origin header is permitted for CORS requests.
func isAllowedOrigin(origin string) bool {
	if origin == "" {
		return false
	}

	// Canonical production and staging domains
	if origin == "https://xeroxgo.vercel.app" || origin == "https://campusprint.vercel.app" {
		return true
	}

	// Local development origins
	if origin == "http://localhost:3000" || origin == "http://localhost:8080" || origin == "http://localhost:5173" ||
		origin == "http://127.0.0.1:3000" || origin == "http://127.0.0.1:8080" || origin == "http://127.0.0.1:5173" {
		return true
	}

	// Preview deployments on Vercel
	if strings.HasPrefix(origin, "https://campusprint-") && strings.HasSuffix(origin, ".vercel.app") {
		return true
	}
	if strings.HasPrefix(origin, "https://xeroxgo-") && strings.HasSuffix(origin, ".vercel.app") {
		return true
	}

	// Custom allowed origins configured via environment variable
	if custom := os.Getenv("XEROXGO_ALLOWED_ORIGINS"); custom != "" {
		for _, o := range strings.Split(custom, ",") {
			if strings.TrimSpace(o) == origin {
				return true
			}
		}
	}

	return false
}
