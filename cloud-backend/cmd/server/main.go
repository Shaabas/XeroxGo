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
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"xeroxgo/cloud-backend/internal/payments"
	"xeroxgo/cloud-backend/internal/pdf"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/cloud-backend/internal/ws"
	"xeroxgo/pkg/contracts"
)

type serverConfig struct {
	addr               string
	dbPath             string
	jobsDir            string
	pricePerSheetPaise int64
	devMode            bool
	demoMode           bool
	downloadSecret     []byte
	webhookSecret      []byte
	kioskSecret        []byte
}

type appServer struct {
	cfg             *serverConfig
	db              *store.Store
	hub             *ws.Hub
	convWake        chan struct{}
	pinLimiter      *PINRateLimiter
	paymentProvider payments.Provider
}

func (s *appServer) getPaymentProvider() payments.Provider {
	if s.paymentProvider != nil {
		return s.paymentProvider
	}
	var secret string
	if s.cfg != nil {
		secret = string(s.cfg.webhookSecret)
	}
	return payments.NewMockProvider(secret)
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

func (s *appServer) createOrderWithPINRetry(ctx context.Context, order *store.Order, maxAttempts int) error {
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
		err = s.db.CreateOrder(ctx, *order)
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

func (s *appServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	demo := false
	if s.cfg != nil {
		demo = s.cfg.demoMode
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    "ok",
		"demo_mode": demo,
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
	if len(fileHeaders) > 1 || metadataRaw != "" {
		type docItemMeta struct {
			Copies        int         `json:"copies"`
			Duplex        interface{} `json:"duplex"`
			ColorMode     string      `json:"color_mode"`
			PageRange     string      `json:"page_range"`
			Orientation   string      `json:"orientation"`
			PagesPerSheet int         `json:"pages_per_sheet"`
			Nup           int         `json:"nup"`
		}
		var metaItems []docItemMeta
		if metadataRaw != "" {
			_ = json.Unmarshal([]byte(metadataRaw), &metaItems)
		}

		pricing, err := s.db.GetPricing(r.Context())
		if err != nil {
			pricing = store.PricingConfig{
				BWSimplexPricePaise:    300,
				BWDuplexPricePaise:     500,
				ColorSimplexPricePaise: 800,
				ColorDuplexPricePaise:  1000,
				ColorEnabled:           true,
			}
		}

		orderHex, err := randomHex(12)
		if err != nil {
			http.Error(w, "failed to generate order id", http.StatusInternalServerError)
			return
		}
		orderID := "ord_" + orderHex

		jobHex, err := randomHex(12)
		if err != nil {
			http.Error(w, "failed to generate job id", http.StatusInternalServerError)
			return
		}
		jobID := "job_" + jobHex

		h := sha256.Sum256([]byte(orderID))
		outputPath := filepath.Join(s.cfg.jobsDir, hex.EncodeToString(h[:])+".pdf")

		var intermediateNormPDFs []string
		defer func() {
			for _, p := range intermediateNormPDFs {
				_ = os.Remove(p)
			}
		}()

		var totalOrderPricePaise int64
		var totalPrintedPages int
		var totalSheets int
		anyDuplex := false
		anyColor := false

		for idx, fh := range fileHeaders {
			originalFilename := fh.Filename
			if !pdf.IsSupportedFormat(originalFilename) {
				_ = os.Remove(outputPath)
				http.Error(w, fmt.Sprintf("unsupported file format %q in document %d", filepath.Ext(originalFilename), idx+1), http.StatusBadRequest)
				return
			}

			f, err := fh.Open()
			if err != nil {
				_ = os.Remove(outputPath)
				http.Error(w, fmt.Sprintf("failed to open uploaded file %q: %v", originalFilename, err), http.StatusBadRequest)
				return
			}

			ext := strings.ToLower(filepath.Ext(originalFilename))
			if ext == "" {
				ext = ".pdf"
			}
			tmpInput, err := os.CreateTemp(s.cfg.jobsDir, "upload_in_*"+ext)
			if err != nil {
				_ = f.Close()
				_ = os.Remove(outputPath)
				http.Error(w, "failed to create temp file", http.StatusInternalServerError)
				return
			}
			tmpInputName := tmpInput.Name()
			_, copyErr := io.Copy(tmpInput, f)
			_ = f.Close()
			_ = tmpInput.Close()
			if copyErr != nil {
				_ = os.Remove(tmpInputName)
				_ = os.Remove(outputPath)
				http.Error(w, "failed to save uploaded file", http.StatusInternalServerError)
				return
			}

			normalizedInputPath := tmpInputName
			if ext != ".pdf" {
				tmpConverted, err := os.CreateTemp(s.cfg.jobsDir, "upload_conv_*.pdf")
				if err != nil {
					_ = os.Remove(tmpInputName)
					_ = os.Remove(outputPath)
					http.Error(w, "failed to create temp file for conversion", http.StatusInternalServerError)
					return
				}
				tmpConvertedName := tmpConverted.Name()
				_ = tmpConverted.Close()

				if err := pdf.ConvertToPDF(r.Context(), tmpInputName, originalFilename, tmpConvertedName); err != nil {
					_ = os.Remove(tmpInputName)
					_ = os.Remove(tmpConvertedName)
					_ = os.Remove(outputPath)
					http.Error(w, fmt.Sprintf("conversion failed for %q: %v", originalFilename, err), http.StatusBadRequest)
					return
				}
				_ = os.Remove(tmpInputName)
				normalizedInputPath = tmpConvertedName
			}
			defer func(path string) { _ = os.Remove(path) }(normalizedInputPath)

			docCopies := 1
			docDuplexMode := contracts.DuplexSimplex
			docColorMode := "BW"
			docPageRange := ""
			docOrientation := "AUTO"
			docPagesPerSheet := 1

			if idx < len(metaItems) {
				if metaItems[idx].Copies >= 1 && metaItems[idx].Copies <= 999 {
					docCopies = metaItems[idx].Copies
				}
				docDuplexMode = parseDuplex(metaItems[idx].Duplex)
				if strings.EqualFold(metaItems[idx].ColorMode, "COLOR") && pricing.ColorEnabled {
					docColorMode = "COLOR"
				}
				docPageRange = strings.TrimSpace(metaItems[idx].PageRange)
				if metaItems[idx].Orientation != "" {
					docOrientation = strings.ToUpper(strings.TrimSpace(metaItems[idx].Orientation))
					if docOrientation != "AUTO" && docOrientation != "PORTRAIT" && docOrientation != "LANDSCAPE" {
						_ = os.Remove(outputPath)
						http.Error(w, fmt.Sprintf("invalid orientation %q in document %d: only AUTO, PORTRAIT, and LANDSCAPE are supported", metaItems[idx].Orientation, idx+1), http.StatusBadRequest)
						return
					}
				}
				nup := metaItems[idx].PagesPerSheet
				if nup <= 0 && metaItems[idx].Nup > 0 {
					nup = metaItems[idx].Nup
				}
				if nup > 0 {
					if nup != 1 && nup != 2 && nup != 4 && nup != 6 {
						_ = os.Remove(outputPath)
						http.Error(w, fmt.Sprintf("invalid pages_per_sheet %d in document %d: only 1, 2, 4, and 6 are supported", nup, idx+1), http.StatusBadRequest)
						return
					}
					docPagesPerSheet = nup
				}
			} else {
				if c, err := strconv.Atoi(r.FormValue("copies")); err == nil && c >= 1 && c <= 999 {
					docCopies = c
				}
				docDuplexMode = parseDuplex(r.FormValue("duplex"))
				if strings.EqualFold(r.FormValue("color_mode"), "COLOR") && pricing.ColorEnabled {
					docColorMode = "COLOR"
				}
				docPageRange = strings.TrimSpace(r.FormValue("page_range"))
				if o := strings.ToUpper(strings.TrimSpace(r.FormValue("orientation"))); o != "" {
					if o != "AUTO" && o != "PORTRAIT" && o != "LANDSCAPE" {
						_ = os.Remove(outputPath)
						http.Error(w, fmt.Sprintf("invalid orientation %q in document %d: only AUTO, PORTRAIT, and LANDSCAPE are supported", r.FormValue("orientation"), idx+1), http.StatusBadRequest)
						return
					}
					docOrientation = o
				}
				nupStr := strings.TrimSpace(r.FormValue("pages_per_sheet"))
				if nupStr == "" {
					nupStr = strings.TrimSpace(r.FormValue("nup"))
				}
				if nupStr != "" {
					n, err := strconv.Atoi(nupStr)
					if err != nil || (n != 1 && n != 2 && n != 4 && n != 6) {
						_ = os.Remove(outputPath)
						http.Error(w, fmt.Sprintf("invalid pages_per_sheet %q in document %d: only 1, 2, 4, and 6 are supported", nupStr, idx+1), http.StatusBadRequest)
						return
					}
					docPagesPerSheet = n
				}
			}

			if docDuplexMode != contracts.DuplexSimplex {
				anyDuplex = true
			}
			if docColorMode == "COLOR" {
				anyColor = true
			}

			tmpNorm, err := os.CreateTemp(s.cfg.jobsDir, "upload_norm_*.pdf")
			if err != nil {
				_ = os.Remove(outputPath)
				http.Error(w, "failed to create intermediate file", http.StatusInternalServerError)
				return
			}
			tmpNormPath := tmpNorm.Name()
			_ = tmpNorm.Close()
			intermediateNormPDFs = append(intermediateNormPDFs, tmpNormPath)

			normRes, err := pdf.NormalizeAndCountWithOptions(r.Context(), normalizedInputPath, tmpNormPath, pdf.NormalizeOptions{
				Copies:        docCopies,
				Duplex:        docDuplexMode,
				ColorMode:     docColorMode,
				PageRange:     docPageRange,
				Orientation:   docOrientation,
				PagesPerSheet: docPagesPerSheet,
			})
			if err != nil {
				_ = os.Remove(outputPath)
				http.Error(w, fmt.Sprintf("normalization failed for %q: %v", originalFilename, err), http.StatusBadRequest)
				return
			}

			isDocColor := strings.EqualFold(docColorMode, "COLOR")
			isDocDuplex := docDuplexMode != contracts.DuplexSimplex

			docPrice, err := payments.CalculateDocumentPricePaise(normRes.TotalPages, isDocColor, isDocDuplex, docCopies)
			if err != nil {
				_ = os.Remove(outputPath)
				http.Error(w, "failed to calculate price", http.StatusInternalServerError)
				return
			}

			totalOrderPricePaise += docPrice
			totalPrintedPages += docPrintedSides
			totalSheets += normRes.TotalSheets
		}

		if len(intermediateNormPDFs) == 1 {
			data, err := os.ReadFile(intermediateNormPDFs[0])
			if err != nil {
				_ = os.Remove(outputPath)
				http.Error(w, "failed to read normalized document", http.StatusInternalServerError)
				return
			}
			if err := os.WriteFile(outputPath, data, 0600); err != nil {
				_ = os.Remove(outputPath)
				http.Error(w, "failed to save output document", http.StatusInternalServerError)
				return
			}
		} else {
			qpdfArgs := []string{"--empty", "--pages"}
			for _, p := range intermediateNormPDFs {
				qpdfArgs = append(qpdfArgs, p, "1-z")
			}
			qpdfArgs = append(qpdfArgs, "--", outputPath)

			cmdMerge := exec.CommandContext(r.Context(), "qpdf", qpdfArgs...)
			outMerge, errMerge := cmdMerge.CombinedOutput()
			if errMerge != nil {
				_ = os.Remove(outputPath)
				http.Error(w, fmt.Sprintf("failed to merge documents: %s: %v", strings.TrimSpace(string(outMerge)), errMerge), http.StatusInternalServerError)
				return
			}
		}

		finalBytes, err := os.ReadFile(outputPath)
		if err != nil {
			_ = os.Remove(outputPath)
			http.Error(w, "failed to read merged document", http.StatusInternalServerError)
			return
		}
		checksumBytes := sha256.Sum256(finalBytes)
		sha256Checksum := hex.EncodeToString(checksumBytes[:])

		baseURL := os.Getenv("XEROXGO_PUBLIC_URL")
		if baseURL == "" {
			scheme := "http"
			if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
				scheme = "https"
			}
			baseURL = fmt.Sprintf("%s://%s", scheme, r.Host)
		}
		baseURL = strings.TrimRight(baseURL, "/")
		downloadURL := generateSignedDownloadURL(baseURL, orderID, s.cfg.downloadSecret, 24*time.Hour)

		overallDuplex := contracts.DuplexSimplex
		if anyDuplex {
			overallDuplex = contracts.DuplexLongEdge
		}
		overallColor := "BW"
		if anyColor {
			overallColor = "COLOR"
		}

		cmd := contracts.PrintJobCommand{
			JobID:          jobID,
			KioskID:        kioskID,
			DisplayCode:    "",
			DownloadURL:    downloadURL,
			SHA256Checksum: sha256Checksum,
			TotalPages:     totalPrintedPages,
			TotalSheets:    totalSheets,
			Copies:         1,
			Duplex:         overallDuplex,
			StartFromPage:  1,
		}

		claimToken, err := randomHex(32)
		if err != nil {
			http.Error(w, "failed to generate recovery credential", http.StatusInternalServerError)
			return
		}

		pinExpiresAt := time.Now().UTC().Add(15 * time.Minute)
		order := store.Order{
			OrderID:      orderID,
			Command:      cmd,
			Status:       contracts.JobStatusPendingPayment,
			PricePaise:   totalOrderPricePaise,
			ColorMode:    overallColor,
			PINExpiresAt: pinExpiresAt,
			ClaimToken:   claimToken,
		}

		if err := s.createOrderWithPINRetry(r.Context(), &order, 25); err != nil {
			_ = os.Remove(outputPath)
			http.Error(w, "failed to persist order", http.StatusInternalServerError)
			return
		}
		displayCode := order.Command.DisplayCode

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"order_id":              orderID,
			"job_id":                jobID,
			"display_code":          displayCode,
			"claim_token":           claimToken,
			"recovery_token":        claimToken,
			"pin_expires_at":        pinExpiresAt.Format(time.RFC3339),
			"color_mode":            overallColor,
			"total_pages":           totalPrintedPages,
			"total_sheets":          totalSheets,
			"copies":                1,
			"duplex":                string(overallDuplex),
			"documents_count":       len(fileHeaders),
			"total_price_paise":     totalOrderPricePaise,
			"status":                string(contracts.JobStatusPendingPayment),
		})
		return
	}

	fileHeader := fileHeaders[0]
	file, err := fileHeader.Open()
	if err != nil {
		http.Error(w, "failed to open file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	originalFilename := fileHeader.Filename
	if !pdf.IsSupportedFormat(originalFilename) {
		http.Error(w, fmt.Sprintf("unsupported file format %q: only PDF, Office documents (DOCX, PPTX, XLSX), and Images (JPG, PNG) are supported", filepath.Ext(originalFilename)), http.StatusBadRequest)
		return
	}

	copiesStr := r.FormValue("copies")
	copies := 1
	if copiesStr != "" {
		c, err := strconv.Atoi(copiesStr)
		if err != nil || c < 1 || c > 999 {
			http.Error(w, "copies must be between 1 and 999", http.StatusBadRequest)
			return
		}
		copies = c
	}

	duplexStr := strings.TrimSpace(r.FormValue("duplex"))
	var duplexMode contracts.DuplexMode
	if duplexStr == "" {
		duplexMode = contracts.DuplexSimplex
	} else {
		switch strings.ToUpper(duplexStr) {
		case string(contracts.DuplexSimplex), "SINGLE", "SINGLE-SIDED", "FALSE":
			duplexMode = contracts.DuplexSimplex
		case string(contracts.DuplexLongEdge), "LONG_EDGE", "DUPLEX", "DOUBLE", "DOUBLE-SIDED", "TRUE":
			duplexMode = contracts.DuplexLongEdge
		case string(contracts.DuplexShortEdge), "SHORT_EDGE":
			duplexMode = contracts.DuplexShortEdge
		default:
			http.Error(w, "invalid duplex mode", http.StatusBadRequest)
			return
		}
	}

	orientationStr := strings.ToUpper(strings.TrimSpace(r.FormValue("orientation")))
	if orientationStr == "" {
		orientationStr = "AUTO"
	}
	if orientationStr != "AUTO" && orientationStr != "PORTRAIT" && orientationStr != "LANDSCAPE" {
		http.Error(w, fmt.Sprintf("invalid orientation %q: only AUTO, PORTRAIT, and LANDSCAPE are supported", r.FormValue("orientation")), http.StatusBadRequest)
		return
	}

	pagesPerSheet := 1
	nupStr := strings.TrimSpace(r.FormValue("pages_per_sheet"))
	if nupStr == "" {
		nupStr = strings.TrimSpace(r.FormValue("nup"))
	}
	if nupStr != "" {
		n, err := strconv.Atoi(nupStr)
		if err != nil || (n != 1 && n != 2 && n != 4 && n != 6) {
			http.Error(w, fmt.Sprintf("invalid pages_per_sheet %q: only 1, 2, 4, and 6 are supported", nupStr), http.StatusBadRequest)
			return
		}
		pagesPerSheet = n
	}

	ext := strings.ToLower(filepath.Ext(originalFilename))
	if ext == "" {
		ext = ".pdf"
	}
	tmpInput, err := os.CreateTemp(s.cfg.jobsDir, "upload_in_*"+ext)
	if err != nil {
		http.Error(w, "failed to create temp file", http.StatusInternalServerError)
		return
	}
	tmpInputName := tmpInput.Name()
	defer func() {
		_ = tmpInput.Close()
		_ = os.Remove(tmpInputName)
	}()

	if _, err := io.Copy(tmpInput, file); err != nil {
		http.Error(w, "failed to save uploaded file", http.StatusInternalServerError)
		return
	}
	_ = tmpInput.Close()

	// Convert to PDF if non-PDF document or image
	normalizedInputPath := tmpInputName
	if ext != ".pdf" {
		tmpConverted, err := os.CreateTemp(s.cfg.jobsDir, "upload_conv_*.pdf")
		if err != nil {
			http.Error(w, "failed to create temp file for conversion", http.StatusInternalServerError)
			return
		}
		tmpConvertedName := tmpConverted.Name()
		defer func() {
			_ = tmpConverted.Close()
			_ = os.Remove(tmpConvertedName)
		}()
		_ = tmpConverted.Close()

		if err := pdf.ConvertToPDF(r.Context(), tmpInputName, originalFilename, tmpConvertedName); err != nil {
			http.Error(w, fmt.Sprintf("document conversion failed: %v", err), http.StatusBadRequest)
			return
		}
		normalizedInputPath = tmpConvertedName
	}

	orderHex, err := randomHex(12)
	if err != nil {
		http.Error(w, "failed to generate order id", http.StatusInternalServerError)
		return
	}
	orderID := "ord_" + orderHex

	jobHex, err := randomHex(12)
	if err != nil {
		http.Error(w, "failed to generate job id", http.StatusInternalServerError)
		return
	}
	jobID := "job_" + jobHex

	h := sha256.Sum256([]byte(orderID))
	outputPath := filepath.Join(s.cfg.jobsDir, hex.EncodeToString(h[:])+".pdf")

	colorMode := strings.ToUpper(strings.TrimSpace(r.FormValue("color_mode")))
	if colorMode != "COLOR" {
		colorMode = "BW"
	}

	pricing, err := s.db.GetPricing(r.Context())
	hasPricing := err == nil
	if hasPricing && !pricing.ColorEnabled {
		// Hardware enforcement: coerce to BW if kiosk color is disabled
		colorMode = "BW"
	}

	pageRange := strings.TrimSpace(r.FormValue("page_range"))

	normRes, err := pdf.NormalizeAndCountWithOptions(r.Context(), normalizedInputPath, outputPath, pdf.NormalizeOptions{
		Copies:        copies,
		Duplex:        duplexMode,
		ColorMode:     colorMode,
		PageRange:     pageRange,
		Orientation:   orientationStr,
		PagesPerSheet: pagesPerSheet,
	})
	if err != nil {
		_ = os.Remove(outputPath)
		http.Error(w, fmt.Sprintf("normalization failed: %v", err), http.StatusBadRequest)
		return
	}

	isColor := strings.EqualFold(colorMode, "COLOR")
	isDuplex := duplexMode != contracts.DuplexSimplex

	totalPricePaise, err := payments.CalculateDocumentPricePaise(normRes.TotalPages, isColor, isDuplex, copies)
	if err != nil {
		_ = os.Remove(outputPath)
		http.Error(w, "failed to calculate price", http.StatusInternalServerError)
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
	downloadURL := generateSignedDownloadURL(baseURL, orderID, s.cfg.downloadSecret, 24*time.Hour)

	cmd := contracts.PrintJobCommand{
		JobID:          jobID,
		KioskID:        kioskID,
		DisplayCode:    "",
		DownloadURL:    downloadURL,
		SHA256Checksum: normRes.SHA256Checksum,
		TotalPages:     normRes.TotalPages,
		TotalSheets:    normRes.TotalSheets,
		Copies:         copies,
		Duplex:         duplexMode,
		StartFromPage:  1,
	}

	claimToken, err := randomHex(32)
	if err != nil {
		http.Error(w, "failed to generate recovery credential", http.StatusInternalServerError)
		return
	}

	pinExpiresAt := time.Now().UTC().Add(15 * time.Minute)
	order := store.Order{
		OrderID:      orderID,
		Command:      cmd,
		Status:       contracts.JobStatusPendingPayment,
		PricePaise:   totalPricePaise,
		ColorMode:    colorMode,
		PINExpiresAt: pinExpiresAt,
		ClaimToken:   claimToken,
	}

	if err := s.createOrderWithPINRetry(r.Context(), &order, 25); err != nil {
		_ = os.Remove(outputPath)
		http.Error(w, "failed to persist order", http.StatusInternalServerError)
		return
	}
	displayCode := order.Command.DisplayCode

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"order_id":              orderID,
		"job_id":                jobID,
		"display_code":          displayCode,
		"claim_token":           claimToken,
		"recovery_token":        claimToken,
		"pin_expires_at":        pinExpiresAt.Format(time.RFC3339),
		"color_mode":            colorMode,
		"total_pages":           normRes.TotalPages,
		"total_sheets":          normRes.TotalSheets,
		"copies":                copies,
		"duplex":                string(duplexMode),
		"price_per_sheet_paise": unitPrice,
		"price_per_side_paise":  unitPrice,
		"unit_price_paise":      unitPrice,
		"total_price_paise":     totalPricePaise,
		"status":                string(contracts.JobStatusPendingPayment),
	})
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
	orderID := strings.TrimPrefix(r.URL.Path, prefix)
	orderID = strings.TrimSuffix(orderID, suffix)
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		http.Error(w, "order id required", http.StatusBadRequest)
		return
	}

	expiresStr := r.URL.Query().Get("expires")
	sig := r.URL.Query().Get("sig")
	if expiresStr == "" || sig == "" {
		http.Error(w, "missing authentication signature", http.StatusUnauthorized)
		return
	}

	expires, err := strconv.ParseInt(expiresStr, 10, 64)
	if err != nil || !verifySignedDownloadURL(orderID, expires, sig, s.cfg.downloadSecret) {
		http.Error(w, "invalid or expired download URL", http.StatusUnauthorized)
		return
	}

	order, err := s.db.GetOrder(r.Context(), orderID)
	if err != nil || order == nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}

	h := sha256.Sum256([]byte(orderID))
	pdfPath := filepath.Join(s.cfg.jobsDir, hex.EncodeToString(h[:])+".pdf")

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
	if calcSHA != strings.ToLower(order.Command.SHA256Checksum) {
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

func (s *appServer) handleConvert(w http.ResponseWriter, r *http.Request) {
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

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	filename := header.Filename
	if !pdf.IsSupportedFormat(filename) {
		http.Error(w, fmt.Sprintf("unsupported file format %q: only PDF, Office documents (DOCX, PPTX, XLSX), and Images (JPG, PNG) are supported", filepath.Ext(filename)), http.StatusBadRequest)
		return
	}

	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" {
		ext = ".tmp"
	}
	tmpInput, err := os.CreateTemp("", "conv_in_*"+ext)
	if err != nil {
		http.Error(w, "failed to create temp file", http.StatusInternalServerError)
		return
	}
	tmpInputName := tmpInput.Name()
	defer os.Remove(tmpInputName)
	defer tmpInput.Close()

	if _, err := io.Copy(tmpInput, file); err != nil {
		http.Error(w, "failed to read uploaded file", http.StatusInternalServerError)
		return
	}
	_ = tmpInput.Close()

	tmpOutput, err := os.CreateTemp("", "conv_out_*.pdf")
	if err != nil {
		http.Error(w, "failed to create output file", http.StatusInternalServerError)
		return
	}
	tmpOutputName := tmpOutput.Name()
	defer os.Remove(tmpOutputName)
	defer tmpOutput.Close()

	if err := pdf.ConvertToPDF(r.Context(), tmpInputName, filename, tmpOutputName); err != nil {
		http.Error(w, fmt.Sprintf("conversion failed: %v", err), http.StatusBadRequest)
		return
	}

	pdfBytes, err := os.ReadFile(tmpOutputName)
	if err != nil {
		http.Error(w, "failed to read converted pdf", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Length", strconv.Itoa(len(pdfBytes)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pdfBytes)
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

	if !pdf.IsSupportedFormat(filename) {
		http.Error(w, fmt.Sprintf("unsupported file format %q: only PDF, Office documents (DOCX, PPTX, XLSX), and Images (JPG, PNG) are supported", filepath.Ext(filename)), http.StatusBadRequest)
		return
	}

	copiesStr := strings.TrimSpace(r.FormValue("copies"))
	copies := 1
	if copiesStr != "" {
		c, err := strconv.Atoi(copiesStr)
		if err != nil || c < 1 || c > 999 {
			http.Error(w, "copies must be between 1 and 999", http.StatusBadRequest)
			return
		}
		copies = c
	}

	duplexStr := strings.TrimSpace(r.FormValue("duplex"))
	var duplexMode contracts.DuplexMode
	if duplexStr == "" {
		duplexMode = contracts.DuplexSimplex
	} else {
		switch strings.ToUpper(duplexStr) {
		case string(contracts.DuplexSimplex), "SINGLE", "SINGLE-SIDED", "FALSE":
			duplexMode = contracts.DuplexSimplex
		case string(contracts.DuplexLongEdge), "LONG_EDGE", "DUPLEX", "DOUBLE", "DOUBLE-SIDED", "TRUE":
			duplexMode = contracts.DuplexLongEdge
		case string(contracts.DuplexShortEdge), "SHORT_EDGE":
			duplexMode = contracts.DuplexShortEdge
		default:
			http.Error(w, "invalid duplex mode", http.StatusBadRequest)
			return
		}
	}

	orientationStr := strings.ToUpper(strings.TrimSpace(r.FormValue("orientation")))
	if orientationStr == "" {
		orientationStr = "AUTO"
	}
	if orientationStr != "AUTO" && orientationStr != "PORTRAIT" && orientationStr != "LANDSCAPE" {
		http.Error(w, fmt.Sprintf("invalid orientation %q: only AUTO, PORTRAIT, and LANDSCAPE are supported", r.FormValue("orientation")), http.StatusBadRequest)
		return
	}

	pagesPerSheet := 1
	nupStr := strings.TrimSpace(r.FormValue("pages_per_sheet"))
	if nupStr == "" {
		nupStr = strings.TrimSpace(r.FormValue("nup"))
	}
	if nupStr != "" {
		n, err := strconv.Atoi(nupStr)
		if err != nil || (n != 1 && n != 2 && n != 4 && n != 6) {
			http.Error(w, fmt.Sprintf("invalid pages_per_sheet %q: only 1, 2, 4, and 6 are supported", nupStr), http.StatusBadRequest)
			return
		}
		pagesPerSheet = n
	}

	colorMode := strings.ToUpper(strings.TrimSpace(r.FormValue("color_mode")))
	if colorMode != "COLOR" {
		colorMode = "BW"
	}

	pageRange := strings.TrimSpace(r.FormValue("page_range"))

	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" {
		ext = ".pdf"
	}
	tmpInput, err := os.CreateTemp(s.cfg.jobsDir, "preview_in_*"+ext)
	if err != nil {
		http.Error(w, "failed to create temp file", http.StatusInternalServerError)
		return
	}
	tmpInputName := tmpInput.Name()
	defer func() {
		_ = tmpInput.Close()
		_ = os.Remove(tmpInputName)
	}()

	if _, err := io.Copy(tmpInput, file); err != nil {
		http.Error(w, "failed to save uploaded file", http.StatusInternalServerError)
		return
	}
	_ = tmpInput.Close()

	normalizedInputPath := tmpInputName
	if ext != ".pdf" {
		tmpConverted, err := os.CreateTemp(s.cfg.jobsDir, "preview_conv_*.pdf")
		if err != nil {
			http.Error(w, "failed to create temp file for conversion", http.StatusInternalServerError)
			return
		}
		tmpConvertedName := tmpConverted.Name()
		defer func() {
			_ = tmpConverted.Close()
			_ = os.Remove(tmpConvertedName)
		}()
		_ = tmpConverted.Close()

		if err := pdf.ConvertToPDF(r.Context(), tmpInputName, filename, tmpConvertedName); err != nil {
			http.Error(w, fmt.Sprintf("document conversion failed: %v", err), http.StatusBadRequest)
			return
		}
		normalizedInputPath = tmpConvertedName
	}

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

	pricing, err := s.db.GetPricing(r.Context())
	if err != nil {
		pricing = store.PricingConfig{
			BWSimplexPricePaise:    300,
			BWDuplexPricePaise:     500,
			ColorSimplexPricePaise: 800,
			ColorDuplexPricePaise:  1000,
			ColorEnabled:           true,
		}
	}
	if !pricing.ColorEnabled {
		colorMode = "BW"
	}

	normRes, err := pdf.NormalizeAndCountWithOptions(r.Context(), normalizedInputPath, tmpOutputName, pdf.NormalizeOptions{
		Copies:        copies,
		Duplex:        duplexMode,
		ColorMode:     colorMode,
		PageRange:     pageRange,
		Orientation:   orientationStr,
		PagesPerSheet: pagesPerSheet,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("preview generation failed: %v", err), http.StatusBadRequest)
		return
	}

	isColor := strings.EqualFold(colorMode, "COLOR")
	isDuplex := duplexMode != contracts.DuplexSimplex
	totalPricePaise, err := payments.CalculateDocumentPricePaise(normRes.TotalPages, isColor, isDuplex, copies)
	if err != nil {
		http.Error(w, "failed to calculate price", http.StatusInternalServerError)
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
			"billed_sides":      totalPrintedSides,
			"total_sheets":     normRes.TotalSheets,
			"copies":           copies,
			"total_price_paise": totalPricePaise,
			"normalized_pages": normRes.TotalPages,
			"padded_pages":     normRes.PaddedPages,
			"sha256_checksum":  normRes.SHA256Checksum,
			"color_mode":       colorMode,
			"duplex":           string(duplexMode),
			"orientation":      orientationStr,
			"pages_per_sheet":  pagesPerSheet,
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
	w.Header().Set("X-Billed-Sides", strconv.Itoa(totalPrintedSides))
	w.Header().Set("X-Total-Sheets", strconv.Itoa(normRes.TotalSheets))
	w.Header().Set("X-Copies", strconv.Itoa(copies))
	w.Header().Set("X-Total-Price-Paise", strconv.FormatInt(totalPricePaise, 10))
	w.Header().Set("X-Normalized-Pages", strconv.Itoa(normRes.TotalPages))
	w.Header().Set("X-Padded-Pages", strconv.Itoa(normRes.PaddedPages))
	w.Header().Set("X-SHA256-Checksum", normRes.SHA256Checksum)
	w.Header().Set("Access-Control-Expose-Headers", "Content-Length, X-Billed-Sides, X-Total-Sheets, X-Copies, X-Total-Price-Paise, X-Normalized-Pages, X-Padded-Pages, X-SHA256-Checksum")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pdfBytes)
}

func (s *appServer) handleSimulatePayment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !s.cfg.devMode {
		http.Error(w, "payment simulation only available in development mode", http.StatusForbidden)
		return
	}
	if s.cfg.demoMode {
		http.Error(w, "direct order simulation is disabled in demo mode; use secure /pay/{token} checkout", http.StatusForbidden)
		return
	}

	prefix := "/api/orders/"
	suffix := "/simulate-payment"
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

	if order.Status == contracts.JobStatusCompleted ||
		order.Status == contracts.JobStatusPrinting ||
		order.Status == contracts.JobStatusSpooling ||
		order.Status == contracts.JobStatusDispatched {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"order_id": orderID,
			"status":   string(order.Status),
		})
		return
	}

	if order.Status == contracts.JobStatusPendingPayment && !order.PINExpiresAt.IsZero() && !time.Now().UTC().Before(order.PINExpiresAt) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"order_id": orderID,
			"status":   "EXPIRED",
			"error":    "order has expired",
		})
		return
	}

	syntheticPayload := map[string]interface{}{
		"event": "payment.captured",
		"payload": map[string]interface{}{
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_sim_" + orderID,
					"order_id": orderID,
					"notes": map[string]interface{}{
						"order_id": orderID,
					},
					"status": "captured",
					"amount": order.PricePaise,
				},
			},
		},
	}
	rawPayload, err := json.Marshal(syntheticPayload)
	if err != nil {
		http.Error(w, "failed to marshal payload", http.StatusInternalServerError)
		return
	}

	mac := hmac.New(sha256.New, s.cfg.webhookSecret)
	mac.Write(rawPayload)
	sig := hex.EncodeToString(mac.Sum(nil))

	if !payments.VerifyWebhookSignature(rawPayload, sig, string(s.cfg.webhookSecret)) {
		http.Error(w, "internal signature verification failed", http.StatusInternalServerError)
		return
	}

	webhookID := "evt_sim_" + orderID
	recorded, err := s.db.RecordWebhook(r.Context(), webhookID)
	if err != nil {
		http.Error(w, "failed to record webhook", http.StatusInternalServerError)
		return
	}
	if !recorded {
		// Idempotent repetition
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"order_id": orderID,
			"status":   string(order.Status),
		})
		return
	}

	requireSession := r.URL.Query().Get("require_session") == "1" || r.URL.Query().Get("strict_session") == "1"
	now := time.Now().UTC()

	var simSessionID string
	// In development simulation: if strict session is not required and no active session exists,
	// auto-create a session so automated integration tests run end-to-end.
	if !requireSession {
		activeSess, _ := s.db.GetActiveKioskSession(r.Context(), order.Command.KioskID, orderID, now)
		if activeSess == nil {
			activeSess, _ = s.db.CreateOrGetKioskSession(r.Context(), order.Command.KioskID, orderID, 5*time.Minute, now)
		}
		if activeSess != nil {
			simSessionID = activeSess.SessionID
		}
	} else {
		if activeSess, _ := s.db.GetActiveKioskSession(r.Context(), order.Command.KioskID, orderID, now); activeSess != nil {
			simSessionID = activeSess.SessionID
		}
	}
	if sessParam := strings.TrimSpace(r.URL.Query().Get("session_id")); sessParam != "" {
		simSessionID = sessParam
	}

	captureRes, err := s.db.CapturePaymentAndCheckDispatch(r.Context(), store.PaymentRecord{
		PaymentID:   "pay_sim_" + orderID,
		OrderID:     orderID,
		KioskID:     order.Command.KioskID,
		SessionID:   simSessionID,
		AmountPaise: order.PricePaise,
		Provider:    "simulate",
		Status:      "captured",
	}, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if captureRes.DispatchEligible && captureRes.Command != nil {
		if s.hub != nil {
			if err := s.hub.SendJob(r.Context(), *captureRes.Command); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"order_id": orderID,
					"status":   string(contracts.JobStatusPaidWaiting),
					"error":    fmt.Sprintf("kiosk offline or dispatch failed: %v", err),
				})
				return
			}
			_ = s.db.MarkDispatchIntentSent(r.Context(), orderID)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"order_id": orderID,
			"status":   string(contracts.JobStatusDispatched),
		})
		return
	}

	// No session or not dispatch eligible
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"order_id": orderID,
		"status":   string(captureRes.OrderStatus),
	})
}

