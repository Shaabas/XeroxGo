package pdf

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"xeroxgo/pkg/contracts"
)

func generateThreePagePDF(t *testing.T, dir string) string {
	t.Helper()

	psPath := filepath.Join(dir, "fixture_input.ps")
	pdfPath := filepath.Join(dir, "fixture_input.pdf")

	psContent := `%!PS
/Helvetica findfont 24 scalefont setfont
100 700 moveto
(Page 1 of XeroxGo Fixture Document) show
showpage

/Helvetica findfont 24 scalefont setfont
100 700 moveto
(Page 2 of XeroxGo Fixture Document) show
showpage

/Helvetica findfont 24 scalefont setfont
100 700 moveto
(Page 3 of XeroxGo Fixture Document) show
showpage
`
	if err := os.WriteFile(psPath, []byte(psContent), 0600); err != nil {
		t.Fatalf("failed to write PostScript fixture: %v", err)
	}

	cmd := exec.Command("gs",
		"-sDEVICE=pdfwrite",
		"-sPAPERSIZE=a4",
		"-dFIXEDMEDIA",
		"-dNOPAUSE",
		"-dBATCH",
		"-dQUIET",
		"-sOutputFile="+pdfPath,
		psPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to generate 3-page fixture PDF with Ghostscript: %s: %v", strings.TrimSpace(string(out)), err)
	}

	return pdfPath
}

func verifyChecksum(t *testing.T, filePath, reportedChecksum string) {
	t.Helper()

	if len(reportedChecksum) != 64 {
		t.Fatalf("expected 64-character checksum, got %d chars: %s", len(reportedChecksum), reportedChecksum)
	}
	if reportedChecksum != strings.ToLower(reportedChecksum) {
		t.Fatalf("expected lowercase checksum, got %s", reportedChecksum)
	}
	if _, err := hex.DecodeString(reportedChecksum); err != nil {
		t.Fatalf("checksum is not valid hex: %v", err)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read file for checksum verification: %v", err)
	}
	hash := sha256.Sum256(data)
	expectedHex := hex.EncodeToString(hash[:])
	if reportedChecksum != expectedHex {
		t.Fatalf("checksum mismatch: reported %s, expected %s", reportedChecksum, expectedHex)
	}
}

func TestNormalizeAndCount_Simplex(t *testing.T) {
	tmpDir := t.TempDir()
	inputPDF := generateThreePagePDF(t, tmpDir)
	outputPDF := filepath.Join(tmpDir, "normalized_simplex.pdf")

	ctx := context.Background()
	res, err := NormalizeAndCount(ctx, inputPDF, outputPDF, 2, contracts.DuplexSimplex)
	if err != nil {
		t.Fatalf("NormalizeAndCount failed: %v", err)
	}

	if res.TotalPages != 3 {
		t.Errorf("TotalPages = %d, want 3", res.TotalPages)
	}
	if res.TotalSheets != 6 {
		t.Errorf("TotalSheets = %d, want 6", res.TotalSheets)
	}

	outInfo, err := os.Stat(outputPDF)
	if err != nil {
		t.Fatalf("normalized output file stat failed: %v", err)
	}
	if outInfo.Size() == 0 {
		t.Fatalf("normalized output file is empty")
	}

	verifyChecksum(t, outputPDF, res.SHA256Checksum)
}

