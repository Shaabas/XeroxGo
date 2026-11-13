package pdf

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"xeroxgo/pkg/contracts"
)

// generateCustomSizedPDF generates a test PDF with explicit width/height in PostScript points.
func generateCustomSizedPDF(t *testing.T, dir string, width, height int, pages int) string {
	t.Helper()
	psPath := filepath.Join(dir, fmt.Sprintf("custom_%dx%d.ps", width, height))
	pdfPath := filepath.Join(dir, fmt.Sprintf("custom_%dx%d.pdf", width, height))

	var ps strings.Builder
	for p := 1; p <= pages; p++ {
		ps.WriteString(fmt.Sprintf(`<< /PageSize [%d %d] >> setpagedevice
/Helvetica findfont 24 scalefont setfont
100 %d moveto
(Custom Dimension Page %d of %d) show
showpage
`, width, height, height-100, p, pages))
	}

	if err := os.WriteFile(psPath, []byte(ps.String()), 0600); err != nil {
		t.Fatalf("failed to write custom PS fixture: %v", err)
	}

	cmd := exec.Command("gs",
		"-sDEVICE=pdfwrite",
		"-dNOPAUSE",
		"-dBATCH",
		"-dQUIET",
		"-sOutputFile="+pdfPath,
		psPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to generate custom-sized PDF with Ghostscript: %s: %v", strings.TrimSpace(string(out)), err)
	}

	return pdfPath
}

