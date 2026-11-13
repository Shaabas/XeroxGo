package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"xeroxgo/cloud-backend/internal/pdf"
	"xeroxgo/cloud-backend/internal/pricing"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

// docItemMeta describes optional per-document settings in multi-file upload metadata JSON.
type docItemMeta struct {
	Copies        int         `json:"copies"`
	Duplex        interface{} `json:"duplex"`
	ColorMode     string      `json:"color_mode"`
	PageRange     string      `json:"page_range"`
	Orientation   string      `json:"orientation"`
	PagesPerSheet int         `json:"pages_per_sheet"`
	Nup           int         `json:"nup"`
}

// docPrintOptions holds parsed and validated print settings for a single document.
type docPrintOptions struct {
	Copies        int
	Duplex        contracts.DuplexMode
	ColorMode     string
	PageRange     string
	Orientation   string
	PagesPerSheet int
}

// parseFormPrintOptions parses and validates print options from standard HTTP multipart form values.
// If docIndex > 0, error messages format with "in document <docIndex>" for multi-file requests.
func parseFormPrintOptions(r *http.Request, colorEnabled bool, docIndex int) (docPrintOptions, error) {
	opts := docPrintOptions{
		Copies:        1,
		Duplex:        contracts.DuplexSimplex,
		ColorMode:     "BW",
		PageRange:     strings.TrimSpace(r.FormValue("page_range")),
		Orientation:   "AUTO",
		PagesPerSheet: 1,
	}

	copiesStr := strings.TrimSpace(r.FormValue("copies"))
	if copiesStr != "" {
		c, err := strconv.Atoi(copiesStr)
		if err != nil || c < 1 || c > 999 {
			return opts, errors.New("copies must be between 1 and 999")
		}
		opts.Copies = c
	}

	duplexStr := strings.TrimSpace(r.FormValue("duplex"))
	if duplexStr != "" {
		switch strings.ToUpper(duplexStr) {
		case string(contracts.DuplexSimplex), "SINGLE", "SINGLE-SIDED", "FALSE":
			opts.Duplex = contracts.DuplexSimplex
		case string(contracts.DuplexLongEdge), "LONG_EDGE", "DUPLEX", "DOUBLE", "DOUBLE-SIDED", "TRUE":
			opts.Duplex = contracts.DuplexLongEdge
		case string(contracts.DuplexShortEdge), "SHORT_EDGE":
			opts.Duplex = contracts.DuplexShortEdge
		default:
			return opts, errors.New("invalid duplex mode")
		}
	}

	rawOrient := r.FormValue("orientation")
	orientationStr := strings.ToUpper(strings.TrimSpace(rawOrient))
	if orientationStr != "" {
		if orientationStr != "AUTO" && orientationStr != "PORTRAIT" && orientationStr != "LANDSCAPE" {
			if docIndex > 0 {
				return opts, fmt.Errorf("invalid orientation %q in document %d: only AUTO, PORTRAIT, and LANDSCAPE are supported", rawOrient, docIndex)
			}
			return opts, fmt.Errorf("invalid orientation %q: only AUTO, PORTRAIT, and LANDSCAPE are supported", rawOrient)
		}
		opts.Orientation = orientationStr
	}

	nupStr := strings.TrimSpace(r.FormValue("pages_per_sheet"))
	if nupStr == "" {
		nupStr = strings.TrimSpace(r.FormValue("nup"))
	}
	if nupStr != "" {
		n, err := strconv.Atoi(nupStr)
		if err != nil || (n != 1 && n != 2 && n != 4 && n != 6) {
			if docIndex > 0 {
				return opts, fmt.Errorf("invalid pages_per_sheet %q in document %d: only 1, 2, 4, and 6 are supported", nupStr, docIndex)
			}
			return opts, fmt.Errorf("invalid pages_per_sheet %q: only 1, 2, 4, and 6 are supported", nupStr)
		}
		opts.PagesPerSheet = n
	}

	if strings.EqualFold(r.FormValue("color_mode"), "COLOR") && colorEnabled {
		opts.ColorMode = "COLOR"
	}

	return opts, nil
}

// parseMetadataPrintOptions parses and validates print options from a multi-file metadata item.
func parseMetadataPrintOptions(meta docItemMeta, colorEnabled bool, docIndex int) (docPrintOptions, error) {
	opts := docPrintOptions{
		Copies:        1,
		Duplex:        parseDuplex(meta.Duplex),
		ColorMode:     "BW",
		PageRange:     strings.TrimSpace(meta.PageRange),
		Orientation:   "AUTO",
		PagesPerSheet: 1,
	}

	if meta.Copies >= 1 && meta.Copies <= 999 {
		opts.Copies = meta.Copies
	}

	if strings.EqualFold(meta.ColorMode, "COLOR") && colorEnabled {
		opts.ColorMode = "COLOR"
	}

	if meta.Orientation != "" {
		orient := strings.ToUpper(strings.TrimSpace(meta.Orientation))
		if orient != "AUTO" && orient != "PORTRAIT" && orient != "LANDSCAPE" {
			return opts, fmt.Errorf("invalid orientation %q in document %d: only AUTO, PORTRAIT, and LANDSCAPE are supported", meta.Orientation, docIndex)
		}
		opts.Orientation = orient
	}

	nup := meta.PagesPerSheet
	if nup <= 0 && meta.Nup > 0 {
		nup = meta.Nup
	}
	if nup > 0 {
		if nup != 1 && nup != 2 && nup != 4 && nup != 6 {
			return opts, fmt.Errorf("invalid pages_per_sheet %d in document %d: only 1, 2, 4, and 6 are supported", nup, docIndex)
		}
		opts.PagesPerSheet = nup
	}

	return opts, nil
}