func TestNormalizeAndCount_Duplex(t *testing.T) {
	tmpDir := t.TempDir()
	inputPDF := generateThreePagePDF(t, tmpDir)

	tests := []struct {
		name        string
		duplex      contracts.DuplexMode
		copies      int
		wantPages   int
		wantSheets  int
		outFileName string
	}{
		{
			name:        "duplex_long_edge_2_copies",
			duplex:      contracts.DuplexLongEdge,
			copies:      2,
			wantPages:   3,
			wantSheets:  4,
			outFileName: "normalized_duplex_long.pdf",
		},
		{
			name:        "duplex_short_edge_1_copy",
			duplex:      contracts.DuplexShortEdge,
			copies:      1,
			wantPages:   3,
			wantSheets:  2,
			outFileName: "normalized_duplex_short.pdf",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outputPDF := filepath.Join(tmpDir, tt.outFileName)
			ctx := context.Background()

			res, err := NormalizeAndCount(ctx, inputPDF, outputPDF, tt.copies, tt.duplex)
			if err != nil {
				t.Fatalf("NormalizeAndCount failed: %v", err)
			}

			if res.TotalPages != tt.wantPages {
				t.Errorf("TotalPages = %d, want %d", res.TotalPages, tt.wantPages)
			}
			if res.TotalSheets != tt.wantSheets {
				t.Errorf("TotalSheets = %d, want %d", res.TotalSheets, tt.wantSheets)
			}

			outInfo, err := os.Stat(outputPDF)
			if err != nil {
				t.Fatalf("output file stat failed: %v", err)
			}
			if outInfo.Size() == 0 {
				t.Fatalf("output file is empty")
			}

			verifyChecksum(t, outputPDF, res.SHA256Checksum)
		})
	}
}

func TestNormalizeAndCount_RejectsCorruptPDF(t *testing.T) {
	tmpDir := t.TempDir()
	corruptPDF := filepath.Join(tmpDir, "corrupt.pdf")
	outputPDF := filepath.Join(tmpDir, "output.pdf")

	if err := os.WriteFile(corruptPDF, []byte("%PDF-1.7 corrupt invalid data content"), 0600); err != nil {
		t.Fatalf("failed to write corrupt file: %v", err)
	}

	ctx := context.Background()
	_, err := NormalizeAndCount(ctx, corruptPDF, outputPDF, 1, contracts.DuplexSimplex)
	if err == nil {
		t.Fatalf("NormalizeAndCount expected error on corrupt PDF, got nil")
	}
}

func TestNormalizeAndCount_RejectsEncryptedPDF(t *testing.T) {
	tmpDir := t.TempDir()
	validPDF := generateThreePagePDF(t, tmpDir)
	encryptedPDF := filepath.Join(tmpDir, "encrypted.pdf")
	outputPDF := filepath.Join(tmpDir, "output.pdf")

	cmd := exec.Command("qpdf", "--encrypt", "user-password", "owner-password", "256", "--", validPDF, encryptedPDF)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to generate encrypted fixture PDF with qpdf: %s: %v", strings.TrimSpace(string(out)), err)
	}

	ctx := context.Background()
	_, err = NormalizeAndCount(ctx, encryptedPDF, outputPDF, 1, contracts.DuplexSimplex)
	if err == nil {
		t.Fatalf("NormalizeAndCount expected error on encrypted PDF, got nil")
	}
}

func TestNormalizeAndCount_RejectsInvalidArguments(t *testing.T) {
	tmpDir := t.TempDir()
	validPDF := generateThreePagePDF(t, tmpDir)
	outputPDF := filepath.Join(tmpDir, "output.pdf")
	ctx := context.Background()

	tests := []struct {
		name       string
		ctx        context.Context
		inputPath  string
		outputPath string
		copies     int
		duplex     contracts.DuplexMode
	}{
		{
			name:       "nil_context",
			ctx:        nil,
			inputPath:  validPDF,
			outputPath: outputPDF,
			copies:     1,
			duplex:     contracts.DuplexSimplex,
		},
		{
			name:       "zero_copies",
			ctx:        ctx,
			inputPath:  validPDF,
			outputPath: outputPDF,
			copies:     0,
			duplex:     contracts.DuplexSimplex,
		},
		{
			name:       "negative_copies",
			ctx:        ctx,
			inputPath:  validPDF,
			outputPath: outputPDF,
			copies:     -1,
			duplex:     contracts.DuplexSimplex,
		},
		{
			name:       "unknown_duplex_mode",
			ctx:        ctx,
			inputPath:  validPDF,
			outputPath: outputPDF,
			copies:     1,
			duplex:     contracts.DuplexMode("TRI_FOLD"),
		},
		{
			name:       "empty_input_path",
			ctx:        ctx,
			inputPath:  "",
			outputPath: outputPDF,
			copies:     1,
			duplex:     contracts.DuplexSimplex,
		},
		{
			name:       "empty_output_path",
			ctx:        ctx,
			inputPath:  validPDF,
			outputPath: "",
			copies:     1,
			duplex:     contracts.DuplexSimplex,
		},
		{
			name:       "same_input_and_output",
			ctx:        ctx,
			inputPath:  validPDF,
			outputPath: validPDF,
			copies:     1,
			duplex:     contracts.DuplexSimplex,
		},
		{
			name:       "nonexistent_input",
			ctx:        ctx,
			inputPath:  filepath.Join(tmpDir, "nonexistent.pdf"),
			outputPath: outputPDF,
			copies:     1,
			duplex:     contracts.DuplexSimplex,
		},
		{
			name:       "directory_as_input",
			ctx:        ctx,
			inputPath:  tmpDir,
			outputPath: outputPDF,
			copies:     1,
			duplex:     contracts.DuplexSimplex,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NormalizeAndCount(tt.ctx, tt.inputPath, tt.outputPath, tt.copies, tt.duplex)
			if err == nil {
				t.Fatalf("expected error for case %q, got nil", tt.name)
			}
		})
	}
}