// TestDocumentEngine_NUp4And6DuplexOddPadding verifies that 4-up and 6-up imposition
// with odd content sides properly append a blank back-page for duplex jobs and compute
// the correct physical sheets and billable sides.
func TestDocumentEngine_NUp4And6DuplexOddPadding(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()

	// 1. 9-page document with 4-up Duplex
	// 9 content pages / 4-up = 3 imposed sides (odd)
	// Duplex with odd sides requires 1 blank padding page -> 4 pages in PDF, 2 physical sheets
	input9 := generateMultiPagePDF(t, tmpDir, 9, false)
	out9Duplex := filepath.Join(tmpDir, "out_9p_4up_duplex.pdf")

	res9, err := NormalizeAndCountWithOptions(ctx, input9, out9Duplex, NormalizeOptions{
		Copies:        1,
		Duplex:        contracts.DuplexLongEdge,
		PagesPerSheet: 4,
	})
	if err != nil {
		t.Fatalf("9-page 4-up duplex normalization failed: %v", err)
	}

	if res9.TotalPages != 3 {
		t.Errorf("TotalPages (billed sides) = %d, want 3", res9.TotalPages)
	}
	if res9.TotalSheets != 2 {
		t.Errorf("TotalSheets = %d, want 2 physical sheets", res9.TotalSheets)
	}
	if res9.PaddedPages != 4 {
		t.Errorf("PaddedPages = %d, want 4 (3 content + 1 blank padding)", res9.PaddedPages)
	}

	// Verify content of Sheet 1 (Pages 1..4), Sheet 2 (Pages 5..8), Sheet 3 (Page 9)
	s1 := extractPageText(t, out9Duplex, 1)
	for p := 1; p <= 4; p++ {
		if !strings.Contains(s1, fmt.Sprintf("Page %d", p)) {
			t.Errorf("Sheet 1 missing Page %d, got: %q", p, s1)
		}
	}
	s2 := extractPageText(t, out9Duplex, 2)
	for p := 5; p <= 8; p++ {
		if !strings.Contains(s2, fmt.Sprintf("Page %d", p)) {
			t.Errorf("Sheet 2 missing Page %d, got: %q", p, s2)
		}
	}
	s3 := extractPageText(t, out9Duplex, 3)
	if !strings.Contains(s3, "Page 9") {
		t.Errorf("Sheet 3 missing Page 9, got: %q", s3)
	}
	// Sheet 4 must be blank padding
	s4 := extractPageText(t, out9Duplex, 4)
	if strings.TrimSpace(s4) != "" {
		t.Errorf("Sheet 4 should be blank padding, got: %q", s4)
	}

	// 2. 13-page document with 6-up Duplex
	// 13 content pages / 6-up = 3 imposed sides (odd)
	// Duplex with odd sides requires 1 blank padding page -> 4 pages in PDF, 2 physical sheets
	input13 := generateMultiPagePDF(t, tmpDir, 13, false)
	out13Duplex := filepath.Join(tmpDir, "out_13p_6up_duplex.pdf")

	res13, err := NormalizeAndCountWithOptions(ctx, input13, out13Duplex, NormalizeOptions{
		Copies:        1,
		Duplex:        contracts.DuplexLongEdge,
		PagesPerSheet: 6,
	})
	if err != nil {
		t.Fatalf("13-page 6-up duplex normalization failed: %v", err)
	}

	if res13.TotalPages != 3 {
		t.Errorf("TotalPages (billed sides) = %d, want 3", res13.TotalPages)
	}
	if res13.TotalSheets != 2 {
		t.Errorf("TotalSheets = %d, want 2 physical sheets", res13.TotalSheets)
	}
	if res13.PaddedPages != 4 {
		t.Errorf("PaddedPages = %d, want 4 (3 content + 1 blank padding)", res13.PaddedPages)
	}

	// Verify content of Sheet 1 (Pages 1..6), Sheet 2 (Pages 7..12), Sheet 3 (Page 13)
	s13_1 := extractPageText(t, out13Duplex, 1)
	for p := 1; p <= 6; p++ {
		if !strings.Contains(s13_1, fmt.Sprintf("Page %d", p)) {
			t.Errorf("Sheet 1 missing Page %d, got: %q", p, s13_1)
		}
	}
	s13_2 := extractPageText(t, out13Duplex, 2)
	for p := 7; p <= 12; p++ {
		if !strings.Contains(s13_2, fmt.Sprintf("Page %d", p)) {
			t.Errorf("Sheet 2 missing Page %d, got: %q", p, s13_2)
		}
	}
	s13_3 := extractPageText(t, out13Duplex, 3)
	if !strings.Contains(s13_3, "Page 13") {
		t.Errorf("Sheet 3 missing Page 13, got: %q", s13_3)
	}
	// Sheet 4 must be blank padding
	s13_4 := extractPageText(t, out13Duplex, 4)
	if strings.TrimSpace(s13_4) != "" {
		t.Errorf("Sheet 4 should be blank padding, got: %q", s13_4)
	}
}