func (s *appServer) handleRazorpayWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	const maxWebhookSize = 1024 * 1024 // 1 MiB
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookSize)
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read webhook body", http.StatusBadRequest)
		return
	}

	sig := r.Header.Get("X-Razorpay-Signature")
	prov := s.getPaymentProvider()
	if sig == "" || !prov.VerifyWebhookSignature(rawBody, sig) {
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	var parsed struct {
		Event   string `json:"event"`
		Payload struct {
			Payment struct {
				Entity struct {
					ID       string `json:"id"`
					OrderID  string `json:"order_id"`
					Amount   int64  `json:"amount"`
					Currency string `json:"currency"`
					Status   string `json:"status"`
					Notes    struct {
						OrderID   string `json:"order_id"`
						SessionID string `json:"session_id"`
						KioskID   string `json:"kiosk_id"`
					} `json:"notes"`
				} `json:"entity"`
			} `json:"payment"`
		} `json:"payload"`
	}

	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		http.Error(w, "malformed webhook json", http.StatusBadRequest)
		return
	}

	if parsed.Event != "payment.captured" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ignored"}`))
		return
	}

	entity := parsed.Payload.Payment.Entity
	if entity.ID == "" {
		http.Error(w, "payment id missing in payment entity", http.StatusBadRequest)
		return
	}
	if entity.Status != "captured" {
		http.Error(w, "payment status is not captured", http.StatusBadRequest)
		return
	}
	if entity.Currency != "" && strings.ToUpper(strings.TrimSpace(entity.Currency)) != "INR" {
		http.Error(w, "invalid currency: only INR is supported", http.StatusBadRequest)
		return
	}

	// Stored attempt association (Requirement 7 & 8: never trust notes alone as proof of transaction ownership)
	var attempt *store.PaymentAttempt
	providerOrderID := strings.TrimSpace(entity.OrderID)
	notesOrderID := strings.TrimSpace(entity.Notes.OrderID)
	notesSessionID := strings.TrimSpace(entity.Notes.SessionID)

	if providerOrderID != "" {
		att, err := s.db.GetPaymentAttemptByProviderOrder(r.Context(), "", providerOrderID)
		if err == nil && att != nil {
			attempt = att
		}
	}
	if attempt == nil {
		lookupOrderID := notesOrderID
		if lookupOrderID == "" {
			lookupOrderID = providerOrderID
		}
		if lookupOrderID != "" {
			att, err := s.db.GetPaymentAttemptByOrderAndSession(r.Context(), lookupOrderID, notesSessionID)
			if err == nil && att != nil {
				attempt = att
			}
		}
	}

	var (
		orderID      string
		sessionID    string
		kioskID      string
		amount       int64
		providerName string
	)

	if attempt != nil {
		// Strictly enforce attempt binding
		if entity.Amount > 0 && entity.Amount != attempt.AmountPaise {
			http.Error(w, fmt.Sprintf("payment amount mismatch: expected %d paise, got %d paise", attempt.AmountPaise, entity.Amount), http.StatusBadRequest)
			return
		}
		if entity.Currency != "" && attempt.Currency != "" && !strings.EqualFold(entity.Currency, attempt.Currency) {
			http.Error(w, "payment currency mismatch with stored attempt", http.StatusBadRequest)
			return
		}
		orderID = attempt.OrderID
		kioskID = attempt.KioskID
		sessionID = attempt.SessionID
		amount = attempt.AmountPaise
		providerName = attempt.Provider
	} else {
		// Fallback for orders without an explicit attempt:
		// Requirement 8: Never trust payment notes alone as proof of transaction ownership!
		// We do NOT trust notes.SessionID to authorize physical dispatch.
		lookupOrderID := notesOrderID
		if lookupOrderID == "" {
			lookupOrderID = providerOrderID
		}
		if lookupOrderID == "" {
			http.Error(w, "unassociated payment: order id missing", http.StatusBadRequest)
			return
		}
		order, err := s.db.GetOrder(r.Context(), lookupOrderID)
		if err != nil || order == nil {
			http.Error(w, "unassociated payment attempt: no matching stored attempt or order found", http.StatusBadRequest)
			return
		}
		if entity.Amount > 0 && entity.Amount != order.PricePaise {
			http.Error(w, fmt.Sprintf("payment amount mismatch: expected %d paise, got %d paise", order.PricePaise, entity.Amount), http.StatusBadRequest)
			return
		}
		orderID = order.OrderID
		kioskID = order.Command.KioskID
		now := time.Now().UTC()
		if notesSessionID != "" {
			sess, sErr := s.db.GetKioskSession(r.Context(), notesSessionID, now)
			if sErr == nil && sess != nil && sess.OrderID == orderID && sess.Status == store.KioskSessionPaymentPending && sess.ExpiresAt.After(now) {
				sessionID = sess.SessionID
			}
		}
		amount = order.PricePaise
		providerName = "razorpay"
	}
	if providerName == "" {
		providerName = "razorpay"
	}

	eventID := entity.ID
	recorded, err := s.db.RecordWebhook(r.Context(), eventID)
	if err != nil {
		http.Error(w, "failed to record webhook", http.StatusInternalServerError)
		return
	}
	if !recorded {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"duplicate"}`))
		return
	}

	now := time.Now().UTC()
	captureRes, err := s.db.CapturePaymentAndCheckDispatch(r.Context(), store.PaymentRecord{
		PaymentID:   eventID,
		OrderID:     orderID,
		KioskID:     kioskID,
		SessionID:   sessionID,
		AmountPaise: amount,
		Provider:    providerName,
		Status:      entity.Status,
	}, now)
	if err != nil {
		if errors.Is(err, store.ErrOrderNotFound) {
			http.Error(w, "order not found", http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if attempt != nil {
		_ = s.db.MarkPaymentAttemptPaid(r.Context(), attempt.AttemptID, eventID)
	}

	if captureRes.AlreadyProcessed {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"already_processed"}`))
		return
	}

	// PHYSICAL PRINT AUTHORIZATION GATE
	// Only dispatch print if valid active kiosk session exists for customer presence
	if captureRes.DispatchEligible && captureRes.Command != nil {
		log.Printf("[PAYMENT] Order %s authorized for print (active session %s)", orderID, captureRes.ActiveSessionID)
		if s.hub != nil {
			if err := s.hub.SendJob(r.Context(), *captureRes.Command); err != nil {
				log.Printf("[PAYMENT] Failed to dispatch job %s to kiosk %s: %v", captureRes.Command.JobID, captureRes.KioskID, err)
			} else {
				_ = s.db.MarkDispatchIntentSent(r.Context(), orderID)
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"dispatched"}`))
		return
	}

	// Late payment or absent active kiosk session: money captured in PAID_WAITING without unattended printing
	log.Printf("[PAYMENT] Payment captured for order %s, but no active kiosk session authorizes printing. Preserving in PAID_WAITING.", orderID)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"paid_unprinted"}`))
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