func generateNPagePDF(t *testing.T, dir string, n int) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("%!PS\n")
	for i := 1; i <= n; i++ {
		sb.WriteString("/Helvetica findfont 24 scalefont setfont\n")
		sb.WriteString("100 700 moveto\n")
		sb.WriteString(strings.ReplaceAll("(Page %d of XeroxGo Test Document) show\nshowpage\n", "%d", strings.TrimSpace(exec.Command("echo", "-n").String())))
		sb.WriteString("showpage\n")
	}

	pdfPath := filepath.Join(dir, "fixture_n_pages.pdf")
	cmd := exec.Command("gs",
		"-sDEVICE=pdfwrite",
		"-sPAPERSIZE=a4",
		"-dFIXEDMEDIA",
		"-dNOPAUSE",
		"-dBATCH",
		"-dQUIET",
		"-sOutputFile="+pdfPath,
		"-c",
	)
	var psCommands strings.Builder
	for i := 1; i <= n; i++ {
		psCommands.WriteString("/Helvetica findfont 24 scalefont setfont 100 700 moveto (Page ")
		psCommands.WriteString(string(rune('0' + i)))
		psCommands.WriteString(") show showpage ")
	}
	cmd.Args = append(cmd.Args, psCommands.String())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to generate %d-page fixture PDF: %s: %v", n, strings.TrimSpace(string(out)), err)
	}
	return pdfPath
}

func TestParsePageRange(t *testing.T) {
	tests := []struct {
		name      string
		rangeStr  string
		maxPages  int
		want      []int
		shouldErr bool
	}{
		{
			name:     "range_1_to_5",
			rangeStr: "1-5",
			maxPages: 5,
			want:     []int{1, 2, 3, 4, 5},
		},
		{
			name:     "discrete_pages_1_3_5",
			rangeStr: "1,3,5",
			maxPages: 5,
			want:     []int{1, 3, 5},
		},
		{
			name:     "mixed_range_and_discrete_1_to_3_and_7",
			rangeStr: "1-3,7",
			maxPages: 10,
			want:     []int{1, 2, 3, 7},
		},
		{
			name:     "whitespace_handling",
			rangeStr: " 1 - 3 ,  7 ",
			maxPages: 10,
			want:     []int{1, 2, 3, 7},
		},
		{
			name:     "all_string",
			rangeStr: "all",
			maxPages: 4,
			want:     []int{1, 2, 3, 4},
		},
		{
			name:     "empty_string_defaults_all",
			rangeStr: "",
			maxPages: 3,
			want:     []int{1, 2, 3},
		},
		{
			name:      "out_of_bounds_single_page",
			rangeStr:  "1-3,7",
			maxPages:  5,
			shouldErr: true,
		},
		{
			name:      "zero_page_invalid",
			rangeStr:  "0-3",
			maxPages:  5,
			shouldErr: true,
		},
		{
			name:      "start_greater_than_end",
			rangeStr:  "5-2",
			maxPages:  5,
			shouldErr: true,
		},
		{
			name:      "malformed_syntax",
			rangeStr:  "1,,3",
			maxPages:  5,
			shouldErr: true,
		},
		{
			name:      "non_numeric",
			rangeStr:  "1-abc",
			maxPages:  5,
			shouldErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePageRange(tt.rangeStr, tt.maxPages)
			if (err != nil) != tt.shouldErr {
				t.Fatalf("ParsePageRange(%q, %d) error = %v, shouldErr = %v", tt.rangeStr, tt.maxPages, err, tt.shouldErr)
			}
			if !tt.shouldErr {
				if len(got) != len(tt.want) {
					t.Fatalf("ParsePageRange got length %d, want %d (%v vs %v)", len(got), len(tt.want), got, tt.want)
				}
				for i := range got {
					if got[i] != tt.want[i] {
						t.Errorf("ParsePageRange index %d = %d, want %d", i, got[i], tt.want[i])
					}
				}
			}
		})
	}
}