// preparedInputDocument represents a verified input document saved to disk and
// optionally converted from office/image formats to PDF, with guaranteed cleanup.
type preparedInputDocument struct {
	PDFPath string
	cleanup func()
}

// prepareInputPDF validates the file extension, streams the uploaded reader to a temporary file,
// converts non-PDF formats (Office documents, images) to standard PDF via headless LibreOffice,
// and returns the prepared PDF path along with a safe cleanup closure.
func prepareInputPDF(
	ctx context.Context,
	file io.Reader,
	filename string,
	jobsDir string,
	prefix string,
	docIndex int,
) (*preparedInputDocument, error) {
	if !pdf.IsSupportedFormat(filename) {
		if docIndex > 0 {
			return nil, fmt.Errorf("unsupported file format %q in document %d", filepath.Ext(filename), docIndex)
		}
		return nil, fmt.Errorf("unsupported file format %q: only PDF, Office documents (DOCX, PPTX, XLSX), and Images (JPG, PNG) are supported", filepath.Ext(filename))
	}

	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" {
		ext = ".pdf"
	}

	tmpInput, err := os.CreateTemp(jobsDir, prefix+ext)
	if err != nil {
		return nil, errors.New("failed to create temp file")
	}
	tmpInputName := tmpInput.Name()

	if _, err := io.Copy(tmpInput, file); err != nil {
		_ = tmpInput.Close()
		_ = os.Remove(tmpInputName)
		return nil, errors.New("failed to save uploaded file")
	}
	_ = tmpInput.Close()

	if ext == ".pdf" {
		return &preparedInputDocument{
			PDFPath: tmpInputName,
			cleanup: func() {
				_ = os.Remove(tmpInputName)
			},
		}, nil
	}

	// Non-PDF formats (DOCX, PPTX, XLSX, images): convert via LibreOffice worker
	tmpConverted, err := os.CreateTemp(jobsDir, strings.Replace(prefix, "_in_", "_conv_", 1)+".pdf")
	if err != nil {
		_ = os.Remove(tmpInputName)
		return nil, errors.New("failed to create temp file for conversion")
	}
	tmpConvertedName := tmpConverted.Name()
	_ = tmpConverted.Close()

	if err := pdf.ConvertToPDF(ctx, tmpInputName, filename, tmpConvertedName); err != nil {
		_ = os.Remove(tmpInputName)
		_ = os.Remove(tmpConvertedName)
		if docIndex > 0 {
			return nil, fmt.Errorf("conversion failed for %q: %v", filename, err)
		}
		return nil, fmt.Errorf("document conversion failed: %v", err)
	}
	_ = os.Remove(tmpInputName)

	return &preparedInputDocument{
		PDFPath: tmpConvertedName,
		cleanup: func() {
			_ = os.Remove(tmpConvertedName)
		},
	}, nil
}

// normalizedDocResult contains the authoritative output from normalization, pricing, and sheet accounting.
type normalizedDocResult struct {
	NormResult   *pdf.NormalizedResult
	UnitPrice    int64
	PrintedSides int
	PricePaise   int64
}

// executeNormalizationAndPricing executes canonical normalization on the prepared input PDF
// and calculates authoritative unit price and total price in integer paise.
func (s *appServer) executeNormalizationAndPricing(
	ctx context.Context,
	inputPDFPath string,
	outputPath string,
	opts docPrintOptions,
	pricingConfig store.PricingConfig,
	hasPricing bool,
) (*normalizedDocResult, error) {
	normRes, err := pdf.NormalizeAndCountWithOptions(ctx, inputPDFPath, outputPath, pdf.NormalizeOptions{
		Copies:        opts.Copies,
		Duplex:        opts.Duplex,
		ColorMode:     opts.ColorMode,
		PageRange:     opts.PageRange,
		Orientation:   opts.Orientation,
		PagesPerSheet: opts.PagesPerSheet,
	})
	if err != nil {
		return nil, err
	}

	unitPrice := s.resolveUnitPrice(pricingConfig, hasPricing, opts.ColorMode, opts.Duplex)
	printedSides := normRes.TotalPages * opts.Copies
	docPrice, err := pricing.CalculatePricePaise(printedSides, unitPrice)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate price: %w", err)
	}

	return &normalizedDocResult{
		NormResult:   normRes,
		UnitPrice:    unitPrice,
		PrintedSides: printedSides,
		PricePaise:   docPrice,
	}, nil
}