// TestDocumentEngine_A4NormalizationGeometry verifies that arbitrary input dimensions
// (e.g. US Letter 612x792 pt, Legal 612x1008 pt) are strictly standardized to ISO A4.
func TestDocumentEngine_A4NormalizationGeometry(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()

	// US Letter input (612 x 792 pt)
	letterPDF := generateCustomSizedPDF(t, tmpDir, 612, 792, 2)
	outPortrait := filepath.Join(tmpDir, "out_letter_to_a4_portrait.pdf")

	resPort, err := NormalizeAndCountWithOptions(ctx, letterPDF, outPortrait, NormalizeOptions{
		Copies:        1,
		Duplex:        contracts.DuplexSimplex,
		PagesPerSheet: 1,
		Orientation:   "PORTRAIT",
	})
	if err != nil {
		t.Fatalf("A4 portrait normalization of Letter failed: %v", err)
	}
	if resPort.TotalPages != 2 || resPort.TotalSheets != 2 {
		t.Errorf("Letter to A4: TotalPages=%d, TotalSheets=%d (want 2, 2)", resPort.TotalPages, resPort.TotalSheets)
	}

	// Verify exact A4 dimensions: 595.28 x 841.89 points (tolerance ± 2.0 pt)
	w1, h1 := extractPageDimensions(t, outPortrait, 1)
	if math.Abs(w1-595.28) > 2.0 || math.Abs(h1-841.89) > 2.0 {
		t.Errorf("Page 1 dimension mismatch for A4 portrait: got %.2f x %.2f, want ~595.28 x ~841.89", w1, h1)
	}

	w2, h2 := extractPageDimensions(t, outPortrait, 2)
	if math.Abs(w2-595.28) > 2.0 || math.Abs(h2-841.89) > 2.0 {
		t.Errorf("Page 2 dimension mismatch for A4 portrait: got %.2f x %.2f, want ~595.28 x ~841.89", w2, h2)
	}

	// Landscape normalization to A4: 841.89 x 595.28 points
	outLandscape := filepath.Join(tmpDir, "out_letter_to_a4_landscape.pdf")
	resLand, err := NormalizeAndCountWithOptions(ctx, letterPDF, outLandscape, NormalizeOptions{
		Copies:        1,
		Duplex:        contracts.DuplexSimplex,
		PagesPerSheet: 1,
		Orientation:   "LANDSCAPE",
	})
	if err != nil {
		t.Fatalf("A4 landscape normalization of Letter failed: %v", err)
	}
	if resLand.TotalPages != 2 || resLand.TotalSheets != 2 {
		t.Errorf("Letter to A4 Landscape: TotalPages=%d, TotalSheets=%d (want 2, 2)", resLand.TotalPages, resLand.TotalSheets)
	}

	wL, hL := extractPageDimensions(t, outLandscape, 1)
	if math.Abs(wL-841.89) > 2.0 || math.Abs(hL-595.28) > 2.0 {
		t.Errorf("Page 1 dimension mismatch for A4 landscape: got %.2f x %.2f, want ~841.89 x ~595.28", wL, hL)
	}
}

// TestDocumentEngine_PageRangeSubsetsAndReordering verifies custom page selection
// with reordering ("5,3,1") and non-contiguous N-up imposition ("2,4,6" with 2-up).
func TestDocumentEngine_PageRangeSubsetsAndReordering(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()

	input6 := generateMultiPagePDF(t, tmpDir, 6, false)

	// 1. Reordered page range "5,3,1"
	outReordered := filepath.Join(tmpDir, "out_reordered.pdf")
	resReorder, err := NormalizeAndCountWithOptions(ctx, input6, outReordered, NormalizeOptions{
		Copies:    1,
		Duplex:    contracts.DuplexSimplex,
		PageRange: "5,3,1",
	})
	if err != nil {
		t.Fatalf("Reordered page range failed: %v", err)
	}
	if resReorder.TotalPages != 3 || resReorder.TotalSheets != 3 {
		t.Errorf("Reordered: TotalPages=%d, TotalSheets=%d (want 3, 3)", resReorder.TotalPages, resReorder.TotalSheets)
	}

	txtP1 := extractPageText(t, outReordered, 1)
	if !strings.Contains(txtP1, "Page 5") {
		t.Errorf("Page 1 should contain Page 5, got: %q", txtP1)
	}
	txtP2 := extractPageText(t, outReordered, 2)
	if !strings.Contains(txtP2, "Page 3") {
		t.Errorf("Page 2 should contain Page 3, got: %q", txtP2)
	}
	txtP3 := extractPageText(t, outReordered, 3)
	if !strings.Contains(txtP3, "Page 1") {
		t.Errorf("Page 3 should contain Page 1, got: %q", txtP3)
	}

	// 2. Non-contiguous range "2,4,6" with 2-up imposition
	// 3 selected pages / 2-up = 2 output sides
	outNupRange := filepath.Join(tmpDir, "out_nup_range.pdf")
	resNupRange, err := NormalizeAndCountWithOptions(ctx, input6, outNupRange, NormalizeOptions{
		Copies:        1,
		Duplex:        contracts.DuplexSimplex,
		PageRange:     "2,4,6",
		PagesPerSheet: 2,
	})
	if err != nil {
		t.Fatalf("Non-contiguous 2-up range failed: %v", err)
	}
	if resNupRange.TotalPages != 2 || resNupRange.TotalSheets != 2 {
		t.Errorf("Non-contiguous 2-up: TotalPages=%d, TotalSheets=%d (want 2, 2)", resNupRange.TotalPages, resNupRange.TotalSheets)
	}

	s1 := extractPageText(t, outNupRange, 1)
	if !strings.Contains(s1, "Page 2") || !strings.Contains(s1, "Page 4") {
		t.Errorf("Sheet 1 missing Page 2 or Page 4, got: %q", s1)
	}
	s2 := extractPageText(t, outNupRange, 2)
	if !strings.Contains(s2, "Page 6") {
		t.Errorf("Sheet 2 missing Page 6, got: %q", s2)
	}
}