func TestNormalizeAndCountWithOptions_OddDuplex3Pages2Copies(t *testing.T) {
	// Requirement 10: 3 pages * 2 copies in duplex = 6 printed sides, 4 physical sheets.
	// Requirement 11: Ensure odd-page duplex documents have a blank back on the final sheet of each copy.
	tmpDir := t.TempDir()
	inputPDF := generateThreePagePDF(t, tmpDir)
	outputPDF := filepath.Join(tmpDir, "duplex_odd_3p_2c.pdf")

	ctx := context.Background()
	res, err := NormalizeAndCountWithOptions(ctx, inputPDF, outputPDF, NormalizeOptions{
		Copies:    2,
		Duplex:    contracts.DuplexLongEdge,
		ColorMode: "BW",
	})
	if err != nil {
		t.Fatalf("NormalizeAndCountWithOptions failed: %v", err)
	}

	if res.TotalPages != 3 {
		t.Errorf("TotalPages = %d, want 3 (printed document pages)", res.TotalPages)
	}
	if res.TotalSheets != 4 {
		t.Errorf("TotalSheets = %d, want 4 (ceil(3/2)*2 physical sheets)", res.TotalSheets)
	}
	if res.PaddedPages != 4 {
		t.Errorf("PaddedPages = %d, want 4 (padded with blank page for duplex boundary)", res.PaddedPages)
	}

	// Verify that the actual output file has 4 pages
	cmd := exec.Command("qpdf", "--show-npages", outputPDF)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("qpdf --show-npages failed: %v", err)
	}
	if strings.TrimSpace(string(out)) != "4" {
		t.Errorf("output file actual page count = %s, want 4", strings.TrimSpace(string(out)))
	}
}

