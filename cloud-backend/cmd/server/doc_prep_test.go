package main

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

func TestDocPrep_ParseFormPrintOptions_Valid(t *testing.T) {
	// 1. Defaults when empty
	reqEmpty, _ := http.NewRequest("POST", "/upload", nil)
	opts, err := parseFormPrintOptions(reqEmpty, false, 0)
	if err != nil {
		t.Fatalf("unexpected error parsing empty form: %v", err)
	}
	if opts.Copies != 1 || opts.Duplex != contracts.DuplexSimplex || opts.ColorMode != "BW" || opts.Orientation != "AUTO" || opts.PagesPerSheet != 1 {
		t.Fatalf("unexpected defaults: %+v", opts)
	}

	// 2. Custom valid options with color enabled
	form := url.Values{}
	form.Set("copies", "5")
	form.Set("duplex", "LONG_EDGE")
	form.Set("orientation", "LANDSCAPE")
	form.Set("pages_per_sheet", "2")
	form.Set("color_mode", "COLOR")
	form.Set("page_range", "1-4")

	reqCustom, _ := http.NewRequest("POST", "/upload", strings.NewReader(form.Encode()))
	reqCustom.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = reqCustom.ParseForm()

	optsCustom, err := parseFormPrintOptions(reqCustom, true, 0)
	if err != nil {
		t.Fatalf("unexpected error parsing custom form: %v", err)
	}
	if optsCustom.Copies != 5 || optsCustom.Duplex != contracts.DuplexLongEdge || optsCustom.ColorMode != "COLOR" ||
		optsCustom.Orientation != "LANDSCAPE" || optsCustom.PagesPerSheet != 2 || optsCustom.PageRange != "1-4" {
		t.Fatalf("unexpected custom options: %+v", optsCustom)
	}

	// 3. Color mode fallback when colorDisabled
	optsNoColor, err := parseFormPrintOptions(reqCustom, false, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if optsNoColor.ColorMode != "BW" {
		t.Fatalf("expected BW fallback when color disabled, got %s", optsNoColor.ColorMode)
	}

	// 4. Duplex synonyms
	synonyms := []struct {
		val      string
		expected contracts.DuplexMode
	}{
		{"SHORT_EDGE", contracts.DuplexShortEdge},
		{"DOUBLE", contracts.DuplexLongEdge},
		{"DOUBLE-SIDED", contracts.DuplexLongEdge},
		{"DUPLEX", contracts.DuplexLongEdge},
		{"TRUE", contracts.DuplexLongEdge},
		{"SINGLE", contracts.DuplexSimplex},
		{"SINGLE-SIDED", contracts.DuplexSimplex},
		{"FALSE", contracts.DuplexSimplex},
	}
	for _, syn := range synonyms {
		f := url.Values{}
		f.Set("duplex", syn.val)
		r, _ := http.NewRequest("POST", "/upload", strings.NewReader(f.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		_ = r.ParseForm()
		o, err := parseFormPrintOptions(r, false, 0)
		if err != nil {
			t.Fatalf("failed parsing duplex synonym %s: %v", syn.val, err)
		}
		if o.Duplex != syn.expected {
			t.Fatalf("duplex synonym %s expected %s, got %s", syn.val, syn.expected, o.Duplex)
		}
	}
}

func TestDocPrep_ParseFormPrintOptions_Errors(t *testing.T) {
	cases := []struct {
		name       string
		field      string
		val        string
		docIdx     int
		wantErrSub string
	}{
		{"copies zero", "copies", "0", 0, "copies must be between 1 and 999"},
		{"copies excessive", "copies", "1000", 0, "copies must be between 1 and 999"},
		{"copies non-int", "copies", "abc", 0, "copies must be between 1 and 999"},
		{"invalid duplex", "duplex", "TRIPLEX", 0, "invalid duplex mode"},
		{"invalid orientation single", "orientation", "DIAGONAL", 0, "invalid orientation \"DIAGONAL\": only AUTO, PORTRAIT, and LANDSCAPE are supported"},
		{"invalid orientation multi", "orientation", "DIAGONAL", 3, "invalid orientation \"DIAGONAL\" in document 3: only AUTO, PORTRAIT, and LANDSCAPE are supported"},
		{"invalid nup single", "pages_per_sheet", "3", 0, "invalid pages_per_sheet \"3\": only 1, 2, 4, and 6 are supported"},
		{"invalid nup multi", "pages_per_sheet", "5", 2, "invalid pages_per_sheet \"5\" in document 2: only 1, 2, 4, and 6 are supported"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{}
			form.Set(tc.field, tc.val)
			req, _ := http.NewRequest("POST", "/upload", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			_ = req.ParseForm()

			_, err := parseFormPrintOptions(req, true, tc.docIdx)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrSub)
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("expected error containing %q, got %q", tc.wantErrSub, err.Error())
			}
		})
	}
}

func TestDocPrep_ParseMetadataPrintOptions(t *testing.T) {
	// Valid metadata
	meta := docItemMeta{
		Copies:        4,
		Duplex:        "LONG_EDGE",
		ColorMode:     "COLOR",
		PageRange:     "2-5",
		Orientation:   "PORTRAIT",
		PagesPerSheet: 4,
	}
	opts, err := parseMetadataPrintOptions(meta, true, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Copies != 4 || opts.Duplex != contracts.DuplexLongEdge || opts.ColorMode != "COLOR" ||
		opts.Orientation != "PORTRAIT" || opts.PagesPerSheet != 4 || opts.PageRange != "2-5" {
		t.Fatalf("unexpected parsed options: %+v", opts)
	}

	// Gated color
	optsNoColor, err := parseMetadataPrintOptions(meta, false, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if optsNoColor.ColorMode != "BW" {
		t.Fatalf("expected BW when color disabled, got %s", optsNoColor.ColorMode)
	}

	// Invalid orientation
	metaInvalidOrient := meta
	metaInvalidOrient.Orientation = "UPSIDE_DOWN"
	_, err = parseMetadataPrintOptions(metaInvalidOrient, true, 2)
	if err == nil || !strings.Contains(err.Error(), "invalid orientation \"UPSIDE_DOWN\" in document 2") {
		t.Fatalf("expected orientation error, got %v", err)
	}

	// Invalid nup
	metaInvalidNup := meta
	metaInvalidNup.PagesPerSheet = 8
	_, err = parseMetadataPrintOptions(metaInvalidNup, true, 4)
	if err == nil || !strings.Contains(err.Error(), "invalid pages_per_sheet 8 in document 4") {
		t.Fatalf("expected pages_per_sheet error, got %v", err)
	}
}

func TestDocPrep_PrepareInputPDF_Lifecycle(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Unsupported format (docIndex = 0)
	_, err := prepareInputPDF(context.Background(), bytes.NewReader([]byte("xyz")), "script.sh", tmpDir, "job_", 0)
	if err == nil || !strings.Contains(err.Error(), "unsupported file format \".sh\": only PDF") {
		t.Fatalf("expected unsupported format error, got %v", err)
	}

	// 2. Unsupported format (docIndex = 2)
	_, err = prepareInputPDF(context.Background(), bytes.NewReader([]byte("xyz")), "binary.exe", tmpDir, "job_", 2)
	if err == nil || !strings.Contains(err.Error(), "unsupported file format \".exe\" in document 2") {
		t.Fatalf("expected multi-file unsupported format error, got %v", err)
	}

	// 3. Valid PDF lifecycle and cleanup
	dummyPDF := []byte("%PDF-1.4\n%EOF\n")
	prep, err := prepareInputPDF(context.Background(), bytes.NewReader(dummyPDF), "sample.pdf", tmpDir, "job_", 0)
	if err != nil {
		t.Fatalf("failed to prepare PDF: %v", err)
	}
	if _, err := os.Stat(prep.PDFPath); err != nil {
		t.Fatalf("expected temp PDF to exist on disk: %v", err)
	}

	// Ensure cleanup deletes file
	prep.cleanup()
	if _, err := os.Stat(prep.PDFPath); !os.IsNotExist(err) {
		t.Fatalf("expected temp PDF to be removed after cleanup: %v", err)
	}
}

func TestDocPrep_ExecuteNormalizationAndPricing(t *testing.T) {
	app, _ := setupDocPricingBaselineServer(t)
	tmpDir := t.TempDir()

	// Create 2-page test PDF
	pdfPath := createNamedDocPDF(t, tmpDir, "doc_prep_test", 2)
	outPath := filepath.Join(tmpDir, "normalized_out.pdf")

	opts := docPrintOptions{
		Copies:        2,
		Duplex:        contracts.DuplexSimplex,
		ColorMode:     "BW",
		PageRange:     "",
		Orientation:   "AUTO",
		PagesPerSheet: 1,
	}

	pricing := store.PricingConfig{
		BWSimplexPricePaise: 200,
		BWDuplexPricePaise:  150,
	}

	res, err := app.executeNormalizationAndPricing(context.Background(), pdfPath, outPath, opts, pricing, true)
	if err != nil {
		t.Fatalf("executeNormalizationAndPricing failed: %v", err)
	}

	if res.NormResult == nil {
		t.Fatal("expected non-nil NormResult")
	}
	if res.NormResult.TotalPages != 2 {
		t.Fatalf("expected 2 normalized pages, got %d", res.NormResult.TotalPages)
	}
	// 2 pages * 2 copies = 4 printed sides
	if res.PrintedSides != 4 {
		t.Fatalf("expected 4 printed sides, got %d", res.PrintedSides)
	}
	if res.UnitPrice != 200 {
		t.Fatalf("expected unit price 200, got %d", res.UnitPrice)
	}
	// 4 sides * 200 paise = 800 paise
	if res.PricePaise != 800 {
		t.Fatalf("expected price 800 paise, got %d", res.PricePaise)
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("expected output file to exist: %v", err)
	}
}