// TestDocumentEngine_CopiesScalingAndOverflow verifies sheet count calculation
// across multiple copies and validates arithmetic overflow protection.
func TestDocumentEngine_CopiesScalingAndOverflow(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()

	input3 := generateThreePagePDF(t, tmpDir)

	// 1. Simplex 3 pages, 4 copies = 3 sides, 12 total sheets
	outSimplex := filepath.Join(tmpDir, "out_sim_4copies.pdf")
	resSimp, err := NormalizeAndCountWithOptions(ctx, input3, outSimplex, NormalizeOptions{
		Copies: 4,
		Duplex: contracts.DuplexSimplex,
	})
	if err != nil {
		t.Fatalf("4 copies simplex failed: %v", err)
	}
	if resSimp.TotalPages != 3 {
		t.Errorf("TotalPages = %d, want 3 content sides per copy", resSimp.TotalPages)
	}
	if resSimp.TotalSheets != 12 {
		t.Errorf("TotalSheets = %d, want 12 (3 sheets * 4 copies)", resSimp.TotalSheets)
	}

	// 2. Duplex 3 pages, 3 copies:
	// 3 content pages -> sheetsPerCopy = ceil(3/2) = 2 sheets.
	// TotalSheets = 2 * 3 = 6 physical sheets.
	// TotalPages = 3 content sides.
	outDuplex := filepath.Join(tmpDir, "out_dup_3copies.pdf")
	resDup, err := NormalizeAndCountWithOptions(ctx, input3, outDuplex, NormalizeOptions{
		Copies: 3,
		Duplex: contracts.DuplexLongEdge,
	})
	if err != nil {
		t.Fatalf("3 copies duplex failed: %v", err)
	}
	if resDup.TotalPages != 3 {
		t.Errorf("TotalPages = %d, want 3 content sides per copy", resDup.TotalPages)
	}
	if resDup.TotalSheets != 6 {
		t.Errorf("TotalSheets = %d, want 6 (2 sheets * 3 copies)", resDup.TotalSheets)
	}

	// 3. Overflow protection with extreme copies count
	outOverflow := filepath.Join(tmpDir, "out_overflow.pdf")
	_, errOverflow := NormalizeAndCountWithOptions(ctx, input3, outOverflow, NormalizeOptions{
		Copies: math.MaxInt,
		Duplex: contracts.DuplexSimplex,
	})
	if errOverflow == nil || !strings.Contains(errOverflow.Error(), "arithmetic overflow") {
		t.Errorf("expected arithmetic overflow error for MaxInt copies, got: %v", errOverflow)
	}
}