func TestNormalizeAndCountWithOptions_CustomRangeSelection(t *testing.T) {
	tmpDir := t.TempDir()
	inputPDF := generateNPagePDF(t, tmpDir, 5)

	ctx := context.Background()

	// 1. Valid custom range "1,3,5" on 5-page PDF in Simplex with 2 copies
	// Total pages = 3, Total sheets = 3 * 2 = 6 sheets
	outSimplex := filepath.Join(tmpDir, "out_simplex.pdf")
	resSimplex, err := NormalizeAndCountWithOptions(ctx, inputPDF, outSimplex, NormalizeOptions{
		Copies:    2,
		Duplex:    contracts.DuplexSimplex,
		PageRange: "1,3,5",
	})
	if err != nil {
		t.Fatalf("NormalizeAndCountWithOptions (simplex 1,3,5) failed: %v", err)
	}
	if resSimplex.TotalPages != 3 {
		t.Errorf("Simplex TotalPages = %d, want 3", resSimplex.TotalPages)
	}
	if resSimplex.TotalSheets != 6 {
		t.Errorf("Simplex TotalSheets = %d, want 6", resSimplex.TotalSheets)
	}

	// 2. Valid custom range "1,3,5" on 5-page PDF in Duplex with 2 copies
	// Total pages = 3, Total sheets = ceil(3/2) * 2 = 4 sheets, padded to 4 pages
	outDuplex := filepath.Join(tmpDir, "out_duplex.pdf")
	resDuplex, err := NormalizeAndCountWithOptions(ctx, inputPDF, outDuplex, NormalizeOptions{
		Copies:    2,
		Duplex:    contracts.DuplexLongEdge,
		PageRange: "1,3,5",
	})
	if err != nil {
		t.Fatalf("NormalizeAndCountWithOptions (duplex 1,3,5) failed: %v", err)
	}
	if resDuplex.TotalPages != 3 {
		t.Errorf("Duplex TotalPages = %d, want 3", resDuplex.TotalPages)
	}
	if resDuplex.TotalSheets != 4 {
		t.Errorf("Duplex TotalSheets = %d, want 4", resDuplex.TotalSheets)
	}
	if resDuplex.PaddedPages != 4 {
		t.Errorf("Duplex PaddedPages = %d, want 4", resDuplex.PaddedPages)
	}

	// 3. Invalid custom range "1-3,7" on 5-page PDF must be rejected
	outInvalid := filepath.Join(tmpDir, "out_invalid.pdf")
	_, errInvalid := NormalizeAndCountWithOptions(ctx, inputPDF, outInvalid, NormalizeOptions{
		Copies:    1,
		Duplex:    contracts.DuplexSimplex,
		PageRange: "1-3,7",
	})
	if errInvalid == nil {
		t.Fatalf("expected error for range '1-3,7' on 5-page document, got nil")
	}
	if !strings.Contains(errInvalid.Error(), "exceeds document page count") {
		t.Errorf("expected out-of-bounds error message, got: %v", errInvalid)
	}
}

func generateMultiPagePDF(t *testing.T, dir string, pageCount int, mixedOrientation bool) string {
	t.Helper()

	psPath := filepath.Join(dir, fmt.Sprintf("fixture_%d_pages.ps", pageCount))
	pdfPath := filepath.Join(dir, fmt.Sprintf("fixture_%d_pages.pdf", pageCount))

	var sb strings.Builder
	sb.WriteString("%!PS\n")
	for p := 1; p <= pageCount; p++ {
		if mixedOrientation && p%2 == 0 {
			sb.WriteString("<< /PageSize [842 595] >> setpagedevice\n")
			sb.WriteString("/Helvetica findfont 36 scalefont setfont\n")
			sb.WriteString("100 400 moveto\n")
			fmt.Fprintf(&sb, "(Content of Page %d Landscape) show\n", p)
		} else {
			sb.WriteString("<< /PageSize [595 842] >> setpagedevice\n")
			sb.WriteString("/Helvetica findfont 36 scalefont setfont\n")
			sb.WriteString("100 700 moveto\n")
			fmt.Fprintf(&sb, "(Content of Page %d Portrait) show\n", p)
		}
		sb.WriteString("showpage\n")
	}

	if err := os.WriteFile(psPath, []byte(sb.String()), 0600); err != nil {
		t.Fatalf("failed to write PostScript fixture: %v", err)
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
		t.Fatalf("failed to generate %d-page fixture PDF: %s: %v", pageCount, strings.TrimSpace(string(out)), err)
	}

	return pdfPath
}

func extractPageText(t *testing.T, pdfPath string, pageNum int) string {
	t.Helper()
	cmd := exec.Command("pdftotext", "-f", fmt.Sprintf("%d", pageNum), "-l", fmt.Sprintf("%d", pageNum), pdfPath, "-")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pdftotext failed for page %d: %s: %v", pageNum, strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out))
}