func main() {
	demoFlag := flag.Bool("demo-mode", false, "Enable demo mode with simulated QR payments")
	addrFlag := flag.String("addr", "", "Server listen address (overrides XEROXGO_CLOUD_ADDR / PORT)")
	flag.Parse()

	demoEnv := strings.TrimSpace(os.Getenv("DEMO_MODE"))
	if demoEnv == "" {
		demoEnv = strings.TrimSpace(os.Getenv("XEROXGO_DEMO_MODE"))
	}
	demoMode := true
	if demoEnv == "0" || strings.ToLower(demoEnv) == "false" {
		demoMode = false
	} else if *demoFlag {
		demoMode = true
	}

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
	if devMode {
		demoMode = true
	}

	dlSecretStr := os.Getenv("XEROXGO_DOWNLOAD_SECRET")
	whSecretStr := os.Getenv("XEROXGO_WEBHOOK_SECRET")

	if !devMode {
		if len(dlSecretStr) < 16 || len(whSecretStr) < 16 {
			log.Fatalf("Fatal: production mode requires non-empty XEROXGO_DOWNLOAD_SECRET and XEROXGO_WEBHOOK_SECRET of at least 16 characters")
		}
	} else {
		if dlSecretStr == "" {
			dlSecretStr = "dev_download_secret_32_bytes_long_123"
		}
		if whSecretStr == "" {
			whSecretStr = "dev_webhook_secret_32_bytes_long_123"
		}
	}

	isPostgres := strings.HasPrefix(dbPath, "postgres://") || strings.HasPrefix(dbPath, "postgresql://")
	if !isPostgres {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
			log.Fatalf("failed to create db directory: %v", err)
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
	var pricePaise int64
	priceEnv := os.Getenv("XEROXGO_PRICE_PER_SHEET_PAISE")
	if priceEnv != "" {
		p, err := strconv.ParseInt(priceEnv, 10, 64)
		if err != nil || p <= 0 {
			log.Fatalf("invalid XEROXGO_PRICE_PER_SHEET_PAISE: %s", priceEnv)
		}
		if err := storeDB.SetPricePerSheetPaise(ctx, p); err != nil {
			log.Fatalf("failed to persist price per sheet: %v", err)
		}
		pricePaise = p
	} else {
		p, err := storeDB.GetPricePerSheetPaise(ctx)
		if err == nil && p > 0 {
			pricePaise = p
		} else if devMode {
			pricePaise = 500
			_ = storeDB.SetPricePerSheetPaise(ctx, pricePaise)
		} else {
			log.Fatalf("pricing unconfigured in production mode")
		}
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
	kioskSecretStr := os.Getenv("XEROXGO_KIOSK_SECRET")
	if kioskSecretStr == "" && demoMode {
		kioskSecretStr = "dev_kiosk_secret_32_bytes_long_1234"
	}
	var kioskSecretBytes []byte
	if kioskSecretStr != "" {
		kioskSecretBytes = []byte(kioskSecretStr)
		hub.SetKioskSecret(kioskSecretBytes)
	} else if !devMode {
		log.Fatalf("XEROXGO_KIOSK_SECRET required in production mode")
	}

	cfg := &serverConfig{
		addr:               addr,
		dbPath:             dbPath,
		jobsDir:            jobsDir,
		pricePerSheetPaise: pricePaise,
		devMode:            devMode,
		demoMode:           demoMode,
		downloadSecret:     []byte(dlSecretStr),
		webhookSecret:      []byte(whSecretStr),
		kioskSecret:        kioskSecretBytes,
	}

	rzpKeyID := strings.TrimSpace(os.Getenv("RAZORPAY_KEY_ID"))
	rzpKeySecret := strings.TrimSpace(os.Getenv("RAZORPAY_KEY_SECRET"))
	paymentProviderType := strings.ToLower(strings.TrimSpace(os.Getenv("PAYMENT_PROVIDER")))

	var paymentProv payments.Provider
	if paymentProviderType == "razorpay" || (paymentProviderType == "" && rzpKeyID != "" && rzpKeySecret != "") {
		if rzpKeyID == "" || rzpKeySecret == "" {
			log.Fatalf("RAZORPAY_KEY_ID and RAZORPAY_KEY_SECRET must be set when PAYMENT_PROVIDER=razorpay")
		}
		var err error
		paymentProv, err = payments.NewRazorpayProvider(payments.RazorpayConfig{
			KeyID:         rzpKeyID,
			KeySecret:     rzpKeySecret,
			WebhookSecret: whSecretStr,
		})
		if err != nil {
			log.Fatalf("failed to initialize Razorpay provider: %v", err)
		}
		log.Printf("[PAYMENT] Initialized Razorpay payment provider (key_id=%s)", rzpKeyID)
	} else {
		paymentProv = payments.NewMockProvider(whSecretStr)
		log.Printf("[PAYMENT] Initialized Mock payment provider (real payments disabled by default)")
	}

	pinLimiter := NewPINRateLimiter(storeDB, 5, 2*time.Minute, 2*time.Minute)
	app := &appServer{
		cfg:             cfg,
		db:              storeDB,
		hub:             hub,
		convWake:        make(chan struct{}, 1),
		pinLimiter:      pinLimiter,
		paymentProvider: paymentProv,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", app.handleHealthz)
	mux.HandleFunc("/api/documents/preview", app.handleDocumentPreview)
	mux.HandleFunc("/api/convert", app.handleConvert)
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
		case strings.HasSuffix(path, "/initiate-payment"):
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
		case strings.HasSuffix(path, "/simulate-payment"):
			app.handleSimulatePayment(w, r)
		case strings.HasSuffix(path, "/recovery-pin"):
			app.handleRecoveryPIN(w, r)
		case strings.HasSuffix(path, "/pdf"):
			app.handleDownloadPDF(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/api/webhooks/razorpay", app.handleRazorpayWebhook)
	mux.HandleFunc("/ws", hub.ServeHTTP)
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
		log.Printf("XeroxGo cloud backend listening on %s (dev_mode=%v, price=%d paise)", addr, devMode, pricePaise)
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

// createIngressHandler wraps the root HTTP mux with CORS headers and proxy secret validation.
func createIngressHandler(mux http.Handler, proxySecret string, devMode bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ingress proxy secret verification for frontend-facing API routes:
		// When XEROXGO_PROXY_SECRET is configured, requests to frontend API routes
		// must carry the secret header injected by Vercel Edge.
		// Excluded routes: /healthz, /ws (has kiosk auth), /api/webhooks/razorpay (has HMAC),
		// and /api/orders/:id/pdf (has HMAC download signature).
		if proxySecret != "" && !devMode {
			path := r.URL.Path
			isExempt := path == "/healthz" ||
				path == "/ws" ||
				path == "/api/webhooks/razorpay" ||
				(strings.HasPrefix(path, "/api/orders/") && (strings.HasSuffix(path, "/pdf") || strings.HasSuffix(path, "/recovery-pin"))) ||
				(strings.HasPrefix(path, "/api/recovery/") && strings.HasSuffix(path, "/pdf")) ||
				(strings.HasPrefix(path, "/api/k/") && (strings.HasSuffix(path, "/recover") || strings.HasSuffix(path, "/orders/lookup") || strings.HasSuffix(path, "/pin/lookup") || strings.HasSuffix(path, "/claim") || strings.HasSuffix(path, "/confirm") || strings.HasSuffix(path, "/release") || strings.HasSuffix(path, "/recovery-pin") || strings.HasSuffix(path, "/initiate-payment") || strings.Contains(path, "/sessions/")))

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