// TestDocumentEngine_CorruptEmptyAndEncryptedHandling verifies that empty files,
// corrupt binary payloads, and password-protected encrypted PDFs are cleanly rejected.
func TestDocumentEngine_CorruptEmptyAndEncryptedHandling(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()

	// 1. Empty (0-byte) file
	emptyFile := filepath.Join(tmpDir, "empty.pdf")
	if err := os.WriteFile(emptyFile, []byte{}, 0600); err != nil {
		t.Fatalf("failed to write empty file: %v", err)
	}
	_, errEmpty := NormalizeAndCountWithOptions(ctx, emptyFile, filepath.Join(tmpDir, "out_empty.pdf"), NormalizeOptions{
		Copies: 1,
		Duplex: contracts.DuplexSimplex,
	})
	if errEmpty == nil {
		t.Error("expected error for 0-byte empty file, got nil")
	}

	// 2. Corrupted file (random text bytes)
	corruptFile := filepath.Join(tmpDir, "corrupt.pdf")
	if err := os.WriteFile(corruptFile, []byte("NOT_A_VALID_PDF_HEADER_DATA_GARBAGE"), 0600); err != nil {
		t.Fatalf("failed to write corrupt file: %v", err)
	}
	_, errCorrupt := NormalizeAndCountWithOptions(ctx, corruptFile, filepath.Join(tmpDir, "out_corrupt.pdf"), NormalizeOptions{
		Copies: 1,
		Duplex: contracts.DuplexSimplex,
	})
	if errCorrupt == nil {
		t.Error("expected error for corrupt file, got nil")
	}

	// 3a. Encrypted PDF with User Password (observed: rejected at qpdf --check with exit code 2 "invalid password")
	validPDF := generateThreePagePDF(t, tmpDir)
	userEncryptedPDF := filepath.Join(tmpDir, "user_encrypted.pdf")
	cmdUser := exec.Command("qpdf", "--encrypt", "user-password", "owner-password", "256", "--", validPDF, userEncryptedPDF)
	outUser, errUser := cmdUser.CombinedOutput()
	if errUser != nil {
		t.Fatalf("failed to generate user-encrypted fixture PDF with qpdf: %s: %v", strings.TrimSpace(string(outUser)), errUser)
	}

	_, errUserEnc := NormalizeAndCountWithOptions(ctx, userEncryptedPDF, filepath.Join(tmpDir, "out_user_enc.pdf"), NormalizeOptions{
		Copies: 1,
		Duplex: contracts.DuplexSimplex,
	})
	if errUserEnc == nil {
		t.Error("expected error for user-password encrypted file, got nil")
	}
	if !strings.Contains(errUserEnc.Error(), "invalid password") && !strings.Contains(errUserEnc.Error(), "pdf is encrypted") {
		t.Errorf("expected rejection indicating encrypted/password failure, got: %v", errUserEnc)
	}

	// 3b. Encrypted PDF with Owner-only Password (empty user password)
	// Observed: passes qpdf --check, then rejected by qpdf --is-encrypted returning "pdf is encrypted"
	ownerEncryptedPDF := filepath.Join(tmpDir, "owner_encrypted.pdf")
	cmdOwner := exec.Command("qpdf", "--encrypt", "", "owner-password", "256", "--", validPDF, ownerEncryptedPDF)
	outOwner, errOwner := cmdOwner.CombinedOutput()
	if errOwner != nil {
		t.Fatalf("failed to generate owner-encrypted fixture PDF with qpdf: %s: %v", strings.TrimSpace(string(outOwner)), errOwner)
	}

	_, errOwnerEnc := NormalizeAndCountWithOptions(ctx, ownerEncryptedPDF, filepath.Join(tmpDir, "out_owner_enc.pdf"), NormalizeOptions{
		Copies: 1,
		Duplex: contracts.DuplexSimplex,
	})
	if errOwnerEnc == nil || !strings.Contains(errOwnerEnc.Error(), "pdf is encrypted") {
		t.Errorf("expected 'pdf is encrypted' error for owner-encrypted PDF, got: %v", errOwnerEnc)
	}

	// 4. Input path == Output path collision
	_, errSame := NormalizeAndCountWithOptions(ctx, validPDF, validPDF, NormalizeOptions{
		Copies: 1,
		Duplex: contracts.DuplexSimplex,
	})
	if errSame == nil || !strings.Contains(errSame.Error(), "same file") {
		t.Errorf("expected 'same file' error, got: %v", errSame)
	}
}