func extractPageDimensions(t *testing.T, pdfPath string, pageNum int) (float64, float64) {
	t.Helper()
	cmd := exec.Command("pdfinfo", "-f", fmt.Sprintf("%d", pageNum), "-l", fmt.Sprintf("%d", pageNum), pdfPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pdfinfo failed for page %d: %s: %v", pageNum, strings.TrimSpace(string(out)), err)
	}
	lines := strings.Split(string(out), "\n")
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "Page") && strings.Contains(l, "size:") {
			parts := strings.Fields(l)
			for i, p := range parts {
				if p == "size:" && i+3 < len(parts) {
					w, errW := strconv.ParseFloat(parts[i+1], 64)
					h, errH := strconv.ParseFloat(parts[i+3], 64)
					if errW == nil && errH == nil {
						return w, h
					}
				}
			}
		}
	}
	t.Fatalf("unable to parse page dimensions from pdfinfo output for page %d:\n%s", pageNum, string(out))
	return 0, 0
}

func TestNormalize_NUp1_Orientation(t *testing.T) {
	tmpDir := t.TempDir()
	inputPDF := generateMultiPagePDF(t, tmpDir, 2, false)
	ctx := context.Background()

	// 1. Explicit Portrait orientation
	outPortrait := filepath.Join(tmpDir, "out_1up_portrait.pdf")
	resPort, err := NormalizeAndCountWithOptions(ctx, inputPDF, outPortrait, NormalizeOptions{
		Copies:        1,
		Duplex:        contracts.DuplexSimplex,
		PagesPerSheet: 1,
		Orientation:   "PORTRAIT",
	})
	if err != nil {
		t.Fatalf("1-up Portrait normalization failed: %v", err)
	}
	if resPort.TotalPages != 2 || resPort.TotalSheets != 2 {
		t.Errorf("1-up Portrait: TotalPages=%d, TotalSheets=%d (want 2, 2)", resPort.TotalPages, resPort.TotalSheets)
	}
	wP, hP := extractPageDimensions(t, outPortrait, 1)
	if wP > hP {
		t.Errorf("expected portrait dimensions (width < height), got width=%.2f, height=%.2f", wP, hP)
	}

	// 2. Explicit Landscape orientation
	outLandscape := filepath.Join(tmpDir, "out_1up_landscape.pdf")
	resLand, err := NormalizeAndCountWithOptions(ctx, inputPDF, outLandscape, NormalizeOptions{
		Copies:        1,
		Duplex:        contracts.DuplexSimplex,
		PagesPerSheet: 1,
		Orientation:   "LANDSCAPE",
	})
	if err != nil {
		t.Fatalf("1-up Landscape normalization failed: %v", err)
	}
	if resLand.TotalPages != 2 || resLand.TotalSheets != 2 {
		t.Errorf("1-up Landscape: TotalPages=%d, TotalSheets=%d (want 2, 2)", resLand.TotalPages, resLand.TotalSheets)
	}
	wL, hL := extractPageDimensions(t, outLandscape, 1)
	if wL < hL {
		t.Errorf("expected landscape dimensions (width > height), got width=%.2f, height=%.2f", wL, hL)
	}
}

func TestNormalize_NUp2_SimplexAndDuplex(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()

	// 8-page document in 2-up Simplex
	// 8 content pages / 2-up = 4 output pages, 4 sheets
	input8 := generateMultiPagePDF(t, tmpDir, 8, false)
	out8Simplex := filepath.Join(tmpDir, "out_8p_2up_simplex.pdf")
	res8Simp, err := NormalizeAndCountWithOptions(ctx, input8, out8Simplex, NormalizeOptions{
		Copies:        1,
		Duplex:        contracts.DuplexSimplex,
		PagesPerSheet: 2,
	})
	if err != nil {
		t.Fatalf("8-page 2-up simplex failed: %v", err)
	}
	if res8Simp.TotalPages != 4 {
		t.Errorf("TotalPages = %d, want 4", res8Simp.TotalPages)
	}
	if res8Simp.TotalSheets != 4 {
		t.Errorf("TotalSheets = %d, want 4", res8Simp.TotalSheets)
	}

	// Verify Sheet 1 contains Page 1 and Page 2
	sheet1Text := extractPageText(t, out8Simplex, 1)
	if !strings.Contains(sheet1Text, "Page 1") || !strings.Contains(sheet1Text, "Page 2") {
		t.Errorf("Sheet 1 text missing Page 1 or Page 2, got: %q", sheet1Text)
	}
	// Verify Sheet 2 contains Page 3 and Page 4
	sheet2Text := extractPageText(t, out8Simplex, 2)
	if !strings.Contains(sheet2Text, "Page 3") || !strings.Contains(sheet2Text, "Page 4") {
		t.Errorf("Sheet 2 text missing Page 3 or Page 4, got: %q", sheet2Text)
	}

	// 5-page document in 2-up Duplex
	// 5 content pages / 2-up = 3 content sheet sides
	// Duplex with odd content sides (3) requires 1 blank padding page -> 4 total pages in PDF, 2 physical sheets
	input5 := generateMultiPagePDF(t, tmpDir, 5, false)
	out5Duplex := filepath.Join(tmpDir, "out_5p_2up_duplex.pdf")
	res5Dup, err := NormalizeAndCountWithOptions(ctx, input5, out5Duplex, NormalizeOptions{
		Copies:        1,
		Duplex:        contracts.DuplexLongEdge,
		PagesPerSheet: 2,
	})
	if err != nil {
		t.Fatalf("5-page 2-up duplex failed: %v", err)
	}
	if res5Dup.TotalPages != 3 {
		t.Errorf("TotalPages = %d, want 3", res5Dup.TotalPages)
	}
	if res5Dup.TotalSheets != 2 {
		t.Errorf("TotalSheets = %d, want 2 physical sheets", res5Dup.TotalSheets)
	}
	if res5Dup.PaddedPages != 4 {
		t.Errorf("PaddedPages = %d, want 4 (3 content + 1 blank padding)", res5Dup.PaddedPages)
	}

	// Check Sheet 3 has Page 5
	sheet3Text := extractPageText(t, out5Duplex, 3)
	if !strings.Contains(sheet3Text, "Page 5") {
		t.Errorf("Sheet 3 text missing Page 5, got: %q", sheet3Text)
	}
	// Check Sheet 4 is blank padding
	sheet4Text := extractPageText(t, out5Duplex, 4)
	if sheet4Text != "" {
		t.Errorf("Sheet 4 should be blank duplex padding, got: %q", sheet4Text)
	}
}

func TestNormalize_NUp4_SimplexAndDuplex(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()

	// 8-page document in 4-up Simplex
	// 8 content pages / 4-up = 2 output pages, 2 sheets
	input8 := generateMultiPagePDF(t, tmpDir, 8, false)
	out8 := filepath.Join(tmpDir, "out_8p_4up.pdf")
	res8, err := NormalizeAndCountWithOptions(ctx, input8, out8, NormalizeOptions{
		Copies:        1,
		Duplex:        contracts.DuplexSimplex,
		PagesPerSheet: 4,
	})
	if err != nil {
		t.Fatalf("8-page 4-up failed: %v", err)
	}
	if res8.TotalPages != 2 || res8.TotalSheets != 2 {
		t.Errorf("TotalPages=%d, TotalSheets=%d, want 2, 2", res8.TotalPages, res8.TotalSheets)
	}

	// Sheet 1 must contain Page 1, 2, 3, 4
	s1 := extractPageText(t, out8, 1)
	for p := 1; p <= 4; p++ {
		if !strings.Contains(s1, fmt.Sprintf("Page %d", p)) {
			t.Errorf("Sheet 1 missing Page %d, got: %q", p, s1)
		}
	}
	// Sheet 2 must contain Page 5, 6, 7, 8
	s2 := extractPageText(t, out8, 2)
	for p := 5; p <= 8; p++ {
		if !strings.Contains(s2, fmt.Sprintf("Page %d", p)) {
			t.Errorf("Sheet 2 missing Page %d, got: %q", p, s2)
		}
	}
}

func TestNormalize_NUp6_Simplex(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()

	// 12-page document in 6-up Simplex
	// 12 content pages / 6-up = 2 output pages, 2 sheets
	input12 := generateMultiPagePDF(t, tmpDir, 12, false)
	out12 := filepath.Join(tmpDir, "out_12p_6up.pdf")
	res12, err := NormalizeAndCountWithOptions(ctx, input12, out12, NormalizeOptions{
		Copies:        1,
		Duplex:        contracts.DuplexSimplex,
		PagesPerSheet: 6,
	})
	if err != nil {
		t.Fatalf("12-page 6-up failed: %v", err)
	}
	if res12.TotalPages != 2 || res12.TotalSheets != 2 {
		t.Errorf("TotalPages=%d, TotalSheets=%d, want 2, 2", res12.TotalPages, res12.TotalSheets)
	}

	// Sheet 1 must contain Page 1..6
	s1 := extractPageText(t, out12, 1)
	for p := 1; p <= 6; p++ {
		if !strings.Contains(s1, fmt.Sprintf("Page %d", p)) {
			t.Errorf("Sheet 1 missing Page %d, got: %q", p, s1)
		}
	}
	// Sheet 2 must contain Page 7..12
	s2 := extractPageText(t, out12, 2)
	for p := 7; p <= 12; p++ {
		if !strings.Contains(s2, fmt.Sprintf("Page %d", p)) {
			t.Errorf("Sheet 2 missing Page %d, got: %q", p, s2)
		}
	}
}

func TestNormalize_MixedOrientationInput(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := context.Background()

	// 6-page document with alternating Portrait and Landscape pages
	mixedPDF := generateMultiPagePDF(t, tmpDir, 6, true)
	outMixed := filepath.Join(tmpDir, "out_mixed_2up.pdf")

	resMixed, err := NormalizeAndCountWithOptions(ctx, mixedPDF, outMixed, NormalizeOptions{
		Copies:        1,
		Duplex:        contracts.DuplexSimplex,
		PagesPerSheet: 2,
	})
	if err != nil {
		t.Fatalf("Mixed orientation 2-up failed: %v", err)
	}
	if resMixed.TotalPages != 3 || resMixed.TotalSheets != 3 {
		t.Errorf("TotalPages=%d, TotalSheets=%d, want 3, 3", resMixed.TotalPages, resMixed.TotalSheets)
	}

	// Check text of Sheet 1: has Page 1 (portrait) and Page 2 (landscape)
	s1 := extractPageText(t, outMixed, 1)
	if !strings.Contains(s1, "Page 1") || !strings.Contains(s1, "Page 2") {
		t.Errorf("Sheet 1 missing Page 1 or Page 2, got: %q", s1)
	}
}

func TestNormalize_InvalidNUpAndOrientation(t *testing.T) {
	tmpDir := t.TempDir()
	inputPDF := generateMultiPagePDF(t, tmpDir, 4, false)
	ctx := context.Background()

	// Invalid N-up (3 on 1)
	_, err3 := NormalizeAndCountWithOptions(ctx, inputPDF, filepath.Join(tmpDir, "out3.pdf"), NormalizeOptions{
		Copies:        1,
		Duplex:        contracts.DuplexSimplex,
		PagesPerSheet: 3,
	})
	if err3 == nil || !strings.Contains(err3.Error(), "invalid pages_per_sheet") {
		t.Errorf("expected error for N-up=3, got: %v", err3)
	}

	// Invalid N-up (5 on 1)
	_, err5 := NormalizeAndCountWithOptions(ctx, inputPDF, filepath.Join(tmpDir, "out5.pdf"), NormalizeOptions{
		Copies:        1,
		Duplex:        contracts.DuplexSimplex,
		PagesPerSheet: 5,
	})
	if err5 == nil || !strings.Contains(err5.Error(), "invalid pages_per_sheet") {
		t.Errorf("expected error for N-up=5, got: %v", err5)
	}

	// Invalid orientation ("DIAGONAL")
	_, errOrient := NormalizeAndCountWithOptions(ctx, inputPDF, filepath.Join(tmpDir, "out_diag.pdf"), NormalizeOptions{
		Copies:        1,
		Duplex:        contracts.DuplexSimplex,
		PagesPerSheet: 1,
		Orientation:   "DIAGONAL",
	})
	if errOrient == nil || !strings.Contains(errOrient.Error(), "invalid orientation") {
		t.Errorf("expected error for Orientation=DIAGONAL, got: %v", errOrient)
	}
}
